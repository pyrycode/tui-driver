// Spike: determine claude's number-select modal commit semantics under a
// TWO-QUEUED-MODAL scenario — does the digit alone commit a permission
// modal, or is the trailing `\r` the commit byte? (ticket #199, split from
// #165; blocks the fix #200).
//
// The safety question: `Answer("1")` writes the two bytes `1\r` in one
// atomic PTY write (keys.go). If two permission modals queue (B behind A)
// AND the digit alone commits A (dismissing it before the `\r` lands), the
// trailing `\r` could reach the freshly-surfaced modal B and accept its
// highlighted default — a grant nobody issued. Whether that exposure is
// real turns on one empirically-undetermined fact:
//
//   - digit alone commits: the `\r` is a redundant, dangerous byte that can
//     leak onto modal B → #200 must reorder / drop the trailing `\r`.
//   - digit-then-`\r` needed: `1\r` is atomic-and-safe — A consumes its own
//     `\r`, nothing reaches B → #200 is a regression test.
//
// Spike #13 (cmd/spike-permission) established the approve keystroke is
// `1\r` and that a bare `\r` alone commits the highlighted default, but it
// DELIBERATELY never drove two queued modals ("never trigger the same tool
// twice and expect both modals to fire", docs/knowledge/codebase/13.md).
// This spike closes that gap.
//
// It is a LIVE-CLAUDE observation spike: the answer only surfaces on the
// operator/CI `make e2e` path, never in the developer's claude-free
// `make check` gate. The developer ships the harness + the deterministic
// classifier (unit-tested here) + a README skeleton with an
// `Empirical result: TBD` placeholder. An operator runs `make e2e`, reads
// the `OBSERVED:` line, and transcribes the finding into the README — that
// recorded finding is the sole input that scopes #200. The developer must
// NOT invent a finding.
//
// Experiment flow (single session, single probe):
//
//	spawn claude → wait idle → handle trust modal
//	→ type a prompt engineered to induce TWO distinct parallel tool calls
//	  (Bash + Read) in one assistant turn
//	→ tail JSONL (the authoritative queuing disambiguator)
//	→ wait for permission modal A; snapshot A (0600); record A's signature
//	→ answer A via Answer("1") — the exact sealed `1\r` under test
//	→ observe B's fate while draining JSONL to end_turn:
//	    · a DISTINCT permission modal appears → B survived A's `\r`
//	      (answer it via AnswerModal — here the confirm is meaningful)
//	    · no modal, turn proceeds             → B not visible
//	→ feed the accumulated observation into the pure classifier
//	→ print OBSERVED: <finding>
//
// The classifier (classify, below) is the testable heart: the live
// observation is stochastic, but its interpretation is deterministic and
// unit-tested (belt-and-suspenders — different fabric).
//
// Spike-quality: single binary, no public API. Reuses only already-shipped
// pkg/tuidriver/ primitives; introduces no new exported symbol. Scaffolding
// modeled line-for-line on cmd/spike-ask-user/main.go.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pyrycode/tui-driver/pkg/tuidriver"
)

const (
	sessionFileWait = 10 * time.Second

	// ptyQuietLimit: a permission modal is a PTY-quiet state (claude waits
	// for input), so the PTY-heartbeat watchdog arm must not fire while a
	// modal sits unanswered. 120 s (spike-ask-user finding #25) gives ample
	// headroom over the sub-second window between a modal rendering and the
	// spike answering it.
	ptyQuietLimit = 120 * time.Second
	// spinnerFreezeLimit is a retained no-op — the spinner-freeze arm was
	// retired in #164 (CLAUDE.md). Any value; it is not armed.
	spinnerFreezeLimit = 30 * time.Second
	shutdownGrace      = 3 * time.Second

	// modalDetectLimit bounds the wait for modal A. If claude does not
	// surface even ONE permission modal within this window, the two-tool
	// prompt failed to trigger the setup — a hard failure, not a finding.
	modalDetectLimit = 60 * time.Second

	// endTurnLimit bounds the post-answer drain to end_turn. A true hang is
	// also caught by the 120 s PTY-quiet watchdog; this fires first.
	endTurnLimit = 60 * time.Second

	// settleWindow: after answering A, give claude a moment to either
	// surface modal B (safe case) or auto-consume it (dangerous case) before
	// the observation loop starts. JSONL events buffer in the tail channel
	// during this window (cap 32; this short turn stays well under).
	settleWindow = 1500 * time.Millisecond

	// bWatchPoll is the PTY re-read cadence while watching for modal B and
	// draining JSONL.
	bWatchPoll = 200 * time.Millisecond
)

// defaultPrompt biases claude toward two independent, permission-gated tool
// calls in ONE assistant message. Both tools are known permission-modal
// triggers per #13 — Bash (Probes 1/2) and Read (Probe 3). Reproduction of
// parallel queuing is itself an observed variable: if claude issues the
// calls sequentially, parallelToolUse is false and the run is honestly
// inconclusive (README notes prompt-tuning as a follow-up, not a faked
// result). Overridable via -prompt.
const defaultPrompt = "In a single step, do these two independent things at once: (1) use the Bash tool to run the shell command `echo queued-modal-probe`, and (2) use the Read tool to read the file `/etc/hostname`. Issue both tool calls together, not one after the other."

// observation is the stochastic live measurement the deterministic
// classifier consumes. Every field is derived from a shipped primitive:
// modalBObserved from DetectModalClass + ParseModalContent, parallelToolUse
// / toolsExecuted from JSONL content blocks, modalsAnswered from the spike's
// own answer count.
type observation struct {
	modalBObserved  bool // a DISTINCT permission modal was present after A's `1\r`
	parallelToolUse bool // some assistant message carried >=2 tool_use blocks
	modalsAnswered  int  // modals the spike explicitly answered (1 or 2)
	toolsExecuted   int  // tool_result blocks seen in JSONL
}

// finding is the epistemic conclusion the spike records. Exactly one of
// these three strings is printed on the OBSERVED line and transcribed into
// the README, and it is the sole input that scopes #200.
type finding string

const (
	// findingSafe: B survived A's `1\r` → commit is digit-then-`\r`; A
	// consumed its own `\r`; the trailing byte did not reach B. #200 → a
	// regression test.
	findingSafe finding = "safe-atomic-digit-cr"
	// findingDangerous: a tool executed that the spike never explicitly
	// approved → B's default was auto-accepted by A's leaked `\r`. Digit
	// alone commits. #200 → remove/reorder the trailing `\r`.
	findingDangerous finding = "dangerous-digit-commits-cr-leaks"
	// findingInconclusive: modals did not queue in parallel, or the queued
	// outcome is ambiguous. Commit semantics under queuing UNDETERMINED —
	// must NOT be read as "safe". A single-modal run cannot distinguish the
	// safe case from a failed setup.
	findingInconclusive finding = "inconclusive-no-queuing"
)

// classify maps an observation to a finding. Pure and deterministic — the
// table test's subject. The stochastic part (whether claude queued two
// modals, whether B survived) lives in the harness; the interpretation of
// what was seen is fixed here so a live run cannot smuggle a wrong
// conclusion past review.
//
// Decision table (each row is a test case):
//
//	!parallelToolUse                                        → inconclusive
//	parallelToolUse && modalBObserved                       → safe
//	parallelToolUse && !modalBObserved && tools > answered  → dangerous
//	parallelToolUse && !modalBObserved && tools <= answered → inconclusive
func classify(o observation) finding {
	if !o.parallelToolUse {
		// Modals did not queue (single tool, or sequential across messages).
		// Commit semantics under queuing were never exercised.
		return findingInconclusive
	}
	if o.modalBObserved {
		// B was still on screen after A's `1\r`: the `\r` did not commit it.
		return findingSafe
	}
	if o.toolsExecuted > o.modalsAnswered {
		// B vanished AND a tool ran that the spike never approved: its
		// default was auto-accepted by the leaked `\r`.
		return findingDangerous
	}
	// Queued but ambiguous (e.g. B rejected, turn cancelled). Record honestly.
	return findingInconclusive
}

// meaningFor200 is the one-line human gloss printed beside the finding so an
// operator transcribing the OBSERVED line into the README carries the
// consequence for #200, not just the enum.
func meaningFor200(f finding) string {
	switch f {
	case findingSafe:
		return "commit is digit-then-CR; `1\\r` is atomic-and-safe; #200 collapses to a regression test"
	case findingDangerous:
		return "digit alone commits; the trailing `\\r` leaked onto modal B; #200 must remove/reorder it"
	default:
		return "two queued modals not reproduced (or ambiguous); commit semantics UNDETERMINED — not 'safe'"
	}
}

func main() {
	promptFlag := flag.String("prompt", defaultPrompt,
		"prompt to send; should induce two distinct parallel tool calls (Bash + Read) in one turn")
	trustFolderFlag := flag.String("trust-folder", "fail",
		"policy when claude's trust-folder dialog appears at idle: 'fail' (default) or 'accept'")
	flag.Parse()

	if *trustFolderFlag != "fail" && *trustFolderFlag != "accept" {
		fmt.Fprintf(os.Stderr, "invalid -trust-folder value %q (want 'fail' or 'accept')\n", *trustFolderFlag)
		os.Exit(2)
	}

	if err := run(*promptFlag, *trustFolderFlag); err != nil {
		fmt.Fprintf(os.Stderr, "spike failed: %v\n", err)
		os.Exit(1)
	}
}

func run(prompt, trustFolderPolicy string) error {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	startedAt := time.Now()

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve cwd: %w", err)
	}
	sessionID := uuid.NewString()
	jsonlPath := tuidriver.SessionJSONLPath(home, cwd, sessionID)
	logger.Printf("session-id-resolved id=%s jsonl=%s", sessionID, jsonlPath)

	rootCtx, cancelCause := context.WithCancelCause(context.Background())
	defer cancelCause(errors.New("run: returning"))

	tr := tuidriver.NewTracker(tuidriver.TrackerOpts{
		PTYQuietLimit:      ptyQuietLimit,
		SpinnerFreezeLimit: spinnerFreezeLimit,
	})
	tr.RecordTransition("start")

	// Drop --permission-mode bypassPermissions so the permission modals
	// actually fire (as spike-permission does). Let claude pick its default.
	cmd := exec.Command("claude", "--session-id", sessionID)
	tuidriver.EnsureClaudeEnv(cmd)

	session, err := tuidriver.Spawn(cmd, tuidriver.SpawnOpts{
		MirrorStderr:  true,
		ShutdownGrace: shutdownGrace,
	})
	if err != nil {
		return fmt.Errorf("pty.Start: %w", err)
	}
	defer func() {
		logger.Printf("shutdown-signalled")
		_ = session.Close()
		cancelCause(errors.New("shutdown"))
	}()
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := session.RunWatchdog(rootCtx, tr, tuidriver.WatchdogOpts{}); err != nil {
			logger.Printf("%v", err)
			cancelCause(err)
		}
	}()

	if err := tuidriver.WaitUntil(rootCtx, func() bool { return tuidriver.IsIdle(session.Snapshot()) }); err != nil {
		return fmt.Errorf("wait idle: %w", err)
	}
	tr.RecordTransition("idle-detected")
	logger.Printf("idle-detected")

	if tuidriver.HasTrustModal(session.Snapshot()) {
		switch trustFolderPolicy {
		case "fail":
			return fmt.Errorf("claude shows the trust-folder dialog — pass `-trust-folder accept` to auto-trust this cwd")
		case "accept":
			if err := session.AcceptTrust(); err != nil {
				return fmt.Errorf("write trust-accept: %w", err)
			}
			tr.RecordTransition("trust-folder-accepted")
			logger.Printf("trust-folder-accepted")
			if err := tuidriver.WaitUntil(rootCtx, func() bool {
				snap := session.Snapshot()
				return !tuidriver.HasTrustModal(snap) && tuidriver.IsIdle(snap)
			}); err != nil {
				return fmt.Errorf("wait idle post-trust: %w", err)
			}
		}
	}

	// Deliver the prompt char-by-char with a trailing `\r` (TypePrompt). A
	// bulk write drops the submit keystroke against some claude builds
	// (docs/knowledge/codebase/9.md); every spike uses this convention.
	if err := session.ClearInputLine(); err != nil {
		return fmt.Errorf("clear input line: %w", err)
	}
	if err := session.TypePrompt(prompt); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	tr.RecordTransition("prompt-written")
	logger.Printf("prompt-written prompt=%q", prompt)

	// Open the JSONL event stream. Interactive claude under --session-id
	// defers JSONL creation until the first input lands, so this runs AFTER
	// prompt-written; TailJSONL reads from offset 0, so no early event is
	// missed. JSONL is the authoritative queuing disambiguator (permission
	// modals have zero JSONL footprint, so parallelToolUse can only be read
	// from the assistant tool_use blocks that arrive post-answer).
	jsonlCtx, jsonlCancel := context.WithTimeout(rootCtx, sessionFileWait)
	jsonlErr := tuidriver.WaitForSessionJSONL(jsonlCtx, jsonlPath)
	jsonlCancel()
	if jsonlErr != nil {
		return fmt.Errorf("open session jsonl: %w", jsonlErr)
	}
	logger.Printf("session-jsonl-opened path=%s", jsonlPath)

	eventCh, terr := tuidriver.TailJSONL(rootCtx, jsonlPath, 0)
	if terr != nil {
		return fmt.Errorf("open events stream: %w", terr)
	}

	// --- Wait for modal A (bounded). Never appearing is a HARD failure: the
	// two-tool prompt failed to trigger even one permission modal, so the
	// harness assumptions are broken — not an inconclusive finding. ---
	deadline := time.Now().Add(modalDetectLimit)
	for tuidriver.DetectModalClass(session.Snapshot()) != tuidriver.ModalClassPermission {
		select {
		case <-rootCtx.Done():
			return context.Cause(rootCtx)
		case <-time.After(bWatchPoll):
			if time.Now().After(deadline) {
				return fmt.Errorf("modal A (permission) not seen within %s — the two-tool prompt did not trigger a permission modal", modalDetectLimit)
			}
		}
	}
	tr.RecordTransition("modal-a-detected")
	logger.Printf("modal-a-detected")

	// Snapshot A (0600) and record A's signature so a same-class successor
	// can be told apart from A itself. A's on-screen "Do you want to
	// proceed?" Prompt is IDENTICAL for Bash and Read modals, so the
	// signature is built from Title + option labels (their command/path text
	// differs). Diagnostic comparison only — never routes a grant, so using
	// the tool-influenceable Label here is fine for a spike (#146/#147
	// forbid Label routing on the PRODUCTION grant path, not a screen diff).
	snapA := session.Snapshot()
	writeSnapshot(logger, "modalA", snapA)
	aSig := modalSignature(snapA)
	logger.Printf("modal-a-signature sig=%q", aSig)

	// --- Answer A via Answer("1") — the exact sealed 2-byte `1\r` keystroke
	// under test. NOT SendKeys (raw hatch) and NOT AnswerModal: AnswerModal's
	// dismissal confirm reads DetectModalClass, which still reports
	// `permission` when a same-class B surfaces, so it would return a
	// confounded "still present" error even though A committed (answer.go /
	// #147). The spike does its OWN B-vs-A observation below instead. ---
	if err := session.Answer("1"); err != nil {
		return fmt.Errorf("answer modal A: %w", err)
	}
	modalsAnswered := 1
	tr.RecordTransition("modal-a-answered")
	logger.Printf("modal-a-answered keystroke=%q bytes=31 0d", "1\\r")

	// Settle: let B render (safe case) or auto-consume (dangerous case).
	select {
	case <-rootCtx.Done():
		return context.Cause(rootCtx)
	case <-time.After(settleWindow):
	}

	// --- Post-answer-A loop: drain JSONL to end_turn while watching the PTY
	// for a DISTINCT permission modal B. Answer B once (via AnswerModal —
	// here no successor is expected, so its class-level dismissal confirm is
	// sound and this exercises the production answer surface). ---
	var (
		parallelToolUse bool
		toolsExecuted   int
		modalBObserved  bool
		gotEndTurn      bool
		distinctTools   = map[string]bool{}
	)

	tr.RecordTransition("observe-b-start")
	ticker := time.NewTicker(bWatchPoll)
	defer ticker.Stop()
	drainDeadline := time.Now().Add(endTurnLimit)

	for !gotEndTurn {
		select {
		case <-rootCtx.Done():
			return context.Cause(rootCtx)
		case ev, ok := <-eventCh:
			if !ok {
				eventCh = nil
				continue
			}
			switch ev.Type {
			case "assistant":
				if n, names := toolUseBlocks(ev); n > 0 {
					if n >= 2 {
						if !parallelToolUse {
							logger.Printf("parallel-tool-use-observed blocks=%d names=%v", n, names)
						}
						parallelToolUse = true
					}
					for _, nm := range names {
						distinctTools[nm] = true
					}
				}
				if tuidriver.IsEndTurn(ev) {
					gotEndTurn = true
				}
			case "user":
				if n := toolResultBlocks(ev); n > 0 {
					toolsExecuted += n
					logger.Printf("tool-result-observed count=%d total=%d", n, toolsExecuted)
				}
			}
		case <-ticker.C:
			// Watch the PTY for modal B until we have answered it once. A
			// permission modal whose signature DIFFERS from A's is B; a
			// matching signature is A not yet cleared (keep polling).
			if !modalBObserved && tuidriver.DetectModalClass(session.Snapshot()) == tuidriver.ModalClassPermission {
				snapB := session.Snapshot()
				if bSig := modalSignature(snapB); bSig != "" && bSig != aSig {
					modalBObserved = true
					writeSnapshot(logger, "modalB", snapB)
					logger.Printf("modal-b-detected sig=%q", bSig)
					// Answer B via the production surface; confirm is meaningful
					// here (no further successor expected). A confirm error is
					// non-fatal — the observation above is authoritative.
					if err := session.AnswerModal(rootCtx, tuidriver.AnswerModalOpts{
						Class:  tuidriver.ModalClassPermission,
						Choice: 1,
					}); err != nil {
						logger.Printf("modal-b-answer confirm-error (non-fatal): %v", err)
					}
					modalsAnswered = 2
					tr.RecordTransition("modal-b-answered")
				}
			}
			if time.Now().After(drainDeadline) {
				return fmt.Errorf("end_turn not seen within %s after answering modal A", endTurnLimit)
			}
		}
	}
	tr.RecordTransition("end-turn-detected")

	obs := observation{
		modalBObserved:  modalBObserved,
		parallelToolUse: parallelToolUse,
		modalsAnswered:  modalsAnswered,
		toolsExecuted:   toolsExecuted,
	}
	f := classify(obs)

	logger.Printf("complete elapsed=%s finding=%s modal_b_observed=%v parallel_tool_use=%v modals_answered=%d tools_executed=%d distinct_tools=%d",
		time.Since(startedAt).Round(time.Millisecond), f, obs.modalBObserved, obs.parallelToolUse, obs.modalsAnswered, obs.toolsExecuted, len(distinctTools))

	// The OBSERVED line is the operator's transcription source and the
	// e2e-runner success marker (observedSuccess = ^OBSERVED). It carries the
	// finding, the raw observation, and the one-line consequence for #200.
	fmt.Printf("OBSERVED: finding=%s meaning=%q modal_b_observed=%v parallel_tool_use=%v modals_answered=%d tools_executed=%d distinct_tools=%d\n",
		f, meaningFor200(f), obs.modalBObserved, obs.parallelToolUse, obs.modalsAnswered, obs.toolsExecuted, len(distinctTools))
	return nil
}

// toolUseBlocks returns how many tool_use content blocks an assistant JSONL
// entry carries and the tool names among them. A SINGLE assistant message
// carrying >=2 tool_use blocks is claude genuinely queuing parallel tool
// calls (the queued-B precondition this spike must confirm) — as opposed to
// two tools issued sequentially across separate messages, which does not
// queue two modals.
func toolUseBlocks(ev tuidriver.JSONLEntry) (int, []string) {
	if ev.Message == nil {
		return 0, nil
	}
	n := 0
	var names []string
	for _, c := range ev.Message.Content {
		if c.Type != "tool_use" {
			continue
		}
		n++
		if name, _ := c.Raw["name"].(string); name != "" {
			names = append(names, name)
		}
	}
	return n, names
}

// toolResultBlocks counts tool_result content blocks in a user JSONL entry —
// each marks one tool claude actually executed. In the dangerous case a tool
// runs that the spike never explicitly approved (B's default was
// auto-accepted by A's leaked `\r`), so toolsExecuted outruns modalsAnswered.
func toolResultBlocks(ev tuidriver.JSONLEntry) int {
	if ev.Message == nil {
		return 0
	}
	n := 0
	for _, c := range ev.Message.Content {
		if c.Type == "tool_result" {
			n++
		}
	}
	return n
}

// modalSignature builds a stable, content-based fingerprint of a permission
// modal from its Title + option labels, so a same-class successor B can be
// told apart from A. Returns "" when snap is not a parseable permission /
// trust modal. NOTE: the modal's Prompt line ("Do you want to proceed?") is
// identical across Bash and Read modals, so Prompt alone cannot discriminate
// A from B — the tool/command text lives in Title and the option labels.
// This is a diagnostic screen-diff, never a grant route, so reading the
// tool-influenceable Label here is safe (unlike the production answer path,
// which #146/#147 pin to Index only).
func modalSignature(snap []byte) string {
	mc := tuidriver.ParseModalContent(snap)
	if mc == nil {
		return ""
	}
	parts := make([]string, 0, len(mc.Options)+1)
	parts = append(parts, mc.Title)
	for _, o := range mc.Options {
		parts = append(parts, o.Label)
	}
	return strings.Join(parts, "\x1f") // unit separator: never appears in labels
}

// writeSnapshot persists a raw PTY snapshot to a 0600-mode tempfile for
// byte-level inspection (defensive against multi-user systems, per the #13
// security review — NOT os.WriteFile's default 0644). A write failure is
// logged, not fatal: the snapshot is diagnostic, not load-bearing.
func writeSnapshot(logger *log.Logger, tag string, snap []byte) {
	path := fmt.Sprintf("/tmp/spike-queued-modals-%s-bytes-%d.bin", tag, time.Now().UnixNano())
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		logger.Printf("warning: %s snapshot-write err=%v", tag, err)
		return
	}
	defer f.Close()
	if _, err := f.Write(snap); err != nil {
		logger.Printf("warning: %s snapshot-write err=%v", tag, err)
		return
	}
	logger.Printf("%s-snapshot path=%s len=%d", tag, path, len(snap))
}

// Spike: drive three probes against interactive `claude` to surface
// permission-prompt modal behavior end-to-end across two `claude` sessions.
//
// Builds on cmd/spike-cancel (the cancellation + recovery path). The new
// empirical surface this spike validates:
//
//  1. Modal PTY shape — box-drawing chars, ANSI color codes, layout.
//  2. Modal JSONL signaling — anything? a new envelope type? a new
//     stop_reason? or nothing at all (purely a TUI affordance).
//  3. Approve keystroke — `1\r`, `y\r`, bare `\r`, or down-arrow + `\r`?
//     Iterated via the -approve-keystroke flag.
//  4. Post-response state machine — does claude resume the tool call
//     immediately and finish the assistant turn, or branch elsewhere?
//  5. Extractability for escalation — modal text + tool name. How much
//     can the spike pull out of the rolling buffer for a hypothetical
//     consumer callback?
//
// Two-session orchestration (per spec § Two-session orchestration):
//
//  Session A: Probe 1 — observe modal, capture bytes + extracted text,
//             log any JSONL events for observationWindow, exit. Shutdown
//             SIGTERMs claude with the modal still open.
//  Session B: Probe 2 — same trigger, send approve keystroke, drive turn
//             to SUCCESS. Then Probe 3 — trigger another modal (with a
//             different tool to bypass any "remembered" approval), log
//             what an ACP escalation callback would receive, sleep
//             escalationWindow to verify the modal stays open, exit.
//
// Modal detection has two predicate variants (selectable via
// -modal-predicate flag, default literal-text):
//
//  literal-text: substring-match against canonical permission-modal text
//                ("Doyouwanttoproceed", "Esc to cancel"). False-positive
//                free at idle (input box doesn't contain these phrases).
//  box-drawing:  any ╭/╰/│/╮/╯/┌/└/├ in the stripped buffer. Cheap but
//                false-positive at idle (claude's input box uses these
//                chars too — verified at -idle-predicate-check time).
//
// Spike-quality: single binary, no public API. Helpers copy-pasted from
// cmd/spike-cancel (attributed inline) — kept here until library
// extraction lands. See cmd/spike-permission/README.md for the
// empirical log.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pyrycode/tui-driver/pkg/tuidriver"
)

const (
	statePollInterval = 50 * time.Millisecond
	sessionFileWait   = 10 * time.Second

	// ptyQuietLimit: universal liveness watchdog. Fires when no new PTY
	// bytes have arrived for this long, regardless of which "phase" of a
	// probe we're in. Replaces the prior stack of state-transition-based
	// ceilings (inactivityLimit / modalClearedLimit / endTurnAfterApprove)
	// which formed a cascade where each fix unmasked the next-tightest.
	// PTY bytes flowing == claude alive: spinner counter incrementing,
	// tool output streaming, ⏺ status redraws, etc. State transitions are
	// discrete milestones, not heartbeats — wrong primitive for streaming.
	// 30 s is well above any observed PTY-quiet gap during normal activity.
	// See findings #19-#24 + Open Questions architectural section.
	//
	// No wall-clock cap — empirically the wall-cap was a 100% false-positive
	// in loop 2 exp B-1 (4/10 runs fired during genuine long-streaming).
	// The user can Ctrl-C the spike at any point if a session runs longer
	// than they want to wait. PTY-quiet catches real wedges; spinner-freeze
	// catches thinking-stalls; that's enough. Library extraction should
	// surface elapsed-time as a metric the consumer reacts to, not as a
	// hard auto-fail.
	ptyQuietLimit = 30 * time.Second

	spinnerFreezeLimit = 30 * time.Second
	shutdownGrace      = 3 * time.Second

	disappearedWindow = 500 * time.Millisecond

	// ptyQuietWindow: post-approve readiness predicate window. The
	// readiness predicate in runAutoRespond — `gotEndTurn ∧ ❯-present ∧
	// rb.QuietFor() ≥ ptyQuietWindow` — fires once claude has stopped
	// emitting bytes for this long. PTY-quiescence is used in place of
	// `tuidriver.IsIdle`'s spinner-absent clause because a `✻ … for Ns`
	// spinner glyph painted during tool execution sits in the 4 KB
	// rolling buffer (pkg/tuidriver/buffer.go:8-13) and the small
	// post-end_turn redraw doesn't push it out, wedging IsIdle forever.
	// Quiescence detects "claude has finished" directly.
	//
	// 1500 ms is the empirically-calibrated window. Derivation:
	// cmd/spike-cancel/main.go:82-91 (the original PTY-quiescence
	// consumer in waitReappeared). Same window adopted by the proven
	// sibling at cmd/spike-multi-turn/main.go:353-400 (PR #74) against
	// the same stuck-glyph failure mode. Ticket #70.
	ptyQuietWindow = 1500 * time.Millisecond

	// --- spike-permission constants ---

	// modalDetectLimit: how long each probe waits for the permission modal
	// to appear after prompt-written. Long enough for claude's planning +
	// first tool decision; short enough that a wedged probe doesn't sit.
	modalDetectLimit = 30 * time.Second

	// observationWindow: Probe 1's window for draining JSONL events after
	// modal-detected, per AC.
	observationWindow = 5 * time.Second

	// escalationWindow: Probe 3's window for confirming the modal stays
	// open after the simulated-escalation log lines, per AC.
	escalationWindow = 3 * time.Second

)

// oscRe matches OSC (Operating System Command) sequences — ESC ] ... BEL.
// Claude uses these for window title and similar metadata; they show up
// throughout the PTY stream and must be stripped alongside CSI sequences
// for text extraction to work.
var oscRe = regexp.MustCompile(`\x1b\][^\x07]*\x07`)

// boxDrawingBytes: the Unicode box-drawing codepoints the modal predicate's
// cheap variant searches for. Combined with the input box's own box-drawing
// chars at idle, this predicate has known false-positive risk — captured
// via the `idle-predicate-check has_modal=true` diagnostic line.
//
// Codepoints: ╭ ╮ ╰ ╯ │ ─ ┌ ┐ └ ┘ ├ ┤
var boxDrawingBytes = []byte("╭╮╰╯│─┌┐└┘├┤")

// modalLiteralTexts: substrings the literal-text predicate searches for.
// Any single match fires hasModal.
//
// Claude renders permission modals with two different paths:
//   - Bash tool: text uses CSI cursor-forward (`\x1b[1C`) between words
//     instead of literal spaces. After ansiRe strip the stripped buffer
//     has "Doyouwanttoproceed" with NO spaces between words. Same root
//     cause as spike #1 finding #8 (spinner verb position).
//   - Read tool: text uses literal spaces. After ansiRe strip the
//     stripped buffer has "Do you want to proceed" with normal spacing.
//
// The predicate must match BOTH variants. "Esctocancel" (always no-space
// in both Bash and Read variants — claude renders the hint with CSI
// regardless of tool) is the most universal marker.
var modalLiteralTexts = [][]byte{
	[]byte("Esctocancel"),
	[]byte("Doyouwanttoproceed"),
	[]byte("Do you want to proceed"),
}

// modalSepRe matches a horizontal-separator line of 20+ consecutive ─.
// Used by extractModalText to find the modal's top/bottom borders inside
// the ANSI-stripped buffer (the modal is bordered by long dash runs, not
// box-corner chars alone).
var modalSepRe = regexp.MustCompile(`─{20,}`)

// ProbeKind selects per-probe behavior inside runSession.
type ProbeKind int

const (
	kindObserve ProbeKind = iota
	kindAutoRespond
	kindEscalate
)

func (k ProbeKind) String() string {
	switch k {
	case kindObserve:
		return "observe"
	case kindAutoRespond:
		return "auto-respond"
	case kindEscalate:
		return "escalate"
	}
	return "unknown"
}

// modalPredicate selects the hasModal detection strategy.
type modalPredicate int

const (
	modalPredLiteralText modalPredicate = iota
	modalPredBoxDrawing
)

func (p modalPredicate) String() string {
	switch p {
	case modalPredLiteralText:
		return "literal-text"
	case modalPredBoxDrawing:
		return "box-drawing"
	}
	return "unknown"
}

// probeSpec pairs a kind with the verbatim prompt.
type probeSpec struct {
	kind   ProbeKind
	prompt string
}

func main() {
	sessionIDFlag := flag.String("session-id", "",
		"UUID to pin Session A's session ID and JSONL filename (default: generate one)")
	approveKeystrokeFlag := flag.String("approve-keystroke", "1-enter",
		"approve keystroke to send: 1-enter | y-enter | enter | down-enter")
	modalPredicateFlag := flag.String("modal-predicate", "literal-text",
		"modal-detection predicate: box-drawing | literal-text")
	trustFolderFlag := flag.String("trust-folder", "fail",
		"policy when claude's trust-folder dialog appears at idle: 'fail' (default — return clear error) or 'accept' (send `1\\r` to auto-trust this cwd, then proceed)")
	flag.Parse()

	approveKey, approveHex, err := parseApproveKeystroke(*approveKeystrokeFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		flag.Usage()
		os.Exit(2)
	}

	pred, err := parseModalPredicate(*modalPredicateFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		flag.Usage()
		os.Exit(2)
	}

	if *trustFolderFlag != "fail" && *trustFolderFlag != "accept" {
		fmt.Fprintf(os.Stderr, "invalid -trust-folder value %q (want 'fail' or 'accept')\n", *trustFolderFlag)
		os.Exit(2)
	}

	if err := run(*sessionIDFlag, approveKey, approveHex, pred, *trustFolderFlag); err != nil {
		fmt.Fprintf(os.Stderr, "spike failed: %v\n", err)
		os.Exit(1)
	}
}

func parseApproveKeystroke(name string) (bytes []byte, hexStr string, err error) {
	switch name {
	case "1-enter":
		return []byte{0x31, 0x0d}, "31 0d", nil
	case "y-enter":
		return []byte{0x79, 0x0d}, "79 0d", nil
	case "enter":
		return []byte{0x0d}, "0d", nil
	case "down-enter":
		return []byte{0x1b, 0x5b, 0x42, 0x0d}, "1b 5b 42 0d", nil
	default:
		return nil, "", fmt.Errorf("invalid -approve-keystroke %q: want 1-enter | y-enter | enter | down-enter", name)
	}
}

func parseModalPredicate(name string) (modalPredicate, error) {
	switch name {
	case "literal-text":
		return modalPredLiteralText, nil
	case "box-drawing":
		return modalPredBoxDrawing, nil
	default:
		return 0, fmt.Errorf("invalid -modal-predicate %q: want box-drawing | literal-text", name)
	}
}

// Default probes per spec § Per-probe drivers. Probe 1 uses Bash via "list";
// Probe 2 uses the same to validate end-to-end SUCCESS; Probe 3 uses a
// DIFFERENT tool (Read) to force a fresh permission prompt in Session B.
var (
	probe1Prompt = "list the files in /tmp"
	probe2Prompt = "list the files in /tmp"
	probe3Prompt = "read /etc/hostname"
)

func run(sessionIDFlag string, approveKey []byte, approveHex string, pred modalPredicate, trustFolderPolicy string) error {
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

	rootCtx, cancelCause := context.WithCancelCause(context.Background())
	defer cancelCause(errors.New("run: returning"))

	// Session A: Probe 1 only.
	sessionA, err := resolveSession(sessionIDFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		flag.Usage()
		os.Exit(2)
	}
	jsonlA := tuidriver.SessionJSONLPath(home, cwd, sessionA)
	logger.Printf("session-id-resolved id=%s jsonl=%s tag=a", sessionA, jsonlA)

	if err := runSession(rootCtx, logger, sessionA, jsonlA, "a", 1, approveKey, approveHex, pred, trustFolderPolicy,
		[]probeSpec{{kind: kindObserve, prompt: probe1Prompt}}); err != nil {
		return fmt.Errorf("session A: %w", err)
	}

	// Session B: Probes 2 + 3. Always generate a fresh UUID — the
	// -session-id flag is consumed by Session A only.
	sessionB, err := resolveSession("")
	if err != nil {
		return fmt.Errorf("session B: generate session id: %w", err)
	}
	jsonlB := tuidriver.SessionJSONLPath(home, cwd, sessionB)

	if err := runSession(rootCtx, logger, sessionB, jsonlB, "b", 2, approveKey, approveHex, pred, trustFolderPolicy,
		[]probeSpec{
			{kind: kindAutoRespond, prompt: probe2Prompt},
			{kind: kindEscalate, prompt: probe3Prompt},
		}); err != nil {
		return fmt.Errorf("session B: %w", err)
	}

	logger.Printf("complete elapsed=%s", time.Since(startedAt).Round(time.Millisecond))
	return nil
}

// runSession owns one claude subprocess: PTY allocation, reader goroutine,
// watchdog goroutine, post-prompt JSONL tailer hook, shutdown defer. Iterates
// the supplied probes sequentially; first probe's prompt-written triggers
// the tailer-open hook.
func runSession(
	parentCtx context.Context,
	parentLogger *log.Logger,
	sessionID string,
	jsonlPath string,
	tag string,
	startProbeN int,
	approveKey []byte,
	approveHex string,
	pred modalPredicate,
	trustFolderPolicy string,
	probes []probeSpec,
) error {
	logger := parentLogger

	ctx, cancelCause := context.WithCancelCause(parentCtx)
	defer cancelCause(errors.New("runSession: returning"))

	tr := tuidriver.NewTracker(tuidriver.TrackerOpts{
		PTYQuietLimit:      ptyQuietLimit,
		SpinnerFreezeLimit: spinnerFreezeLimit,
	})
	tr.RecordTransition(fmt.Sprintf("session=%s start", tag))

	// Drop --permission-mode bypassPermissions (spikes #1/#9/#11 used it
	// to skip the modal; we WANT the modal here). Let claude pick its own
	// default permission mode.
	cmd := exec.Command("claude", "--session-id", sessionID)
	tuidriver.EnsureClaudeEnv(cmd)

	session, err := tuidriver.Spawn(cmd, tuidriver.SpawnOpts{
		Mirror:        os.Stderr,
		ShutdownGrace: shutdownGrace,
	})
	if err != nil {
		return fmt.Errorf("pty.Start: %w", err)
	}
	defer func() {
		logger.Printf("shutdown-signalled tag=%s", tag)
		_ = session.Close()
		cancelCause(errors.New("shutdown"))
	}()
	rb := session.Buffer
	ptmx := session.PTY

	var wg sync.WaitGroup

	// Watchdog: PTY-quiet-for-30s liveness + 30s spinner-freeze. No
	// wall-clock cap — long sessions are fine if claude is actually
	// producing; operator can Ctrl-C if they want to stop sooner.
	// PTY-heartbeat replaces the prior state-transition-based stack
	// (inactivityLimit / modalClearedLimit / endTurnAfterApproveLimit)
	// per findings #19-#24 cascade observation.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := tuidriver.RunWatchdog(ctx, rb, tr, tuidriver.WatchdogOpts{}); err != nil {
			logger.Printf("%v", err)
			cancelCause(err)
		}
	}()

	// Wait for idle (❯ glyph + no spinner). Same predicate as the other
	// spikes.
	if err := tuidriver.WaitUntil(ctx, func() bool {
		return tuidriver.IsIdle(rb.Snapshot())
	}); err != nil {
		return fmt.Errorf("wait idle: %w", err)
	}
	tr.RecordTransition(fmt.Sprintf("session=%s idle-detected", tag))
	logger.Printf("idle-detected tag=%s", tag)

	if tuidriver.HasTrustModal(rb.Snapshot()) {
		switch trustFolderPolicy {
		case "fail":
			return fmt.Errorf("claude shows the trust-folder dialog — this cwd hasn't been trusted yet. Run `claude` interactively in this directory once, accept trust, exit, then re-run the spike. Or pass `-trust-folder accept` to auto-trust")
		case "accept":
			if _, err := ptmx.Write([]byte("1\r")); err != nil {
				return fmt.Errorf("write trust-accept keystroke: %w", err)
			}
			tr.RecordTransition(fmt.Sprintf("session=%s trust-folder-accepted", tag))
			logger.Printf("trust-folder-accepted tag=%s bytes=31 0d", tag)
			if err := tuidriver.WaitUntil(ctx, func() bool {
				snap := rb.Snapshot()
				return !tuidriver.HasTrustModal(snap) && tuidriver.IsIdle(snap)
			}); err != nil {
				return fmt.Errorf("wait for idle post-trust-accept: %w", err)
			}
			tr.RecordTransition(fmt.Sprintf("session=%s idle-detected-post-trust", tag))
			logger.Printf("idle-detected-post-trust tag=%s", tag)
		}
	}

	// Diagnostic: does the chosen modal predicate produce a false positive
	// at idle (before any prompt)? The README documents the baseline.
	idleHasModal := hasModal(rb.Snapshot(), pred)
	logger.Printf("idle-predicate-check tag=%s predicate=%s has_modal=%v",
		tag, pred.String(), idleHasModal)

	// Session-scoped JSONL event stream. Populated lazily by openTailerHook
	// (after the first probe writes a prompt, since interactive `claude
	// --session-id` defers JSONL creation until the first input arrives).
	var eventCh <-chan tuidriver.JSONLEntry

	openTailerOnce := sync.Once{}
	openTailerHook := func() error {
		var hookErr error
		openTailerOnce.Do(func() {
			jsonlCtx, jsonlCancel := context.WithTimeout(ctx, sessionFileWait)
			jsonlErr := tuidriver.WaitForSessionJSONL(jsonlCtx, jsonlPath)
			jsonlCancel()
			if jsonlErr != nil {
				hookErr = fmt.Errorf("open session jsonl: %w", jsonlErr)
				return
			}
			logger.Printf("session-jsonl-opened path=%s offset=0 tag=%s", jsonlPath, tag)
			ch, terr := tuidriver.TailJSONL(ctx, jsonlPath, 0)
			if terr != nil {
				hookErr = fmt.Errorf("open events stream: %w", terr)
				return
			}
			eventCh = ch
		})
		return hookErr
	}

	for i, p := range probes {
		probeN := startProbeN + i
		switch p.kind {
		case kindObserve:
			if err := runObserve(ctx, logger, probeN, p.prompt, pred, session, rb, &eventCh, tr, openTailerHook); err != nil {
				return fmt.Errorf("probe %d: %w", probeN, err)
			}
		case kindAutoRespond:
			if err := runAutoRespond(ctx, logger, probeN, p.prompt, approveKey, approveHex, pred, session, rb, &eventCh, tr, openTailerHook); err != nil {
				return fmt.Errorf("probe %d: %w", probeN, err)
			}
		case kindEscalate:
			if err := runEscalate(ctx, logger, probeN, p.prompt, pred, session, rb, &eventCh, tr); err != nil {
				return fmt.Errorf("probe %d: %w", probeN, err)
			}
		default:
			return fmt.Errorf("probe %d: unsupported kind %s", probeN, p.kind.String())
		}
	}

	return nil
}

// runObserve drives Probe 1 (Session A's only probe): trigger modal,
// capture bytes + extracted text, log JSONL events for observationWindow,
// return. The session-level shutdown defer SIGTERMs claude on return,
// leaving the modal open (the spike's whole point).
func runObserve(
	ctx context.Context,
	logger *log.Logger,
	probeN int,
	prompt string,
	pred modalPredicate,
	session *tuidriver.Session,
	rb *tuidriver.Buffer,
	eventChRef *<-chan tuidriver.JSONLEntry,
	tr *tuidriver.Tracker,
	postPromptHook func() error,
) error {
	logger.Printf("probe=%d probe-start kind=%q prompt=%q", probeN, ProbeKind(kindObserve).String(), prompt)
	tr.RecordTransition(fmt.Sprintf("probe=%d probe-start", probeN))

	if err := session.ClearInputLine(); err != nil {
		return fmt.Errorf("clear input line: %w", err)
	}
	if err := session.TypePrompt(prompt); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	tr.RecordTransition(fmt.Sprintf("probe=%d prompt-written", probeN))
	logger.Printf("probe=%d prompt-written", probeN)

	if postPromptHook != nil {
		if err := postPromptHook(); err != nil {
			return err
		}
	}

	eventCh := *eventChRef

	// Wait for the modal to appear.
	if err := waitForModal(ctx, rb, pred, modalDetectLimit); err != nil {
		return fmt.Errorf("probe %d: %w", probeN, err)
	}
	tr.RecordTransition(fmt.Sprintf("probe=%d modal-detected", probeN))
	logger.Printf("probe=%d modal-detected pattern=%q", probeN, pred.String())

	// Snapshot bytes + persist to tempfile + log truncated extracted text.
	snap := rb.Snapshot()
	tr.RecordTransition(fmt.Sprintf("probe=%d modal-bytes-snapshot", probeN))
	logger.Printf("probe=%d modal-bytes-snapshot len=%d", probeN, len(snap))

	snapPath := fmt.Sprintf("/tmp/spike-permission-probe%d-bytes-%d.bin", probeN, time.Now().UnixNano())
	if err := writeTempSnapshot(snapPath, snap); err != nil {
		logger.Printf("warning: probe=%d snapshot-write-err err=%v", probeN, err)
	} else {
		logger.Printf("probe=%d modal-bytes-snapshot-path=%s", probeN, snapPath)
	}

	text := extractModalText(snap)
	tr.RecordTransition(fmt.Sprintf("probe=%d modal-text-extracted", probeN))
	logger.Printf("probe=%d modal-text-extracted text=%q", probeN, truncateForLog(text, 200))

	// Drain JSONL events for observationWindow. Bump tracker once at start
	// and once at end so the watchdog doesn't trip during a 5 s quiet
	// window.
	tr.RecordTransition(fmt.Sprintf("probe=%d observation-window-start", probeN))
	logger.Printf("probe=%d observation-window-start window=%s", probeN, observationWindow)

	count := 0
	deadline := time.After(observationWindow)
loop:
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case ev, ok := <-eventCh:
			if !ok {
				eventCh = nil
				continue
			}
			if ev.Type != "assistant" {
				continue
			}
			logObservationEvent(logger, probeN, ev)
			count++
		case <-deadline:
			break loop
		}
	}

	tr.RecordTransition(fmt.Sprintf("probe=%d observation-window-end", probeN))
	logger.Printf("probe=%d observation-window-end events=%d", probeN, count)
	return nil
}

// runAutoRespond drives Probe 2 (Session B's first probe): trigger modal,
// send approve keystroke, wait for modal-cleared AND end_turn, extract
// assistant text by msg_id, print SUCCESS.
func runAutoRespond(
	ctx context.Context,
	logger *log.Logger,
	probeN int,
	prompt string,
	approveKey []byte,
	approveHex string,
	pred modalPredicate,
	session *tuidriver.Session,
	rb *tuidriver.Buffer,
	eventChRef *<-chan tuidriver.JSONLEntry,
	tr *tuidriver.Tracker,
	postPromptHook func() error,
) error {
	logger.Printf("probe=%d probe-start kind=%q prompt=%q", probeN, ProbeKind(kindAutoRespond).String(), prompt)
	tr.RecordTransition(fmt.Sprintf("probe=%d probe-start", probeN))

	if err := session.ClearInputLine(); err != nil {
		return fmt.Errorf("clear input line: %w", err)
	}
	if err := session.TypePrompt(prompt); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	tr.RecordTransition(fmt.Sprintf("probe=%d prompt-written", probeN))
	logger.Printf("probe=%d prompt-written", probeN)

	if postPromptHook != nil {
		if err := postPromptHook(); err != nil {
			return err
		}
	}

	eventCh := *eventChRef

	if err := waitForModal(ctx, rb, pred, modalDetectLimit); err != nil {
		return fmt.Errorf("probe %d: %w", probeN, err)
	}
	tr.RecordTransition(fmt.Sprintf("probe=%d modal-detected", probeN))
	logger.Printf("probe=%d modal-detected pattern=%q", probeN, pred.String())

	text := extractModalText(rb.Snapshot())
	logger.Printf("probe=%d modal-text-extracted text=%q", probeN, truncateForLog(text, 200))

	if err := sendKeystroke(session, approveKey); err != nil {
		return fmt.Errorf("send approve: %w", err)
	}
	tr.RecordTransition(fmt.Sprintf("probe=%d response-keystroke-sent", probeN))
	logger.Printf("probe=%d response-keystroke-sent bytes=%s", probeN, approveHex)

	// Single post-keystroke loop. Two transitions to log along the way:
	//
	//   modal-cleared: first JSONL `assistant` event after keystroke-sent.
	//     Per finding #15 the modal has ZERO JSONL footprint pre-approve;
	//     the moment claude emits any assistant content it has moved past
	//     the modal. The JSONL tailer pre-filters to type=assistant only
	//     (see tailJSONL), so the first event from eventCh post-keystroke
	//     IS the modal-cleared positive signal — no need for a separate
	//     PTY rolling-buffer `hasModal` check (which was finding #23's
	//     bug: modal text persists in the 4096-byte rolling buffer when
	//     streaming responses don't push it out fast enough).
	//
	//   end-turn-detected: post-approve readiness predicate. Three
	//     load-bearing clauses, all required:
	//       (a) gotEndTurn — JSONL has reported stop_reason=end_turn for
	//           this turn (model done speaking).
	//       (b) ❯ glyph present in StripANSI(rb.Snapshot()) — input
	//           prompt is back on screen.
	//       (c) rb.QuietFor() ≥ ptyQuietWindow (1500 ms) — claude has
	//           stopped emitting bytes; not still redrawing.
	//
	//     The OLD predicate (gotEndTurn ∧ tuidriver.IsIdle ∧ stable
	//     idleStableWindow) wedged on claude 2.1.148: a `✻ Verb for Ns`
	//     spinner glyph painted during tool execution stays in the 4 KB
	//     rolling buffer (pkg/tuidriver/buffer.go:8-13) post-end_turn,
	//     and the small final assistant message + title-bar redraws
	//     don't emit enough bytes to roll it out. IsIdle's spinner-absent
	//     half therefore stays false forever and the watchdog fires
	//     ~30 s later. Ticket #70.
	//
	//     PTY-quiescence subsumes the safety property the spinner-absent
	//     clause was meant to provide: 1500 ms of zero bytes means claude
	//     has by definition stopped redrawing (spinner or anything else),
	//     so further interaction is safe. Strictly stronger than the OLD
	//     predicate on the rendering-activity axis (quiescence cannot be
	//     faked by a transient buffer state); weaker on the spinner-glyph
	//     axis — which is exactly the wedge being removed.
	//
	//     Same predicate shape as cmd/spike-multi-turn/main.go:353-400
	//     (post-#74) and cmd/spike-cancel/main.go:513-568 (the original
	//     PTY-quiescence consumer, waitReappeared). Empirical derivation
	//     of the 1500 ms window: cmd/spike-cancel/main.go:82-91.
	//
	// No per-phase wall-clock deadlines here. Liveness is enforced by
	// the session-level PTY-heartbeat watchdog (tracker.checkWatchdog) —
	// PTY quiet for >ptyQuietLimit fires `cancelCause` and surfaces via
	// ctx.Done. The session wall-cap is the outer safety net.
	var (
		events             []tuidriver.JSONLEntry
		latestEndTurnMsgID string
		gotEndTurn         bool
		modalCleared       bool
	)

	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()

	check := func() bool {
		if !gotEndTurn {
			return false
		}
		stripped := tuidriver.StripANSI(rb.Snapshot())
		if !bytes.Contains(stripped, tuidriver.IdleGlyph) {
			return false
		}
		return rb.QuietFor() >= ptyQuietWindow
	}

	for !check() {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case ev, ok := <-eventCh:
			if !ok {
				eventCh = nil
				continue
			}
			if ev.Type != "assistant" {
				continue
			}
			events = append(events, ev)
			if !modalCleared {
				modalCleared = true
				tr.RecordTransition(fmt.Sprintf("probe=%d modal-cleared", probeN))
				logger.Printf("probe=%d modal-cleared", probeN)
			}
			if tuidriver.IsEndTurn(ev) {
				if id := msgIDOf(ev); id != "" {
					latestEndTurnMsgID = id
				}
				gotEndTurn = true
			}
		case <-ticker.C:
			// No per-phase deadline; PTY-heartbeat watchdog handles
			// "claude wedged" at the tracker level.
		}
	}

	tr.RecordTransition(fmt.Sprintf("probe=%d end-turn-detected", probeN))
	logger.Printf("probe=%d end-turn-detected msg_id=%s", probeN, latestEndTurnMsgID)

	out := extractByMsgID(events, latestEndTurnMsgID)
	tr.RecordTransition(fmt.Sprintf("probe=%d assistant-text-extracted", probeN))
	logger.Printf("probe=%d assistant-text-extracted len=%d", probeN, len(out))

	fmt.Printf("SUCCESS: %s\n", out)
	return nil
}

// runEscalate drives Probe 3 (Session B's second probe): trigger another
// modal (different tool to force a fresh permission prompt), log what a
// consumer callback would receive, sleep escalationWindow, verify the
// modal is still open, return. Session-level shutdown defer SIGTERMs.
func runEscalate(
	ctx context.Context,
	logger *log.Logger,
	probeN int,
	prompt string,
	pred modalPredicate,
	session *tuidriver.Session,
	rb *tuidriver.Buffer,
	eventChRef *<-chan tuidriver.JSONLEntry,
	tr *tuidriver.Tracker,
) error {
	logger.Printf("probe=%d probe-start kind=%q prompt=%q", probeN, ProbeKind(kindEscalate).String(), prompt)
	tr.RecordTransition(fmt.Sprintf("probe=%d probe-start", probeN))

	eventCh := *eventChRef

	// Drain any straggler events from Probe 2 so they don't pollute
	// Probe 3's diagnostics.
drain:
	for {
		select {
		case <-eventCh:
		default:
			break drain
		}
	}

	if err := session.ClearInputLine(); err != nil {
		return fmt.Errorf("clear input line: %w", err)
	}
	if err := session.TypePrompt(prompt); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	tr.RecordTransition(fmt.Sprintf("probe=%d prompt-written", probeN))
	logger.Printf("probe=%d prompt-written", probeN)

	if err := waitForModal(ctx, rb, pred, modalDetectLimit); err != nil {
		return fmt.Errorf("probe %d: %w", probeN, err)
	}
	tr.RecordTransition(fmt.Sprintf("probe=%d modal-detected", probeN))
	logger.Printf("probe=%d modal-detected pattern=%q", probeN, pred.String())

	snap := rb.Snapshot()
	snapPath := fmt.Sprintf("/tmp/spike-permission-probe%d-bytes-%d.bin", probeN, time.Now().UnixNano())
	if err := writeTempSnapshot(snapPath, snap); err != nil {
		logger.Printf("warning: probe=%d snapshot-write-err err=%v", probeN, err)
		logger.Printf("probe=%d modal-bytes-snapshot len=%d", probeN, len(snap))
	} else {
		logger.Printf("probe=%d modal-bytes-snapshot len=%d path=%s", probeN, len(snap), snapPath)
	}

	text := extractModalText(snap)
	tool := extractToolName(snap)
	tr.RecordTransition(fmt.Sprintf("probe=%d modal-text-extracted", probeN))
	logger.Printf("probe=%d modal-text-extracted text=%q", probeN, truncateForLog(text, 200))

	tr.RecordTransition(fmt.Sprintf("probe=%d modal-escalation-callback-would-receive", probeN))
	logger.Printf("probe=%d modal-escalation-callback-would-receive text=%q tool=%s",
		probeN, truncateForLog(text, 200), tool)

	tr.RecordTransition(fmt.Sprintf("probe=%d escalation-simulated", probeN))
	logger.Printf("probe=%d escalation-simulated", probeN)

	// Sleep escalationWindow; afterwards verify the modal is still up.
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-time.After(escalationWindow):
	}

	stillOpen := hasModal(rb.Snapshot(), pred)
	tr.RecordTransition(fmt.Sprintf("probe=%d modal-still-open", probeN))
	logger.Printf("probe=%d modal-still-open=%v", probeN, stillOpen)

	return nil
}

// waitForModal polls hasModal at statePollInterval until true or limit
// elapses. Returns the watchdog-shaped error on timeout.
func waitForModal(ctx context.Context, rb *tuidriver.Buffer, pred modalPredicate, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()
	for {
		if hasModal(rb.Snapshot(), pred) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("modal not detected within %s", limit)
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-ticker.C:
		}
	}
}

// hasModal reports whether the rolling-buffer snapshot currently shows a
// permission modal. Two predicate variants, both ANSI-strip first.
func hasModal(snap []byte, pred modalPredicate) bool {
	stripped := tuidriver.StripANSI(snap)
	switch pred {
	case modalPredBoxDrawing:
		return bytes.ContainsAny(stripped, string(boxDrawingBytes))
	case modalPredLiteralText:
		for _, lit := range modalLiteralTexts {
			if bytes.Contains(stripped, lit) {
				return true
			}
		}
		return false
	}
	return false
}

// extractModalText pulls the visible modal text out of a rolling-buffer
// snapshot.
//
// Strategy: ANSI+OSC strip → find the LAST "Doyouwanttoproceed" → walk
// back to the nearest dash-separator line (modalSepRe) OR up to ~8
// newlines if no separator → walk forward to the "Esctocancel" hint OR
// up to ~8 newlines if no marker → clean per-line border/whitespace.
//
// Two reasons this beats splitting on modalSepRe directly: (1) the Read
// tool's modal doesn't always render the long-dash separator that Bash
// uses, so a Split-based strategy collapses to "whole buffer" and grabs
// the spinner-animation noise above the modal; (2) anchoring on the
// proceed-marker is robust against varying snapshot-vs-modal timing.
func extractModalText(snap []byte) string {
	stripped := oscRe.ReplaceAll(snap, nil)
	stripped = tuidriver.StripANSI(stripped)
	full := string(stripped)

	// Bash modal uses no-space form; Read modal uses spaced form.
	idx := strings.LastIndex(full, "Doyouwanttoproceed")
	if idx < 0 {
		idx = strings.LastIndex(full, "Do you want to proceed")
	}
	if idx < 0 {
		return ""
	}

	// Modal anchor strategy: claude prefixes the modal block with a long
	// horizontal-dash run (`──────...──────`). Find the LAST dash run
	// occurring BEFORE the proceed-marker — everything after that
	// position is the modal content.
	//
	// For Bash modal: dash run separates the tool action description
	// ("Listing 1 directory…(ctrl+o to expand)") from the modal block
	// ("Bash command / ls /tmp / List files in /tmp / Do you want to
	// proceed? / ...").
	//
	// For Read modal: dash run separates the file-list display
	// ("⎿ /etc/hostname") from the modal block ("Read file
	// Read(/etc/hostname) Do you want to proceed?").
	//
	// In both cases the modal anchor is the position immediately AFTER
	// the dash run. Fallback if no dash run is present: start of the
	// marker line.
	dashLocs := modalSepRe.FindAllStringIndex(full[:idx], -1)
	var start int
	if len(dashLocs) > 0 {
		// Position immediately after the last dash run before the marker.
		start = dashLocs[len(dashLocs)-1][1]
	} else {
		// No dash separator — fall back to start of the marker line.
		start = idx
		for start > 0 && stripped[start-1] != '\n' {
			start--
		}
	}

	// Walk forward to "Esctocancel" or up to ~8 newlines past the marker.
	end := idx
	newlines := 0
	for end < len(stripped) {
		if bytes.HasPrefix(stripped[end:], []byte("Esctocancel")) {
			// Include the rest of this line.
			rest := stripped[end:]
			nl := bytes.IndexByte(rest, '\n')
			if nl < 0 {
				end = len(stripped)
			} else {
				end += nl
			}
			break
		}
		if stripped[end] == '\n' {
			newlines++
			if newlines >= 8 {
				break
			}
		}
		end++
	}
	if end > len(stripped) {
		end = len(stripped)
	}

	return cleanModalLines(string(stripped[start:end]))
}

// cleanModalLines: trim per-line whitespace + box-drawing border chars,
// drop empty lines, join with \n.
func cleanModalLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		// Trim leading/trailing whitespace + box-drawing border chars.
		ln = strings.TrimFunc(ln, func(r rune) bool {
			if r == ' ' || r == '\t' || r == '\r' {
				return true
			}
			switch r {
			case '╭', '╮', '╰', '╯', '│', '─', '┌', '┐', '└', '┘', '├', '┤':
				return true
			}
			return false
		})
		if ln == "" {
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// extractToolName: best-effort. Look for known tool name tokens in the
// modal region; return the first match. Empty string if none found.
func extractToolName(snap []byte) string {
	stripped := tuidriver.StripANSI(snap)
	tools := []string{"Bash", "Read", "Write", "Edit", "WebFetch", "WebSearch", "Glob", "Grep", "Task"}
	// Restrict the search to the modal region (last ~1 KB before the
	// "Doyouwanttoproceed" question).
	idx := bytes.LastIndex(stripped, []byte("Doyouwanttoproceed"))
	if idx < 0 {
		idx = bytes.LastIndex(stripped, []byte("Do you want to proceed"))
	}
	region := stripped
	if idx >= 0 {
		start := idx - 1024
		if start < 0 {
			start = 0
		}
		region = stripped[start:idx]
	}
	for _, t := range tools {
		if bytes.Contains(region, []byte(t)) {
			return t
		}
	}
	return ""
}

// sendKeystroke writes the keystroke bytes as a single bulk write. The
// keystroke is at most a few bytes (approve: "1\r" / "y\r" / "\r" /
// "\x1b[B\r"). Renamed from spike-cancel's sendCancel because this spike
// uses the same writer for the approve keystroke too.
func sendKeystroke(session *tuidriver.Session, keystroke []byte) error {
	_, err := session.Write(keystroke)
	return err
}

// writeTempSnapshot writes snap bytes to path with mode 0600 (defensive
// against multi-user systems, per security review). NOT os.WriteFile
// (whose default is 0644).
func writeTempSnapshot(path string, snap []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(snap)
	return err
}

// truncateForLog clips long modal text for the live log; the README and
// the bytes tempfile hold the full version.
func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// logObservationEvent emits one log line for an assistant JSONL event that
// arrived during Probe 1's observation window. Same shape as
// logCancelEvent from spike-cancel.
func logObservationEvent(logger *log.Logger, probeN int, ev tuidriver.JSONLEntry) {
	var msgID string
	var rawMap map[string]any
	if ev.Message != nil {
		msgID = ev.Message.ID
		rawMap = ev.Message.Raw
	}

	stopRepr := "<missing>"
	if raw, present := rawMap["stop_reason"]; present {
		if raw == nil {
			stopRepr = "<nil>"
		} else if s, ok := raw.(string); ok {
			stopRepr = s
		} else {
			stopRepr = fmt.Sprintf("%v", raw)
		}
	}

	logger.Printf("probe=%d observation-event type=assistant stop_reason=%s msg_id=%s",
		probeN, stopRepr, msgID)
}

// hasSpinnerGlyph: kept for spike-suite parity with cmd/spike-cancel; not
// referenced by any active spike-permission probe.
func hasSpinnerGlyph(snap []byte) bool {
	stripped := tuidriver.StripANSI(snap)
	return bytes.Contains(stripped, tuidriver.SpinnerGlyph)
}

// isToolUse reports whether an assistant event carries
// stop_reason=tool_use. Nil-Message guard for non-assistant envelopes
// emitted by library TailJSONL.
func isToolUse(ev tuidriver.JSONLEntry) bool {
	if ev.Message == nil {
		return false
	}
	return ev.Message.StopReason == "tool_use"
}

// extractByMsgID walks every assistant event whose .message.id == targetID
// and concatenates content[].type=="text" blocks in JSONL arrival order.
// Same shape as cmd/spike-cancel/main.go's extractByMsgID.
func extractByMsgID(events []tuidriver.JSONLEntry, targetID string) string {
	if targetID == "" {
		return ""
	}
	var b strings.Builder
	for _, ev := range events {
		if msgIDOf(ev) != targetID {
			continue
		}
		for _, c := range ev.Message.Content {
			if c.Type != "text" {
				continue
			}
			if txt, _ := c.Raw["text"].(string); txt != "" {
				b.WriteString(txt)
			}
		}
	}
	return b.String()
}

// msgIDOf returns the message ID of ev, or "" if ev has no Message
// envelope. The nil guard is load-bearing: TailJSONL emits every entry
// type and only assistant/user carry a Message.
func msgIDOf(ev tuidriver.JSONLEntry) string {
	if ev.Message == nil {
		return ""
	}
	return ev.Message.ID
}

// resolveSession turns the operator-supplied flag into a normalised session
// ID. Empty flag → generate a fresh v4 UUID; non-empty → must parse as a
// valid UUID.
func resolveSession(flagValue string) (string, error) {
	if flagValue == "" {
		u, err := uuid.NewRandom()
		if err != nil {
			return "", fmt.Errorf("generate session id: %w", err)
		}
		return u.String(), nil
	}
	u, err := uuid.Parse(flagValue)
	if err != nil {
		return "", fmt.Errorf("invalid --session-id: %w", err)
	}
	return u.String(), nil
}

// Compile-time references to symbols that exist for parity with
// cmd/spike-cancel but aren't called in spike-permission's happy path.
// Keeping the symbols ensures the helper set stays in sync with the
// rest of the spike-suite for the eventual library extraction.
var (
	_ = hasSpinnerGlyph
	_ = isToolUse
	_ = disappearedWindow
)

// Spike: drive three interactive `claude` probes end-to-end through a PTY,
// the first two cancelling a response mid-flight and the third verifying
// the session is recoverable post-cancel.
//
// Builds on cmd/spike-multi-turn (the multi-turn happy path). The new
// empirical surface this spike validates:
//
//  1. Which keystroke actually interrupts claude mid-response (hypothesis:
//     ESC `\x1b`, supported by the `esc to interrupt` PTY hint visible
//     during processing). Iterated via the -cancel-keystroke flag.
//  2. What the JSONL records for a cancelled response — partial assistant
//     message, new `stop_reason` value (canceled / null / max_tokens),
//     a brand-new envelope `type`, or nothing at all.
//  3. How long `cancel-sent → ❯-reappeared` actually takes.
//  4. (Probe 2 only) Whether the tool subprocess gets killed cleanly when
//     a tool-use turn is cancelled mid-execution.
//  5. Whether the same `--session-id` is recoverable post-cancel: does a
//     fresh prompt land and produce `SUCCESS:`, or is the session poisoned.
//
// Probe order (per spec § Probe ordering): kindThinking → kindToolUse →
// kindRecovery. Probe 3's `say hello` recovery turn reuses spike-multi-turn's
// msg_id-grouped extraction unchanged.
//
// Spike-quality: single binary, no public API. Helpers copy-pasted from
// cmd/spike-multi-turn (attributed inline) — kept here until library
// extraction lands. See cmd/spike-cancel/README.md for the empirical log.
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
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pyrycode/tui-driver/pkg/tuidriver"
)

const (
	statePollInterval  = 50 * time.Millisecond
	sessionFileWait    = 10 * time.Second
	ptyQuietLimit      = 60 * time.Second
	spinnerFreezeLimit = 30 * time.Second
	shutdownGrace      = 3 * time.Second

	disappearedWindow = 500 * time.Millisecond

	// cancelRecoveryLimit: post-cancel watchdog window per AC. Cancellation
	// should be near-instant; if ❯-reappeared doesn't fire within this
	// window the probe aborts with "watchdog: stuck after cancel for <Ns>".
	cancelRecoveryLimit = 30 * time.Second

	// waitConditionLimit: how long Probe 1 waits for the spinner glyph and
	// Probe 2 waits for stop_reason=tool_use before giving up. Long enough
	// for any reasonable claude startup + first-token delay; short enough
	// that a wedged probe doesn't sit forever.
	waitConditionLimit = 30 * time.Second

	// toolUseStartGrace: after the JSONL signals stop_reason=tool_use,
	// sleep briefly to let the tool subprocess actually start emitting
	// output. Sidesteps "what does tool execution look like in the PTY"
	// by not requiring a specific PTY pattern.
	toolUseStartGrace = 200 * time.Millisecond

	// ptyQuietWindow: post-cancel, the predicate "the rolling buffer hasn't
	// received new bytes for this long" stands in for "claude has finished
	// settling." The spec's first-cut predicate ("hasSpinnerGlyph becomes
	// false AND isIdle true") fails empirically because the spinner glyph
	// the wait-for-kickoff step observed sits in the 4096-byte rolling
	// buffer and post-cancel claude doesn't emit enough bytes to roll past
	// it (observed empirically: ~1.4 KB of redraw + title-bar updates,
	// then silence). The PTY-quiescence proxy detects that silence
	// directly. Documented in the spec's open question #4.
	ptyQuietWindow = 1500 * time.Millisecond
)

// ProbeKind selects per-probe behavior inside runProbe.
type ProbeKind int

const (
	kindThinking ProbeKind = iota
	kindToolUse
	kindRecovery
)

func (k ProbeKind) String() string {
	switch k {
	case kindThinking:
		return "thinking"
	case kindToolUse:
		return "tool-use"
	case kindRecovery:
		return "recovery"
	}
	return "unknown"
}

// probeSpec pairs a kind with the verbatim prompt the AC prescribes.
type probeSpec struct {
	kind   ProbeKind
	prompt string
}

// Probes 1-3 are spike #11's ticket AC verbatim test set — README
// observations for spike #11 are reproducible against these strings,
// don't change their wording. Probes 4+ are appended during follow-up
// experiments for additional coverage.
//
// Probe 4 (loop 3 C-4, 2026-05-18): cancel-then-resend-same-prompt —
// after Probe 3's recovery, retry the original thinking prompt to see
// whether claude does the work fresh or remembers it was cancelled.
// Probe 5 (loop 3 C-4): cancel-then-followup-referencing-cancelled —
// ask claude what it was doing when interrupted. Tests whether the
// cancellation marker (`user(text "[Request interrupted by user]")`)
// is part of claude's conversation memory.
var probes = []probeSpec{
	{kindThinking, "think carefully and write a 1000-word essay on the philosophy of monads"},
	{kindToolUse, "recursively list all files under /tmp"},
	{kindRecovery, "say hello"},
	{kindRecovery, "think carefully and write a 1000-word essay on the philosophy of monads"},
	{kindRecovery, "What were you working on just now when I interrupted you? Answer in one sentence."},
}

func main() {
	sessionIDFlag := flag.String("session-id", "", "UUID to pin claude's session ID and JSONL filename (default: generate one)")
	cancelKeystrokeFlag := flag.String("cancel-keystroke", "esc",
		"cancel keystroke to send: esc | double-esc | ctrl-c")
	trustFolderFlag := flag.String("trust-folder", "fail",
		"policy when claude's trust-folder dialog appears at idle: 'fail' (default — return clear error) or 'accept' (send `1\\r` to auto-trust this cwd, then proceed)")
	flag.Parse()

	keystroke, hex, err := parseCancelKeystroke(*cancelKeystrokeFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		flag.Usage()
		os.Exit(2)
	}

	if *trustFolderFlag != "fail" && *trustFolderFlag != "accept" {
		fmt.Fprintf(os.Stderr, "invalid -trust-folder value %q (want 'fail' or 'accept')\n", *trustFolderFlag)
		os.Exit(2)
	}

	if err := run(*sessionIDFlag, keystroke, hex, *trustFolderFlag); err != nil {
		fmt.Fprintf(os.Stderr, "spike failed: %v\n", err)
		os.Exit(1)
	}
}

func parseCancelKeystroke(name string) (bytes []byte, hexStr string, err error) {
	switch name {
	case "esc":
		return []byte{0x1b}, "1b", nil
	case "double-esc":
		return []byte{0x1b, 0x1b}, "1b 1b", nil
	case "ctrl-c":
		return []byte{0x03}, "03", nil
	default:
		return nil, "", fmt.Errorf("invalid -cancel-keystroke %q: want esc | double-esc | ctrl-c", name)
	}
}

func run(sessionIDFlag string, cancelKey []byte, cancelHex string, trustFolderPolicy string) error {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	startedAt := time.Now()

	sessionID, err := resolveSession(sessionIDFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		flag.Usage()
		os.Exit(2)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve cwd: %w", err)
	}
	jsonlPath := tuidriver.SessionJSONLPath(home, cwd, sessionID)
	logger.Printf("session-id-resolved id=%s jsonl=%s", sessionID, jsonlPath)

	rootCtx, cancelCause := context.WithCancelCause(context.Background())
	defer cancelCause(errors.New("run: returning"))

	tr := tuidriver.NewTracker(tuidriver.TrackerOpts{
		PTYQuietLimit:      ptyQuietLimit,
		SpinnerFreezeLimit: spinnerFreezeLimit,
	})
	tr.RecordTransition("start")

	// --permission-mode bypassPermissions: Probe 2's "recursively list all
	// files under /tmp" invokes claude's Bash tool. Under the default
	// permission mode, claude renders a modal that wedges the spike.
	cmd := exec.Command("claude",
		"--session-id", sessionID,
		"--permission-mode", "bypassPermissions",
	)
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

	// Watchdog: 1 Hz inactivity + spinner-freeze enforcement.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := session.RunWatchdog(rootCtx, tr, tuidriver.WatchdogOpts{}); err != nil {
			logger.Printf("%v", err)
			cancelCause(err)
		}
	}()

	// --- linear state machine (session-level) ---

	if err := tuidriver.WaitUntil(rootCtx, func() bool {
		return tuidriver.IsIdle(session.Snapshot())
	}); err != nil {
		return fmt.Errorf("wait idle: %w", err)
	}
	tr.RecordTransition("idle-detected")
	logger.Printf("idle-detected")

	if tuidriver.HasTrustModal(session.Snapshot()) {
		switch trustFolderPolicy {
		case "fail":
			return fmt.Errorf("claude shows the trust-folder dialog — this cwd hasn't been trusted yet. Run `claude` interactively in this directory once, accept trust, exit, then re-run the spike. Or pass `-trust-folder accept` to auto-trust")
		case "accept":
			if err := session.AcceptTrust(); err != nil {
				return fmt.Errorf("write trust-accept keystroke: %w", err)
			}
			tr.RecordTransition("trust-folder-accepted")
			logger.Printf("trust-folder-accepted bytes=31 0d")
			if err := tuidriver.WaitUntil(rootCtx, func() bool {
				snap := session.Snapshot()
				return !tuidriver.HasTrustModal(snap) && tuidriver.IsIdle(snap)
			}); err != nil {
				return fmt.Errorf("wait for idle post-trust-accept: %w", err)
			}
			tr.RecordTransition("idle-detected-post-trust")
			logger.Printf("idle-detected-post-trust")
		}
	}

	// Shared events channel for the whole session; one tailer goroutine
	// services all probes. Tailer is started inside probe 1's hook because
	// interactive `claude --session-id` defers JSONL creation until the
	// first input arrives.
	var eventCh <-chan tuidriver.JSONLEntry

	probe1Hook := func() error {
		jsonlCtx, jsonlCancel := context.WithTimeout(rootCtx, sessionFileWait)
		jsonlErr := tuidriver.WaitForSessionJSONL(jsonlCtx, jsonlPath)
		jsonlCancel()
		if jsonlErr != nil {
			return fmt.Errorf("open session jsonl: %w", jsonlErr)
		}
		logger.Printf("session-jsonl-opened path=%s offset=0", jsonlPath)
		ch, terr := tuidriver.TailJSONL(rootCtx, jsonlPath, 0)
		if terr != nil {
			return fmt.Errorf("open events stream: %w", terr)
		}
		eventCh = ch
		return nil
	}

	for i, p := range probes {
		probeN := i + 1
		var hook func() error
		if probeN == 1 {
			hook = probe1Hook
		}
		if err := runProbe(rootCtx, logger, probeN, p.kind, p.prompt, cancelKey, cancelHex, session, &eventCh, tr, hook); err != nil {
			return fmt.Errorf("probe %d: %w", probeN, err)
		}
	}

	logger.Printf("complete elapsed=%s", time.Since(startedAt).Round(time.Millisecond))
	return nil
}

// runProbe drives one probe end-to-end.
//
// For kindThinking/kindToolUse it submits the prompt, waits for the
// kind-specific wait condition, sends the cancel keystroke, waits for
// ❯-reappeared (with cancelRecoveryLimit deadline), and logs any
// assistant events that arrive between cancel-sent and ❯-reappeared.
//
// For kindRecovery it submits the prompt and drives a full turn to
// assistant-text-extracted + recovery-turn-success, no cancellation.
//
// On Probe 1 only, postPromptHook opens the deterministic JSONL and
// starts the tailer (the JSONL only appears after first input under
// --session-id).
func runProbe(
	ctx context.Context,
	logger *log.Logger,
	probeN int,
	kind ProbeKind,
	prompt string,
	cancelKey []byte,
	cancelHex string,
	session *tuidriver.Session,
	eventChRef *<-chan tuidriver.JSONLEntry,
	tr *tuidriver.Tracker,
	postPromptHook func() error,
) error {
	logger.Printf("probe=%d probe-start kind=%q prompt=%q", probeN, kind.String(), prompt)
	tr.RecordTransition(fmt.Sprintf("probe=%d probe-start", probeN))

	// Residual-event drain: discard any straggler events from a prior probe
	// so this probe's wait conditions / accumulators don't see them. Probe
	// 1's pre-hook drain is a no-op because the channel slot is still nil.
	if ch := *eventChRef; ch != nil {
	drain:
		for {
			select {
			case <-ch:
			default:
				break drain
			}
		}
	}

	// Empirical: after a cancelled probe, claude restores the previously
	// submitted prompt as the drafted input. Without an explicit clear,
	// the next typePrompt appends to that residue and the concatenation
	// gets submitted (observed in run4: probe 2's JSONL `user` record
	// showed probe 1's prompt + probe 2's prompt concatenated). Ctrl-U
	// (0x15) kills the input line; on a probe whose input box is already
	// empty (Probe 1) the keystroke is a no-op.
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

	if kind == kindRecovery {
		return runRecovery(ctx, logger, probeN, session, eventCh, tr)
	}
	return runCancel(ctx, logger, probeN, kind, cancelKey, cancelHex, session, eventCh, tr)
}

// runCancel implements the cancel-probe behavior (Probes 1+2). It waits
// for the kind-specific "claude has started something to cancel" signal,
// sends the cancel keystroke, then waits for ❯-reappeared while draining
// any assistant events into log lines.
func runCancel(
	ctx context.Context,
	logger *log.Logger,
	probeN int,
	kind ProbeKind,
	cancelKey []byte,
	cancelHex string,
	session *tuidriver.Session,
	eventCh <-chan tuidriver.JSONLEntry,
	tr *tuidriver.Tracker,
) error {
	if err := waitForKickoff(ctx, logger, probeN, kind, session, eventCh, tr); err != nil {
		return err
	}

	// Snapshot whether the spinner glyph is currently visible. Used to
	// decide which ❯-reappeared predicate to apply (see waitReappeared).
	preCancelHadSpinner := hasSpinnerGlyph(session.Snapshot())

	if err := sendCancel(session, cancelKey); err != nil {
		return fmt.Errorf("send cancel: %w", err)
	}
	cancelSentAt := time.Now()
	tr.RecordTransition(fmt.Sprintf("probe=%d cancel-sent", probeN))
	logger.Printf("probe=%d cancel-sent keystroke=%s", probeN, cancelHex)

	if err := waitReappeared(ctx, logger, probeN, preCancelHadSpinner, cancelSentAt, session, eventCh); err != nil {
		return err
	}
	tr.RecordTransition(fmt.Sprintf("probe=%d ❯-reappeared", probeN))
	logger.Printf("probe=%d ❯-reappeared", probeN)

	elapsed := time.Since(cancelSentAt).Round(time.Millisecond)
	tr.RecordTransition(fmt.Sprintf("probe=%d elapsed-after-cancel", probeN))
	logger.Printf("probe=%d elapsed-after-cancel=%s", probeN, elapsed)

	return nil
}

// waitForKickoff blocks until the kind-specific "claude has started" signal
// is observed, or waitConditionLimit elapses.
//
// kindThinking: poll for the ✻ glyph in the rolling buffer.
// kindToolUse: accumulate assistant events from eventCh until one carries
// stop_reason=tool_use, then sleep toolUseStartGrace for the tool subprocess
// to actually begin executing.
func waitForKickoff(
	ctx context.Context,
	logger *log.Logger,
	probeN int,
	kind ProbeKind,
	session *tuidriver.Session,
	eventCh <-chan tuidriver.JSONLEntry,
	tr *tuidriver.Tracker,
) error {
	deadline := time.Now().Add(waitConditionLimit)
	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()

	switch kind {
	case kindThinking:
		for {
			if hasSpinnerGlyph(session.Snapshot()) {
				tr.RecordTransition(fmt.Sprintf("probe=%d spinner-or-tool-visible", probeN))
				logger.Printf("probe=%d spinner-or-tool-visible kind=spinner-glyph", probeN)
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("probe %d: wait condition (spinner glyph) not observed within %s",
					probeN, waitConditionLimit)
			}
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case <-ticker.C:
			}
		}
	case kindToolUse:
		for {
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case ev, ok := <-eventCh:
				if !ok {
					eventCh = nil
					continue
				}
				if isToolUse(ev) {
					msgID := msgIDOf(ev)
					tr.RecordTransition(fmt.Sprintf("probe=%d spinner-or-tool-visible", probeN))
					logger.Printf("probe=%d spinner-or-tool-visible kind=tool-use-stop-reason msg_id=%s",
						probeN, msgID)
					// Give the tool subprocess a moment to actually start.
					select {
					case <-ctx.Done():
						return context.Cause(ctx)
					case <-time.After(toolUseStartGrace):
					}
					return nil
				}
			case <-ticker.C:
				if time.Now().After(deadline) {
					return fmt.Errorf("probe %d: wait condition (stop_reason=tool_use) not observed within %s",
						probeN, waitConditionLimit)
				}
			}
		}
	default:
		return fmt.Errorf("probe %d: waitForKickoff called with unsupported kind %s", probeN, kind.String())
	}
}

// waitReappeared blocks until claude's TUI has returned to idle after the
// cancel keystroke. Drains assistant events arriving in this window into
// `probe=N jsonl-cancel-event` log lines.
//
// Predicate: ❯ idle glyph present (direct bytes.Contains check, NOT via
// tuidriver.IsIdle) AND the PTY has been quiet for at least ptyQuietWindow.
//
// We deliberately do NOT use tuidriver.IsIdle here. IsIdle's spinner-absent
// half is exactly what wedges this function against claude 2.1.148: the
// pre-cancel `✻ Forming…` glyph paint stays painted in the 4 KB rolling
// buffer (pkg/tuidriver/buffer.go:8-13) because post-cancel claude emits
// only ~1.4 KB of redraw — well under the buffer cap — so the glyph never
// rolls out. IsIdle then returns false forever, the predicate wedges, and
// the 30 s cancelRecoveryLimit watchdog trips on every run. Ticket #69 has
// the byte-count math (1577 bytes of churn since the last `✻` paint at
// watchdog-trip time vs. the 4096-byte buffer cap). PTY-quiescence subsumes
// the safety the spinner-absent clause was meant to provide: when claude
// has emitted zero bytes for 1.5 s, it has by definition stopped redrawing.
// The new predicate is weaker on the spinner-glyph axis but strictly
// stronger on the rendering-activity axis (1500 ms of zero PTY bytes vs.
// 250 ms of glyph-absence — quiescence cannot be faked by a transient
// buffer state).
//
// Same predicate shape as cmd/spike-multi-turn/main.go:391-400 (PR #74 /
// ticket #73), which validated PTY-quiescence empirically against the same
// stuck-glyph-in-rolling-buffer failure mode. Three consumers now share
// this shape: waitReappeared and runRecovery in this file (both ticket
// #69), and runTurn in spike-multi-turn (ticket #73). See this binary's
// README § *Surprises / findings* (findings 1 and 8) for the empirical
// derivation.
//
// preCancelHadSpinner is recorded for the README but no longer drives the
// predicate.
//
// On timeout (cancelRecoveryLimit), returns the watchdog-shaped error.
func waitReappeared(
	ctx context.Context,
	logger *log.Logger,
	probeN int,
	preCancelHadSpinner bool,
	cancelSentAt time.Time,
	session *tuidriver.Session,
	eventCh <-chan tuidriver.JSONLEntry,
) error {
	_ = preCancelHadSpinner
	deadline := cancelSentAt.Add(cancelRecoveryLimit)
	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()

	stable := func() bool {
		stripped := tuidriver.StripANSI(session.Snapshot())
		if !bytes.Contains(stripped, tuidriver.IdleGlyph) {
			return false
		}
		return session.QuietFor() >= ptyQuietWindow
	}

	for {
		if stable() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("watchdog: stuck after cancel for %s",
				time.Since(cancelSentAt).Round(time.Second))
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case ev, ok := <-eventCh:
			if !ok {
				eventCh = nil
				continue
			}
			// Library TailJSONL emits every envelope type; the pre-migration
			// local tailer filtered to assistant-only before send. Re-enforce
			// that filter here so logCancelEvent only sees assistant entries
			// (preserves the existing `type=assistant` log line shape).
			if ev.Type != "assistant" {
				continue
			}
			logCancelEvent(logger, probeN, ev)
		case <-ticker.C:
		}
	}
}

// logCancelEvent emits one `probe=N jsonl-cancel-event` log line for an
// assistant event that arrived between cancel-sent and ❯-reappeared.
// stop_reason can be a string, JSON null, or absent — encode all three
// distinctly so the README can record what claude actually emits.
func logCancelEvent(logger *log.Logger, probeN int, ev tuidriver.JSONLEntry) {
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

	logger.Printf("probe=%d jsonl-cancel-event type=assistant stop_reason=%s msg_id=%s",
		probeN, stopRepr, msgID)
}

// runRecovery drives Probe 3: same shape as spike-multi-turn's runTurn
// from prompt-written onward. Detects turn-complete on the conjunction
// `gotEndTurn ∧ ❯ idle glyph present (direct bytes.Contains check, NOT
// via tuidriver.IsIdle) ∧ rb.QuietFor() ≥ ptyQuietWindow`, then extracts
// the assistant text by msg_id grouping.
//
// Pre-#69 this used `gotEndTurn ∧ tuidriver.IsIdle(rb) stable for
// idleStableWindow = 250ms` — the verbatim pre-#74 spike-multi-turn
// predicate that PR #74 already proved wedges. IsIdle's spinner-absent
// half is exactly the wedging clause on claude 2.1.148: after a fast
// assistant response the `✻ Brewed for Ns` glyph is painted in the 4 KB
// rolling buffer and post-`end_turn` claude emits well under 4 KB, so the
// glyph never rolls out and IsIdle stays false. PTY-quiescence subsumes
// the safety property the spinner-absent clause was meant to provide: when
// claude has emitted zero bytes for 1.5 s, it has by definition stopped
// redrawing.
//
// Same predicate shape as cmd/spike-multi-turn/main.go:391-400 (PR #74 /
// ticket #73) and cmd/spike-cancel/main.go:waitReappeared (ticket #69,
// the same file). Three consumers share the PTY-quiescence pattern; if
// the duplication becomes a maintenance burden the library extraction at
// #58-#62 is the right home.
func runRecovery(
	ctx context.Context,
	logger *log.Logger,
	probeN int,
	session *tuidriver.Session,
	eventCh <-chan tuidriver.JSONLEntry,
	tr *tuidriver.Tracker,
) error {
	// ❯-disappeared observer: optional, same shape as spike-multi-turn's.
	observerDone := make(chan struct{})
	go func() {
		defer close(observerDone)
		deadline := time.Now().Add(disappearedWindow)
		ticker := time.NewTicker(statePollInterval)
		defer ticker.Stop()
		for {
			if !tuidriver.IsIdle(session.Snapshot()) {
				logger.Printf("probe=%d ❯-disappeared", probeN)
				return
			}
			if time.Now().After(deadline) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	var (
		events             []tuidriver.JSONLEntry
		latestEndTurnMsgID string
		gotEndTurn         bool
	)

	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()

	check := func() bool {
		if !gotEndTurn {
			return false
		}
		stripped := tuidriver.StripANSI(session.Snapshot())
		if !bytes.Contains(stripped, tuidriver.IdleGlyph) {
			return false
		}
		return session.QuietFor() >= ptyQuietWindow
	}

	for !check() {
		select {
		case <-ctx.Done():
			<-observerDone
			return fmt.Errorf("probe=%d: %w", probeN, context.Cause(ctx))
		case ev, ok := <-eventCh:
			if !ok {
				eventCh = nil
				continue
			}
			events = append(events, ev)
			if tuidriver.IsEndTurn(ev) {
				if id := msgIDOf(ev); id != "" {
					latestEndTurnMsgID = id
				}
				gotEndTurn = true
			}
		case <-ticker.C:
		}
	}
	<-observerDone

	tr.RecordTransition(fmt.Sprintf("probe=%d end-turn-detected", probeN))
	logger.Printf("probe=%d end-turn-detected msg_id=%s", probeN, latestEndTurnMsgID)

	tr.RecordTransition(fmt.Sprintf("probe=%d ❯-reappeared", probeN))
	logger.Printf("probe=%d ❯-reappeared", probeN)

	text := extractByMsgID(events, latestEndTurnMsgID)
	tr.RecordTransition(fmt.Sprintf("probe=%d assistant-text-extracted", probeN))
	logger.Printf("probe=%d assistant-text-extracted len=%d", probeN, len(text))

	fmt.Printf("SUCCESS: %s\n", text)

	if len(text) > 0 {
		tr.RecordTransition(fmt.Sprintf("probe=%d recovery-turn-success", probeN))
		logger.Printf("probe=%d recovery-turn-success len=%d", probeN, len(text))
	}
	return nil
}

// sendCancel writes the cancel keystroke as a single bulk write. The cancel
// sequence is at most 2 bytes; the inter-byte-delay reasoning that drives
// Session.TypePrompt (claude's input handler can swallow `\r` arriving in
// the same buffered write as the prompt body after a tool-use wind-down)
// does not apply here — there is no `\r`.
func sendCancel(session *tuidriver.Session, keystroke []byte) error {
	return session.SendKeys(string(keystroke))
}

// hasSpinnerGlyph reports whether the ✻ glyph is present in the rolling
// buffer (after ANSI strip). Used as the "claude has started processing"
// signal because the verb-and-elapsed-seconds spinner regex (now
// tuidriver.ParseSpinner) rarely matches the bursty TUI output before the
// glyph itself has been painted (spike-multi-turn finding 8).
func hasSpinnerGlyph(snap []byte) bool {
	stripped := tuidriver.StripANSI(snap)
	return bytes.Contains(stripped, tuidriver.SpinnerGlyph)
}

// isToolUse reports whether an assistant event carries
// stop_reason=tool_use. Nil-Message guard for non-assistant envelopes
// emitted by library TailJSONL (returns false on attachment / system /
// permission-mode / … entries).
func isToolUse(ev tuidriver.JSONLEntry) bool {
	if ev.Message == nil {
		return false
	}
	return ev.Message.StopReason == "tool_use"
}

// extractByMsgID walks every assistant event whose .message.id == targetID
// and concatenates content[].type=="text" blocks in JSONL arrival order.
// Same shape as cmd/spike-multi-turn/main.go's extractByMsgID.
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

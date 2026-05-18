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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/pyrycode/tui-driver/pkg/tuidriver"
)

const (
	statePollInterval  = 50 * time.Millisecond
	jsonlTailInterval  = 50 * time.Millisecond
	sessionFileWait    = 10 * time.Second
	sessionFilePoll    = 100 * time.Millisecond
	watchdogTick       = 1 * time.Second
	inactivityLimit    = 60 * time.Second
	spinnerFreezeLimit = 30 * time.Second
	shutdownGrace      = 3 * time.Second

	disappearedWindow = 500 * time.Millisecond
	idleStableWindow  = 250 * time.Millisecond

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

	// clearLineSettle: brief pause after Ctrl-U so the input handler can
	// process the line-kill before the next byte arrives.
	clearLineSettle = 50 * time.Millisecond
)

// copied from cmd/spike-multi-turn/main.go — keep in sync until library extraction
var spinnerRe = regexp.MustCompile(`✻\s+(\S+(?:\s+\S+)?)\s+for\s+(?:(\d+)m\s+)?(\d+)s`)

// copied from cmd/spike-multi-turn/main.go — keep in sync until library extraction

// copied from cmd/spike-multi-turn/main.go — keep in sync until library extraction
var idleGlyph = []byte("\xe2\x9d\xaf")

// spinnerGlyph: the bare ✻ codepoint as UTF-8 bytes. Used as the "claude
// has started processing" signal because the full spinnerRe regex never
// matches in practice (see spike-multi-turn finding 8). The glyph itself
// is the only stable signal across every verb / aphorism / form.
var spinnerGlyph = []byte("\xe2\x9c\xbb")

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

	projDir, err := projectsDir()
	if err != nil {
		return fmt.Errorf("resolve projects dir: %w", err)
	}
	logger.Printf("projects-dir path=%s", projDir)

	sessionID, jsonlPath, err := resolveSession(sessionIDFlag, projDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		flag.Usage()
		os.Exit(2)
	}
	logger.Printf("session-id-resolved id=%s jsonl=%s", sessionID, jsonlPath)

	rootCtx, cancelCause := context.WithCancelCause(context.Background())
	defer cancelCause(errors.New("run: returning"))

	rb := tuidriver.NewBuffer(0)
	tr := newTracker()
	tr.recordTransition("start")

	// --permission-mode bypassPermissions: Probe 2's "recursively list all
	// files under /tmp" invokes claude's Bash tool. Under the default
	// permission mode, claude renders a modal that wedges the spike.
	cmd := exec.Command("claude",
		"--session-id", sessionID,
		"--permission-mode", "bypassPermissions",
	)
	tuidriver.EnsureClaudeEnv(cmd)

	ptmx, err := tuidriver.StartPTY(cmd)
	if err != nil {
		return fmt.Errorf("pty.Start: %w", err)
	}

	cmdExited := make(chan error, 1)
	go func() { cmdExited <- cmd.Wait() }()

	var wg sync.WaitGroup

	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			logger.Printf("shutdown-signalled")
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-cmdExited:
			case <-time.After(shutdownGrace):
				_ = cmd.Process.Signal(syscall.SIGKILL)
				<-cmdExited
			}
			_ = ptmx.Close()
			cancelCause(errors.New("shutdown"))
			wg.Wait()
		})
	}
	defer shutdown()

	// copied from cmd/spike-multi-turn/main.go — keep in sync until library extraction
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				rb.Append(chunk)
				_, _ = os.Stderr.Write(chunk)
			}
			if rerr != nil {
				return
			}
		}
	}()

	// copied from cmd/spike-multi-turn/main.go — keep in sync until library extraction
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(watchdogTick)
		defer ticker.Stop()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-ticker.C:
				snap := rb.Snapshot()
				stripped := tuidriver.StripANSI(snap)
				_, total, ok := matchSpinner(stripped)
				tr.observeSpinner(ok, total)
				if werr := tr.checkWatchdog(); werr != nil {
					logger.Printf("%v", werr)
					cancelCause(werr)
					return
				}
			}
		}
	}()

	// --- linear state machine (session-level) ---

	if err := waitUntil(rootCtx, func() bool {
		return isIdle(rb.Snapshot())
	}); err != nil {
		return fmt.Errorf("wait idle: %w", err)
	}
	tr.recordTransition("idle-detected")
	logger.Printf("idle-detected")

	if tuidriver.HasTrustModal(rb.Snapshot()) {
		switch trustFolderPolicy {
		case "fail":
			return fmt.Errorf("claude shows the trust-folder dialog — this cwd hasn't been trusted yet. Run `claude` interactively in this directory once, accept trust, exit, then re-run the spike. Or pass `-trust-folder accept` to auto-trust")
		case "accept":
			if _, err := ptmx.Write([]byte("1\r")); err != nil {
				return fmt.Errorf("write trust-accept keystroke: %w", err)
			}
			tr.recordTransition("trust-folder-accepted")
			logger.Printf("trust-folder-accepted bytes=31 0d")
			if err := waitUntil(rootCtx, func() bool {
				snap := rb.Snapshot()
				return !tuidriver.HasTrustModal(snap) && isIdle(snap)
			}); err != nil {
				return fmt.Errorf("wait for idle post-trust-accept: %w", err)
			}
			tr.recordTransition("idle-detected-post-trust")
			logger.Printf("idle-detected-post-trust")
		}
	}

	eventCh := make(chan map[string]any, 32)

	probe1Hook := func() error {
		if err := openSessionJSONL(jsonlPath); err != nil {
			return fmt.Errorf("open session jsonl: %w", err)
		}
		logger.Printf("session-jsonl-opened path=%s offset=0", jsonlPath)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if terr := tailJSONL(rootCtx, logger, jsonlPath, 0, eventCh); terr != nil {
				if !errors.Is(terr, context.Canceled) {
					logger.Printf("jsonl-tailer-error err=%v", terr)
					cancelCause(terr)
				}
			}
		}()
		return nil
	}

	for i, p := range probes {
		probeN := i + 1
		var hook func() error
		if probeN == 1 {
			hook = probe1Hook
		}
		if err := runProbe(rootCtx, logger, probeN, p.kind, p.prompt, cancelKey, cancelHex, ptmx, rb, eventCh, tr, hook); err != nil {
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
	ptmx *os.File,
	rb *tuidriver.Buffer,
	eventCh <-chan map[string]any,
	tr *tracker,
	postPromptHook func() error,
) error {
	logger.Printf("probe=%d probe-start kind=%q prompt=%q", probeN, kind.String(), prompt)
	tr.recordTransition(fmt.Sprintf("probe=%d probe-start", probeN))

	// Residual-event drain: discard any straggler events from a prior probe
	// so this probe's wait conditions / accumulators don't see them.
drain:
	for {
		select {
		case <-eventCh:
		default:
			break drain
		}
	}

	// Empirical: after a cancelled probe, claude restores the previously
	// submitted prompt as the drafted input. Without an explicit clear,
	// the next typePrompt appends to that residue and the concatenation
	// gets submitted (observed in run4: probe 2's JSONL `user` record
	// showed probe 1's prompt + probe 2's prompt concatenated). Ctrl-U
	// (0x15) kills the input line; on a probe whose input box is already
	// empty (Probe 1) the keystroke is a no-op.
	if err := clearInputLine(ptmx); err != nil {
		return fmt.Errorf("clear input line: %w", err)
	}

	if err := typePrompt(ptmx, prompt); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	tr.recordTransition(fmt.Sprintf("probe=%d prompt-written", probeN))
	logger.Printf("probe=%d prompt-written", probeN)

	if postPromptHook != nil {
		if err := postPromptHook(); err != nil {
			return err
		}
	}

	if kind == kindRecovery {
		return runRecovery(ctx, logger, probeN, rb, eventCh, tr)
	}
	return runCancel(ctx, logger, probeN, kind, cancelKey, cancelHex, ptmx, rb, eventCh, tr)
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
	ptmx *os.File,
	rb *tuidriver.Buffer,
	eventCh <-chan map[string]any,
	tr *tracker,
) error {
	if err := waitForKickoff(ctx, logger, probeN, kind, rb, eventCh, tr); err != nil {
		return err
	}

	// Snapshot whether the spinner glyph is currently visible. Used to
	// decide which ❯-reappeared predicate to apply (see waitReappeared).
	preCancelHadSpinner := hasSpinnerGlyph(rb.Snapshot())

	if err := sendCancel(ptmx, cancelKey); err != nil {
		return fmt.Errorf("send cancel: %w", err)
	}
	cancelSentAt := time.Now()
	tr.recordTransition(fmt.Sprintf("probe=%d cancel-sent", probeN))
	logger.Printf("probe=%d cancel-sent keystroke=%s", probeN, cancelHex)

	if err := waitReappeared(ctx, logger, probeN, preCancelHadSpinner, cancelSentAt, rb, eventCh); err != nil {
		return err
	}
	tr.recordTransition(fmt.Sprintf("probe=%d ❯-reappeared", probeN))
	logger.Printf("probe=%d ❯-reappeared", probeN)

	elapsed := time.Since(cancelSentAt).Round(time.Millisecond)
	tr.recordTransition(fmt.Sprintf("probe=%d elapsed-after-cancel", probeN))
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
	rb *tuidriver.Buffer,
	eventCh <-chan map[string]any,
	tr *tracker,
) error {
	deadline := time.Now().Add(waitConditionLimit)
	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()

	switch kind {
	case kindThinking:
		for {
			if hasSpinnerGlyph(rb.Snapshot()) {
				tr.recordTransition(fmt.Sprintf("probe=%d spinner-or-tool-visible", probeN))
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
			case ev := <-eventCh:
				if isToolUse(ev) {
					msgID := msgIDOf(ev)
					tr.recordTransition(fmt.Sprintf("probe=%d spinner-or-tool-visible", probeN))
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
// Predicate: ❯ idle glyph present AND the PTY has been quiet for at least
// ptyQuietWindow. The spec's first-cut predicate ("hasSpinnerGlyph
// becomes false AND isIdle true") would have been tighter, but post-cancel
// claude doesn't emit enough bytes to roll the spinner glyph out of the
// 4096-byte rolling buffer — the glyph stays painted indefinitely even
// though no new spinner paint has happened. The quiescence predicate
// detects "claude has finished settling" directly, which is what we
// actually care about. See spec open question #4 and this binary's
// README § *Surprises / findings*.
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
	rb *tuidriver.Buffer,
	eventCh <-chan map[string]any,
) error {
	_ = preCancelHadSpinner
	deadline := cancelSentAt.Add(cancelRecoveryLimit)
	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()

	stable := func() bool {
		if !isIdle(rb.Snapshot()) {
			return false
		}
		return rb.QuietFor() >= ptyQuietWindow
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
		case ev := <-eventCh:
			logCancelEvent(logger, probeN, ev)
		case <-ticker.C:
		}
	}
}

// logCancelEvent emits one `probe=N jsonl-cancel-event` log line for an
// assistant event that arrived between cancel-sent and ❯-reappeared.
// stop_reason can be a string, JSON null, or absent — encode all three
// distinctly so the README can record what claude actually emits.
func logCancelEvent(logger *log.Logger, probeN int, ev map[string]any) {
	msg, _ := ev["message"].(map[string]any)
	msgID, _ := msg["id"].(string)

	stopRepr := "<missing>"
	if raw, present := msg["stop_reason"]; present {
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
// `gotEndTurn ∧ isIdle(rb)` stable for idleStableWindow, then extracts
// the assistant text by msg_id grouping.
func runRecovery(
	ctx context.Context,
	logger *log.Logger,
	probeN int,
	rb *tuidriver.Buffer,
	eventCh <-chan map[string]any,
	tr *tracker,
) error {
	// ❯-disappeared observer: optional, same shape as spike-multi-turn's.
	observerDone := make(chan struct{})
	go func() {
		defer close(observerDone)
		deadline := time.Now().Add(disappearedWindow)
		ticker := time.NewTicker(statePollInterval)
		defer ticker.Stop()
		for {
			if !isIdle(rb.Snapshot()) {
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
		events             []map[string]any
		latestEndTurnMsgID string
		gotEndTurn         bool
	)

	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()

	var idleSince time.Time
	check := func() bool {
		if !gotEndTurn {
			return false
		}
		if !isIdle(rb.Snapshot()) {
			idleSince = time.Time{}
			return false
		}
		if idleSince.IsZero() {
			idleSince = time.Now()
			return false
		}
		return time.Since(idleSince) >= idleStableWindow
	}

	for !check() {
		select {
		case <-ctx.Done():
			<-observerDone
			return fmt.Errorf("probe=%d: %w", probeN, context.Cause(ctx))
		case ev := <-eventCh:
			events = append(events, ev)
			if isEndTurn(ev) {
				if id := msgIDOf(ev); id != "" {
					latestEndTurnMsgID = id
				}
				gotEndTurn = true
			}
		case <-ticker.C:
		}
	}
	<-observerDone

	tr.recordTransition(fmt.Sprintf("probe=%d end-turn-detected", probeN))
	logger.Printf("probe=%d end-turn-detected msg_id=%s", probeN, latestEndTurnMsgID)

	tr.recordTransition(fmt.Sprintf("probe=%d ❯-reappeared", probeN))
	logger.Printf("probe=%d ❯-reappeared", probeN)

	text := extractByMsgID(events, latestEndTurnMsgID)
	tr.recordTransition(fmt.Sprintf("probe=%d assistant-text-extracted", probeN))
	logger.Printf("probe=%d assistant-text-extracted len=%d", probeN, len(text))

	fmt.Printf("SUCCESS: %s\n", text)

	if len(text) > 0 {
		tr.recordTransition(fmt.Sprintf("probe=%d recovery-turn-success", probeN))
		logger.Printf("probe=%d recovery-turn-success len=%d", probeN, len(text))
	}
	return nil
}

// sendCancel writes the cancel keystroke as a single bulk write. The cancel
// sequence is at most 2 bytes; the inter-byte-delay reasoning that drives
// typePrompt (claude's input handler can swallow `\r` arriving in the same
// buffered write as the prompt body after a tool-use wind-down) does not
// apply here — there is no `\r`.
func sendCancel(ptmx *os.File, keystroke []byte) error {
	_, err := ptmx.Write(keystroke)
	return err
}

// clearInputLine sends Ctrl-U (0x15, kill-to-beginning-of-line) so any
// drafted text left in claude's input box after a cancel is cleared
// before the next prompt is typed. Idempotent on an empty input.
func clearInputLine(ptmx *os.File) error {
	if _, err := ptmx.Write([]byte{0x15}); err != nil {
		return err
	}
	time.Sleep(clearLineSettle)
	return nil
}

// hasSpinnerGlyph reports whether the ✻ glyph is present in the rolling
// buffer (after ANSI strip). Used as the "claude has started processing"
// signal because spinnerRe never matches in practice (spike-multi-turn
// finding 8).
func hasSpinnerGlyph(snap []byte) bool {
	stripped := tuidriver.StripANSI(snap)
	return bytes.Contains(stripped, spinnerGlyph)
}

// isToolUse reports whether an assistant event carries
// stop_reason=tool_use. Same shape as isEndTurn.
func isToolUse(ev map[string]any) bool {
	msg, _ := ev["message"].(map[string]any)
	stop, _ := msg["stop_reason"].(string)
	return stop == "tool_use"
}

// extractByMsgID: copied from cmd/spike-multi-turn/main.go — keep in sync
// until library extraction. Walks every assistant event whose
// .message.id == targetID, concatenates content[].type=="text" blocks in
// JSONL arrival order.
func extractByMsgID(events []map[string]any, targetID string) string {
	if targetID == "" {
		return ""
	}
	var b strings.Builder
	for _, ev := range events {
		if msgIDOf(ev) != targetID {
			continue
		}
		msg, _ := ev["message"].(map[string]any)
		content, _ := msg["content"].([]any)
		for _, c := range content {
			cm, _ := c.(map[string]any)
			if t, _ := cm["type"].(string); t != "text" {
				continue
			}
			if txt, _ := cm["text"].(string); txt != "" {
				b.WriteString(txt)
			}
		}
	}
	return b.String()
}

// typePrompt: copied from cmd/spike-multi-turn/main.go — keep in sync until
// library extraction. Writes the prompt one byte at a time with a 10 ms
// inter-byte delay, then a 50 ms pause, then `\r`. Bulk-writing
// prompt+"\r" after a prior turn's wind-down loses the `\r`.
func typePrompt(ptmx *os.File, prompt string) error {
	for i := 0; i < len(prompt); i++ {
		if _, err := ptmx.Write([]byte{prompt[i]}); err != nil {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := ptmx.Write([]byte("\r")); err != nil {
		return err
	}
	return nil
}

func msgIDOf(ev map[string]any) string {
	msg, _ := ev["message"].(map[string]any)
	id, _ := msg["id"].(string)
	return id
}

// --- pattern matching ---
// copied from cmd/spike-multi-turn/main.go — keep in sync until library extraction

func matchSpinner(stripped []byte) (verb string, totalSeconds int, ok bool) {
	m := spinnerRe.FindSubmatch(stripped)
	if m == nil {
		return "", 0, false
	}
	var minutes int
	if len(m[2]) > 0 {
		minutes, _ = strconv.Atoi(string(m[2]))
	}
	seconds, _ := strconv.Atoi(string(m[3]))
	return string(m[1]), minutes*60 + seconds, true
}

func isIdle(snap []byte) bool {
	stripped := tuidriver.StripANSI(snap)
	if !bytes.Contains(stripped, idleGlyph) {
		return false
	}
	return !spinnerRe.Match(stripped)
}

// --- tracker (state + watchdog bookkeeping) ---
// copied from cmd/spike-multi-turn/main.go — keep in sync until library extraction

type tracker struct {
	mu                    sync.Mutex
	currentState          string
	lastTransitionAt      time.Time
	lastSpinnerProgressAt time.Time
	lastSpinnerTotal      int
	spinnerActive         bool
}

func newTracker() *tracker { return &tracker{} }

func (t *tracker) recordTransition(state string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.currentState = state
	t.lastTransitionAt = time.Now()
}

func (t *tracker) observeSpinner(visible bool, totalSeconds int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if !visible {
		t.spinnerActive = false
		t.lastSpinnerTotal = 0
		return
	}
	if !t.spinnerActive {
		t.spinnerActive = true
		t.lastSpinnerTotal = totalSeconds
		t.lastSpinnerProgressAt = now
		return
	}
	if totalSeconds > t.lastSpinnerTotal {
		t.lastSpinnerTotal = totalSeconds
		t.lastSpinnerProgressAt = now
	}
}

func (t *tracker) checkWatchdog() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if !t.lastTransitionAt.IsZero() && now.Sub(t.lastTransitionAt) > inactivityLimit {
		return fmt.Errorf("watchdog: stuck in state %s for %s",
			t.currentState, now.Sub(t.lastTransitionAt).Round(time.Second))
	}
	if t.spinnerActive && now.Sub(t.lastSpinnerProgressAt) > spinnerFreezeLimit {
		return fmt.Errorf("watchdog: spinner counter frozen at %ds for %s",
			t.lastSpinnerTotal, now.Sub(t.lastSpinnerProgressAt).Round(time.Second))
	}
	return nil
}

// --- generic predicate wait ---
// copied from cmd/spike-multi-turn/main.go — keep in sync until library extraction

func waitUntil(ctx context.Context, predicate func() bool) error {
	if predicate() {
		return nil
	}
	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-ticker.C:
			if predicate() {
				return nil
			}
		}
	}
}

// --- JSONL discovery + tailing ---
// copied from cmd/spike-multi-turn/main.go — keep in sync until library extraction

func projectsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects", tuidriver.EncodeCwd(cwd)), nil
}


func resolveSession(flagValue string, dir string) (sessionID string, jsonlPath string, err error) {
	if flagValue == "" {
		u, gerr := uuid.NewRandom()
		if gerr != nil {
			return "", "", fmt.Errorf("generate session id: %w", gerr)
		}
		sessionID = u.String()
	} else {
		u, perr := uuid.Parse(flagValue)
		if perr != nil {
			return "", "", fmt.Errorf("invalid --session-id: %w", perr)
		}
		sessionID = u.String()
	}
	jsonlPath = filepath.Join(dir, sessionID+".jsonl")
	return sessionID, jsonlPath, nil
}

func openSessionJSONL(jsonlPath string) error {
	deadline := time.Now().Add(sessionFileWait)
	for {
		_, err := os.Stat(jsonlPath)
		if err == nil {
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat session jsonl: %w", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("session JSONL did not appear at %s within %s", jsonlPath, sessionFileWait)
		}
		time.Sleep(sessionFilePoll)
	}
}

func tailJSONL(
	ctx context.Context,
	logger *log.Logger,
	path string,
	startOffset int64,
	out chan<- map[string]any,
) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open session jsonl: %w", err)
	}
	defer f.Close()

	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return fmt.Errorf("seek session jsonl: %w", err)
	}

	reader := bufio.NewReader(f)
	var partial []byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, rerr := reader.ReadBytes('\n')
		if len(chunk) > 0 {
			partial = append(partial, chunk...)
		}
		if rerr == nil {
			line := bytes.TrimRight(partial, "\r\n")
			partial = partial[:0]
			if len(line) == 0 {
				continue
			}
			var obj map[string]any
			if jerr := json.Unmarshal(line, &obj); jerr != nil {
				logger.Printf("jsonl-parse-warning err=%v", jerr)
				continue
			}
			if _, ok := obj["message"].(map[string]any); !ok {
				continue
			}
			if t, _ := obj["type"].(string); t != "assistant" {
				continue
			}
			select {
			case out <- obj:
			case <-ctx.Done():
				return ctx.Err()
			}
		} else if rerr == io.EOF {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(jsonlTailInterval):
			}
		} else {
			return fmt.Errorf("read session jsonl: %w", rerr)
		}
	}
}

// --- assistant-event helpers ---
// copied from cmd/spike-multi-turn/main.go — keep in sync until library extraction

func isEndTurn(ev map[string]any) bool {
	msg, _ := ev["message"].(map[string]any)
	stop, _ := msg["stop_reason"].(string)
	return stop == "end_turn"
}

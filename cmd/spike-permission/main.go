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

	"github.com/creack/pty"
	"github.com/google/uuid"
)

const (
	// copied from cmd/spike-cancel/main.go — keep in sync until library extraction
	rollingBufferCap   = 4096
	statePollInterval  = 50 * time.Millisecond
	jsonlTailInterval  = 50 * time.Millisecond
	sessionFileWait    = 10 * time.Second
	sessionFilePoll    = 100 * time.Millisecond
	watchdogTick = 1 * time.Second

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
	ptyQuietLimit = 30 * time.Second

	// sessionWallCap: outermost safety net for genuinely runaway sessions
	// where claude emits bytes but doesn't progress (e.g. theoretical loop
	// on a status banner redraw — not observed, cheap to defend against).
	// Real probes complete in <60 s; 10 min is enormous headroom.
	sessionWallCap = 10 * time.Minute

	spinnerFreezeLimit = 30 * time.Second
	shutdownGrace      = 3 * time.Second

	disappearedWindow = 500 * time.Millisecond
	idleStableWindow  = 250 * time.Millisecond

	ptyRows = 40
	ptyCols = 120

	// clearLineSettle: brief pause after Ctrl-U so the input handler can
	// process the line-kill before the next byte arrives.
	clearLineSettle = 50 * time.Millisecond

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

// copied from cmd/spike-cancel/main.go — keep in sync until library extraction
var spinnerRe = regexp.MustCompile(`✻\s+(\S+(?:\s+\S+)?)\s+for\s+(?:(\d+)m\s+)?(\d+)s`)

// copied from cmd/spike-cancel/main.go — keep in sync until library extraction
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// oscRe matches OSC (Operating System Command) sequences — ESC ] ... BEL.
// Claude uses these for window title and similar metadata; they show up
// throughout the PTY stream and must be stripped alongside CSI sequences
// for text extraction to work.
var oscRe = regexp.MustCompile(`\x1b\][^\x07]*\x07`)

// copied from cmd/spike-cancel/main.go — keep in sync until library extraction
var idleGlyph = []byte("\xe2\x9d\xaf")

// copied from cmd/spike-cancel/main.go — keep in sync until library extraction
var spinnerGlyph = []byte("\xe2\x9c\xbb")

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

	if err := run(*sessionIDFlag, approveKey, approveHex, pred); err != nil {
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

func run(sessionIDFlag string, approveKey []byte, approveHex string, pred modalPredicate) error {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	startedAt := time.Now()

	projDir, err := projectsDir()
	if err != nil {
		return fmt.Errorf("resolve projects dir: %w", err)
	}
	logger.Printf("projects-dir path=%s", projDir)

	rootCtx, cancelCause := context.WithCancelCause(context.Background())
	defer cancelCause(errors.New("run: returning"))

	// Session A: Probe 1 only.
	sessionA, jsonlA, err := newSessionID(sessionIDFlag, projDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		flag.Usage()
		os.Exit(2)
	}
	logger.Printf("session-id-resolved id=%s jsonl=%s tag=a", sessionA, jsonlA)

	if err := runSession(rootCtx, logger, sessionA, jsonlA, "a", 1, approveKey, approveHex, pred,
		[]probeSpec{{kind: kindObserve, prompt: probe1Prompt}}); err != nil {
		return fmt.Errorf("session A: %w", err)
	}

	// Session B: Probes 2 + 3. Always generate a fresh UUID — the
	// -session-id flag is consumed by Session A only.
	sessionB, jsonlB, err := newSessionID("", projDir)
	if err != nil {
		return fmt.Errorf("session B: generate session id: %w", err)
	}
	logger.Printf("session-id-resolved id=%s jsonl=%s tag=b", sessionB, jsonlB)

	if err := runSession(rootCtx, logger, sessionB, jsonlB, "b", 2, approveKey, approveHex, pred,
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
	probes []probeSpec,
) error {
	logger := parentLogger

	ctx, cancelCause := context.WithCancelCause(parentCtx)
	defer cancelCause(errors.New("runSession: returning"))

	rb := &rollingBuffer{}
	tr := newTracker()
	tr.recordTransition(fmt.Sprintf("session=%s start", tag))

	// Drop --permission-mode bypassPermissions (spikes #1/#9/#11 used it
	// to skip the modal; we WANT the modal here). Let claude pick its own
	// default permission mode.
	cmd := exec.Command("claude", "--session-id", sessionID)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("pty.Start: %w", err)
	}
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: ptyRows, Cols: ptyCols}); err != nil {
		logger.Printf("warning: pty.Setsize: %v", err)
	}

	cmdExited := make(chan error, 1)
	go func() { cmdExited <- cmd.Wait() }()

	var wg sync.WaitGroup

	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			logger.Printf("shutdown-signalled tag=%s", tag)
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

	// PTY reader: append to rolling buffer + mirror raw bytes to stderr.
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				rb.append(chunk)
				_, _ = os.Stderr.Write(chunk)
			}
			if rerr != nil {
				return
			}
		}
	}()

	// Watchdog: PTY-quiet-for-30s liveness + 30s spinner-freeze + 10min
	// session wall-cap. Session-scoped (resets when Session B starts).
	// PTY-heartbeat replaces the prior state-transition-based stack
	// (inactivityLimit / modalClearedLimit / endTurnAfterApproveLimit)
	// per findings #19-#24 cascade observation.
	sessionStart := time.Now()
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(watchdogTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				snap := rb.snapshot()
				stripped := ansiRe.ReplaceAll(snap, nil)
				_, total, ok := matchSpinner(stripped)
				tr.observeSpinner(ok, total)
				if werr := tr.checkWatchdog(rb, sessionStart); werr != nil {
					logger.Printf("%v", werr)
					cancelCause(werr)
					return
				}
			}
		}
	}()

	// Wait for idle (❯ glyph + no spinner). Same predicate as the other
	// spikes.
	if err := waitUntil(ctx, func() bool {
		return isIdle(rb.snapshot())
	}); err != nil {
		return fmt.Errorf("wait idle: %w", err)
	}
	tr.recordTransition(fmt.Sprintf("session=%s idle-detected", tag))
	logger.Printf("idle-detected tag=%s", tag)

	// Diagnostic: does the chosen modal predicate produce a false positive
	// at idle (before any prompt)? The README documents the baseline.
	idleHasModal := hasModal(rb.snapshot(), pred)
	logger.Printf("idle-predicate-check tag=%s predicate=%s has_modal=%v",
		tag, pred.String(), idleHasModal)

	eventCh := make(chan map[string]any, 32)

	openTailerOnce := sync.Once{}
	openTailerHook := func() error {
		var hookErr error
		openTailerOnce.Do(func() {
			if err := openSessionJSONL(jsonlPath); err != nil {
				hookErr = fmt.Errorf("open session jsonl: %w", err)
				return
			}
			logger.Printf("session-jsonl-opened path=%s offset=0 tag=%s", jsonlPath, tag)
			wg.Add(1)
			go func() {
				defer wg.Done()
				if terr := tailJSONL(ctx, logger, jsonlPath, 0, eventCh); terr != nil {
					if !errors.Is(terr, context.Canceled) {
						logger.Printf("jsonl-tailer-error err=%v", terr)
						cancelCause(terr)
					}
				}
			}()
		})
		return hookErr
	}

	for i, p := range probes {
		probeN := startProbeN + i
		switch p.kind {
		case kindObserve:
			if err := runObserve(ctx, logger, probeN, p.prompt, pred, ptmx, rb, eventCh, tr, openTailerHook); err != nil {
				return fmt.Errorf("probe %d: %w", probeN, err)
			}
		case kindAutoRespond:
			if err := runAutoRespond(ctx, logger, probeN, p.prompt, approveKey, approveHex, pred, ptmx, rb, eventCh, tr, openTailerHook); err != nil {
				return fmt.Errorf("probe %d: %w", probeN, err)
			}
		case kindEscalate:
			if err := runEscalate(ctx, logger, probeN, p.prompt, pred, ptmx, rb, eventCh, tr); err != nil {
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
	ptmx *os.File,
	rb *rollingBuffer,
	eventCh <-chan map[string]any,
	tr *tracker,
	postPromptHook func() error,
) error {
	logger.Printf("probe=%d probe-start kind=%q prompt=%q", probeN, ProbeKind(kindObserve).String(), prompt)
	tr.recordTransition(fmt.Sprintf("probe=%d probe-start", probeN))

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

	// Wait for the modal to appear.
	if err := waitForModal(ctx, rb, pred, modalDetectLimit); err != nil {
		return fmt.Errorf("probe %d: %w", probeN, err)
	}
	tr.recordTransition(fmt.Sprintf("probe=%d modal-detected", probeN))
	logger.Printf("probe=%d modal-detected pattern=%q", probeN, pred.String())

	// Snapshot bytes + persist to tempfile + log truncated extracted text.
	snap := rb.snapshot()
	tr.recordTransition(fmt.Sprintf("probe=%d modal-bytes-snapshot", probeN))
	logger.Printf("probe=%d modal-bytes-snapshot len=%d", probeN, len(snap))

	snapPath := fmt.Sprintf("/tmp/spike-permission-probe%d-bytes-%d.bin", probeN, time.Now().UnixNano())
	if err := writeTempSnapshot(snapPath, snap); err != nil {
		logger.Printf("warning: probe=%d snapshot-write-err err=%v", probeN, err)
	} else {
		logger.Printf("probe=%d modal-bytes-snapshot-path=%s", probeN, snapPath)
	}

	text := extractModalText(snap)
	tr.recordTransition(fmt.Sprintf("probe=%d modal-text-extracted", probeN))
	logger.Printf("probe=%d modal-text-extracted text=%q", probeN, truncateForLog(text, 200))

	// Drain JSONL events for observationWindow. Bump tracker once at start
	// and once at end so the watchdog doesn't trip during a 5 s quiet
	// window.
	tr.recordTransition(fmt.Sprintf("probe=%d observation-window-start", probeN))
	logger.Printf("probe=%d observation-window-start window=%s", probeN, observationWindow)

	count := 0
	deadline := time.After(observationWindow)
loop:
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case ev := <-eventCh:
			logObservationEvent(logger, probeN, ev)
			count++
		case <-deadline:
			break loop
		}
	}

	tr.recordTransition(fmt.Sprintf("probe=%d observation-window-end", probeN))
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
	ptmx *os.File,
	rb *rollingBuffer,
	eventCh <-chan map[string]any,
	tr *tracker,
	postPromptHook func() error,
) error {
	logger.Printf("probe=%d probe-start kind=%q prompt=%q", probeN, ProbeKind(kindAutoRespond).String(), prompt)
	tr.recordTransition(fmt.Sprintf("probe=%d probe-start", probeN))

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

	if err := waitForModal(ctx, rb, pred, modalDetectLimit); err != nil {
		return fmt.Errorf("probe %d: %w", probeN, err)
	}
	tr.recordTransition(fmt.Sprintf("probe=%d modal-detected", probeN))
	logger.Printf("probe=%d modal-detected pattern=%q", probeN, pred.String())

	text := extractModalText(rb.snapshot())
	logger.Printf("probe=%d modal-text-extracted text=%q", probeN, truncateForLog(text, 200))

	if err := sendKeystroke(ptmx, approveKey); err != nil {
		return fmt.Errorf("send approve: %w", err)
	}
	tr.recordTransition(fmt.Sprintf("probe=%d response-keystroke-sent", probeN))
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
	//   end-turn-detected: JSONL stop_reason=end_turn AND PTY idle (❯
	//     visible). Per spike #2 finding the gap is ~100µs; passive
	//     verification suffices.
	//
	// No per-phase wall-clock deadlines here. Liveness is enforced by
	// the session-level PTY-heartbeat watchdog (tracker.checkWatchdog) —
	// PTY quiet for >ptyQuietLimit fires `cancelCause` and surfaces via
	// ctx.Done. The session wall-cap is the outer safety net.
	var (
		events             []map[string]any
		latestEndTurnMsgID string
		gotEndTurn         bool
		modalCleared       bool
	)

	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()

	var idleSince time.Time
	check := func() bool {
		if !gotEndTurn {
			return false
		}
		if !isIdle(rb.snapshot()) {
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
			return context.Cause(ctx)
		case ev := <-eventCh:
			events = append(events, ev)
			if !modalCleared {
				modalCleared = true
				tr.recordTransition(fmt.Sprintf("probe=%d modal-cleared", probeN))
				logger.Printf("probe=%d modal-cleared", probeN)
			}
			if isEndTurn(ev) {
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

	tr.recordTransition(fmt.Sprintf("probe=%d end-turn-detected", probeN))
	logger.Printf("probe=%d end-turn-detected msg_id=%s", probeN, latestEndTurnMsgID)

	out := extractByMsgID(events, latestEndTurnMsgID)
	tr.recordTransition(fmt.Sprintf("probe=%d assistant-text-extracted", probeN))
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
	ptmx *os.File,
	rb *rollingBuffer,
	eventCh <-chan map[string]any,
	tr *tracker,
) error {
	logger.Printf("probe=%d probe-start kind=%q prompt=%q", probeN, ProbeKind(kindEscalate).String(), prompt)
	tr.recordTransition(fmt.Sprintf("probe=%d probe-start", probeN))

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

	if err := clearInputLine(ptmx); err != nil {
		return fmt.Errorf("clear input line: %w", err)
	}
	if err := typePrompt(ptmx, prompt); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	tr.recordTransition(fmt.Sprintf("probe=%d prompt-written", probeN))
	logger.Printf("probe=%d prompt-written", probeN)

	if err := waitForModal(ctx, rb, pred, modalDetectLimit); err != nil {
		return fmt.Errorf("probe %d: %w", probeN, err)
	}
	tr.recordTransition(fmt.Sprintf("probe=%d modal-detected", probeN))
	logger.Printf("probe=%d modal-detected pattern=%q", probeN, pred.String())

	snap := rb.snapshot()
	snapPath := fmt.Sprintf("/tmp/spike-permission-probe%d-bytes-%d.bin", probeN, time.Now().UnixNano())
	if err := writeTempSnapshot(snapPath, snap); err != nil {
		logger.Printf("warning: probe=%d snapshot-write-err err=%v", probeN, err)
		logger.Printf("probe=%d modal-bytes-snapshot len=%d", probeN, len(snap))
	} else {
		logger.Printf("probe=%d modal-bytes-snapshot len=%d path=%s", probeN, len(snap), snapPath)
	}

	text := extractModalText(snap)
	tool := extractToolName(snap)
	tr.recordTransition(fmt.Sprintf("probe=%d modal-text-extracted", probeN))
	logger.Printf("probe=%d modal-text-extracted text=%q", probeN, truncateForLog(text, 200))

	tr.recordTransition(fmt.Sprintf("probe=%d modal-escalation-callback-would-receive", probeN))
	logger.Printf("probe=%d modal-escalation-callback-would-receive text=%q tool=%s",
		probeN, truncateForLog(text, 200), tool)

	tr.recordTransition(fmt.Sprintf("probe=%d escalation-simulated", probeN))
	logger.Printf("probe=%d escalation-simulated", probeN)

	// Sleep escalationWindow; afterwards verify the modal is still up.
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-time.After(escalationWindow):
	}

	stillOpen := hasModal(rb.snapshot(), pred)
	tr.recordTransition(fmt.Sprintf("probe=%d modal-still-open", probeN))
	logger.Printf("probe=%d modal-still-open=%v", probeN, stillOpen)

	return nil
}

// waitForModal polls hasModal at statePollInterval until true or limit
// elapses. Returns the watchdog-shaped error on timeout.
func waitForModal(ctx context.Context, rb *rollingBuffer, pred modalPredicate, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()
	for {
		if hasModal(rb.snapshot(), pred) {
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
	stripped := ansiRe.ReplaceAll(snap, nil)
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
	stripped = ansiRe.ReplaceAll(stripped, nil)
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
	stripped := ansiRe.ReplaceAll(snap, nil)
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
func sendKeystroke(ptmx *os.File, keystroke []byte) error {
	_, err := ptmx.Write(keystroke)
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
func logObservationEvent(logger *log.Logger, probeN int, ev map[string]any) {
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

	logger.Printf("probe=%d observation-event type=assistant stop_reason=%s msg_id=%s",
		probeN, stopRepr, msgID)
}

// clearInputLine sends Ctrl-U (0x15) so any drafted text left in claude's
// input box is cleared before the next prompt is typed. Idempotent on an
// empty input. copied from cmd/spike-cancel/main.go — keep in sync until
// library extraction.
func clearInputLine(ptmx *os.File) error {
	if _, err := ptmx.Write([]byte{0x15}); err != nil {
		return err
	}
	time.Sleep(clearLineSettle)
	return nil
}

// typePrompt: copied from cmd/spike-cancel/main.go — keep in sync until
// library extraction. Writes the prompt one byte at a time with a 10 ms
// inter-byte delay, then a 50 ms pause, then `\r`.
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

// hasSpinnerGlyph: copied from cmd/spike-cancel/main.go — keep in sync until
// library extraction.
func hasSpinnerGlyph(snap []byte) bool {
	stripped := ansiRe.ReplaceAll(snap, nil)
	return bytes.Contains(stripped, spinnerGlyph)
}

// isToolUse: copied from cmd/spike-cancel/main.go — keep in sync until
// library extraction.
func isToolUse(ev map[string]any) bool {
	msg, _ := ev["message"].(map[string]any)
	stop, _ := msg["stop_reason"].(string)
	return stop == "tool_use"
}

// extractByMsgID: copied from cmd/spike-cancel/main.go — keep in sync until
// library extraction. Walks every assistant event whose .message.id ==
// targetID, concatenates content[].type=="text" blocks in arrival order.
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

// msgIDOf: copied from cmd/spike-cancel/main.go — keep in sync until
// library extraction.
func msgIDOf(ev map[string]any) string {
	msg, _ := ev["message"].(map[string]any)
	id, _ := msg["id"].(string)
	return id
}

// --- rolling buffer ---
// copied from cmd/spike-cancel/main.go — keep in sync until library extraction

type rollingBuffer struct {
	mu           sync.Mutex
	buf          []byte
	lastAppendAt time.Time
}

func (r *rollingBuffer) append(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > rollingBufferCap {
		fresh := make([]byte, rollingBufferCap)
		copy(fresh, r.buf[len(r.buf)-rollingBufferCap:])
		r.buf = fresh
	}
	r.lastAppendAt = time.Now()
}

func (r *rollingBuffer) snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, len(r.buf))
	copy(out, r.buf)
	return out
}

func (r *rollingBuffer) quietFor() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastAppendAt.IsZero() {
		return 0
	}
	return time.Since(r.lastAppendAt)
}

// --- pattern matching ---
// copied from cmd/spike-cancel/main.go — keep in sync until library extraction

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
	stripped := ansiRe.ReplaceAll(snap, nil)
	if !bytes.Contains(stripped, idleGlyph) {
		return false
	}
	return !spinnerRe.Match(stripped)
}

// --- tracker (state + watchdog bookkeeping) ---
// copied from cmd/spike-cancel/main.go — keep in sync until library extraction

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

// checkWatchdog is called from the per-tick goroutine with the rolling
// buffer + the session-start timestamp. Replaces the prior
// state-transition-based inactivity check with a PTY-heartbeat check:
// PTY bytes flowing == claude alive, regardless of which state the
// probe thinks it's in.
func (t *tracker) checkWatchdog(rb *rollingBuffer, sessionStart time.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if quiet := rb.quietFor(); quiet > ptyQuietLimit {
		return fmt.Errorf("watchdog: PTY quiet for %s (last state: %s)",
			quiet.Round(time.Second), t.currentState)
	}
	if t.spinnerActive && now.Sub(t.lastSpinnerProgressAt) > spinnerFreezeLimit {
		return fmt.Errorf("watchdog: spinner counter frozen at %ds for %s",
			t.lastSpinnerTotal, now.Sub(t.lastSpinnerProgressAt).Round(time.Second))
	}
	if elapsed := now.Sub(sessionStart); elapsed > sessionWallCap {
		return fmt.Errorf("watchdog: session exceeded wall-clock cap of %s (elapsed %s, last state: %s)",
			sessionWallCap, elapsed.Round(time.Second), t.currentState)
	}
	return nil
}

// --- generic predicate wait ---
// copied from cmd/spike-cancel/main.go — keep in sync until library extraction

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
// copied from cmd/spike-cancel/main.go — keep in sync until library extraction

func projectsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects", encodeCwd(cwd)), nil
}

func encodeCwd(cwd string) string {
	var b strings.Builder
	b.Grow(len(cwd))
	for i := 0; i < len(cwd); i++ {
		c := cwd[i]
		if c == '/' || c == '.' || c == ' ' {
			b.WriteByte('-')
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// newSessionID is like spike-cancel's resolveSession but takes a single
// projectsDir argument and returns (id, jsonlPath, err). The flag value
// (if non-empty) is parsed; otherwise a fresh UUIDv4 is generated.
func newSessionID(flagValue string, dir string) (sessionID string, jsonlPath string, err error) {
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
// copied from cmd/spike-cancel/main.go — keep in sync until library extraction

func isEndTurn(ev map[string]any) bool {
	msg, _ := ev["message"].(map[string]any)
	stop, _ := msg["stop_reason"].(string)
	return stop == "end_turn"
}

// Compile-time references to symbols that exist for parity with
// cmd/spike-cancel but aren't called in spike-permission's happy path.
// Keeping the symbols ensures the helper set stays in sync with the
// rest of the spike-suite for the eventual library extraction.
var (
	_ = hasSpinnerGlyph
	_ = isToolUse
	_ = disappearedWindow
	_ = idleStableWindow
)

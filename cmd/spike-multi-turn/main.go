// Spike: drive three sequential interactive `claude` turns end-to-end through a PTY.
//
// Builds on cmd/spike-one-turn (the one-turn happy path). The new empirical
// surface this spike validates:
//
//  1. A single Anthropic message may serialise as N JSONL lines (one per
//     content block) sharing the same msg_id and stop_reason. Concatenate
//     by msg_id, not by record.
//  2. Tool-use streams blocks interleaved with user(tool_result) events
//     under the same msg_id (stop_reason=tool_use).
//  3. `stop_reason` lives on every delta of a message — "first line with
//     stop_reason=end_turn" is not a reliable turn-complete signal.
//
// Turn-complete is therefore: at least one assistant line with
// stop_reason=end_turn for this turn AND the TUI is back to ❯ (idle).
// Extraction groups assistant events by `.message.id == latestEndTurnMsgID`
// and concatenates content[].type=="text" blocks in JSONL order.
//
// Spike-quality: single binary, no public API. See
// cmd/spike-multi-turn/README.md for the empirical log.
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

	// ptyQuietWindow: post-end_turn, the predicate "❯ idle glyph visible
	// AND the rolling buffer has been quiet for this long" stands in for
	// "TUI ready to accept input." Empirical value reused from
	// spike-cancel (see cmd/spike-cancel/main.go:82-91): claude emits
	// ~1.4 KB of redraw + title-bar updates after end_turn, then falls
	// silent; 1500 ms covers that tail comfortably. The first-cut
	// predicate (`gotEndTurn ∧ IsIdle for 250ms`) wedges against claude
	// 2.1.148 because a `✻ Brewed for Ns` glyph stays painted in the
	// 4096-byte rolling buffer (pkg/tuidriver/buffer.go:8-13) — claude
	// doesn't emit enough bytes post-end_turn to roll it out, so IsIdle
	// stays false forever. PTY-quiescence sidesteps the glyph-residue
	// problem entirely by observing silence directly.
	ptyQuietWindow = 1500 * time.Millisecond
)

// The probe prompts.
//
// Prompts 1-3 are spike #2's ticket AC verbatim test set — README
// observations for spike #2 are reproducible against these exact strings,
// don't change their wording. Prompts 4+ are appended during follow-up
// experiments for additional coverage.
//
// Prompt 4 (loop 2 exp B-2, 2026-05-18): parallel-tool stress, soft
// wording — claude chose serial reads under this phrasing. Pattern from
// pre-spike finding #2 still reproduced (tool_use blocks under one
// msg_id, interleaved with tool_result events) but the parallel-in-flight
// case (multiple tool_use BEFORE first tool_result) wasn't triggered.
//
// Prompt 5 (loop 3 exp C-3, 2026-05-18): same probe with explicit
// parallel-call wording — "issue both tool calls in a single response
// before waiting for results." Aim is to observe the parallel-in-flight
// shape claude can produce (per Anthropic API docs that say tool_use is
// list-typed) and validate the msg_id-grouped extractor handles N
// concurrent tool_use blocks within one assistant message.
var prompts = []string{
	"say hello",
	"list the files in /tmp",
	"think carefully and compute 1+2+3+...+100, showing your reasoning",
	"read /etc/hosts and /etc/passwd and summarize the differences in one sentence",
	"Read /etc/hosts and /etc/passwd in parallel — issue both Read tool calls in a single assistant response BEFORE waiting for any result, then summarize the differences in one sentence",
}

func main() {
	sessionIDFlag := flag.String("session-id", "", "UUID to pin claude's session ID and JSONL filename (default: generate one)")
	trustFolderFlag := flag.String("trust-folder", "fail",
		"policy when claude's trust-folder dialog appears at idle: 'fail' (default — return clear error) or 'accept' (send `1\\r` to auto-trust this cwd, then proceed)")
	flag.Parse()

	if *trustFolderFlag != "fail" && *trustFolderFlag != "accept" {
		fmt.Fprintf(os.Stderr, "invalid -trust-folder value %q (want 'fail' or 'accept')\n", *trustFolderFlag)
		os.Exit(2)
	}

	if err := run(*sessionIDFlag, *trustFolderFlag); err != nil {
		fmt.Fprintf(os.Stderr, "spike failed: %v\n", err)
		os.Exit(1)
	}
}

func run(sessionIDFlag string, trustFolderPolicy string) error {
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

	// --permission-mode bypassPermissions: turn 2's "list the files in /tmp"
	// prompt invokes claude's Bash tool. Under default permission mode, claude
	// renders a modal ("1. Yes / 2. Yes, allow reading…") and blocks waiting
	// for a keystroke choice — observed empirically in this spike's first run.
	// Modal handling is out of scope per the ticket; the bypass lets the tool
	// run unattended so the spike can observe the tool_use → tool_result →
	// end_turn JSONL pattern that is the actual subject of turn 2. Recorded
	// as a README finding.
	cmd := exec.Command("claude",
		"--session-id", sessionID,
		"--permission-mode", "bypassPermissions",
	)
	tuidriver.EnsureClaudeEnv(cmd)

	session, err := tuidriver.Spawn(cmd, tuidriver.SpawnOpts{
		Mirror:        os.Stderr,
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
	rb := session.Buffer
	ptmx := session.PTY

	var wg sync.WaitGroup

	// Watchdog: 1 Hz inactivity + spinner-freeze enforcement.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := tuidriver.RunWatchdog(rootCtx, rb, tr, tuidriver.WatchdogOpts{}); err != nil {
			logger.Printf("%v", err)
			cancelCause(err)
		}
	}()

	// --- linear state machine (session-level) ---

	if err := tuidriver.WaitUntil(rootCtx, func() bool {
		return tuidriver.IsIdle(rb.Snapshot())
	}); err != nil {
		return fmt.Errorf("wait idle: %w", err)
	}
	tr.RecordTransition("idle-detected")
	logger.Printf("idle-detected")

	if tuidriver.HasTrustModal(rb.Snapshot()) {
		switch trustFolderPolicy {
		case "fail":
			return fmt.Errorf("claude shows the trust-folder dialog — this cwd hasn't been trusted yet. Run `claude` interactively in this directory once, accept trust, exit, then re-run the spike. Or pass `-trust-folder accept` to auto-trust")
		case "accept":
			if _, err := ptmx.Write([]byte("1\r")); err != nil {
				return fmt.Errorf("write trust-accept keystroke: %w", err)
			}
			tr.RecordTransition("trust-folder-accepted")
			logger.Printf("trust-folder-accepted bytes=31 0d")
			if err := tuidriver.WaitUntil(rootCtx, func() bool {
				snap := rb.Snapshot()
				return !tuidriver.HasTrustModal(snap) && tuidriver.IsIdle(snap)
			}); err != nil {
				return fmt.Errorf("wait for idle post-trust-accept: %w", err)
			}
			tr.RecordTransition("idle-detected-post-trust")
			logger.Printf("idle-detected-post-trust")
		}
	}

	// Shared events channel for the whole session; one tailer goroutine
	// services all turns. Tailer is started inside turn 1's
	// postPromptHook because interactive `claude --session-id` defers JSONL
	// creation until the first input arrives (see spike-one-turn finding #9).
	var eventCh <-chan tuidriver.JSONLEntry

	turn1Hook := func() error {
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

	for i, p := range prompts {
		turn := i + 1
		var hook func() error
		if turn == 1 {
			hook = turn1Hook
		}
		if _, _, err := runTurn(rootCtx, logger, turn, p, session, rb, &eventCh, tr, hook); err != nil {
			return fmt.Errorf("turn %d: %w", turn, err)
		}
	}

	logger.Printf("complete elapsed=%s", time.Since(startedAt).Round(time.Millisecond))
	return nil
}

// runTurn drives one user→assistant exchange. Assumes the orchestrator has
// already reached idle for this turn (turn 1: post-idle-detected; turns 2+:
// post-prior-turn's ❯-reappeared).
//
// On turn 1, postPromptHook opens the deterministic JSONL and starts the
// tailer goroutine — the file only exists once claude processes the first
// input, so the open MUST happen after prompt-written but before this turn
// reads any events. For turns 2+ postPromptHook is nil.
//
// eventChRef points at the caller's session-scoped tail channel slot. On
// turn 1 the slot is nil at entry and the hook fills it; on turns 2+ the
// slot is already populated. The pre-hook drain therefore gates on
// `*eventChRef != nil`, and the receive loop dereferences once post-hook.
//
// Returns the extracted assistant text and the msg_id used for extraction.
func runTurn(
	ctx context.Context,
	logger *log.Logger,
	turn int,
	prompt string,
	session *tuidriver.Session,
	rb *tuidriver.Buffer,
	eventChRef *<-chan tuidriver.JSONLEntry,
	tr *tuidriver.Tracker,
	postPromptHook func() error,
) (string, string, error) {
	logger.Printf("turn=%d turn-start prompt=%q", turn, prompt)
	tr.RecordTransition(fmt.Sprintf("turn=%d turn-start", turn))

	// Drain any residual events queued from a prior turn. After a prior
	// turn's end_turn was observed, additional delta lines for the same
	// msg_id may have been buffered onto eventCh while runTurn was already
	// returning. If any of those slip into this turn's accumulation, the
	// "gotEndTurn && isIdle" predicate could fire spuriously before this
	// turn's response even begins. Drain non-blocking. Turn 1's pre-hook
	// drain is a no-op because the channel slot is still nil.
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

	if err := session.TypePrompt(prompt); err != nil {
		return "", "", fmt.Errorf("write prompt: %w", err)
	}
	tr.RecordTransition(fmt.Sprintf("turn=%d prompt-written", turn))
	logger.Printf("turn=%d prompt-written", turn)

	// ❯-disappeared observer: short-lived goroutine. Polls isIdle for up to
	// disappearedWindow after prompt-written. Logs the optional line at most
	// once iff ❯ was observed missing. Exits silently if ❯ stays visible.
	observerDone := make(chan struct{})
	go func() {
		defer close(observerDone)
		deadline := time.Now().Add(disappearedWindow)
		ticker := time.NewTicker(statePollInterval)
		defer ticker.Stop()
		for {
			if !tuidriver.IsIdle(rb.Snapshot()) {
				logger.Printf("turn=%d ❯-disappeared", turn)
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

	if postPromptHook != nil {
		if err := postPromptHook(); err != nil {
			<-observerDone
			return "", "", err
		}
	}

	eventCh := *eventChRef

	// Accumulate assistant events and detect turn-complete.
	//
	// Predicate: (a) at least one assistant line with stop_reason=end_turn
	// has been seen for this turn, AND (b) the ❯ idle glyph is present in
	// the stripped rolling buffer, AND (c) the PTY has been quiet for at
	// least ptyQuietWindow. All three clauses are load-bearing — JSONL
	// end_turn means "model done speaking"; ❯-present means "input prompt
	// is on screen"; PTY-quiescence means "claude has stopped redrawing."
	// The next prompt is safe to write only when all three hold.
	//
	// We deliberately do NOT use tuidriver.IsIdle here. IsIdle's
	// spinner-absent half is exactly what wedges this spike against
	// claude 2.1.148: a stuck `✻ Brewed for Ns` glyph stays painted in
	// the 4 KB rolling buffer (pkg/tuidriver/buffer.go:8-13) and never
	// rolls out, keeping IsIdle false forever even though the assistant
	// turn is over. PTY-quiescence subsumes the safety property the
	// spinner-absent clause was meant to provide: when claude has emitted
	// zero bytes for 1.5 s, it has by definition stopped redrawing the
	// spinner (or anything else), so further input is safe. The new
	// predicate is therefore weaker than the old one on the spinner-glyph
	// axis but strictly stronger on the rendering-activity axis (1500 ms
	// of zero bytes vs. 250 ms of glyph-absence — quiescence cannot be
	// faked by a transient buffer state).
	//
	// Same predicate shape as cmd/spike-cancel/main.go:513-568, which
	// validated PTY-quiescence empirically against the same
	// stuck-glyph-in-rolling-buffer failure mode (see
	// cmd/spike-cancel/main.go:82-91 for the empirical derivation of the
	// 1500 ms window). Ticket #73.
	var (
		events             []tuidriver.JSONLEntry
		latestEndTurnMsgID string
		gotEndTurn         bool
		assistantText      string
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
			<-observerDone
			return "", "", fmt.Errorf("turn=%d: %w", turn, context.Cause(ctx))
		case ev, ok := <-eventCh:
			if !ok {
				// Tail goroutine exited (ctx-cancel or unrecoverable read
				// error). Nil the local copy so this case stops firing on
				// the closed-channel zero value at every tick; the loop
				// continues until check() flips or the watchdog wedges ctx.
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

	tr.RecordTransition(fmt.Sprintf("turn=%d end-turn-detected", turn))
	logger.Printf("turn=%d end-turn-detected msg_id=%s", turn, latestEndTurnMsgID)

	tr.RecordTransition(fmt.Sprintf("turn=%d ❯-reappeared", turn))
	logger.Printf("turn=%d ❯-reappeared", turn)

	assistantText = extractByMsgID(events, latestEndTurnMsgID)
	tr.RecordTransition(fmt.Sprintf("turn=%d assistant-text-extracted", turn))
	logger.Printf("turn=%d assistant-text-extracted len=%d", turn, len(assistantText))

	fmt.Printf("SUCCESS: %s\n", assistantText)

	return assistantText, latestEndTurnMsgID, nil
}

// extractByMsgID collects every assistant event in `events` whose
// .message.id == targetID, walks each one's content[] in JSONL arrival
// order, and concatenates blocks whose type == "text". `thinking` and
// `tool_use` blocks are skipped — they are not the user-visible answer.
//
// Returns "" if no matching events exist; runTurn only calls this after at
// least one end_turn-tagged event for targetID has been observed, so empty
// means every block under that msg_id was non-text (which would be a
// finding worth recording in the README).
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
// type (assistant, user, attachment, …) and only assistant/user carry a
// Message — non-message envelopes would nil-deref without the guard.
func msgIDOf(ev tuidriver.JSONLEntry) string {
	if ev.Message == nil {
		return ""
	}
	return ev.Message.ID
}

// resolveSession turns the operator-supplied flag into a normalised session ID.
// Empty flag → generate a fresh v4 UUID; non-empty → must parse as a valid
// UUID (uuid.Parse accepts hyphenated and non-hyphenated forms; .String()
// re-emits the canonical lowercase-hyphenated shape that matches claude's
// filename convention).
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

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
// Spike-quality: single binary, no public API. Helpers copy-pasted from
// cmd/spike-one-turn (attributed inline) — kept here until library
// extraction lands. See cmd/spike-multi-turn/README.md for the empirical log.
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

	// idleStableWindow: how long isIdle must be CONTINUOUSLY true after
	// end_turn arrived before we declare turn-complete. Guards against a
	// transient idle observation while the TUI is still mid-redraw. The
	// primary protection against the inter-turn race is the per-byte
	// typePrompt delay (see typePrompt) — this stability window is a
	// secondary belt-and-suspenders gate at minimal overhead.
	idleStableWindow = 250 * time.Millisecond
)

// copied from cmd/spike-one-turn/main.go — keep in sync until library extraction
var spinnerRe = regexp.MustCompile(`✻\s+(\S+(?:\s+\S+)?)\s+for\s+(?:(\d+)m\s+)?(\d+)s`)

// copied from cmd/spike-one-turn/main.go — keep in sync until library extraction

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

	// PTY reader: drains the master fd into the rolling buffer and mirrors
	// raw output to stderr so a human watching the spike sees what claude does.
	// copied from cmd/spike-one-turn/main.go
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

	// Watchdog: 1 Hz inactivity + spinner-freeze enforcement.
	// copied from cmd/spike-one-turn/main.go
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
		return tuidriver.IsIdle(rb.Snapshot())
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
				return !tuidriver.HasTrustModal(snap) && tuidriver.IsIdle(snap)
			}); err != nil {
				return fmt.Errorf("wait for idle post-trust-accept: %w", err)
			}
			tr.recordTransition("idle-detected-post-trust")
			logger.Printf("idle-detected-post-trust")
		}
	}

	// Shared events channel for the whole session; one tailer goroutine
	// services all three turns. Tailer is started inside turn 1's
	// postPromptHook because interactive `claude --session-id` defers JSONL
	// creation until the first input arrives (see spike-one-turn finding #9).
	eventCh := make(chan map[string]any, 32)

	turn1Hook := func() error {
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

	for i, p := range prompts {
		turn := i + 1
		var hook func() error
		if turn == 1 {
			hook = turn1Hook
		}
		if _, _, err := runTurn(rootCtx, logger, turn, p, ptmx, rb, eventCh, tr, hook); err != nil {
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
// reads any events. For turns 2 and 3 postPromptHook is nil.
//
// Returns the extracted assistant text and the msg_id used for extraction.
func runTurn(
	ctx context.Context,
	logger *log.Logger,
	turn int,
	prompt string,
	ptmx *os.File,
	rb *tuidriver.Buffer,
	eventCh <-chan map[string]any,
	tr *tracker,
	postPromptHook func() error,
) (string, string, error) {
	logger.Printf("turn=%d turn-start prompt=%q", turn, prompt)
	tr.recordTransition(fmt.Sprintf("turn=%d turn-start", turn))

	// Drain any residual events queued from a prior turn. After a prior
	// turn's end_turn was observed, additional delta lines for the same
	// msg_id may have been buffered onto eventCh while runTurn was already
	// returning. If any of those slip into this turn's accumulation, the
	// "gotEndTurn && isIdle" predicate could fire spuriously before this
	// turn's response even begins. Drain non-blocking.
drain:
	for {
		select {
		case <-eventCh:
		default:
			break drain
		}
	}

	if err := typePrompt(ptmx, prompt); err != nil {
		return "", "", fmt.Errorf("write prompt: %w", err)
	}
	tr.recordTransition(fmt.Sprintf("turn=%d prompt-written", turn))
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

	// Accumulate assistant events and detect turn-complete.
	//
	// Predicate: at least one assistant line with stop_reason=end_turn has
	// been seen for this turn AND tuidriver.IsIdle(rb) is true. The conjunction is
	// load-bearing — JSONL end_turn means "model done speaking"; isIdle
	// (❯ visible, spinner gone) means "TUI ready to accept input." Both
	// must hold to safely write the next prompt.
	var (
		events             []map[string]any
		latestEndTurnMsgID string
		gotEndTurn         bool
		assistantText      string
	)
	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()

	// idleSince tracks the time at which isIdle first became true after
	// gotEndTurn. Reset to zero whenever isIdle is observed false. The
	// predicate fires only once idleSince is non-zero AND the gap to now
	// exceeds idleStableWindow.
	var idleSince time.Time

	check := func() bool {
		if !gotEndTurn {
			return false
		}
		if !tuidriver.IsIdle(rb.Snapshot()) {
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
			return "", "", fmt.Errorf("turn=%d: %w", turn, context.Cause(ctx))
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

	tr.recordTransition(fmt.Sprintf("turn=%d end-turn-detected", turn))
	logger.Printf("turn=%d end-turn-detected msg_id=%s", turn, latestEndTurnMsgID)

	tr.recordTransition(fmt.Sprintf("turn=%d ❯-reappeared", turn))
	logger.Printf("turn=%d ❯-reappeared", turn)

	assistantText = extractByMsgID(events, latestEndTurnMsgID)
	tr.recordTransition(fmt.Sprintf("turn=%d assistant-text-extracted", turn))
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

// typePrompt writes the prompt body one byte at a time with a small delay
// between bytes, then a brief pause, then `\r`. Observed empirically: writing
// `prompt+"\r"` as a single buffered write works for turn 1 (after the
// session boot redraw) but fails after a tool-use turn 2 — claude's input
// area echoes every character but never recognises the trailing `\r` as
// submit. Char-by-char with a 10 ms inter-byte delay reliably submits.
//
// Hypothesis: claude's input handler debounces input differently when a
// previous turn's wind-down redraw is still in flight; a single bulk write
// arrives faster than the input handler can transition out of the prior
// turn's state. Spike documents this as a finding; the post-spike library
// will need to make the inter-byte delay configurable.
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
// copied from cmd/spike-one-turn/main.go — keep in sync until library extraction

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


// --- tracker (state + watchdog bookkeeping) ---
// copied from cmd/spike-one-turn/main.go — keep in sync until library extraction

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
// copied from cmd/spike-one-turn/main.go — keep in sync until library extraction

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
// copied from cmd/spike-one-turn/main.go — keep in sync until library extraction

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
// copied from cmd/spike-one-turn/main.go — keep in sync until library extraction

func isEndTurn(ev map[string]any) bool {
	msg, _ := ev["message"].(map[string]any)
	stop, _ := msg["stop_reason"].(string)
	return stop == "end_turn"
}

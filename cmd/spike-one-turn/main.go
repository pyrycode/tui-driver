// Spike: drive one interactive `claude` turn end-to-end through a PTY.
//
// Sequence:
//
//	allocate PTY → spawn `claude` → wait for ❯ (idle)
//	→ open the session JSONL claude already created at startup and record its size
//	→ write "What is 2+2?\r"
//	→ start JSONL tailer (seek to recorded offset, then tail appended lines)
//	→ wait for the ✻ spinner regex (thinking)
//	→ wait for BOTH: spinner gone AND assistant event with stop_reason=="end_turn"
//	→ concatenate every content[].text on that record → SUCCESS: <text>
//	→ SIGTERM (3 s grace) → SIGKILL → close PTY → wg.Wait
//
// This file is spike-quality: single binary, no public API. The reusable
// primitives are deliberately not extracted yet — they settle after the spike
// surfaces friction. See cmd/spike-one-turn/README.md for the empirical log.
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
	rollingBufferCap   = 4096
	statePollInterval  = 50 * time.Millisecond
	jsonlTailInterval  = 50 * time.Millisecond
	sessionFileWait    = 10 * time.Second
	sessionFilePoll    = 100 * time.Millisecond
	watchdogTick       = 1 * time.Second
	inactivityLimit    = 60 * time.Second
	spinnerFreezeLimit = 30 * time.Second
	shutdownGrace      = 3 * time.Second

	promptText = "What is 2+2?\r"

	ptyRows = 40
	ptyCols = 120
)

// spinnerRe matches the thinking indicator. The verb (group 1) is 1–2 words and
// varies per prompt — capture it for the empirical log; do NOT depend on any
// specific value. Time-tail is `Ns` or `Nm Ns` (groups 2,3).
var spinnerRe = regexp.MustCompile(`✻\s+(\S+(?:\s+\S+)?)\s+for\s+(?:(\d+)m\s+)?(\d+)s`)

// CSI sequences in claude's output wrap the spinner and prompt glyphs in color
// codes. A single-pass strip is enough for regex matching at spike fidelity.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// idleGlyph is the UTF-8 encoding of ❯ — claude's input-line prompt marker.
var idleGlyph = []byte("\xe2\x9d\xaf")

func main() {
	sessionIDFlag := flag.String("session-id", "", "UUID to pin claude's session ID and JSONL filename (default: generate one)")
	flag.Parse()

	if err := run(*sessionIDFlag); err != nil {
		fmt.Fprintf(os.Stderr, "spike failed: %v\n", err)
		os.Exit(1)
	}
}

func run(sessionIDFlag string) error {
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

	rb := &rollingBuffer{}
	tr := newTracker()
	tr.recordTransition("start")

	cmd := exec.Command("claude", "--session-id", sessionID)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("pty.Start: %w", err)
	}
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: ptyRows, Cols: ptyCols}); err != nil {
		// Non-fatal — claude will still draw, just possibly clipped.
		logger.Printf("warning: pty.Setsize: %v", err)
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

	// Watchdog: 1 Hz inactivity + spinner-freeze enforcement.
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
				snap := rb.snapshot()
				stripped := ansiRe.ReplaceAll(snap, nil)
				verb, total, ok := matchSpinner(stripped)
				_ = verb
				tr.observeSpinner(ok, total)
				if werr := tr.checkWatchdog(); werr != nil {
					logger.Printf("%v", werr)
					cancelCause(werr)
					return
				}
			}
		}
	}()

	// --- linear state machine ---

	if err := waitUntil(rootCtx, func() bool {
		return isIdle(rb.snapshot())
	}); err != nil {
		return fmt.Errorf("wait idle: %w", err)
	}
	tr.recordTransition("idle-detected")
	logger.Printf("idle-detected")

	if err := detectTrustModal(rb.snapshot()); err != nil {
		return err
	}

	if _, err := ptmx.Write([]byte(promptText)); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	tr.recordTransition("prompt-written")
	logger.Printf("prompt-written")

	// Wait for the deterministic session JSONL to exist. Interactive claude
	// under --session-id defers JSONL creation until first input is received
	// (verified empirically; documented in README finding #9), so we poll
	// AFTER prompt-written rather than after idle. The file is brand new in
	// this flow — tail from offset 0 and let the parser filter skip startup
	// envelopes the same way it does for non-assistant events.
	if err := openSessionJSONL(jsonlPath); err != nil {
		return fmt.Errorf("open session jsonl: %w", err)
	}
	logger.Printf("session-jsonl-opened path=%s offset=0", jsonlPath)

	eventCh := make(chan map[string]any, 32)
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

	// Terminate on JSONL end_turn. Spinner observation is opportunistic: the
	// slow path emits thinking-detected → spinner-gone; the fast path skips
	// both (trivial prompts can resolve before the spinner renders, and even
	// when it does render, finding #8 currently blocks regex match). The
	// thinkingObserved && !spinnerGone guard preserves slow-path log order.
	var (
		thinkingObserved bool
		thinkingVerb     string
		spinnerGone      bool
		gotEndTurn       bool
		assistantText    string
	)
	probe := time.NewTicker(statePollInterval)
	defer probe.Stop()
	for !(gotEndTurn && (!thinkingObserved || spinnerGone)) {
		select {
		case <-rootCtx.Done():
			return fmt.Errorf("wait termination: %w", context.Cause(rootCtx))
		case ev := <-eventCh:
			if !gotEndTurn && isEndTurn(ev) {
				assistantText = extractAssistantText(ev)
				gotEndTurn = true
				tr.recordTransition("end-turn-detected")
				logger.Printf("end-turn-detected")
			}
		case <-probe.C:
			stripped := ansiRe.ReplaceAll(rb.snapshot(), nil)
			v, _, ok := matchSpinner(stripped)
			switch {
			case ok && !thinkingObserved:
				thinkingVerb = v
				thinkingObserved = true
				tr.recordTransition("thinking-detected")
				logger.Printf("thinking-detected verb=%q", thinkingVerb)
			case !ok && thinkingObserved && !spinnerGone:
				spinnerGone = true
				tr.recordTransition("spinner-gone")
				logger.Printf("spinner-gone")
			}
		}
	}

	tr.recordTransition("assistant-text-extracted")
	logger.Printf("assistant-text-extracted len=%d", len(assistantText))

	fmt.Printf("SUCCESS: %s\n", assistantText)

	logger.Printf("complete elapsed=%s", time.Since(startedAt).Round(time.Millisecond))
	return nil
}

// --- rolling buffer ---

type rollingBuffer struct {
	mu  sync.Mutex
	buf []byte
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
}

func (r *rollingBuffer) snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, len(r.buf))
	copy(out, r.buf)
	return out
}

// --- pattern matching ---

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

// detectTrustModal: claude shows a "trust this folder" dialog on first
// use of any previously-unseen cwd. The spike's isIdle predicate matches
// inside it (claude renders ❯ in the modal's input field), so without
// this check the spike would type its prompt into the trust modal and
// time out opaquely on "session JSONL did not appear within 10s." Fire
// this right after idle-detected and return a clear error pointing at
// the fix (run claude interactively in the cwd once, accept trust).
//
// Handles both Bash-style space-stripped rendering (Itrustthisfolder)
// and Read-style space-preserved rendering (I trust this folder).
// See loop 2 exp B-5 (2026-05-18) for derivation.
func detectTrustModal(snap []byte) error {
	stripped := ansiRe.ReplaceAll(snap, nil)
	if bytes.Contains(stripped, []byte("trust this folder")) ||
		bytes.Contains(stripped, []byte("trustthisfolder")) ||
		bytes.Contains(stripped, []byte("Quicksafetycheck")) {
		return fmt.Errorf("claude shows the trust-folder dialog — this cwd hasn't been trusted yet. Run `claude` interactively in this directory once, accept trust, exit, then re-run the spike")
	}
	return nil
}

// isIdle: ❯ glyph is present AND spinner regex does not match. The TUI redraws
// the input line below the spinner during thinking, so ❯ alone is not enough.
func isIdle(snap []byte) bool {
	stripped := ansiRe.ReplaceAll(snap, nil)
	if !bytes.Contains(stripped, idleGlyph) {
		return false
	}
	return !spinnerRe.Match(stripped)
}

// --- tracker (state + watchdog bookkeeping) ---

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

// observeSpinner is called from the watchdog tick with whether the spinner
// regex matched right now and its total-seconds reading. It manages the
// freeze-detection bookkeeping: progress is "strictly greater than the
// previous reading" while the spinner is continuously visible.
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

// projectsDir resolves $HOME/.claude/projects/<encoded-cwd>/ at runtime.
// The encoding is byte-by-byte: '/', '.', and ' ' become '-', everything else
// passes through. Adjacent chars therefore produce '--' (non-reversible).
// Space mapping confirmed empirically against vault cwd `Second Brain`
// landing at `...-Second-Brain` in claude's projects dir (2026-05-17).
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

// encodeCwd matches claude's empirically-confirmed projects-dir
// encoding rule (loop 2 exp B-4, 2026-05-18): every non-alphanumeric
// byte → '-' (one-to-one substitution, not run-collapsed). Tested
// against /private/tmp/encode test (with) [brackets] & amp+plus_under
// which claude wrote to `-private-tmp-encode-test--with---brackets----amp-plus-under`
// — every special char including `_` became exactly one '-'.
func encodeCwd(cwd string) string {
	var b strings.Builder
	b.Grow(len(cwd))
	for i := 0; i < len(cwd); i++ {
		c := cwd[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// resolveSession turns the operator-supplied flag into a normalised session ID
// and the deterministic JSONL path. Empty flag → generate a fresh v4 UUID;
// non-empty → must parse as a valid UUID (uuid.Parse accepts hyphenated and
// non-hyphenated forms; .String() re-emits the canonical lowercase-hyphenated
// shape that matches claude's filename convention).
//
// The function does not stat jsonlPath — the discovery loop owns that.
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

// openSessionJSONL polls the deterministic JSONL path until it appears. The
// path was pinned up-front via --session-id, so there is no directory scan
// and no mtime heuristic — pre-existing stale .jsonl files in the same
// directory are structurally invisible. Interactive claude defers JSONL
// creation until first input (see README finding #9), so callers should wait
// until after prompt-written before invoking this.
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
			// Filter: only assistant events with a message object can carry
			// stop_reason. Everything else (permission-mode, file-history-snapshot,
			// attachment, ai-title, system, last-prompt, user, and future
			// envelopes) is silently ignored.
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
			// EOF with no newline → keep the partial bytes and wait for more.
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

func isEndTurn(ev map[string]any) bool {
	msg, _ := ev["message"].(map[string]any)
	stop, _ := msg["stop_reason"].(string)
	return stop == "end_turn"
}

func extractAssistantText(ev map[string]any) string {
	msg, _ := ev["message"].(map[string]any)
	content, _ := msg["content"].([]any)
	var b strings.Builder
	for _, c := range content {
		cm, _ := c.(map[string]any)
		if t, _ := cm["type"].(string); t != "text" {
			continue
		}
		if txt, _ := cm["text"].(string); txt != "" {
			b.WriteString(txt)
		}
	}
	return b.String()
}

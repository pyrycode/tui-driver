// Spike: observational probe of claude's slash-command picker — a
// multiselect-style modal with a different PTY shape from the permission
// modal documented in spike #13. Loop 3 exp C-2 (2026-05-18).
//
// Sequence:
//
//	allocate PTY → spawn `claude` → wait for ❯ (idle)
//	→ handle trust-folder modal per policy (same shape as spike-one-turn)
//	→ write the trigger keystroke (default '/') without an Enter
//	→ sleep settleWindow for the picker to render fully
//	→ snapshot rolling buffer, write a binary dump to /tmp
//	→ log structural observations (line count, longest option, etc.)
//	→ ESC to dismiss the picker (no commitment to running anything)
//	→ shutdown
//
// The probe is intentionally minimal — purely observational. The output is
// the binary snapshot file + structural log lines. Library extraction will
// use these to design a multiselect-modal predicate + extractor (distinct
// from the permission-modal primitives, which assume 3 options + literal-
// text predicate).
//
// Spike-quality: single binary, no public API. Helpers copy-pasted from
// cmd/spike-one-turn (attributed inline) — kept here until library
// extraction lands.
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
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const (
	rollingBufferCap   = 4096
	statePollInterval  = 50 * time.Millisecond
	watchdogTick       = 1 * time.Second
	ptyQuietLimit      = 30 * time.Second
	spinnerFreezeLimit = 30 * time.Second
	shutdownGrace      = 3 * time.Second

	ptyRows = 40
	ptyCols = 120

	// settleWindow: time to wait after sending the trigger keystroke before
	// snapshotting the picker. Claude's picker animates in (fade + slide),
	// so a too-short window misses the fully-rendered state. 1.5 s is plenty.
	settleWindow = 1500 * time.Millisecond
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)
var oscRe = regexp.MustCompile(`\x1b\][^\x07]*\x07`)
var idleGlyph = []byte("\xe2\x9d\xaf")
var spinnerGlyph = []byte("\xe2\x9c\xbb")
var spinnerRe = regexp.MustCompile(`✻\s+(\S+(?:\s+\S+)?)\s+for\s+(?:(\d+)m\s+)?(\d+)s`)

func main() {
	triggerFlag := flag.String("trigger", "/",
		"keystroke to trigger the picker: '/' (slash-command autocomplete, default), '@' (file picker), '#' (memory)")
	trustFolderFlag := flag.String("trust-folder", "fail",
		"policy when claude's trust-folder dialog appears at idle: 'fail' (default — return clear error) or 'accept' (send `1\\r` to auto-trust this cwd, then proceed)")
	flag.Parse()

	if *trustFolderFlag != "fail" && *trustFolderFlag != "accept" {
		fmt.Fprintf(os.Stderr, "invalid -trust-folder value %q (want 'fail' or 'accept')\n", *trustFolderFlag)
		os.Exit(2)
	}

	if err := run(*triggerFlag, *trustFolderFlag); err != nil {
		fmt.Fprintf(os.Stderr, "spike failed: %v\n", err)
		os.Exit(1)
	}
}

func run(trigger string, trustFolderPolicy string) error {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	startedAt := time.Now()
	logger.Printf("trigger=%q", trigger)

	rootCtx, cancelCause := context.WithCancelCause(context.Background())
	defer cancelCause(errors.New("run: returning"))

	rb := &rollingBuffer{}
	tr := newTracker()
	tr.recordTransition("start")

	// No --session-id flag — we're not driving a turn, just observing UI.
	// No --permission-mode either; the picker is a UI affordance, doesn't
	// invoke tools.
	cmd := exec.Command("claude")
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

	// PTY reader.
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

	// Watchdog: PTY-quiet + spinner-freeze. No wall-cap (per loop 2 wrap-up).
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
				_, total, ok := matchSpinner(stripped)
				tr.observeSpinner(ok, total)
				if werr := tr.checkWatchdog(rb); werr != nil {
					logger.Printf("%v", werr)
					cancelCause(werr)
					return
				}
			}
		}
	}()

	// Wait for idle.
	if err := waitUntil(rootCtx, func() bool { return isIdle(rb.snapshot()) }); err != nil {
		return fmt.Errorf("wait idle: %w", err)
	}
	tr.recordTransition("idle-detected")
	logger.Printf("idle-detected")

	// Trust-folder dialog handling. Same shape as spike-one-turn.
	if hasTrustModal(rb.snapshot()) {
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
				snap := rb.snapshot()
				return !hasTrustModal(snap) && isIdle(snap)
			}); err != nil {
				return fmt.Errorf("wait for idle post-trust-accept: %w", err)
			}
			tr.recordTransition("idle-detected-post-trust")
			logger.Printf("idle-detected-post-trust")
		}
	}

	// Send the trigger keystroke (no Enter — we want the picker to OPEN,
	// not commit a command). Claude's TUI uses the keystroke to begin
	// pre-fix matching against available commands and renders the picker.
	if _, err := ptmx.Write([]byte(trigger)); err != nil {
		return fmt.Errorf("write trigger: %w", err)
	}
	tr.recordTransition("trigger-sent")
	logger.Printf("trigger-sent bytes=%x", []byte(trigger))

	// Settle window: let the picker fully render.
	select {
	case <-rootCtx.Done():
		return context.Cause(rootCtx)
	case <-time.After(settleWindow):
	}

	// Snapshot. Dump raw bytes to /tmp for byte-level inspection;
	// emit structural observations to the state log.
	snap := rb.snapshot()
	stripped := oscRe.ReplaceAll(ansiRe.ReplaceAll(snap, nil), nil)
	dumpPath := fmt.Sprintf("/tmp/spike-multiselect-bytes-%d.bin", time.Now().UnixNano())
	if err := os.WriteFile(dumpPath, snap, 0o644); err != nil {
		logger.Printf("warning: write snapshot: %v", err)
	}
	tr.recordTransition("picker-snapshot")
	logger.Printf("picker-snapshot path=%s raw_len=%d stripped_len=%d", dumpPath, len(snap), len(stripped))

	// Structural metrics — what's in the snapshot?
	// 1. Box-drawing chars present? (Picker is likely rendered as a box.)
	hasBox := bytes.ContainsAny(stripped, "╭╮╰╯│─┌┐└┘├┤")
	// 2. Line count after stripping.
	lines := bytes.Split(stripped, []byte("\n"))
	// 3. Longest non-empty line (probably picker width).
	var longest int
	for _, l := range lines {
		if len(l) > longest {
			longest = len(l)
		}
	}
	// 4. Numbered options? Look for "1." through "9." patterns.
	var numberedOptions []int
	for digit := 1; digit <= 9; digit++ {
		if bytes.Contains(stripped, []byte(fmt.Sprintf("%d.", digit))) {
			numberedOptions = append(numberedOptions, digit)
		}
	}
	// 5. Slash-prefix-command lines? Count lines starting with `/`.
	var slashLines int
	for _, l := range lines {
		trimmed := bytes.TrimLeft(l, " \t")
		if len(trimmed) > 0 && trimmed[0] == '/' {
			slashLines++
		}
	}

	logger.Printf("picker-shape has_box_drawing=%v line_count=%d longest_line=%d numbered_options=%v slash_command_lines=%d",
		hasBox, len(lines), longest, numberedOptions, slashLines)

	// Look for some specific picker hints we might recognize.
	hints := []string{
		"to apply", "to select", "to navigate", "Esc to", "esc to",
		"Tab to", "tab to", "Enter to", "enter to",
	}
	var foundHints []string
	for _, h := range hints {
		if bytes.Contains(stripped, []byte(h)) {
			foundHints = append(foundHints, h)
		}
	}
	if len(foundHints) > 0 {
		logger.Printf("picker-hints found=%v", foundHints)
	}

	// Send ESC to dismiss the picker — no commitment to running anything.
	if _, err := ptmx.Write([]byte{0x1b}); err != nil {
		return fmt.Errorf("write esc: %w", err)
	}
	tr.recordTransition("picker-dismissed")
	logger.Printf("picker-dismissed bytes=1b")

	// Brief verification window: confirm picker is gone (we want to know
	// the dismiss keystroke works and how long it takes).
	dismissDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(dismissDeadline) {
		if isIdle(rb.snapshot()) {
			logger.Printf("idle-detected-post-dismiss elapsed=%s", time.Since(dismissDeadline.Add(-3*time.Second)).Round(time.Millisecond))
			break
		}
		select {
		case <-rootCtx.Done():
			return context.Cause(rootCtx)
		case <-time.After(100 * time.Millisecond):
		}
	}

	logger.Printf("complete elapsed=%s", time.Since(startedAt).Round(time.Millisecond))
	fmt.Printf("OBSERVED: picker snapshot at %s\n", dumpPath)
	return nil
}

// --- shared primitives copied from cmd/spike-one-turn / cmd/spike-permission ---

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

func (t *tracker) checkWatchdog(rb *rollingBuffer) error {
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
	return nil
}

func waitUntil(ctx context.Context, predicate func() bool) error {
	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()
	for {
		if predicate() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func isIdle(snap []byte) bool {
	stripped := ansiRe.ReplaceAll(snap, nil)
	if !bytes.Contains(stripped, idleGlyph) {
		return false
	}
	if bytes.Contains(stripped, spinnerGlyph) {
		return false
	}
	return true
}

func hasTrustModal(snap []byte) bool {
	stripped := ansiRe.ReplaceAll(snap, nil)
	return bytes.Contains(stripped, []byte("Quicksafetycheck"))
}

func matchSpinner(stripped []byte) (verb string, totalSeconds int, ok bool) {
	m := spinnerRe.FindSubmatch(stripped)
	if m == nil {
		return "", 0, false
	}
	verb = string(m[1])
	if len(m[3]) > 0 {
		fmt.Sscanf(string(m[3]), "%d", &totalSeconds)
		if len(m[2]) > 0 {
			var minutes int
			fmt.Sscanf(string(m[2]), "%d", &minutes)
			totalSeconds += minutes * 60
		}
	}
	return verb, totalSeconds, true
}

// Compile-time references so we keep parity with the rest of the spike suite.
var (
	_ = filepath.Join
	_ = strings.Repeat
)

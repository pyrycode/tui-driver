package tuidriver

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// idleGlyphTest is the UTF-8 encoding of ❯, claude's input-line prompt marker.
// tui-driver legitimately owns claude-TUI screen literals, so they live freely
// in its tests (unlike a consumer, which the substrate guard fences).
const idleGlyphTest = "\xe2\x9d\xaf"

func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var b bytes.Buffer
	return slog.New(slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelDebug})), &b
}

func TestHasPastedChip(t *testing.T) {
	tests := []struct {
		name string
		snap []byte
		want bool
	}{
		{"chip present plain", []byte("[Pasted text +3 lines] " + idleGlyphTest + " "), true},
		{"ansi-escaped chip", []byte("[Pasted\x1b[0m text +3 lines] " + idleGlyphTest), true},
		{"no chip at idle", []byte(idleGlyphTest + " "), false},
		{"empty snapshot", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasPastedChip(tt.snap); got != tt.want {
				t.Errorf("hasPastedChip(%q) = %v, want %v", tt.snap, got, tt.want)
			}
		})
	}
}

func TestShouldTypePrompt(t *testing.T) {
	overCap := strings.Repeat("x", typePromptMaxLen+1)
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"short single-line", "What is 2+2?", true},
		{"empty", "", true},
		{"exactly at the length cap", strings.Repeat("x", typePromptMaxLen), true},
		{"runner-comparison prompt", "Reply with the single word OK and nothing else.", true},
		{"sigterm prompt", "Use the Bash tool to run `sleep 30`. Do nothing else.", true},
		{"multi-line", "line one\nline two", false},
		{"single trailing newline", "ok\n", false},
		{"embedded carriage return", "before\rafter", false},
		{"embedded ESC", "a\x1bb", false},
		{"embedded tab", "a\tb", false},
		{"embedded NUL", "a\x00b", false},
		{"over the length cap", overCap, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldTypePrompt(tt.text); got != tt.want {
				t.Errorf("shouldTypePrompt(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestDeliverPrompt_CommitSuccess(t *testing.T) {
	var writes, clears int
	logger, logs := captureLogger()
	deps := deliverDeps{
		write:     func(string) error { writes++; return nil },
		clear:     func() error { clears++; return nil },
		didCommit: func(time.Duration) bool { return true },
		hasChip:   func() bool { t.Fatal("hasChip should not be consulted after a commit"); return false },
	}
	res, err := deliverPrompt(context.Background(), DeliverOpts{Prompt: "p", Logger: logger}, deps)
	if err != nil {
		t.Fatalf("deliverPrompt: %v", err)
	}
	if !res.Committed {
		t.Error("Committed = false, want true")
	}
	if res.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", res.Attempts)
	}
	if writes != 1 || clears != 0 {
		t.Errorf("writes=%d clears=%d, want 1/0", writes, clears)
	}
	if strings.Contains(logs.String(), "re-delivering") {
		t.Errorf("unexpected re-deliver marker on clean commit: %s", logs.String())
	}
}

// TestDeliverPrompt_CommitSlowNoChip is the #227 case: no commit signal yet,
// but the input box shows no pasted-text chip, so the paste committed and the
// signals are merely lagging. It must NOT re-deliver (re-pasting an in-flight
// turn is destructive) and must report Committed.
func TestDeliverPrompt_CommitSlowNoChip(t *testing.T) {
	var writes, clears int
	logger, logs := captureLogger()
	deps := deliverDeps{
		write:     func(string) error { writes++; return nil },
		clear:     func() error { clears++; return nil },
		didCommit: func(time.Duration) bool { return false },
		hasChip:   func() bool { return false },
	}
	res, err := deliverPrompt(context.Background(), DeliverOpts{Prompt: "p", Logger: logger}, deps)
	if err != nil {
		t.Fatalf("deliverPrompt: %v", err)
	}
	if !res.Committed {
		t.Error("Committed = false, want true (committed-but-slow)")
	}
	if res.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (no re-deliver)", res.Attempts)
	}
	if writes != 1 || clears != 0 {
		t.Errorf("writes=%d clears=%d, want 1/0 (must not re-deliver)", writes, clears)
	}
	if !strings.Contains(logs.String(), "committed-but-slow") {
		t.Errorf("missing committed-but-slow marker: %s", logs.String())
	}
}

// TestDeliverPrompt_ChipPresentReDeliver: chip present every attempt and no
// commit ever → re-delivers up to MaxAttempts, clears before each retry, and
// reports Committed=false (the residual-wedge backstop).
func TestDeliverPrompt_ChipPresentReDeliver(t *testing.T) {
	var writes, clears int
	logger, logs := captureLogger()
	deps := deliverDeps{
		write:     func(string) error { writes++; return nil },
		clear:     func() error { clears++; return nil },
		didCommit: func(time.Duration) bool { return false },
		hasChip:   func() bool { return true },
	}
	res, err := deliverPrompt(context.Background(), DeliverOpts{Prompt: "p", MaxAttempts: 3, Logger: logger}, deps)
	if err != nil {
		t.Fatalf("deliverPrompt: %v", err)
	}
	if res.Committed {
		t.Error("Committed = true, want false (residual wedge)")
	}
	if res.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", res.Attempts)
	}
	if writes != 3 || clears != 2 {
		t.Errorf("writes=%d clears=%d, want 3/2", writes, clears)
	}
	if !strings.Contains(logs.String(), "re-delivering") {
		t.Errorf("missing re-deliver marker: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "after retries") {
		t.Errorf("missing give-up marker: %s", logs.String())
	}
}

// TestDeliverPrompt_CtxCancelledMidLoop: ctx is cancelled during the commit
// poll (didCommit returns false, mirroring promptDidCommit on cancel). Even with
// the pasted-text chip still present — the TestDeliverPrompt_ChipPresentReDeliver
// path that would otherwise re-deliver — the loop must stop after the in-flight
// attempt: no further clear/write, at most the one write already issued (#160).
func TestDeliverPrompt_CtxCancelledMidLoop(t *testing.T) {
	var writes, clears int
	logger, logs := captureLogger()
	ctx, cancel := context.WithCancel(context.Background())
	deps := deliverDeps{
		write:     func(string) error { writes++; return nil },
		clear:     func() error { clears++; return nil },
		didCommit: func(time.Duration) bool { cancel(); return false },
		hasChip:   func() bool { return true },
	}
	res, err := deliverPrompt(ctx, DeliverOpts{Prompt: "p", MaxAttempts: 3, Logger: logger}, deps)
	if err != nil {
		t.Fatalf("deliverPrompt: %v", err)
	}
	if res.Committed {
		t.Error("Committed = true, want false (cancelled, not committed)")
	}
	if res.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (no re-delivery after cancel)", res.Attempts)
	}
	if writes != 1 || clears != 0 {
		t.Errorf("writes=%d clears=%d, want 1/0 (no re-delivery after cancel)", writes, clears)
	}
	if strings.Contains(logs.String(), "pasted-text chip present); re-delivering") {
		t.Errorf("unexpected re-deliver marker after cancel: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "context cancelled between delivery attempts") {
		t.Errorf("missing cancel marker: %s", logs.String())
	}
}

// TestDeliverPrompt_ChipThenCommit: chip present on attempt 1, then the
// re-delivery commits on attempt 2.
func TestDeliverPrompt_ChipThenCommit(t *testing.T) {
	var writes, clears, commitChecks int
	deps := deliverDeps{
		write: func(string) error { writes++; return nil },
		clear: func() error { clears++; return nil },
		didCommit: func(time.Duration) bool {
			commitChecks++
			return commitChecks >= 2 // false on attempt 1, true on attempt 2
		},
		hasChip: func() bool { return true },
	}
	logger, _ := captureLogger()
	res, err := deliverPrompt(context.Background(), DeliverOpts{Prompt: "p", Logger: logger}, deps)
	if err != nil {
		t.Fatalf("deliverPrompt: %v", err)
	}
	if !res.Committed {
		t.Error("Committed = false, want true (recovered on attempt 2)")
	}
	if res.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", res.Attempts)
	}
	if writes != 2 || clears != 1 {
		t.Errorf("writes=%d clears=%d, want 2/1", writes, clears)
	}
}

func TestDeliverPrompt_WriteError(t *testing.T) {
	deps := deliverDeps{
		write:     func(string) error { return os.ErrClosed },
		clear:     func() error { return nil },
		didCommit: func(time.Duration) bool { return false },
		hasChip:   func() bool { return false },
	}
	logger, _ := captureLogger()
	_, err := deliverPrompt(context.Background(), DeliverOpts{Prompt: "p", Logger: logger}, deps)
	if err == nil || !strings.Contains(err.Error(), "write prompt") {
		t.Errorf("err = %v, want it to wrap a write-prompt failure", err)
	}
}

func TestDeliverPrompt_ClearError(t *testing.T) {
	deps := deliverDeps{
		write:     func(string) error { return nil },
		clear:     func() error { return os.ErrClosed },
		didCommit: func(time.Duration) bool { return false },
		hasChip:   func() bool { return true }, // forces a retry → clear is called
	}
	logger, _ := captureLogger()
	_, err := deliverPrompt(context.Background(), DeliverOpts{Prompt: "p", MaxAttempts: 3, Logger: logger}, deps)
	if err == nil || !strings.Contains(err.Error(), "clear input line") {
		t.Errorf("err = %v, want it to wrap a clear-input-line failure", err)
	}
}

func TestPromptDidCommit(t *testing.T) {
	t.Run("spinner visible returns true", func(t *testing.T) {
		s := &Session{buffer: NewBuffer(0)}
		s.buffer.Append(SpinnerGlyph) // ✻ → IsThinking true
		if !s.promptDidCommit(context.Background(), "", 0, time.Second) {
			t.Error("promptDidCommit = false, want true (spinner visible)")
		}
	})

	t.Run("spinner wins over a non-growing jsonl", func(t *testing.T) {
		// AC 4: the spinner is checked first and short-circuits before the
		// growth check runs. The JSONL sits at exactly the baseline (no growth
		// for this turn), yet the thinking spinner still reports committed.
		s := &Session{buffer: NewBuffer(0)}
		s.buffer.Append(SpinnerGlyph)
		jsonl := filepath.Join(t.TempDir(), "session.jsonl")
		content := []byte("{\"turn\":1}\n")
		if err := os.WriteFile(jsonl, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if !s.promptDidCommit(context.Background(), jsonl, int64(len(content)), time.Second) {
			t.Error("promptDidCommit = false, want true (spinner wins over non-growing jsonl)")
		}
	})

	t.Run("jsonl grows past baseline returns true", func(t *testing.T) {
		// AC 3 / turn one: the file first appears with content (or grows from
		// empty). With baseline 0, any non-empty file exceeds it via the file
		// signal alone (no spinner).
		s := &Session{buffer: NewBuffer(0)} // no spinner
		jsonl := filepath.Join(t.TempDir(), "session.jsonl")
		if err := os.WriteFile(jsonl, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if !s.promptDidCommit(context.Background(), jsonl, 0, time.Second) {
			t.Error("promptDidCommit = false, want true (jsonl grew past baseline 0)")
		}
	})

	t.Run("pre-existing jsonl that does not grow returns false", func(t *testing.T) {
		// AC 1, 2, 5 / turn two: a stale JSONL from a prior turn sits at the
		// baseline and does not grow during this delivery, with no spinner —
		// so no commit signal fires and the poll times out. This is the
		// regression the ticket restores: turn-two paste-recovery still runs
		// because promptDidCommit reports not-committed.
		s := &Session{buffer: NewBuffer(0)} // no spinner
		jsonl := filepath.Join(t.TempDir(), "session.jsonl")
		content := []byte("{\"turn\":1}\n")
		if err := os.WriteFile(jsonl, content, 0o600); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if s.promptDidCommit(context.Background(), jsonl, int64(len(content)), 120*time.Millisecond) {
			t.Error("promptDidCommit = true, want false (jsonl did not grow past baseline)")
		}
		if time.Since(start) < 100*time.Millisecond {
			t.Errorf("returned in %v, want it to honor the timeout", time.Since(start))
		}
	})

	t.Run("no signal times out to false", func(t *testing.T) {
		s := &Session{buffer: NewBuffer(0)}
		missing := filepath.Join(t.TempDir(), "absent.jsonl")
		start := time.Now()
		if s.promptDidCommit(context.Background(), missing, 0, 120*time.Millisecond) {
			t.Error("promptDidCommit = true, want false (no signal)")
		}
		if time.Since(start) < 100*time.Millisecond {
			t.Errorf("returned in %v, want it to honor the timeout", time.Since(start))
		}
	})

	t.Run("ctx cancel returns false promptly", func(t *testing.T) {
		s := &Session{buffer: NewBuffer(0)}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if s.promptDidCommit(ctx, "", 0, 5*time.Second) {
			t.Error("promptDidCommit = true, want false (ctx cancelled)")
		}
	})
}

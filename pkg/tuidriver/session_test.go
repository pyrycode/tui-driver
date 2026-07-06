package tuidriver

import (
	"bytes"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSpawnAndBufferReceivesOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	// `echo hello` writes "hello\n" then exits.
	cmd := exec.Command("echo", "hello")
	s, err := Spawn(cmd, SpawnOpts{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()

	// Process exits quickly. Wait then verify Buffer captured the output.
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	got := string(s.Snapshot())
	if !strings.Contains(got, "hello") {
		t.Errorf("Buffer.Snapshot = %q, want it to contain %q", got, "hello")
	}
}

// TestWaitBoundsReaderDrainAndReturnsExitErr drives Wait's two-arm select by
// hand — closing exited and controlling readerDone directly — so the
// bounded-grace behaviour is verified deterministically on every platform. The
// realistic PTY-holder repro (session_signal_other_test.go) only reproduces on
// Linux; this covers the exact changed logic everywhere, including the Darwin
// dev gate.
func TestWaitBoundsReaderDrainAndReturnsExitErr(t *testing.T) {
	exitErr := errors.New("real exit status")

	t.Run("reader drains: returns immediately with the exit error", func(t *testing.T) {
		s := &Session{
			exited:        make(chan struct{}),
			readerDone:    make(chan struct{}),
			shutdownGrace: time.Hour, // must not be reached on the drain path
			exitErr:       exitErr,
		}
		close(s.exited)
		close(s.readerDone)

		start := time.Now()
		err := s.Wait()
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("drain path took %v, want ~immediate (grace timer must not fire)", elapsed)
		}
		if !errors.Is(err, exitErr) {
			t.Errorf("Wait err = %v, want the real exit error %v", err, exitErr)
		}
	})

	t.Run("reader parked: returns the real exit error within the grace", func(t *testing.T) {
		s := &Session{
			exited:        make(chan struct{}),
			readerDone:    make(chan struct{}), // left open — simulates a held-open PTY slave
			shutdownGrace: 200 * time.Millisecond,
			exitErr:       exitErr,
		}
		close(s.exited)

		start := time.Now()
		err := s.Wait()
		elapsed := time.Since(start)
		// Waited the drain window (not instant) but stayed bounded (not hung).
		if elapsed < 100*time.Millisecond {
			t.Errorf("Wait returned after %v, want it to wait ~the grace window first", elapsed)
		}
		if elapsed > 2*time.Second {
			t.Errorf("Wait returned after %v, want bounded by the grace (%v) + slack", elapsed, s.shutdownGrace)
		}
		// The real exit error flows through the timeout path — never a substitute.
		if !errors.Is(err, exitErr) {
			t.Errorf("Wait err = %v, want the real exit error %v (never a sentinel/substitute)", err, exitErr)
		}
	})
}

func TestSpawnWriteRawSendsToPTY(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	// `cat` echoes input. Send "ping\n" via the internal write funnel, expect
	// it back in the buffer. (The public Write seam is gone; writeRaw is the
	// single path all typed keystroke methods funnel through.)
	cmd := exec.Command("cat")
	s, err := Spawn(cmd, SpawnOpts{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()

	if err := s.writeRaw([]byte("ping\n")); err != nil {
		t.Fatalf("writeRaw: %v", err)
	}
	// Wait for cat to echo it back.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains(s.Snapshot(), []byte("ping")) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("Buffer never received echoed input; snap=%q", s.Snapshot())
}

func TestBracketedPasteWrapping(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []byte
	}{
		{
			name: "short single line",
			in:   "hi",
			want: []byte("\x1b[200~hi\x1b[201~\r"),
		},
		{
			name: "multi-line preserves embedded newlines verbatim",
			in:   "line one\nline two\nline three",
			want: []byte("\x1b[200~line one\nline two\nline three\x1b[201~\r"),
		},
		{
			name: "empty input still emits markers + commit",
			in:   "",
			want: []byte("\x1b[200~\x1b[201~\r"),
		},
		{
			name: "embedded CR survives unchanged inside body",
			in:   "before\rafter",
			want: []byte("\x1b[200~before\rafter\x1b[201~\r"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bracketedPaste(tt.in)
			if !bytes.Equal(got, tt.want) {
				t.Errorf("bracketedPaste(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSessionWritePromptSendsBracketedPaste(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	// `cat` echoes whatever bytes hit its stdin back to stdout — the PTY
	// master sees both, so the buffer captures exactly what we wrote.
	cmd := exec.Command("cat")
	s, err := Spawn(cmd, SpawnOpts{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()

	if err := s.WritePrompt("ping"); err != nil {
		t.Fatalf("WritePrompt: %v", err)
	}
	expected := []byte("\x1b[200~ping\x1b[201~")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains(s.Snapshot(), expected) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("Buffer never received bracketed-paste sequence; snap=%q", s.Snapshot())
}

func TestSessionClearInputLineSendsCtrlU(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	// 0x15 is the VKILL character for the PTY line discipline in cooked
	// mode — it is consumed by the kernel before reaching the child's
	// stdin. Switch the slave to raw mode (no line discipline) so the
	// echo round-trip preserves the byte. A "READY" sentinel after stty
	// proves the mode switch has taken effect before we call into the
	// API under test.
	cmd := exec.Command("sh", "-c", "stty raw -echo 2>/dev/null; printf READY; exec cat")
	s, err := Spawn(cmd, SpawnOpts{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()

	readyDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(readyDeadline) {
		if bytes.Contains(s.Snapshot(), []byte("READY")) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !bytes.Contains(s.Snapshot(), []byte("READY")) {
		t.Fatalf("stty/cat never reached READY; snap=%q", s.Snapshot())
	}

	if err := s.ClearInputLine(); err != nil {
		t.Fatalf("ClearInputLine: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		// Trim the READY sentinel and look for 0x15 in the remainder.
		snap := s.Snapshot()
		idx := bytes.Index(snap, []byte("READY"))
		if idx >= 0 && bytes.Contains(snap[idx+len("READY"):], []byte{0x15}) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("Buffer never received Ctrl-U after READY; snap=%q", s.Snapshot())
}

func TestSessionTypePromptSendsBytesThenCommit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	cmd := exec.Command("cat")
	s, err := Spawn(cmd, SpawnOpts{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()

	if err := s.TypePrompt("ping"); err != nil {
		t.Fatalf("TypePrompt: %v", err)
	}
	// cat in cooked-PTY mode may translate \r to \n on echo; assert that
	// the body landed and that either CR or LF followed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := s.Snapshot()
		if bytes.Contains(snap, []byte("ping\r")) || bytes.Contains(snap, []byte("ping\n")) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("Buffer never received typed prompt + commit; snap=%q", s.Snapshot())
}

func TestSessionTypePromptInterByteTiming(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	var sleeps []time.Duration
	oldSleep := sleepFn
	sleepFn = func(d time.Duration) { sleeps = append(sleeps, d) }
	t.Cleanup(func() { sleepFn = oldSleep })

	cmd := exec.Command("cat")
	s, err := Spawn(cmd, SpawnOpts{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()

	if err := s.TypePrompt("abc"); err != nil {
		t.Fatalf("TypePrompt: %v", err)
	}

	want := []time.Duration{
		PromptInterByteDelay,
		PromptInterByteDelay,
		PromptInterByteDelay,
		PromptCommitSettle,
	}
	if len(sleeps) != len(want) {
		t.Fatalf("sleepFn called %d times, want %d (got %v)", len(sleeps), len(want), sleeps)
	}
	for i, d := range want {
		if sleeps[i] != d {
			t.Errorf("sleep[%d] = %v, want %v", i, sleeps[i], d)
		}
	}
}

func TestSessionTypePromptEmptyString(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	var sleeps []time.Duration
	oldSleep := sleepFn
	sleepFn = func(d time.Duration) { sleeps = append(sleeps, d) }
	t.Cleanup(func() { sleepFn = oldSleep })

	cmd := exec.Command("cat")
	s, err := Spawn(cmd, SpawnOpts{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()

	if err := s.TypePrompt(""); err != nil {
		t.Fatalf("TypePrompt: %v", err)
	}

	// Empty body: no inter-byte sleeps, just the commit settle before \r.
	want := []time.Duration{PromptCommitSettle}
	if len(sleeps) != len(want) {
		t.Fatalf("sleepFn called %d times, want %d (got %v)", len(sleeps), len(want), sleeps)
	}
	if sleeps[0] != want[0] {
		t.Errorf("sleep[0] = %v, want %v", sleeps[0], want[0])
	}
}

func TestSessionClearInputLineTiming(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	var sleeps []time.Duration
	oldSleep := sleepFn
	sleepFn = func(d time.Duration) { sleeps = append(sleeps, d) }
	t.Cleanup(func() { sleepFn = oldSleep })

	cmd := exec.Command("cat")
	s, err := Spawn(cmd, SpawnOpts{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()

	if err := s.ClearInputLine(); err != nil {
		t.Fatalf("ClearInputLine: %v", err)
	}

	want := []time.Duration{ClearLineSettle}
	if len(sleeps) != len(want) {
		t.Fatalf("sleepFn called %d times, want %d (got %v)", len(sleeps), len(want), sleeps)
	}
	if sleeps[0] != want[0] {
		t.Errorf("sleep[0] = %v, want %v", sleeps[0], want[0])
	}
}

func TestSessionCloseIsIdempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	cmd := exec.Command("sleep", "10")
	s, err := Spawn(cmd, SpawnOpts{ShutdownGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// First Close should SIGTERM (sleep handles it, exits with signal).
	err1 := s.Close()
	// Second Close is a no-op.
	err2 := s.Close()
	if err1 == nil {
		// SIGTERM-killed sleep returns a non-nil ExitError; nil here is
		// surprising on macOS but valid on some systems — don't fail.
		t.Logf("first Close returned nil (acceptable but unusual)")
	}
	if err1 != err2 {
		t.Errorf("Close not idempotent: first=%v, second=%v", err1, err2)
	}
}

func TestSpawnSetsBufferDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	cmd := exec.Command("true")
	s, err := Spawn(cmd, SpawnOpts{}) // BufferCap=0 → default
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()
	if s.buffer == nil {
		t.Fatal("buffer is nil")
	}
	// We can't directly inspect cap, but we know NewBuffer(0) =
	// DefaultBufferCap. Append > cap bytes, snap should equal cap.
	big := bytes.Repeat([]byte("x"), DefaultBufferCap+100)
	s.buffer.Append(big)
	if got := len(s.Snapshot()); got != DefaultBufferCap {
		t.Errorf("buffer cap appears to be %d, want %d", got, DefaultBufferCap)
	}
}

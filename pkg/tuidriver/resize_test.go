package tuidriver

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"
)

// drainUntil reads chunks from ch into an accumulator until it contains want or
// the 2s deadline elapses, failing the test on timeout or premature close. It is
// mirror_test.go's drain idiom, factored one-shot for the resize tests that each
// make a single assertion. bytes.Contains tolerates the \r\n raw-mode endings.
func drainUntil(t *testing.T, ch <-chan []byte, want []byte) {
	t.Helper()
	var acc []byte
	deadline := time.After(2 * time.Second)
	for !bytes.Contains(acc, want) {
		select {
		case chunk, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed before %q; acc=%q", want, acc)
			}
			acc = append(acc, chunk...)
		case <-deadline:
			t.Fatalf("never saw %q; acc=%q", want, acc)
		}
	}
}

// TestResizeChildObservesNewSize is the key test (AC 1 + AC 2). The child blocks
// on read until the test releases it, then reports its terminal size via stty.
// Resizing BEFORE releasing the read makes stty size report the post-resize dims
// deterministically — no dependence on a SIGWINCH-interrupts-sleep race (that
// race is real: SIGWINCH's default disposition is ignore, so it never interrupts
// an external sleep). The observed "50 100" (different from the 40×120 default)
// is the child seeing the new size; SIGWINCH delivery itself is the kernel's
// unconditional response to the same TIOCSWINSZ ioctl whose effect is observed
// here (AC 1), per spec.
func TestResizeChildObservesNewSize(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	cmd := exec.Command("sh", "-c", "stty -echo 2>/dev/null; IFS= read -r _; stty size")
	s, err := Spawn(cmd, SpawnOpts{MirrorOutput: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()
	ch := s.MirrorOutput()
	if ch == nil {
		t.Fatal("MirrorOutput() = nil, want non-nil when enabled")
	}
	if err := s.Resize(50, 100); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := s.AttachInput([]byte("\n")); err != nil {
		t.Fatalf("AttachInput: %v", err)
	}
	drainUntil(t, ch, []byte("50 100"))
}

// TestResizeDefaultPreservedWhenNotCalled guards AC 3: a session that never
// calls Resize keeps the 40×120 StartPTY default, so existing callers are
// byte-for-byte unaffected. Same child shape, no Resize, asserts the default.
func TestResizeDefaultPreservedWhenNotCalled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	cmd := exec.Command("sh", "-c", "stty -echo 2>/dev/null; IFS= read -r _; stty size")
	s, err := Spawn(cmd, SpawnOpts{MirrorOutput: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()
	ch := s.MirrorOutput()
	if ch == nil {
		t.Fatal("MirrorOutput() = nil, want non-nil when enabled")
	}
	if err := s.AttachInput([]byte("\n")); err != nil {
		t.Fatalf("AttachInput: %v", err)
	}
	drainUntil(t, ch, []byte(fmt.Sprintf("%d %d", DefaultPtyRows, DefaultPtyCols)))
}

// TestResizeTracksGridDims pins #226's session-state half: a fresh session
// reports the 40x120 StartPTY default via gridDims (the accessor the per-tick
// classifier reads), and Resize updates it. gridDims returns (cols, rows) to
// match the NewGrid / classify dimension order.
func TestResizeTracksGridDims(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	cmd := exec.Command("sh", "-c", "IFS= read -r _")
	s, err := Spawn(cmd, SpawnOpts{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()

	if c, r := s.gridDims(); c != int(DefaultPtyCols) || r != int(DefaultPtyRows) {
		t.Fatalf("initial gridDims = (%d,%d), want (%d,%d)", c, r, DefaultPtyCols, DefaultPtyRows)
	}
	if err := s.Resize(50, 100); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if c, r := s.gridDims(); c != 100 || r != 50 {
		t.Fatalf("gridDims after Resize(50,100) = (%d,%d), want (100,50)", c, r)
	}
}

// TestSessionResizeDrivesDetectionDims is #226's AC integration test: resize a
// live session, then assert the per-tick classifier renders detection at the
// new dimensions, not the fixed 120x40 default. It drives the real mergeEvents
// loop with the session's own gridDims accessor and captures the dimensions the
// single render (gridForClassify, #225) is called with. Before #226 the loop
// always rendered at (0,0)->default; after it, the resized (100,50) flows
// through.
//
// Race-safety: the gridForClassify swap happens before the goroutine starts
// (happens-before via goroutine launch); the restore happens only after
// assertEventChClosed confirms the loop returned and closed out (happens-before
// via channel close), so no reader of the seam remains. The capture vars are
// guarded by mu.
func TestSessionResizeDrivesDetectionDims(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	cmd := exec.Command("sh", "-c", "IFS= read -r _")
	s, err := Spawn(cmd, SpawnOpts{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()
	if err := s.Resize(50, 100); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	orig := gridForClassify
	var mu sync.Mutex
	var gotCols, gotRows int
	var captured bool
	gridForClassify = func(snap []byte, cols, rows int) *Grid {
		mu.Lock()
		if !captured {
			gotCols, gotRows, captured = cols, rows, true
		}
		mu.Unlock()
		return orig(snap, cols, rows)
	}

	ctx, cancel := context.WithCancel(context.Background())
	jsonlCh := make(chan JSONLEntry)
	out := make(chan Event, defaultEventBuffer)
	go mergeEvents(ctx, s.buffer.Snapshot, s.gridDims, s.buffer.QuietFor, DefaultPTYQuietLimit, jsonlCh, out, DefaultPollInterval)

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		done := captured
		mu.Unlock()
		if done {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("classifier never rendered a tick within 2s")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()
	assertEventChClosed(t, out, 500*time.Millisecond)
	gridForClassify = orig

	if gotCols != 100 || gotRows != 50 {
		t.Fatalf("classifier rendered at (%d,%d), want the resized (100,50)", gotCols, gotRows)
	}
}

// TestResizeAfterCloseErrors guards AC 4: Resize on a closed session returns a
// non-nil error (the closed master FD yields EBADF) and does not panic — a
// panic here would fail the test.
func TestResizeAfterCloseErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	cmd := exec.Command("echo", "bye")
	s, err := Spawn(cmd, SpawnOpts{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Logf("Close returned %v (acceptable)", err)
	}
	if err := s.Resize(50, 100); err == nil {
		t.Fatal("Resize after Close = nil, want non-nil error")
	}
}

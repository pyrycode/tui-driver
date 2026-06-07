package tuidriver

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestMirrorOutputNilWhenDisabled guards the opt-in seal: a default Spawn
// returns a nil mirror stream, so no behaviour changes unless enabled.
func TestMirrorOutputNilWhenDisabled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	cmd := exec.Command("echo", "hi")
	s, err := Spawn(cmd, SpawnOpts{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()
	if ch := s.MirrorOutput(); ch != nil {
		t.Errorf("MirrorOutput() = %v, want nil when SpawnOpts.MirrorOutput is false", ch)
	}
}

// TestMirrorOutputDeliversVerbatim asserts the emitted bytes arrive on the
// stream untransformed. Torn content from a missing per-chunk copy would
// surface here as a corrupted concatenation.
func TestMirrorOutputDeliversVerbatim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	cmd := exec.Command("echo", "mirror-verbatim-output")
	s, err := Spawn(cmd, SpawnOpts{MirrorOutput: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()
	ch := s.MirrorOutput()
	if ch == nil {
		t.Fatal("MirrorOutput() = nil, want non-nil when enabled")
	}
	var got []byte
	for chunk := range ch {
		got = append(got, chunk...)
	}
	if !bytes.Contains(got, []byte("mirror-verbatim-output")) {
		t.Errorf("mirror stream missing emitted bytes; got %q", got)
	}
}

// TestMirrorOutputMultiChunkOrderedAndIndependent drives a payload spanning
// many 4 KB read chunks through the stream and asserts it arrives byte-for-byte
// in order. Waiting for the reader to finish first means every surviving chunk
// is already queued, so a missing per-chunk copy (chunks aliasing the reused
// read buffer) surfaces deterministically as a corrupted concatenation. The
// payload (~40 KB ≈ 10 chunks) stays well under defaultMirrorOutputBuffer, so
// nothing is dropped.
func TestMirrorOutputMultiChunkOrderedAndIndependent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	var want bytes.Buffer
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&want, "LINE%05d\n", i)
	}
	path := filepath.Join(t.TempDir(), "payload.txt")
	if err := os.WriteFile(path, want.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	// stty raw -echo so file bytes round-trip verbatim (no \n→\r\n output
	// post-processing); exec cat so the process exits when the file is drained.
	cmd := exec.Command("sh", "-c", "stty raw -echo 2>/dev/null; exec cat '"+path+"'")
	s, err := Spawn(cmd, SpawnOpts{MirrorOutput: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()
	ch := s.MirrorOutput()
	_ = s.Wait() // reader drained the PTY and closed the stream; chunks are queued.

	var got []byte
	for chunk := range ch {
		got = append(got, chunk...)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Errorf("mirror stream mismatch: got %d bytes, want %d", len(got), want.Len())
	}
}

// TestAttachInputReachesPTY exercises both directions: keystrokes written via
// AttachInput reach the hosted process, whose echo comes back on the mirror
// stream.
func TestAttachInputReachesPTY(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	cmd := exec.Command("sh", "-c", "stty raw -echo 2>/dev/null; printf READY; exec cat")
	s, err := Spawn(cmd, SpawnOpts{MirrorOutput: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()
	ch := s.MirrorOutput()
	if ch == nil {
		t.Fatal("MirrorOutput() = nil, want non-nil when enabled")
	}

	var acc []byte
	recvUntil := func(want []byte) {
		t.Helper()
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

	recvUntil([]byte("READY"))
	if err := s.AttachInput([]byte("ping\r")); err != nil {
		t.Fatalf("AttachInput: %v", err)
	}
	recvUntil([]byte("ping"))
}

// TestMirrorOutputClosesOnClose asserts the stream closes cleanly on Close for
// a still-running process — range terminates, no send-on-closed panic.
func TestMirrorOutputClosesOnClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	cmd := exec.Command("sh", "-c", "stty raw -echo 2>/dev/null; printf hello; exec cat")
	s, err := Spawn(cmd, SpawnOpts{MirrorOutput: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	ch := s.MirrorOutput()
	if err := s.Close(); err != nil {
		t.Logf("Close returned %v (acceptable)", err)
	}

	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("draining the stream after Close did not terminate; channel not closed")
	}
}

// TestMirrorOutputClosedAfterWait asserts the LIFO close ordering: the stream
// (mirrorOut) is closed before readerDone, so a consumer that calls Wait and
// then drains always observes the stream already closed.
func TestMirrorOutputClosedAfterWait(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	cmd := exec.Command("echo", "bye")
	s, err := Spawn(cmd, SpawnOpts{MirrorOutput: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()
	ch := s.MirrorOutput()
	_ = s.Wait() // returns only after readerDone, which closes after mirrorOut.

	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("draining the stream after Wait did not terminate; channel not closed")
	}
}

// TestMirrorOutputSlowConsumerDoesNotWedgeClose is the key safety test: a
// consumer that enables the stream but never drains it must not stall the
// shared reader goroutine. The drop-newest policy keeps the reader off a full
// channel, so its readerDone still closes and Close returns promptly. A
// blocking send would wedge the reader on a full channel and hang Close's
// <-readerDone.
func TestMirrorOutputSlowConsumerDoesNotWedgeClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	// yes floods the PTY past defaultMirrorOutputBuffer chunks in well under
	// the sleep below.
	cmd := exec.Command("sh", "-c", "stty raw -echo 2>/dev/null; exec yes")
	s, err := Spawn(cmd, SpawnOpts{MirrorOutput: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	_ = s.MirrorOutput() // obtained but deliberately never drained.
	time.Sleep(200 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		_ = s.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return; reader likely blocked on a full mirror channel (drop policy broken)")
	}
}

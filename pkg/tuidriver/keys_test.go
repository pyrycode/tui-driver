package tuidriver

import (
	"bytes"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestArrowKeyBytes(t *testing.T) {
	tests := []struct {
		name string
		k    ArrowKey
		want []byte
	}{
		{"up", ArrowUp, []byte{0x1b, '[', 'A'}},
		{"down", ArrowDown, []byte{0x1b, '[', 'B'}},
		{"left", ArrowLeft, []byte{0x1b, '[', 'D'}},
		{"right", ArrowRight, []byte{0x1b, '[', 'C'}},
		{"unknown returns nil", ArrowKey(99), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.k.bytes(); !bytes.Equal(got, tt.want) {
				t.Errorf("ArrowKey(%d).bytes() = %q, want %q", tt.k, got, tt.want)
			}
		})
	}
}

// spawnRawCat starts `cat` with the PTY slave in raw, no-echo mode so bytes
// written to the master round-trip back verbatim (no line-discipline
// transformation of \r or control bytes). Returns the session after the READY
// sentinel confirms the mode switch took effect.
func spawnRawCat(t *testing.T) *Session {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	cmd := exec.Command("sh", "-c", "stty raw -echo 2>/dev/null; printf READY; exec cat")
	s, err := Spawn(cmd, SpawnOpts{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains(s.Snapshot(), []byte("READY")) {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stty/cat never reached READY; snap=%q", s.Snapshot())
	return nil
}

// waitForBytes polls the session buffer (after the READY sentinel) for want.
func waitForBytes(t *testing.T, s *Session, want []byte) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := s.Snapshot()
		idx := bytes.Index(snap, []byte("READY"))
		if idx >= 0 && bytes.Contains(snap[idx+len("READY"):], want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("buffer never received %q after READY; snap=%q", want, s.Snapshot())
}

func TestSessionKeystrokeMethods(t *testing.T) {
	t.Run("AcceptTrust sends 1\\r", func(t *testing.T) {
		s := spawnRawCat(t)
		if err := s.AcceptTrust(); err != nil {
			t.Fatalf("AcceptTrust: %v", err)
		}
		waitForBytes(t, s, []byte("1\r"))
	})

	t.Run("Answer sends choice + \\r", func(t *testing.T) {
		s := spawnRawCat(t)
		if err := s.Answer("2"); err != nil {
			t.Fatalf("Answer: %v", err)
		}
		waitForBytes(t, s, []byte("2\r"))
	})

	t.Run("Answer empty sends bare \\r", func(t *testing.T) {
		s := spawnRawCat(t)
		if err := s.Answer(""); err != nil {
			t.Fatalf("Answer: %v", err)
		}
		waitForBytes(t, s, []byte("\r"))
	})

	t.Run("SendEsc sends 0x1b", func(t *testing.T) {
		s := spawnRawCat(t)
		if err := s.SendEsc(); err != nil {
			t.Fatalf("SendEsc: %v", err)
		}
		waitForBytes(t, s, []byte{0x1b})
	})

	t.Run("Navigate sends arrow CSI", func(t *testing.T) {
		s := spawnRawCat(t)
		if err := s.Navigate(ArrowDown); err != nil {
			t.Fatalf("Navigate: %v", err)
		}
		waitForBytes(t, s, []byte{0x1b, '[', 'B'})
	})

	t.Run("SendKeys sends raw bytes verbatim", func(t *testing.T) {
		s := spawnRawCat(t)
		if err := s.SendKeys("\x1b[B\r"); err != nil {
			t.Fatalf("SendKeys: %v", err)
		}
		waitForBytes(t, s, []byte("\x1b[B\r"))
	})
}

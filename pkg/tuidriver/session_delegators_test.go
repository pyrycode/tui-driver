package tuidriver

import (
	"bytes"
	"testing"
	"time"
)

// TestSessionBufferDelegators pins that Snapshot / QuietFor / LastAppendAt
// delegate to the rolling buffer — the read seam that replaces the exported
// Buffer field.
func TestSessionBufferDelegators(t *testing.T) {
	s := &Session{Buffer: NewBuffer(0)}

	if got := s.Snapshot(); len(got) != 0 {
		t.Errorf("Snapshot on empty = %q, want empty", got)
	}
	if !s.LastAppendAt().IsZero() {
		t.Error("LastAppendAt before any append = non-zero, want zero")
	}
	if d := s.QuietFor(); d != 0 {
		t.Errorf("QuietFor before any append = %v, want 0", d)
	}

	s.Buffer.Append([]byte("hello"))

	if got := s.Snapshot(); !bytes.Equal(got, []byte("hello")) {
		t.Errorf("Snapshot = %q, want %q", got, "hello")
	}
	if s.LastAppendAt().IsZero() {
		t.Error("LastAppendAt after append = zero, want set")
	}
	if d := s.QuietFor(); d < 0 || d > time.Second {
		t.Errorf("QuietFor = %v, want a small positive duration", d)
	}
}

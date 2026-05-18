package tuidriver

import (
	"bytes"
	"testing"
	"time"
)

func TestBufferAppendAndSnapshot(t *testing.T) {
	b := NewBuffer(0) // default cap
	b.Append([]byte("hello"))
	b.Append([]byte(" world"))
	got := b.Snapshot()
	if !bytes.Equal(got, []byte("hello world")) {
		t.Errorf("Snapshot = %q, want %q", got, "hello world")
	}
}

func TestBufferTrimsToCap(t *testing.T) {
	b := NewBuffer(8)
	b.Append([]byte("AAAAAAAA"))      // 8 bytes — exactly cap
	b.Append([]byte("BCDE"))          // pushes 4 oldest A's out
	want := []byte("AAAABCDE")
	if got := b.Snapshot(); !bytes.Equal(got, want) {
		t.Errorf("after overflow: Snapshot = %q, want %q", got, want)
	}
}

func TestBufferSnapshotIsCopy(t *testing.T) {
	b := NewBuffer(0)
	b.Append([]byte("xy"))
	snap := b.Snapshot()
	snap[0] = '?'
	if got := b.Snapshot(); !bytes.Equal(got, []byte("xy")) {
		t.Errorf("Snapshot mutation leaked into buffer: %q", got)
	}
}

func TestBufferQuietForZeroBeforeAnyAppend(t *testing.T) {
	b := NewBuffer(0)
	if d := b.QuietFor(); d != 0 {
		t.Errorf("QuietFor before any append = %v, want 0", d)
	}
}

func TestBufferQuietForMonotonic(t *testing.T) {
	b := NewBuffer(0)
	b.Append([]byte("x"))
	d1 := b.QuietFor()
	time.Sleep(5 * time.Millisecond)
	d2 := b.QuietFor()
	if d2 <= d1 {
		t.Errorf("QuietFor non-monotonic: d1=%v d2=%v", d1, d2)
	}
}

func TestBufferAppendResetsQuiet(t *testing.T) {
	b := NewBuffer(0)
	b.Append([]byte("x"))
	time.Sleep(10 * time.Millisecond)
	b.Append([]byte("y"))
	if d := b.QuietFor(); d > 5*time.Millisecond {
		t.Errorf("QuietFor after fresh append = %v, want close to 0", d)
	}
}

func TestBufferLastAppendAtTracksAppend(t *testing.T) {
	b := NewBuffer(0)
	before := b.LastAppendAt()
	if !before.IsZero() {
		t.Errorf("LastAppendAt before any append = %v, want zero", before)
	}
	t0 := time.Now()
	b.Append([]byte("x"))
	after := b.LastAppendAt()
	if after.Before(t0) {
		t.Errorf("LastAppendAt %v is before t0 %v", after, t0)
	}
}

func TestBufferNewBufferDefaultsCap(t *testing.T) {
	b := NewBuffer(-1)
	// Append 5000 bytes; should trim to DefaultBufferCap.
	big := bytes.Repeat([]byte("x"), 5000)
	b.Append(big)
	if got := len(b.Snapshot()); got != DefaultBufferCap {
		t.Errorf("len(Snapshot) = %d, want %d", got, DefaultBufferCap)
	}
}

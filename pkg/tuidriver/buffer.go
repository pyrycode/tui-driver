package tuidriver

import (
	"sync"
	"time"
)

// DefaultBufferCap is the rolling buffer capacity (16384 bytes) the live
// consumer (pyry agent-run, via BufferCap: 0) inherits. It is the rolling
// window sized so a full-screen panel keeps classifying for as long as it is
// physically on screen while ordinary status repaints (spinner / token-counter
// frames) trickle in — a smaller window evicted the panel's header bytes while
// the panel never left the screen. Region-scoped detectors read only the
// bottom rendered rows, so a larger window only starts VT100 reconstruction
// further back (strictly more faithful, fewer dropped characters); no detector
// loses protection.
const DefaultBufferCap = 16384

// Buffer is a thread-safe rolling byte buffer that tracks the time of the
// most recent append. Used as the PTY-side state-detection substrate: read
// goroutine appends raw PTY bytes; pattern matchers call Snapshot to inspect
// the latest rendering; the watchdog reads QuietFor as the PTY-heartbeat
// signal (loop 2 B-1).
//
// Construct with NewBuffer. The zero value is not usable.
type Buffer struct {
	mu           sync.Mutex
	buf          []byte
	cap          int
	lastAppendAt time.Time
}

// NewBuffer creates a Buffer with the given capacity. If cap <= 0,
// DefaultBufferCap is used. The buffer trims to cap on each Append.
func NewBuffer(cap int) *Buffer {
	if cap <= 0 {
		cap = DefaultBufferCap
	}
	return &Buffer{cap: cap}
}

// Append adds p to the buffer and updates LastAppendAt. If the buffer
// exceeds capacity, the oldest bytes are dropped.
func (b *Buffer) Append(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.cap {
		fresh := make([]byte, b.cap)
		copy(fresh, b.buf[len(b.buf)-b.cap:])
		b.buf = fresh
	}
	b.lastAppendAt = time.Now()
}

// Snapshot returns a copy of the buffer's current contents. Safe to read
// outside the lock.
func (b *Buffer) Snapshot() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, len(b.buf))
	copy(out, b.buf)
	return out
}

// QuietFor returns the time since the last Append. Returns 0 if the buffer
// has never been appended to.
func (b *Buffer) QuietFor() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastAppendAt.IsZero() {
		return 0
	}
	return time.Since(b.lastAppendAt)
}

// LastAppendAt returns the timestamp of the most recent Append, or the zero
// value if the buffer has never been appended to.
func (b *Buffer) LastAppendAt() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastAppendAt
}

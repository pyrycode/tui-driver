package tuidriver

import (
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWriteSerialize_NoInterleave drives three writers at the same PTY at once
// and asserts no logical write is split by another writer's bytes.
//
// The load-bearing racer is TypePrompt: it issues len(text)+1 separate
// os.File.Write calls, and Go's per-FD write lock serializes each individual
// write but NOT the sequence — so without a shared write mutex, another
// writer's Write can land between two of TypePrompt's byte-writes and split the
// prompt. A test that raced only single-write paths (WritePrompt vs.
// AttachInput) would pass even without the fix, because the per-FD lock already
// serializes those; that is the "-race alone will not prove serialization"
// caveat in the AC. So writers A and C both use TypePrompt, and their multi-byte
// runs are what the contiguity assertion protects.
//
// Wire shapes (alphabets pairwise disjoint and disjoint from the \r/\n
// separators, so "no payload split" ⟺ every maximal single-letter run has the
// expected length):
//   - A: TypePrompt("A"×16)         → "A…A"(16) + "\r"
//   - B: AttachInput("B"×16 + "\n") → "B…B"(16) + "\n"  (one atomic write)
//   - C: TypePrompt("C"×16)         → "C…C"(16) + "\r"
//
// sleepFn is swapped for runtime.Gosched so the test runs sub-second while
// forcing a scheduling point between every one of TypePrompt's byte-writes —
// the widest possible interleaving window. Self-checked once by removing the
// writeMu acquisition from TypePrompt: the A/C run-length assertion then fails
// on a split run, proving the test exercises the invariant.
func TestWriteSerialize_NoInterleave(t *testing.T) {
	oldSleep := sleepFn
	sleepFn = func(time.Duration) { runtime.Gosched() }
	t.Cleanup(func() { sleepFn = oldSleep })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	s := &Session{pty: w}

	// A pipe is lossless and order-preserving; drain it concurrently so a full
	// kernel buffer never blocks a writer holding writeMu.
	var received []byte
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		received, _ = io.ReadAll(r)
	}()

	const (
		iterations = 200
		markerLen  = 16
	)
	writers := []func() error{
		func() error { return s.TypePrompt(strings.Repeat("A", markerLen)) },
		func() error { return s.AttachInput([]byte(strings.Repeat("B", markerLen) + "\n")) },
		func() error { return s.TypePrompt(strings.Repeat("C", markerLen)) },
	}

	var wg sync.WaitGroup
	for _, write := range writers {
		wg.Add(1)
		go func(fn func() error) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if err := fn(); err != nil {
					t.Errorf("write %d: %v", i, err)
					return
				}
			}
		}(write)
	}
	wg.Wait()
	if err := w.Close(); err != nil { // EOF to the drainer
		t.Fatalf("close pipe: %v", err)
	}
	<-drainDone

	// Walk the received stream: every maximal run of a marker letter must be
	// exactly markerLen long. A foreign byte landing inside a run splits it into
	// shorter runs, tripping this. Non-letter bytes (\r, \n) are run boundaries.
	counts := map[byte]int{}
	for i := 0; i < len(received); {
		c := received[i]
		if c != 'A' && c != 'B' && c != 'C' {
			i++
			continue
		}
		j := i
		for j < len(received) && received[j] == c {
			j++
		}
		if runLen := j - i; runLen != markerLen {
			t.Fatalf("run of %q at offset %d has length %d, want %d: a concurrent writer split a payload", c, i, runLen, markerLen)
		}
		counts[c]++
		i = j
	}

	for _, c := range []byte{'A', 'B', 'C'} {
		if counts[c] != iterations {
			t.Errorf("received %d complete %q tokens, want %d", counts[c], c, iterations)
		}
	}
}

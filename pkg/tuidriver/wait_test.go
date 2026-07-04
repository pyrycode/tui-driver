package tuidriver

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitUntilImmediateTrue(t *testing.T) {
	calls := 0
	err := WaitUntil(context.Background(), func() bool {
		calls++
		return true
	})
	if err != nil {
		t.Errorf("WaitUntil(true) returned err=%v, want nil", err)
	}
	if calls != 1 {
		t.Errorf("predicate called %d times, want 1", calls)
	}
}

func TestWaitUntilEventuallyTrue(t *testing.T) {
	var n atomic.Int32
	err := WaitUntil(context.Background(), func() bool {
		return n.Add(1) >= 3
	})
	if err != nil {
		t.Errorf("WaitUntil returned err=%v, want nil", err)
	}
}

func TestWaitUntilContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := WaitUntil(ctx, func() bool { return false })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("WaitUntil(cancelled) = %v, want context.Canceled", err)
	}
}

func TestWaitUntilPropagatesCauseFromCancelCause(t *testing.T) {
	customErr := errors.New("custom cancellation reason")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(customErr)
	err := WaitUntil(ctx, func() bool { return false })
	if !errors.Is(err, customErr) {
		t.Errorf("WaitUntil cause = %v, want %v", err, customErr)
	}
}

func TestWaitUntilContextDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := WaitUntil(ctx, func() bool { return false })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitUntil(timeout) = %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Errorf("WaitUntil took %v, want close to 100ms", d)
	}
}

func TestWaitUntilNoAllocOnImmediateTrue(t *testing.T) {
	// Verify the short-circuit: if predicate is true on entry, no ticker
	// is created. We can't directly check allocations but we can check
	// that the call returns much faster than one tick.
	start := time.Now()
	_ = WaitUntil(context.Background(), func() bool { return true })
	if d := time.Since(start); d > 5*time.Millisecond {
		t.Errorf("immediate-true WaitUntil took %v, want <5ms (no ticker)", d)
	}
}

// TestWaitUntilOrExitPredicateTrueBeatsExit pins the entry short-circuit: an
// already-satisfied predicate returns nil even when the process has also
// exited (exited closed) — ready wins over a same-instant exit.
func TestWaitUntilOrExitPredicateTrueBeatsExit(t *testing.T) {
	exited := make(chan struct{})
	close(exited)
	s := &Session{exited: exited, exitErr: errors.New("exit status 1")}
	if err := s.waitUntilOrExit(context.Background(), func() bool { return true }); err != nil {
		t.Errorf("waitUntilOrExit(predicate true) = %v, want nil", err)
	}
}

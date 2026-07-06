package tuidriver

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRunWatchdogContextCancellation covers AC 6(a): cancellation of the
// supplied context returns the goroutine cleanly within one tick.
func TestRunWatchdogContextCancellation(t *testing.T) {
	buf := NewBuffer(0)
	buf.Append([]byte("x"))
	tr := NewTracker(TrackerOpts{}) // generous defaults — no wedge expected

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWatchdogLoop(ctx, buf, tr, WatchdogOpts{Tick: 20 * time.Millisecond})
	}()

	// Park briefly so RunWatchdog has set up its ticker and is parked in
	// the select; cancel exercises the ctx.Done() arm rather than a
	// pre-loop short-circuit.
	time.Sleep(40 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunWatchdog on ctx-cancel = %v, want nil", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("RunWatchdog did not return within 200ms of ctx-cancel")
	}
}

// TestRunWatchdogPTYQuietWedge covers AC 6(b) via the PTY-quiet arm:
// a stale buffer (no Append within PTYQuietLimit) makes CheckWatchdog
// return non-nil, and RunWatchdog surfaces that error verbatim and exits.
func TestRunWatchdogPTYQuietWedge(t *testing.T) {
	buf := NewBuffer(0)
	buf.Append([]byte("x"))
	tr := NewTracker(TrackerOpts{
		PTYQuietLimit: 50 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := runWatchdogLoop(ctx, buf, tr, WatchdogOpts{Tick: 20 * time.Millisecond})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("RunWatchdog with stale buffer = nil, want non-nil error")
	}
	if !strings.Contains(err.Error(), "PTY quiet") {
		t.Errorf("error = %q, want it to contain 'PTY quiet'", err.Error())
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("elapsed = %v, want < 200ms (loop exited promptly on wedge)", elapsed)
	}
}

// TestSessionRunWatchdogMethod proves the method form drives the loop against
// the session's own buffer — the same wedge fires through s.RunWatchdog as
// through the loop directly, with no raw buffer handed to the consumer.
func TestSessionRunWatchdogMethod(t *testing.T) {
	s := &Session{buffer: NewBuffer(0)}
	s.buffer.Append([]byte("x"))
	tr := NewTracker(TrackerOpts{
		PTYQuietLimit: 50 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := s.RunWatchdog(ctx, tr, WatchdogOpts{Tick: 20 * time.Millisecond})
	if err == nil {
		t.Fatal("Session.RunWatchdog with stale buffer = nil, want PTY-quiet wedge")
	}
	if !strings.Contains(err.Error(), "PTY quiet") {
		t.Errorf("error = %q, want it to contain 'PTY quiet'", err.Error())
	}
}

// TestRunWatchdogDefaultTickApplied verifies the zero-value
// WatchdogOpts.Tick falls through to DefaultWatchdogTick. Indirect
// proof: with the 1s default tick and a buffer that goes stale at
// 50ms, the wedge cannot fire before the first tick at ~1s.
func TestRunWatchdogDefaultTickApplied(t *testing.T) {
	buf := NewBuffer(0)
	buf.Append([]byte("x"))
	tr := NewTracker(TrackerOpts{
		PTYQuietLimit: 50 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := runWatchdogLoop(ctx, buf, tr, WatchdogOpts{}) // default tick
	elapsed := time.Since(start)

	// ctx times out before the first 1s tick — RunWatchdog returns nil.
	if err != nil {
		t.Errorf("RunWatchdog (default tick) = %v, want nil (ctx timeout, no tick yet)", err)
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 200ms (default tick suppressed early wedge)", elapsed)
	}
}

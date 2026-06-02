package tuidriver

import (
	"context"
	"time"
)

// DefaultWatchdogTick is the loop period RunWatchdog uses when
// WatchdogOpts.Tick is zero. Matches the 1 Hz cadence every spike binary
// runs at — fast enough that the wedge-detection latency stays under
// SpinnerFreezeLimit / PTYQuietLimit, slow enough that the per-tick
// snapshot + StripANSI + regex work is dwarfed by idle time.
const DefaultWatchdogTick = 1 * time.Second

// WatchdogOpts configures RunWatchdog.
type WatchdogOpts struct {
	// Tick is the loop period. Zero or negative → DefaultWatchdogTick.
	// Tests drive synthetic wedges with sub-100 ms ticks; production
	// consumers should leave this zero unless they have a measured
	// reason to deviate.
	Tick time.Duration
}

// RunWatchdog blocks on the calling goroutine, driving the spike-
// validated 1 Hz watchdog loop against this session's rolling buffer until
// ctx is cancelled or tr.CheckWatchdog reports a wedge. Returns nil on ctx
// cancellation (the caller asked to stop, matching Spawn/Session.Wait's
// cancellation convention) and the CheckWatchdog error verbatim on wedge
// (suitable for direct logging — the error string already names the failure
// mode and the last recorded state). The function returns at most once; the
// ticker is released via defer on every return path.
//
// Per-tick work (in order):
//
//  1. buf.Snapshot — copy the rolling buffer.
//  2. ParseSpinner(snap) — extract (verb, totalSeconds, ok) from class-A
//     spinner renderings; class-B/C/D return ok=false, equivalent to
//     "spinner not visible" for the freeze arm.
//  3. tr.ObserveSpinner(ok, totalSeconds) — record progress for the
//     spinner-freeze arm of CheckWatchdog.
//  4. tr.CheckWatchdog(buf) — evaluate both arms (PTY-quiet and
//     spinner-freeze). A non-nil return ends the loop.
//
// Customising this work list is intentionally not exposed; the
// calibration is the validated cross-spike default. If a future
// consumer needs a different shape, this comment documents the cost
// of divergence.
//
// Recommended consumer pattern — spawn in a goroutine alongside the
// linear state machine, log the non-nil return, and cancel the parent
// context with the error as cause:
//
//	go func() {
//	    if err := sess.RunWatchdog(ctx, tr, tuidriver.WatchdogOpts{}); err != nil {
//	        log.Printf("%v", err)
//	        cancelCause(err)
//	    }
//	}()
//
// Concurrency: RunWatchdog reads via Buffer.Snapshot / Buffer.QuietFor
// (thread-safe per Buffer's contract) and Tracker.ObserveSpinner /
// Tracker.CheckWatchdog (thread-safe per Tracker's contract). No
// internal goroutines are spawned. Nil buf or tr will panic on first
// dereference, matching the library's "construct or you get a
// nil-deref" posture.
func (s *Session) RunWatchdog(ctx context.Context, tr *Tracker, opts WatchdogOpts) error {
	return runWatchdogLoop(ctx, s.Buffer, tr, opts)
}

// RunWatchdog is the free-function form, retained transitionally for consumers
// that hold a *Buffer directly.
//
// Deprecated: use Session.RunWatchdog, which reaches the session's buffer
// internally so consumers never touch the raw buffer. This free function is
// removed in the breaking step.
func RunWatchdog(ctx context.Context, buf *Buffer, tr *Tracker, opts WatchdogOpts) error {
	return runWatchdogLoop(ctx, buf, tr, opts)
}

// runWatchdogLoop is the unexported loop body shared by Session.RunWatchdog and
// the transitional free function, and the seam watchdog_test.go drives with a
// raw *Buffer. The per-tick work list (Snapshot → ParseSpinner →
// ObserveSpinner → CheckWatchdog) is the cross-spike-validated calibration; see
// the RunWatchdog doc for why it is intentionally not configurable.
func runWatchdogLoop(ctx context.Context, buf *Buffer, tr *Tracker, opts WatchdogOpts) error {
	tick := opts.Tick
	if tick <= 0 {
		tick = DefaultWatchdogTick
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_, total, ok := ParseSpinner(buf.Snapshot())
			tr.ObserveSpinner(ok, total)
			if err := tr.CheckWatchdog(buf); err != nil {
				return err
			}
		}
	}
}

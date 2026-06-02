package tuidriver

import (
	"context"
	"time"
)

// DefaultPollInterval is the polling cadence used by WaitUntil. 50ms is
// fine-grained enough to feel instant on state transitions (typical TUI
// updates land in 1-3 ticks) while keeping CPU overhead negligible.
const DefaultPollInterval = 50 * time.Millisecond

// WaitUntil polls predicate at DefaultPollInterval until it returns true
// or ctx is cancelled. Returns ctx's cause (from context.Cause) on
// cancellation, or nil when predicate succeeded. Short-circuits if
// predicate is already true on entry — no ticker setup, no allocation.
//
// Use as the standard "wait for a TUI state transition" primitive:
//
//	err := tuidriver.WaitUntil(ctx, func() bool {
//	    return tuidriver.IsIdle(session.Snapshot())
//	})
//
// For custom poll intervals, the caller can write the loop directly —
// the implementation here is intentionally small. Most state-detection
// flows are well-served by 50ms regardless.
func WaitUntil(ctx context.Context, predicate func() bool) error {
	if predicate() {
		return nil
	}
	ticker := time.NewTicker(DefaultPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-ticker.C:
			if predicate() {
				return nil
			}
		}
	}
}

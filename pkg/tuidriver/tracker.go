package tuidriver

import (
	"fmt"
	"sync"
	"time"
)

// Default watchdog limits. The 30s values match the validated values
// from loop 2 B-1 (PTY-heartbeat architecture) and the spinner-freeze
// detector across all 6 spike binaries.
const (
	DefaultPTYQuietLimit      = 30 * time.Second
	DefaultSpinnerFreezeLimit = 30 * time.Second
)

// TrackerOpts configures NewTracker.
type TrackerOpts struct {
	// PTYQuietLimit is the duration after which CheckWatchdog reports a
	// wedge when no PTY bytes have arrived (read from Buffer.QuietFor).
	// 0 → DefaultPTYQuietLimit.
	//
	// Set this LARGER than the longest expected legitimate PTY-quiet
	// window for the consumer's workload. claude can be quiet during
	// modal-waiting states (permission, AskUserQuestion, etc.) — modal-
	// aware consumers should pause the tracker or extend this limit
	// before entering those states. Finding #25 documents the coupling.
	PTYQuietLimit time.Duration

	// SpinnerFreezeLimit is the duration after which CheckWatchdog
	// reports a wedge when the thinking spinner is visible but its
	// seconds-counter has not incremented (i.e. claude appears to be
	// thinking but is actually stuck). 0 → DefaultSpinnerFreezeLimit.
	SpinnerFreezeLimit time.Duration
}

// Tracker records state transitions and runs the two-arm watchdog
// (PTY-heartbeat + spinner-freeze) the spike binaries use to detect a
// hung claude.
//
// Two arms:
//
//  1. PTY-quiet — Buffer.QuietFor() > PTYQuietLimit. Detects a true
//     wedge (claude crashed, deadlocked, lost stdin).
//  2. Spinner-freeze — ObserveSpinner records when the ✻ seconds-counter
//     last incremented. If the spinner stays visible without progress
//     for SpinnerFreezeLimit, the watchdog fires. Detects claude stuck
//     in a tool call that doesn't produce PTY output but also doesn't
//     advance.
//
// State transitions are pure bookkeeping — CheckWatchdog does NOT look
// at LastTransitionAt. The state name is included in error messages
// for debugging only.
//
// Construct with NewTracker. Safe for concurrent use.
type Tracker struct {
	mu                    sync.Mutex
	currentState          string
	lastTransitionAt      time.Time
	lastSpinnerProgressAt time.Time
	lastSpinnerTotal      int
	spinnerActive         bool
	ptyQuietLimit         time.Duration
	spinnerFreezeLimit    time.Duration
}

// NewTracker returns a Tracker with the given limits. Zero/negative
// limits fall through to the package defaults.
func NewTracker(opts TrackerOpts) *Tracker {
	t := &Tracker{
		ptyQuietLimit:      opts.PTYQuietLimit,
		spinnerFreezeLimit: opts.SpinnerFreezeLimit,
	}
	if t.ptyQuietLimit <= 0 {
		t.ptyQuietLimit = DefaultPTYQuietLimit
	}
	if t.spinnerFreezeLimit <= 0 {
		t.spinnerFreezeLimit = DefaultSpinnerFreezeLimit
	}
	return t
}

// RecordTransition stores the named state + the current time. Used by
// consumers to mark milestones (e.g. "prompt-written", "modal-detected")
// so watchdog errors include a useful "last state" hint.
func (t *Tracker) RecordTransition(state string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.currentState = state
	t.lastTransitionAt = time.Now()
}

// CurrentState returns the most recently recorded state name, or "" if
// no transition has been recorded.
func (t *Tracker) CurrentState() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.currentState
}

// ObserveSpinner is called from the watchdog tick with whether the
// thinking spinner is currently visible and its current seconds-counter
// reading. Bookkeeping for the freeze detector — progress is "strictly
// greater than the previous reading while spinner is continuously
// visible." Consumers typically call this every ~1s.
func (t *Tracker) ObserveSpinner(visible bool, totalSeconds int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if !visible {
		t.spinnerActive = false
		t.lastSpinnerTotal = 0
		return
	}
	if !t.spinnerActive {
		t.spinnerActive = true
		t.lastSpinnerTotal = totalSeconds
		t.lastSpinnerProgressAt = now
		return
	}
	if totalSeconds > t.lastSpinnerTotal {
		t.lastSpinnerTotal = totalSeconds
		t.lastSpinnerProgressAt = now
	}
}

// CheckWatchdog returns nil if both watchdog arms are satisfied, or a
// descriptive error otherwise. Consumers call it periodically (typical
// cadence: 1Hz) and treat any non-nil return as a fatal wedge condition.
//
// The error format is human-readable for logging:
//
//	"watchdog: PTY quiet for 35s (last state: prompt-written)"
//	"watchdog: spinner counter frozen at 2s for 31s"
func (t *Tracker) CheckWatchdog(buf *Buffer) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if quiet := buf.QuietFor(); quiet > t.ptyQuietLimit {
		return fmt.Errorf("watchdog: PTY quiet for %s (last state: %s)",
			quiet.Round(time.Second), t.currentState)
	}
	if t.spinnerActive && now.Sub(t.lastSpinnerProgressAt) > t.spinnerFreezeLimit {
		return fmt.Errorf("watchdog: spinner counter frozen at %ds for %s",
			t.lastSpinnerTotal, now.Sub(t.lastSpinnerProgressAt).Round(time.Second))
	}
	return nil
}

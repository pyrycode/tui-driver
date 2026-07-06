package tuidriver

import (
	"fmt"
	"sync"
	"time"
)

// DefaultPTYQuietLimit is the PTYQuietLimit NewTracker applies when the
// opts value is zero. The 30s value matches the validated value from
// loop 2 B-1 (PTY-heartbeat architecture) across all spike binaries.
const DefaultPTYQuietLimit = 30 * time.Second

// DefaultSpinnerFreezeLimit is retained for source compatibility and has
// had no effect since #164, when the spinner-freeze watchdog arm was
// retired (ParseSpinner matches 0/667 pinned-Claude frames — #124). Kept
// exported so the external pyry consumer and the spike binaries that name
// it keep compiling. For freeze detection use the PTY-quiet arm and, for a
// JSONL-correlated stall signal, Events()'s EventKindStallDetected.
const DefaultSpinnerFreezeLimit = 30 * time.Second

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

	// SpinnerFreezeLimit is retained for source compatibility and is now
	// accepted-and-ignored. It configured the spinner-freeze watchdog arm,
	// retired in #164 because ParseSpinner matches 0/667 pinned-Claude
	// frames (#124) so the arm could never fire. The field is kept so the
	// spike binaries and the external pyry consumer that set it keep
	// compiling; setting it has no effect. For freeze detection use the
	// PTY-quiet arm and, for a JSONL-correlated stall signal, Events()'s
	// EventKindStallDetected.
	SpinnerFreezeLimit time.Duration
}

// Tracker records state transitions and runs the single-arm watchdog the
// spike binaries use to detect a hung claude.
//
// One arm:
//
//   - PTY-quiet — Buffer.QuietFor() > PTYQuietLimit. Detects a true wedge
//     (claude crashed, deadlocked, lost stdin). A genuinely frozen claude
//     stops repainting, so this arm covers the freeze case too.
//
// A former second arm (spinner-freeze) was retired in #164; see
// SpinnerFreezeLimit for details.
//
// State transitions are pure bookkeeping — CheckWatchdog does NOT look
// at LastTransitionAt. The state name is included in error messages
// for debugging only.
//
// Construct with NewTracker. Safe for concurrent use.
type Tracker struct {
	mu               sync.Mutex
	currentState     string
	lastTransitionAt time.Time
	ptyQuietLimit    time.Duration
}

// NewTracker returns a Tracker with the given limit. A zero/negative
// PTYQuietLimit falls through to DefaultPTYQuietLimit. opts.SpinnerFreezeLimit
// is accepted and ignored (see TrackerOpts.SpinnerFreezeLimit).
func NewTracker(opts TrackerOpts) *Tracker {
	t := &Tracker{
		ptyQuietLimit: opts.PTYQuietLimit,
	}
	if t.ptyQuietLimit <= 0 {
		t.ptyQuietLimit = DefaultPTYQuietLimit
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

// CheckWatchdog returns nil if the PTY-quiet arm is satisfied, or a
// descriptive error otherwise. Consumers call it periodically (typical
// cadence: 1Hz) and treat any non-nil return as a fatal wedge condition.
//
// The error format is human-readable for logging:
//
//	"watchdog: PTY quiet for 35s (last state: prompt-written)"
func (t *Tracker) CheckWatchdog(buf *Buffer) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if quiet := buf.QuietFor(); quiet > t.ptyQuietLimit {
		return fmt.Errorf("watchdog: PTY quiet for %s (last state: %s)",
			quiet.Round(time.Second), t.currentState)
	}
	return nil
}

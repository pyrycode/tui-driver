package tuidriver

import (
	"strings"
	"testing"
	"time"
)

func TestTrackerDefaultsApplied(t *testing.T) {
	tr := NewTracker(TrackerOpts{})
	if tr.ptyQuietLimit != DefaultPTYQuietLimit {
		t.Errorf("ptyQuietLimit = %v, want %v", tr.ptyQuietLimit, DefaultPTYQuietLimit)
	}
	if tr.spinnerFreezeLimit != DefaultSpinnerFreezeLimit {
		t.Errorf("spinnerFreezeLimit = %v, want %v", tr.spinnerFreezeLimit, DefaultSpinnerFreezeLimit)
	}
}

func TestTrackerExplicitLimitsRespected(t *testing.T) {
	tr := NewTracker(TrackerOpts{
		PTYQuietLimit:      120 * time.Second,
		SpinnerFreezeLimit: 45 * time.Second,
	})
	if tr.ptyQuietLimit != 120*time.Second {
		t.Errorf("custom PTYQuietLimit not applied: %v", tr.ptyQuietLimit)
	}
	if tr.spinnerFreezeLimit != 45*time.Second {
		t.Errorf("custom SpinnerFreezeLimit not applied: %v", tr.spinnerFreezeLimit)
	}
}

func TestTrackerRecordTransition(t *testing.T) {
	tr := NewTracker(TrackerOpts{})
	if got := tr.CurrentState(); got != "" {
		t.Errorf("CurrentState before any transition = %q, want \"\"", got)
	}
	tr.RecordTransition("idle-detected")
	if got := tr.CurrentState(); got != "idle-detected" {
		t.Errorf("CurrentState = %q, want idle-detected", got)
	}
	tr.RecordTransition("prompt-written")
	if got := tr.CurrentState(); got != "prompt-written" {
		t.Errorf("CurrentState after second transition = %q, want prompt-written", got)
	}
}

func TestTrackerCheckWatchdogQuietBuffer(t *testing.T) {
	// Use very short PTYQuietLimit to make the test fast.
	tr := NewTracker(TrackerOpts{PTYQuietLimit: 50 * time.Millisecond})
	tr.RecordTransition("test-state")
	buf := NewBuffer(0)
	buf.Append([]byte("initial"))

	// Immediately after Append, watchdog should be satisfied.
	if err := tr.CheckWatchdog(buf); err != nil {
		t.Errorf("CheckWatchdog right after Append = %v, want nil", err)
	}

	// Wait past the limit; watchdog should fire.
	time.Sleep(80 * time.Millisecond)
	err := tr.CheckWatchdog(buf)
	if err == nil {
		t.Fatal("CheckWatchdog after quiet > limit = nil, want error")
	}
	if !strings.Contains(err.Error(), "PTY quiet") {
		t.Errorf("error = %q, want it to contain 'PTY quiet'", err.Error())
	}
	if !strings.Contains(err.Error(), "test-state") {
		t.Errorf("error = %q, want it to contain last state", err.Error())
	}
}

func TestTrackerCheckWatchdogSpinnerFreeze(t *testing.T) {
	tr := NewTracker(TrackerOpts{
		PTYQuietLimit:      1 * time.Hour, // disable PTY-quiet arm
		SpinnerFreezeLimit: 50 * time.Millisecond,
	})
	buf := NewBuffer(0)
	buf.Append([]byte("x"))

	// Spinner starts at 2s.
	tr.ObserveSpinner(true, 2)
	if err := tr.CheckWatchdog(buf); err != nil {
		t.Errorf("CheckWatchdog right after spinner-start = %v, want nil", err)
	}

	// Same reading repeated past the freeze limit — fires.
	time.Sleep(80 * time.Millisecond)
	tr.ObserveSpinner(true, 2) // no progress
	err := tr.CheckWatchdog(buf)
	if err == nil {
		t.Fatal("CheckWatchdog with frozen spinner = nil, want error")
	}
	if !strings.Contains(err.Error(), "spinner counter frozen") {
		t.Errorf("error = %q, want it to contain 'spinner counter frozen'", err.Error())
	}
}

func TestTrackerSpinnerProgressResetsFreeze(t *testing.T) {
	tr := NewTracker(TrackerOpts{
		PTYQuietLimit:      1 * time.Hour,
		SpinnerFreezeLimit: 50 * time.Millisecond,
	})
	buf := NewBuffer(0)
	buf.Append([]byte("x"))

	tr.ObserveSpinner(true, 2)
	time.Sleep(30 * time.Millisecond)
	tr.ObserveSpinner(true, 3) // progress before freeze limit
	time.Sleep(30 * time.Millisecond)
	if err := tr.CheckWatchdog(buf); err != nil {
		t.Errorf("CheckWatchdog after progress = %v, want nil", err)
	}
}

func TestTrackerSpinnerDisappearedClearsState(t *testing.T) {
	tr := NewTracker(TrackerOpts{
		PTYQuietLimit:      1 * time.Hour,
		SpinnerFreezeLimit: 50 * time.Millisecond,
	})
	buf := NewBuffer(0)
	buf.Append([]byte("x"))

	tr.ObserveSpinner(true, 2)
	time.Sleep(30 * time.Millisecond)
	tr.ObserveSpinner(false, 0) // spinner gone
	time.Sleep(60 * time.Millisecond)
	if err := tr.CheckWatchdog(buf); err != nil {
		t.Errorf("CheckWatchdog after spinner disappeared = %v, want nil (no freeze should be active)", err)
	}
}

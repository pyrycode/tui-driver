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
}

func TestTrackerExplicitLimitsRespected(t *testing.T) {
	tr := NewTracker(TrackerOpts{
		PTYQuietLimit: 120 * time.Second,
	})
	if tr.ptyQuietLimit != 120*time.Second {
		t.Errorf("custom PTYQuietLimit not applied: %v", tr.ptyQuietLimit)
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

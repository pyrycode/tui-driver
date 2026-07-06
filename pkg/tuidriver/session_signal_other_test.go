//go:build !windows

package tuidriver

import (
	"os/exec"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestGroupSignalTarget pins the deterministic safety guard (AC #4): the
// function must never yield a group target that would signal the caller's own
// group (kill(0)) or every process the caller may signal (kill(-1)).
func TestGroupSignalTarget(t *testing.T) {
	tests := []struct {
		name       string
		pid        int
		wantTarget int
		wantOK     bool
	}{
		{"typical child pid", 1234, -1234, true},
		{"smallest safe pid", 2, -2, true},
		{"init pid rejected (would be kill(-1))", 1, 0, false},
		{"zero rejected (would be kill(0))", 0, 0, false},
		{"negative rejected", -5, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, ok := groupSignalTarget(tt.pid)
			if ok != tt.wantOK {
				t.Fatalf("groupSignalTarget(%d) ok = %v, want %v", tt.pid, ok, tt.wantOK)
			}
			if target != tt.wantTarget {
				t.Errorf("groupSignalTarget(%d) target = %d, want %d", tt.pid, target, tt.wantTarget)
			}
			// Cross-cutting invariant: a valid group target never addresses
			// "own group" (0) or "every owned process" (-1).
			if ok && target >= -1 {
				t.Errorf("groupSignalTarget(%d) yielded unsafe target %d (must be < -1)", tt.pid, target)
			}
		})
	}
}

// TestSpawnChildIsProcessGroupLeader pins the invariant the whole fix relies on
// (AC #1): immediately after Spawn the child's process-group ID equals its PID,
// i.e. it is a group leader. This holds without any SysProcAttr/Setpgid change
// on tui-driver's side — it comes entirely from creack/pty spawning with Setsid.
func TestSpawnChildIsProcessGroupLeader(t *testing.T) {
	cmd := exec.Command("cat")
	s, err := Spawn(cmd, SpawnOpts{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer s.Close()

	pid := s.cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("Getpgid(%d): %v", pid, err)
	}
	if pgid != pid {
		t.Errorf("child pgid = %d, want %d (== pid, i.e. group leader)", pgid, pid)
	}
}

// TestCloseReapsProcessGroupGrandchild proves the group signal reaps a
// grandchild that a leader-only kill would orphan (AC #3).
//
// The leader shell backgrounds a HUP-ignoring 300s sleeper — the grandchild —
// then execs cat to hold the PTY open so Close exercises the SIGTERM path. The
// grandchild ignores SIGHUP (the shell traps HUP to an empty handler, setting
// SIG_IGN, which survives the exec into sleep) for a load-bearing reason:
// Close's final s.pty.Close() delivers SIGHUP to the whole foreground process
// group, which on its own reaps an ordinary sleeper — so an ordinary sleeper
// would die even under the pre-fix leader-only signal, making the test pass
// with the bug present (a false green). By ignoring SIGHUP, the grandchild
// survives everything EXCEPT the explicit process-group SIGTERM/SIGKILL, so its
// death here evidences the group signal specifically. Verified: with
// signalShutdown reverted to leader-only this grandchild survives Close; with
// the group signal it is reaped. A 300s sleeper cannot exit on its own inside
// the window.
func TestCloseReapsProcessGroupGrandchild(t *testing.T) {
	cmd := exec.Command("sh", "-c", `sh -c 'trap "" HUP; exec sleep 300' & echo GC=$!; exec cat`)
	s, err := Spawn(cmd, SpawnOpts{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Belt-and-suspenders: Close reaps the group, but if the test fails early
	// the grandchild would leak — reap it explicitly by PID on cleanup.
	var gcPid int
	t.Cleanup(func() {
		s.Close()
		if gcPid > 0 {
			_ = syscall.Kill(gcPid, syscall.SIGKILL)
		}
	})

	// Poll the buffer for the grandchild PID the shell echoed.
	re := regexp.MustCompile(`GC=(\d+)`)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m := re.FindSubmatch(s.Snapshot()); m != nil {
			gcPid, _ = strconv.Atoi(string(m[1]))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if gcPid <= 0 {
		t.Fatalf("never saw GC=<pid> in snapshot; snap=%q", s.Snapshot())
	}

	// Pre-assert the grandchild is alive before Close (signal 0 probes existence).
	if err := syscall.Kill(gcPid, 0); err != nil {
		t.Fatalf("grandchild %d not alive before Close: %v", gcPid, err)
	}

	if err := s.Close(); err != nil {
		// SIGTERM-killed leader returns a non-nil exit error; that's expected,
		// not a failure of this test.
		t.Logf("Close returned %v (expected for a signalled leader)", err)
	}

	// The grandchild must now be reaped. Poll until kill(pid, 0) reports ESRCH.
	//
	// PID-reuse caveat (same one the spike's pgrep orphan-check carries): reuse
	// of this exact PID as a new process within the few-second window is
	// negligible on a test host. If this ever flakes, tighten by also matching
	// the process command; do not pre-build that.
	reaped := false
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(gcPid, 0); err == syscall.ESRCH {
			reaped = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !reaped {
		t.Errorf("grandchild %d still alive after Close — group signal did not reach it", gcPid)
	}
}

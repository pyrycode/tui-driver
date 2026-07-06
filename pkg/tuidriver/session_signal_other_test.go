//go:build !windows

package tuidriver

import (
	"errors"
	"os/exec"
	"regexp"
	"runtime"
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

// TestWaitBoundedWhenGrandchildHoldsPTY proves Wait returns within the bounded
// grace when the leader has exited but a tool grandchild still holds the PTY
// slave FD open, so the master read never returns EOF and the reader never
// drains (AC #1, AC #4).
//
// The leader sets trap '' HUP (SIG_IGN) *before* forking, so the backgrounded
// sleep 300 inherits the ignore disposition from birth — no fork-to-trap race —
// then exit 7 makes the leader exit non-zero. The sleeper (the grandchild)
// shares the leader's process group, inherits fd 0/1/2 = the PTY slave, and
// ignores the SIGHUP the kernel sends the foreground group when the
// controlling-terminal leader exits, so it survives still holding the slave
// open. With the bug present, Wait blocks forever on readerDone; the outer 5s
// guard turns that into a fast failure instead of hanging the suite.
//
// Linux-only. On Darwin/BSD the kernel revoke(2)s the controlling terminal when
// the session leader exits, forcibly invalidating every open descriptor to the
// slave PTY — even in a surviving, SIGHUP-ignoring, setsid-detached grandchild
// (verified empirically: fd1/fd2 show "(revoked)"). The master then reads EOF,
// readerDone closes, and Wait cannot hang — so this scenario is structurally
// impossible to reproduce on Darwin and the precondition below would false-red.
// The production fix is platform-agnostic and is covered on every platform by
// the deterministic Wait select unit test in session_test.go; this test is the
// realistic Linux repro, exercised by the ubuntu-latest CI gate.
func TestWaitBoundedWhenGrandchildHoldsPTY(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("controlling-terminal revoke on session-leader exit makes the held-slave hang unreproducible off Linux; see doc comment")
	}
	const grace = 500 * time.Millisecond
	cmd := exec.Command("sh", "-c", `trap '' HUP; sleep 300 & echo GC=$!; exit 7`)
	s, err := Spawn(cmd, SpawnOpts{ShutdownGrace: grace})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Register cleanup first so it runs *after* the assertion — calling Close
	// before asserting would close the PTY master, let the read return, and mask
	// the bug. Reap the grandchild explicitly by PID as a backstop.
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

	// Confirm the leader is reaped (exited closed) before we assert on Wait.
	select {
	case <-s.exited:
	case <-time.After(2 * time.Second):
		t.Fatalf("leader never exited")
	}

	// Self-validating precondition: the grandchild must still hold the slave
	// open, so the reader must NOT have drained. If it did, Wait would return
	// fast even with the bug present — a silent false green. Hard-fail here so
	// the repro proves itself.
	select {
	case <-s.readerDone:
		t.Fatalf("precondition broken: reader drained before Wait — grandchild did not hold the slave open")
	default:
	}

	// The AC assertion: Wait is bounded and returns the leader's real status.
	type waitResult struct {
		err     error
		elapsed time.Duration
	}
	resultCh := make(chan waitResult, 1)
	go func() {
		start := time.Now()
		werr := s.Wait()
		resultCh <- waitResult{err: werr, elapsed: time.Since(start)}
	}()

	select {
	case res := <-resultCh:
		// Bounded, not merely "eventually": well under the 5s outer guard and
		// comfortably above the 500ms grace.
		if res.elapsed >= 2*time.Second {
			t.Errorf("Wait took %v, want bounded by ShutdownGrace (%v) + slack", res.elapsed, grace)
		}
		// Real exit status, never nil-by-default or a sentinel.
		var exitErr *exec.ExitError
		if !errors.As(res.err, &exitErr) {
			t.Fatalf("Wait err = %v, want *exec.ExitError", res.err)
		}
		if exitErr.ExitCode() != 7 {
			t.Errorf("Wait exit code = %d, want 7", exitErr.ExitCode())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return — unbounded block on readerDone regressed")
	}

	// The timeout must not have torn the reader/grandchild down: the sleeper is
	// still alive, evidencing that the reader was blocked *because* the slave was
	// held and Wait left it parked.
	if err := syscall.Kill(gcPid, 0); err != nil {
		t.Errorf("grandchild %d not alive after Wait: %v — Wait must not reap the reader", gcPid, err)
	}
}

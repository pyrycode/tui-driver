//go:build !windows

package tuidriver

import "syscall"

// groupSignalTarget maps a leader PID to the kill(2) target that addresses the
// leader's whole process group, or reports ok == false when no *safe* group
// target exists. It performs no syscalls — it is the pure, deterministic safety
// guard that makes a catastrophic group signal structurally impossible.
//
// kill(2) treats a negative first argument as a process-group id: kill(-pid)
// signals every process in group pid. Two negative-argument values are
// catastrophic and must never be produced here:
//
//   - kill(-1) signals every process the caller may signal.
//   - kill(0)  signals every process in the *caller's own* group.
//
// So any pid <= 1 (which would negate to 0 or -1, or is already non-positive)
// yields (0, false); the caller falls back to signalling the single leader.
// A real spawned child always has a kernel PID > 1, so the guard never fires in
// practice — it exists to make the failure structurally unreachable.
//
// For pid > 1, -pid is always < -1, so the target addresses exactly process
// group pid and nothing else. Whenever ok is true, target < -1.
func groupSignalTarget(pid int) (target int, ok bool) {
	if pid <= 1 {
		return 0, false
	}
	return -pid, true
}

// signalShutdown delivers sig to the hosted process's whole process group, so a
// hard Close reaps the tool subprocesses claude forked (Bash, etc.) instead of
// orphaning them. The child is its own group leader (PGID == PID) because
// creack/pty spawns it with Setsid, and forked tool children inherit that
// group, so negating the leader PID reaches all of them in one kill(2).
//
// Precondition: s.cmd.Process != nil (guaranteed by Close's nil-check).
//
// Degrades safely: if the guard rejects the PID or the group signal errors
// (e.g. ESRCH because the group already exited during the SIGTERM grace, or
// EPERM), it falls back to signalling the single leader process — exactly the
// pre-fix behaviour. Errors are intentionally swallowed; shutdown is
// best-effort by contract.
//
// Using -pid rather than querying syscall.Getpgid keeps this a single syscall
// and degrades safely even if the group-leader invariant were ever broken: for
// pid > 1, -pid is always a specific group id < -1, so a stale/absent group
// yields ESRCH and the leader-only fallback fires — never a catastrophic
// kill(-1) or kill(0).
func (s *Session) signalShutdown(sig syscall.Signal) {
	if target, ok := groupSignalTarget(s.cmd.Process.Pid); ok {
		if err := syscall.Kill(target, sig); err == nil {
			return
		}
	}
	_ = s.cmd.Process.Signal(sig)
}

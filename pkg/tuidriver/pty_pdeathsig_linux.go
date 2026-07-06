//go:build linux

package tuidriver

import (
	"os/exec"
	"syscall"
)

// setParentDeathSignal arranges for the kernel to send SIGKILL to cmd the
// instant its parent (this process) dies — a hard backstop for the case where
// the host daemon crashes or is SIGKILLed without running Session.Close, which
// would otherwise leave the PTY child reparented to init and rendering headless.
//
// Called before pty.Start, the setting survives creack/pty's spawn:
// StartWithSize only *adds* Setsid/Setctty to a caller-set SysProcAttr, never
// overwriting it, so allocating the struct here when nil leaves that later
// augmentation intact.
//
// Platform coverage: Linux gets this hard parent-death SIGKILL, delivered by
// the kernel the moment the spawning process dies with no cleanup required;
// other platforms have no kernel parent-death signal and rely on Session.Close
// (#168) for orderly shutdown (see pty_pdeathsig_other.go).
//
// Caveat (golang/go#27505): Linux delivers Pdeathsig when the OS *thread* that
// spawned the child exits, not necessarily the whole process. In practice Go
// parks rather than destroys threads, so this is rare — and a spurious early
// kill fails in the safe direction (claude dies rather than survives headless),
// which is the whole point. No mitigation warranted.
func setParentDeathSignal(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}

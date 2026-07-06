//go:build !linux

package tuidriver

import "os/exec"

// setParentDeathSignal is a no-op on non-Linux platforms. Neither darwin nor
// windows exposes a kernel parent-death signal — Pdeathsig does not exist in
// their syscall.SysProcAttr, so touching it would break the cross-build — and
// these platforms rely on Session.Close (#168) for orderly shutdown. A host
// that crashes without running Close leaves the PTY child reparented to init.
func setParentDeathSignal(cmd *exec.Cmd) {}

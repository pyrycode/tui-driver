//go:build windows

package tuidriver

import "syscall"

// signalShutdown delivers sig to the hosted process. Windows has no POSIX
// process groups and no syscall.Kill, so this preserves exactly the pre-fix
// leader-only behaviour: SIGTERM is a no-op error Windows ignores; SIGKILL maps
// to os.Kill. Zero behaviour change on Windows.
//
// Precondition: s.cmd.Process != nil (guaranteed by Close's nil-check).
func (s *Session) signalShutdown(sig syscall.Signal) {
	_ = s.cmd.Process.Signal(sig)
}

//go:build darwin

package tuidriver

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"
)

// maxPathLen mirrors darwin's MAXPATHLEN (sys/syslimits.h). F_GETPATH
// writes the canonical path into a buffer of at least this size.
const maxPathLen = 1024

// canonicalisePath resolves p to its on-disk canonical form (symlinks
// resolved, case canonicalised on case-insensitive APFS) by opening the
// path and asking the kernel for the fd's canonical path via
// fcntl(F_GETPATH). filepath.EvalSymlinks is insufficient on darwin
// because it does not canonicalise case when there is no symlink to
// resolve. F_GETPATH is the kernel-blessed mechanism (os.Getwd on darwin
// uses the same call) and requires no global state.
//
// Returns ("", false) on any failure (non-existent path, permission
// denied, fcntl error); callers fall back to encoding the input as-passed.
func canonicalisePath(p string) (string, bool) {
	f, err := os.Open(p)
	if err != nil {
		return "", false
	}
	defer f.Close()
	var buf [maxPathLen]byte
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_GETPATH, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return "", false
	}
	n := bytes.IndexByte(buf[:], 0)
	if n < 0 {
		n = len(buf)
	}
	return string(buf[:n]), true
}

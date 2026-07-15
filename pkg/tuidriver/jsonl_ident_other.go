//go:build !windows

package tuidriver

import (
	"os"
	"syscall"
)

// statIdentity extracts the (device, inode) generation identity from fi
// via fi.Sys().(*syscall.Stat_t). It is the Unix half of the platform
// pair that lets tailJSONLLoop tell a rotated log (new inode) apart from
// a plain append.
//
// The uint64 casts are load-bearing: syscall.Stat_t.Dev is int32 on
// darwin and uint64 on linux, so the //go:build !windows tag covers both
// field types and the cast normalises them to one comparable width.
//
// If the Sys() assertion fails (no Stat_t behind this FileInfo), returns
// a zero fileIdentity (known == false), so rotation detection self-
// disables rather than misfiring on a bogus identity.
func statIdentity(fi os.FileInfo) fileIdentity {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}
	}
	return fileIdentity{dev: uint64(st.Dev), ino: uint64(st.Ino), known: true}
}

//go:build windows

package tuidriver

import "os"

// statIdentity is the Windows stub of the platform pair. Inode identity
// is not available via os.FileInfo on Windows, so it returns a zero
// fileIdentity (known == false): rotation detection — which compares two
// identities for a generation change — is thus a documented no-op on
// Windows (two unknown identities never compare equal). Truncation
// detection, which needs only file size, still works on all platforms.
func statIdentity(fi os.FileInfo) fileIdentity {
	return fileIdentity{}
}

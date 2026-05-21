//go:build !darwin

package tuidriver

import "path/filepath"

// canonicalisePath resolves p to its on-disk canonical form via
// filepath.EvalSymlinks. On case-sensitive filesystems (default Linux
// ext4/xfs/btrfs) a differently-cased lookup returns ENOENT and the
// caller's fallback fires — there is nothing to case-canonicalise.
//
// Returns ("", false) on any resolution failure; callers fall back to
// encoding the input as-passed.
func canonicalisePath(p string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", false
	}
	return resolved, true
}

package tuidriver

import "strings"

// EncodeCwd derives the projects-dir name that claude writes under
// ~/.claude/projects/ for a given working directory. The rule: every byte
// outside [a-zA-Z0-9] is mapped to exactly one '-'. Adjacent special bytes
// produce adjacent hyphens (no run-collapse).
//
// The input is first resolved to its on-disk canonical form (symlinks
// resolved; case canonicalised on case-insensitive filesystems) so the
// output matches the directory claude actually writes to. If the path
// does not exist (or cannot be resolved), the input is encoded as-passed
// — there is no error path, callers that pass logical paths for testing
// still get a usable result.
//
// Byte-transform empirically derived 2026-05-18 (loop 2 B-4) by inspecting
// what claude wrote for a cwd containing 9 distinct special characters.
// Generalizes loop 1's narrower "/, ., space → -" mapping.
func EncodeCwd(cwd string) string {
	if canonical, ok := canonicalisePath(cwd); ok {
		cwd = canonical
	}
	var b strings.Builder
	b.Grow(len(cwd))
	for i := 0; i < len(cwd); i++ {
		c := cwd[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

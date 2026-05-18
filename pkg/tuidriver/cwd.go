package tuidriver

import "strings"

// EncodeCwd derives the projects-dir name that claude writes under
// ~/.claude/projects/ for a given working directory. The rule: every byte
// outside [a-zA-Z0-9] is mapped to exactly one '-'. Adjacent special bytes
// produce adjacent hyphens (no run-collapse).
//
// Empirically derived 2026-05-18 (loop 2 B-4) by inspecting what claude
// wrote for a cwd containing 9 distinct special characters. Generalizes
// loop 1's narrower "/, ., space → -" mapping.
func EncodeCwd(cwd string) string {
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

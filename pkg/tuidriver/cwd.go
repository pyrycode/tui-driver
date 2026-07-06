package tuidriver

import (
	"strings"
	"unicode/utf16"
)

// EncodeCwd derives the projects-dir name that claude writes under
// ~/.claude/projects/ for a given working directory. The rule: each UTF-16
// code unit outside [a-zA-Z0-9] is mapped to exactly one '-', matching
// claude's JS `.replace(/[^a-zA-Z0-9]/g,'-')` encoding. A BMP rune (e.g. `ö`,
// U+00F6) is one UTF-16 code unit → one hyphen; an astral rune (e.g. `😀`,
// U+1F600, a surrogate pair) is two code units → two hyphens. Adjacent special
// characters produce adjacent hyphens (no run-collapse).
//
// The input is first resolved to its on-disk canonical form (symlinks
// resolved; case canonicalised on case-insensitive filesystems) so the
// output matches the directory claude actually writes to. If the path
// does not exist (or cannot be resolved), the input is encoded as-passed
// — there is no error path, callers that pass logical paths for testing
// still get a usable result.
//
// UTF-16 rule observed against real claude 2.1.199 on 2026-07-06: a cwd leaf
// `Työ😀` produced the projects-dir suffix `-Ty---` (see cmd/probe-cwd-encoding
// and docs/knowledge/codebase/206.md). This replaced the earlier per-byte
// derivation (loop 2 B-4), which exercised only ASCII specials and so was
// never verified against claude's non-ASCII output.
func EncodeCwd(cwd string) string {
	if canonical, ok := canonicalisePath(cwd); ok {
		cwd = canonical
	}
	// Output byte-length ≤ input byte-length: each non-alnum rune emits at
	// most as many hyphens as it has UTF-8 bytes (BMP ≤ 3 bytes → 1 hyphen;
	// astral 4 bytes → 2 hyphens), so len(cwd) stays a valid upper bound.
	var b strings.Builder
	b.Grow(len(cwd))
	for _, r := range cwd {
		if r < 128 && isASCIIAlnum(byte(r)) {
			b.WriteByte(byte(r))
			continue
		}
		for range utf16.Encode([]rune{r}) {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func isASCIIAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

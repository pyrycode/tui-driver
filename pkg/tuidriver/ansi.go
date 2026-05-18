package tuidriver

import "regexp"

// ansiCsiRe matches CSI escape sequences (ESC `[` … final-byte). Sufficient
// for the spike binaries' state-detection predicates (idle / spinner / trust
// / permission / picker headers). NOT sufficient for parsing column-aligned
// text — cursor-positioning sequences like `\x1b[<N>C` lose information
// when stripped to nothing. A proper VT100 grid emulator is the deferred
// fix; see the project's loop 6 F-1 finding for context.
var ansiCsiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// StripANSI removes CSI escape sequences from snap. Loses positioning
// information for cursor-movement sequences — use only for predicates that
// match on stripped text, not for structured parsing of column-aligned UIs.
func StripANSI(snap []byte) []byte {
	return ansiCsiRe.ReplaceAll(snap, nil)
}

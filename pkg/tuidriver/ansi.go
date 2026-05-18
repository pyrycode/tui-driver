package tuidriver

import "regexp"

// ansiCsiRe matches CSI escape sequences (ESC `[` … final-byte). Sufficient
// for state-detection predicates (idle / spinner / trust / permission /
// modal-class anchors). NOT sufficient for parsing column-aligned text —
// cursor-positioning sequences like `\x1b[<N>C` lose information when
// stripped to nothing; use Render (vt10x-backed) for accurate parsing.
var ansiCsiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// ansiOscRe matches OSC (Operating System Command) sequences:
// ESC `]` … BEL (`\x07`). Used by terminals for title bars and palette
// queries. claude emits these too; modal-class predicates need to ignore
// them to avoid false anchor matches embedded in OSC payloads.
var ansiOscRe = regexp.MustCompile(`\x1b\][^\x07]*\x07`)

// StripANSI removes CSI escape sequences from snap. Loses positioning
// information for cursor-movement sequences — use only for predicates that
// match on stripped text, not for structured parsing of column-aligned UIs.
func StripANSI(snap []byte) []byte {
	return ansiCsiRe.ReplaceAll(snap, nil)
}

// StripANSIString is the string variant of StripANSI. Useful when the
// caller is already operating on a string (e.g. after a chained text-level
// stripper of color codes and cursor-forward sequences).
func StripANSIString(s string) string {
	return ansiCsiRe.ReplaceAllString(s, "")
}

// StripOSC removes OSC (Operating System Command) sequences from snap.
// Pair with StripANSI when implementing modal-class predicates so OSC
// payloads (window title updates, etc.) don't accidentally match anchors.
func StripOSC(snap []byte) []byte {
	return ansiOscRe.ReplaceAll(snap, nil)
}

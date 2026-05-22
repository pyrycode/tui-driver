package main

import "regexp"

// tryLineRe matches the picker's `Try "<starter prompt>"` line, whose prompt
// text rotates per invocation in claude 2.1.148+ (baked into the binary, not
// from ~/.claude/history.jsonl). Anchor on the structural signature claude
// emits for it:
//
//	\x1b[7m T \x1b[27m \x1b[2m ry [cursor-positions + prompt words] \x1b[22m
//
// `T` is rendered in reverse-video as the tappable keystroke target, then
// `ry` is rendered in faint, then the rotating quoted prompt follows with
// per-word cursor-positioning escapes. The terminating \x1b[22m turns off
// the faint introduced by \x1b[2m — that's the closer.
//
// The opening signature appears exactly once in every captured fixture
// (verified empirically against picker/mcp/agents captures on 2026-05-22).
// (?s) lets `.` cross newline bytes; the {0,256} bound prevents runaway
// matching if the closer is somehow absent.
var tryLineRe = regexp.MustCompile(`(?s)\x1b\[7mT\x1b\[27m\x1b\[2mry.{0,256}?\x1b\[22m`)

// tryLineReplacement is the constant marker that replaces the Try line so
// that two normalised captures with different starter prompts compare equal.
var tryLineReplacement = []byte("<TRY_LINE_MASKED>")

// normalize masks volatile content out of a raw PTY-byte capture so that
// back-to-back captures of the same TUI state under identical env compare
// equal. The function is pure; the same input always produces the same
// output. Apply to both the live capture and the committed fixture before
// bytes.Equal — asymmetric normalisation defeats the byte-compare model.
//
// Currently masks: the `Try "<starter prompt>"` line that claude 2.1.148+
// emits with a per-invocation random prompt text.
func normalize(b []byte) []byte {
	return tryLineRe.ReplaceAll(b, tryLineReplacement)
}

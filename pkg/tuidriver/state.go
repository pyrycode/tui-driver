package tuidriver

import (
	"bytes"
	"regexp"
	"strconv"
)

// IdleGlyph is the UTF-8 encoding of ❯ — claude's input-line prompt
// marker. Present whenever the input line is visible, including DURING
// thinking (the TUI redraws the input line below the spinner), so checking
// for ❯ alone is not sufficient to determine idle state.
var IdleGlyph = []byte("\xe2\x9d\xaf")

// SpinnerGlyph is the UTF-8 encoding of ✻ — claude's thinking indicator.
// Present in every spinner-class rendering observed across loops 1-6:
//
//	`✻ Baked for 2s`                       (class A — verb + counter)
//	`✻ Channeling…`                        (class B — verb + ellipsis)
//	`✻ Actualizing… (2s · ↓1 tokens)`      (class C — verb + ellipsis + tokens, loop 2 B-3)
//	`✻ <full-sentence aphorism>`           (class D — still unobserved as of 2026-05-18)
//
// Library predicates use glyph-presence rather than regex-matching so they
// remain correct as claude adds new spinner-text variants. Consumers that
// need the verb/seconds extraction should accept that the regex-based
// extractor is incomplete (class C is documented as not matching).
var SpinnerGlyph = []byte("\xe2\x9c\xbb")

// IsIdle reports whether snap shows claude at the input prompt with no
// thinking spinner active: ❯ glyph present AND ✻ glyph absent. The
// conjunction is load-bearing — ❯ alone is not enough (it's also present
// during thinking, redrawn beneath the spinner).
//
// Use this as the "claude ready for input" predicate before writing
// keystrokes. Modal states (permission, trust-folder, ask-user-question,
// etc.) ALSO render ❯; check DetectModalClass to disambiguate when modal
// handling matters.
func IsIdle(snap []byte) bool {
	stripped := StripANSI(snap)
	if !bytes.Contains(stripped, IdleGlyph) {
		return false
	}
	return !bytes.Contains(stripped, SpinnerGlyph)
}

// IsThinking reports whether snap shows claude's thinking spinner. Used as
// the post-keystroke "claude received my prompt and started processing"
// signal in flows where waiting for end-turn alone is too late (e.g. a
// warm-up turn that needs the spinner-appeared signal before checking
// IsIdle again — otherwise the post-keystroke ❯ redraw fools IsIdle into
// reporting idle prematurely).
func IsThinking(snap []byte) bool {
	return bytes.Contains(StripANSI(snap), SpinnerGlyph)
}

// spinnerTokensRe matches the live output-token counter in claude's
// class-C spinner rendering: `✻ Verb… (Ns · ↓N tokens)`. Loop 2 B-3
// catalogued this as the form that surfaces token counts during streaming.
// Classes A (`✻ Baked for Ns`) and B (`✻ Channeling…`) do not include
// token counts.
//
// The arrow `↓` (U+2193) is the disambiguator — no other observed
// rendering uses it. We match it directly to avoid false positives on
// trailing-token text elsewhere in the buffer.
var spinnerTokensRe = regexp.MustCompile(`↓\s*(\d+)\s*tokens?`)

// ParseSpinnerTokens extracts the live output-token counter from claude's
// class-C spinner rendering. Returns (n, true) when the counter is
// present and (0, false) otherwise — including when the spinner is in
// class A/B form (no counter), no spinner is visible, or the buffer
// doesn't contain the marker.
//
// Use as a real-time progress signal during streaming: poll the rolling
// buffer at ~1 Hz, take the most-recent count as the current generated-
// token total for the in-progress turn. The counter resets at turn
// boundaries.
//
// The authoritative per-turn token count is the JSONL `usage` block
// claude writes after end_turn. ParseSpinnerTokens is the live preview;
// JSONL is the post-hoc truth.
//
// On a snapshot containing multiple matches (the rolling buffer captured
// several spinner repaints), returns the LAST match — the most-recent
// count.
func ParseSpinnerTokens(snap []byte) (n int, ok bool) {
	matches := spinnerTokensRe.FindAllSubmatch(StripANSI(snap), -1)
	if len(matches) == 0 {
		return 0, false
	}
	last := matches[len(matches)-1]
	v, err := strconv.Atoi(string(last[1]))
	if err != nil {
		return 0, false
	}
	return v, true
}

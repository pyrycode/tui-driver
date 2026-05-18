package tuidriver

import "bytes"

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

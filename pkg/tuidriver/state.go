package tuidriver

import (
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

// InterruptHint is claude's on-screen "esc to interrupt" hint — the second,
// independent busy anchor folded into busyInRegion alongside SpinnerGlyph.
// It is the more reliable in-flight anchor now that the ✻ spinner's text
// format has drifted dead (CLAUDE.md § Spinner caveat), so consumers writing
// their own PTY-quiescence checks should prefer it.
//
// A const string, not a []byte var like the glyphs: it has no external
// []byte consumer, and ContainsInLastRows takes a string. Match it over the
// Grid's space-preserved rendered form — the multi-word phrase is exactly
// what a StripANSI whole-buffer scan would corrupt (claude renders the
// inter-word gaps as CSI cursor-forwards that strip to nothing; the grid
// repaints them to real spaces). Match the full contiguous phrase, never a
// shorter substring: benign in-region content ("…without interrupting the
// main conversation" in the slash-picker) contains "interrupt" and would
// forge busy on an idle picker.
const InterruptHint = "esc to interrupt"

// statusRegionRows bounds the idle/busy status region to the bottom N
// rendered rows of the grid. Sized to include the input line (❯, ~row -3
// from bottom) and the spinner line redrawn just above it (✻, ~row -5),
// while excluding transcript body above the overlay (~row -6 and up when
// idle). Small by construction — widening toward the whole grid would
// reintroduce the mid-transcript forgeries this slice removes. Calibrated
// from the committed captures (real idle ❯ at -3 in mcp-empty-snapshot.bin)
// and pinned in both directions by the regressions in state_test.go: too
// small fails the realistic-thinking pin, too wide fails the forgery cases.
const statusRegionRows = 6

// busyInRegion reports whether a busy anchor is present in the status
// region — THE single busy predicate for the idle/busy axis. Region-scoping
// lives in exactly one place so IsIdle and IsThinking stay coherent. Two
// independent anchors are OR'd here: the spinner glyph ✻ (SpinnerGlyph) and
// claude's "esc to interrupt" hint (InterruptHint). They fail independently —
// claude would have to change both the glyph and the hint wording in one
// release to defeat the check. Both key on the grid's space-preserved
// rendered form; this must never reintroduce a StripANSI whole-buffer
// substring path — that would both re-corrupt the multi-word hint's
// inter-word spaces and re-open the mid-transcript forgery #153 closed.
func busyInRegion(g *Grid) bool {
	return g.ContainsInLastRows(string(SpinnerGlyph), statusRegionRows) ||
		g.ContainsInLastRows(InterruptHint, statusRegionRows)
}

// IsIdle reports whether snap shows claude at the input prompt with no
// thinking spinner active, decided from the bottom status region of the
// rendered grid (not a substring anywhere in the raw history buffer): ❯
// glyph present in the region AND no busy anchor present in the region. The
// conjunction is load-bearing — ❯ alone is not enough (it's also present
// during thinking, redrawn beneath the spinner). Region-scoping means a
// stale ❯ scrolled off into history no longer forges idle.
//
// Use this as the "claude ready for input" predicate before writing
// keystrokes. Modal states (permission, trust-folder, ask-user-question,
// etc.) ALSO render ❯; check DetectModalClass to disambiguate when modal
// handling matters.
func IsIdle(snap []byte) bool {
	g := NewGrid(snap, 0, 0)
	if !g.ContainsInLastRows(string(IdleGlyph), statusRegionRows) {
		return false
	}
	return !busyInRegion(g)
}

// IsThinking reports whether claude's thinking spinner is present in the
// bottom status region of the rendered grid (not a substring anywhere in
// the raw history buffer). Used as the post-keystroke "claude received my
// prompt and started processing" signal in flows where waiting for end-turn
// alone is too late (e.g. a warm-up turn that needs the spinner-appeared
// signal before checking IsIdle again — otherwise the post-keystroke ❯
// redraw fools IsIdle into reporting idle prematurely). Region-scoping means
// a stale ✻ printed as transcript content no longer forges thinking.
func IsThinking(snap []byte) bool {
	return busyInRegion(NewGrid(snap, 0, 0))
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

// spinnerForRe matches claude's class-A spinner rendering (verb + time
// counter): `✻ Verb for Ns` or `✻ Verb for Nm Ns`. The verb (group 1) is
// 1–2 words and varies per prompt — its empirical value is logged but
// no consumer depends on a specific verb. Groups 2 and 3 are the
// optional minutes-tail and the seconds-tail respectively.
//
// Loop 2 B-3 catalogued the four spinner-class renderings claude emits:
//
//	`✻ Baked for 2s`                       (class A — verb + counter)
//	`✻ Channeling…`                        (class B — verb + ellipsis)
//	`✻ Actualizing… (2s · ↓1 tokens)`      (class C — verb + ellipsis + tokens)
//	`✻ <full-sentence aphorism>`           (class D — unobserved as of 2026-05-23)
//
// This regex requires the literal `for` keyword so it matches class A
// only — neither the parens-and-bullet form of class C nor the bare
// ellipsis of classes B/D produce a match. The class gap is the
// documented sibling of ParseSpinnerTokens's class-A/B gap.
var spinnerForRe = regexp.MustCompile(`✻\s+(\S+(?:\s+\S+)?)\s+for\s+(?:(\d+)m\s+)?(\d+)s`)

// ParseSpinner extracts the verb and total-seconds counter from claude's
// class-A spinner rendering. Returns (verb, minutes*60+seconds, true) on
// match and ("", 0, false) otherwise — including when the spinner is in
// class B (no `for Ns` tail), class C (parentheses, not `for`), class D
// (unobserved as of 2026-05-23), no spinner is visible, or the buffer
// doesn't contain the marker. Strips ANSI internally.
//
// Consumer paths:
//   - The verb is empirical telemetry, logged by every spike binary
//     when the thinking-detected transition fires; no consumer makes a
//     correctness decision on the verb's specific value.
//   - The total-seconds counter feeds Tracker.ObserveSpinner — the
//     spinner-freeze arm of Tracker.CheckWatchdog needs strictly-
//     increasing readings while the spinner stays visible. A class-B or
//     class-C snapshot returns ok=false, which ObserveSpinner treats
//     identically to "spinner not visible" — the freeze arm goes dormant
//     until a class-A rendering appears.
//
// Sibling extractor: ParseSpinnerTokens (over the same snapshot, extracts
// the live token counter from class-C renderings). Both are class-
// incomplete by construction.
func ParseSpinner(snap []byte) (verb string, totalSeconds int, ok bool) {
	m := spinnerForRe.FindSubmatch(StripANSI(snap))
	if m == nil {
		return "", 0, false
	}
	var minutes int
	if len(m[2]) > 0 {
		minutes, _ = strconv.Atoi(string(m[2]))
	}
	seconds, _ := strconv.Atoi(string(m[3]))
	return string(m[1]), minutes*60 + seconds, true
}

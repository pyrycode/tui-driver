package tuidriver

import (
	"regexp"
	"strconv"
)

// compactingPhraseRe matches the phrase in claude's auto-compaction banner. When
// the conversation fills claude's context, claude summarizes it to reclaim room
// and renders, in the bottom status region (2.1.199 live capture via /compact):
//
//	✢ Compacting conversation…
//	  ▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱ 0%
//
// The leading glyph animates across the spinner frames ✻ ✳ ✢ ✶ ✽, an elapsed
// timer like "(16s)" can appear inline in the bar, the bar fills as it
// progresses, and at narrower widths the phrase and bar collapse onto one row.
// A compacting claude goes silent on the content channel (nothing on the session
// JSONL), so a remote head that only watches content just looks frozen; this
// detector lets pyry surface a distinct "Compacting conversation" status.
// Secondary win: the runner stops mis-reading the silent compaction stretch as a
// wedge.
//
// The phrase alone is the anchor — the glyph, timer, bar fill, and percentage
// all animate. ⚠️ Add any new anchor to the #221 negative regression suite
// (anchor_forgery_test.go) so a transcript quotation of it stays non-firing.
var compactingPhraseRe = regexp.MustCompile(`Compacting conversation`)

// compactingBarRe matches the progress-bar run that co-signs the phrase: two or
// more of claude's parallelogram bar cells, empty ▱ (U+25B1) filling to ▰
// (U+25B0). This bar is the load-bearing forgery guard, and the reason this
// detector needs a co-signal at all where the sibling banners get by on region
// scoping alone. Unlike every other banner anchor, the phrase "Compacting
// conversation" demonstrably appears in ordinary agent transcripts — this very
// ticket's body made agent-run recordings match it — so region scoping is not
// enough on its own. A bare `\d+%` is not a safe co-signal either, because the
// bottom status region carries a percentage of its own ("You've used 82% of your
// weekly limit"). Prose cannot carry a ▱▰ bar run, so requiring it alongside the
// phrase closes the forgery a quotation of the phrase would otherwise open.
var compactingBarRe = regexp.MustCompile(`[\x{25B0}\x{25B1}]{2,}`)

// compactingPctRe extracts the progress percentage from the bar row. Capture
// group 1 is the 0–100 value.
var compactingPctRe = regexp.MustCompile(`(\d{1,3})%`)

// HasCompacting reports whether snap shows claude's auto-compaction banner in the
// bottom status region of the rendered screen: the phrase "Compacting
// conversation" AND a progress-bar run, both within bannerRegionRows.
//
// ADVISORY signal, not a fatal condition — like HasNetworkFailure / HasApiRetry /
// HasMidResponseError. It means claude is busy summarizing its context and will
// be silent on the content channel meanwhile; a consumer surfaces "Compacting
// conversation" to a host UI rather than treating the quiet as a hang.
//
// Returns false on nil/empty snap. Independent of the modal/idle/thinking axes —
// the banner sits in the lower status area and coexists with the dominant axis,
// like the other status banners.
func HasCompacting(snap []byte) bool {
	present, _, _ := compactingInRegion(NewGrid(snap, 0, 0))
	return present
}

// ParseCompacting returns the compaction progress percentage from claude's
// auto-compaction banner. Returns (percent, true) when the banner is present AND
// a bar row carries an `N%` that reads as 0–100; returns (0, false) when no
// banner is present, or it is present but the percentage is absent or out of
// range. Never panics. Follows the ParseApiRetry / ParseSpinner (value, ok)
// idiom. The percentage animates, so a consumer polls it — it is deliberately
// not streamed as event payload (see EventKindPtyCompactingShown).
func ParseCompacting(snap []byte) (int, bool) {
	present, percent, parsed := compactingInRegion(NewGrid(snap, 0, 0))
	if !present {
		return 0, false
	}
	return percent, parsed
}

// compactingInRegion scans the bottom status region of the rendered grid for
// claude's auto-compaction banner. Returns (present, percent, parsed): present is
// whether BOTH the phrase and a bar run sit in the region — they may share a row
// or sit on adjacent rows, since the banner wraps at narrow widths, so the check
// is region-wide rather than single-row. percent/parsed are the parsed progress,
// zero/false when no bar row carries a valid 0–100 percentage.
//
// The per-tick classifier renders the snapshot once and threads that grid here
// (#225); HasCompacting / ParseCompacting stay the thin single-snapshot wrappers,
// exactly as apiRetryInRegion relates to its exported wrappers. The percentage is
// parsed from the bar row only, never the whole region, so it can never scrape
// the weekly-limit line's own "82%".
func compactingInRegion(g *Grid) (present bool, percent int, parsed bool) {
	var phrase, bar bool
	for _, row := range g.LastRows(bannerRegionRows) {
		if compactingPhraseRe.MatchString(row) {
			phrase = true
		}
		if compactingBarRe.MatchString(row) {
			bar = true
			if m := compactingPctRe.FindStringSubmatch(row); m != nil {
				if v, err := strconv.Atoi(m[1]); err == nil && v <= 100 {
					percent, parsed = v, true
				}
			}
		}
	}
	if phrase && bar {
		return true, percent, parsed
	}
	return false, 0, false
}

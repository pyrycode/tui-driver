package tuidriver

import (
	"strings"
	"testing"
)

func TestHasCompactingEmpty(t *testing.T) {
	if HasCompacting(nil) {
		t.Errorf("HasCompacting(nil) = true, want false")
	}
	if HasCompacting([]byte("idle TUI bytes")) {
		t.Errorf("HasCompacting(idle) = true, want false")
	}
	if pct, ok := ParseCompacting(nil); ok || pct != 0 {
		t.Errorf("ParseCompacting(nil) = (%d, %v), want (0, false)", pct, ok)
	}
}

// TestHasCompactingRegion is the content-forgery regression, mirroring
// TestHasApiRetryRegion: the compaction banner quoted in the on-screen transcript
// body, pushed above the bottom status region, must NOT fire; the same banner in
// the live status region must, and its percentage must parse. The banner text is
// a verbatim 2.1.199 capture (driven via /compact). \r\n so vt10x renders flat
// rows.
func TestHasCompactingRegion(t *testing.T) {
	body := strings.Repeat("transcript body line\r\n", 25)

	// Forged above the region: the FULL banner (phrase AND bar) at the top,
	// pushed far above the bottom status rows by the transcript body below it.
	// Non-vacuous — the bar co-signal IS present, so only region scoping can
	// reject it, not a missing co-signal.
	forged := []byte("✢ Compacting conversation…\r\n  ▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱ 0%\r\n" + body)
	if !strings.Contains(string(forged), "Compacting conversation") {
		t.Fatal("fixture lost the forged phrase — the forgery contrast is void")
	}
	if HasCompacting(forged) {
		t.Errorf("forged-above-region: HasCompacting = true, want false")
	}

	// Live status banner, two-row form: at 2.1.199's width the phrase and the
	// progress bar wrap onto adjacent rows, both inside the bottom region just
	// above the input box.
	live := []byte(body + "✶ Compacting conversation…\r\n  ▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱ 40%\r\n────\r\n❯ \r\n")
	if !HasCompacting(live) {
		t.Errorf("live-status two-row: HasCompacting = false, want true")
	}
	pct, ok := ParseCompacting(live)
	if !ok {
		t.Fatalf("live-status: ParseCompacting ok = false, want true")
	}
	if pct != 40 {
		t.Errorf("live-status: ParseCompacting = %d, want 40", pct)
	}
}

// TestHasCompactingOneRowForm covers the width where claude collapses the banner
// onto a single row (phrase, bar, and percentage together) — the form the live
// capture showed at the default render width.
func TestHasCompactingOneRowForm(t *testing.T) {
	body := strings.Repeat("transcript body line\r\n", 25)
	live := []byte(body + "✳ Compacting conversation…▱▱▱▱▱▱▱▱▱▱▱▱ 0%\r\n────\r\n❯ \r\n")
	if !HasCompacting(live) {
		t.Errorf("one-row form: HasCompacting = false, want true")
	}
	if pct, ok := ParseCompacting(live); !ok || pct != 0 {
		t.Errorf("one-row form: ParseCompacting = (%d, %v), want (0, true)", pct, ok)
	}
}

// TestHasCompactingRequiresBar is the crux of the co-signal design: the phrase
// alone in the status region does NOT fire, because region scoping is not enough
// for a phrase this ticket proved appears in real agent transcripts. Crucially, a
// bare percentage in the region (claude's own "You've used 82% of your weekly
// limit") is NOT a bar and must not stand in for one — otherwise a quoted phrase
// plus the ever-present weekly-limit line would false-fire.
func TestHasCompactingRequiresBar(t *testing.T) {
	body := strings.Repeat("transcript body line\r\n", 25)

	// Phrase in the region, no bar anywhere → no fire.
	phraseOnly := []byte(body + "assistant: I am Compacting conversation notes for you.\r\n────\r\n❯ \r\n")
	if HasCompacting(phraseOnly) {
		t.Errorf("phrase without bar: HasCompacting = true, want false")
	}

	// Phrase in the region AND the weekly-limit percentage in the region, but no
	// bar → still no fire (the bare % is not the co-signal).
	phrasePlusPct := []byte(body + "assistant: while Compacting conversation state…\r\nYou've used 82% of your weekly limit · resets Jul 23\r\n❯ \r\n")
	if HasCompacting(phrasePlusPct) {
		t.Errorf("phrase + bare weekly-limit percentage, no bar: HasCompacting = true, want false")
	}
	if pct, ok := ParseCompacting(phrasePlusPct); ok || pct != 0 {
		t.Errorf("no-bar: ParseCompacting = (%d, %v), want (0, false)", pct, ok)
	}
}

// TestHasCompactingFilledBar confirms the detector recognises the bar in its
// filled state (▰, U+25B0) as well as empty (▱, U+25B1) — the bar cells flip from
// empty to filled as compaction progresses.
func TestHasCompactingFilledBar(t *testing.T) {
	body := strings.Repeat("transcript body line\r\n", 25)
	live := []byte(body + "✻ Compacting conversation…▰▰▰▰▰▰▰▱▱▱ 70%\r\n────\r\n❯ \r\n")
	if !HasCompacting(live) {
		t.Errorf("filled-bar: HasCompacting = false, want true")
	}
	if pct, ok := ParseCompacting(live); !ok || pct != 70 {
		t.Errorf("filled-bar: ParseCompacting = (%d, %v), want (70, true)", pct, ok)
	}
}

// TestParseCompactingOutOfRange: a bar row whose number exceeds 100 is not a real
// progress percentage. The banner still DETECTS (phrase + bar present), but the
// percentage reports unparsed rather than a bogus value — the (value, ok) idiom.
func TestParseCompactingOutOfRange(t *testing.T) {
	body := strings.Repeat("transcript body line\r\n", 25)
	live := []byte(body + "✻ Compacting conversation…▱▱▱▱▱▱ 250%\r\n────\r\n❯ \r\n")
	if !HasCompacting(live) {
		t.Fatalf("out-of-range: HasCompacting = false, want true (phrase + bar present)")
	}
	if pct, ok := ParseCompacting(live); ok || pct != 0 {
		t.Errorf("out-of-range: ParseCompacting = (%d, %v), want (0, false)", pct, ok)
	}
}

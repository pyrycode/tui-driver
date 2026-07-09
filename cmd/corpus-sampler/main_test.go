package main

import (
	"path/filepath"
	"reflect"
	"testing"
)

// fixture is a crafted cast (cmd/corpus-sampler/testdata/stable-sample-ok.cast):
//
//	event  ts    frame                       inter-event gap -> action
//	0      0.0   idle "…ready 12"            (no predecessor)
//	1      0.6   spinner "Working"           0.6 >= 0.5 -> sample event 0 (idle)
//	2      0.7   spinner "Thinking"          0.1 <  0.5 -> NO sample (event 1 skipped)
//	3      1.3   idle "…ready 999"           0.6 >= 0.5 -> sample event 2 (spinner)
//	4      1.9   idle "…ready 999"           0.6 >= 0.5 -> sample event 3 (idle)
//
// The two idle frames differ only by the trailing digit run (12 vs 999), so they
// render to different raw grids but fold to one hash after normalize collapses
// digit runs: distinct = {idle, spinner:Thinking}, idle Seen == 2. The
// sub-threshold gap proves the tool gates on the time gap, not an event stride —
// the "Working" spinner is never sampled.
func fixture() string { return filepath.Join("testdata", "stable-sample-ok.cast") }

// noOutputFixture is a header-only cast (zero output events) — used to pin that
// -final emits no final sample when there is nothing rendered (AC 1).
func noOutputFixture() string { return filepath.Join("testdata", "no-output-ok.cast") }

func TestCollect_StableScreensDedupeAndCounts(t *testing.T) {
	distinct, casts, events, gaps, err := collect([]string{fixture()}, 0.5, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if casts != 1 {
		t.Errorf("casts = %d, want 1", casts)
	}
	if events != 5 {
		t.Errorf("events = %d, want 5", events)
	}
	if gaps != 3 {
		t.Errorf("gaps = %d, want 3 (three >=0.5s inter-event gaps)", gaps)
	}
	if len(distinct) != 2 {
		t.Fatalf("distinct = %d, want 2 (idle folds across two gaps; spinner is one)", len(distinct))
	}

	// The sub-threshold gap must not have produced a sample: event 1 ("Working")
	// is the only frame behind a <0.5s gap, so no distinct sample may carry it.
	for _, s := range distinct {
		if s.Event == 1 {
			t.Errorf("sub-threshold gap sampled event 1 (hash %s in %s); gap gating failed", s.Hash, s.Cast)
		}
	}

	// -final off: every sample is gap-only. This is the AC 5c regression pin —
	// turning the flag off changes nothing about the sample shape or provenance.
	for _, s := range distinct {
		if !reflect.DeepEqual(s.Source, []string{"gap"}) {
			t.Errorf("source = %v, want [gap] with -final off (hash %s in %s)", s.Source, s.Hash, s.Cast)
		}
	}
}

func TestCollect_RepeatedScreenCollapsesWithSeenCount(t *testing.T) {
	distinct, _, _, _, err := collect([]string{fixture()}, 0.5, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	// distinct[0] is the idle screen — the first sample emitted (event 0), and the
	// screen that recurs at event 3, so it collapses to one entry with Seen == 2.
	idle := distinct[0]
	if idle.Seen != 2 {
		t.Errorf("idle screen Seen = %d, want 2 (hash %s in %s)", idle.Seen, idle.Hash, idle.Cast)
	}
	// The spinner screen occurs once.
	spinner := distinct[1]
	if spinner.Seen != 1 {
		t.Errorf("spinner screen Seen = %d, want 1 (hash %s in %s)", spinner.Seen, spinner.Hash, spinner.Cast)
	}
}

func TestCollect_ProvenanceMatchesStableFrameRule(t *testing.T) {
	distinct, _, _, _, err := collect([]string{fixture()}, 0.5, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	// First distinct entry is the idle frame painted by event 0 (Event = i-1 for
	// the first >=0.5s gap at i=1), stable through [0.0, 0.6].
	idle := distinct[0]
	if idle.Event != 0 {
		t.Errorf("idle Event = %d, want 0 (hash %s)", idle.Event, idle.Hash)
	}
	if idle.TS != 0.0 {
		t.Errorf("idle TS = %v, want 0.0 (hash %s)", idle.TS, idle.Hash)
	}
	if idle.Cols != 120 || idle.Rows != 40 {
		t.Errorf("idle dims = %dx%d, want 120x40 (hash %s)", idle.Cols, idle.Rows, idle.Hash)
	}
	if idle.Cast != "stable-sample-ok.cast" {
		t.Errorf("idle Cast = %q, want stable-sample-ok.cast", idle.Cast)
	}
	if idle.Tag != "ok" {
		t.Errorf("idle Tag = %q, want ok (hash %s in %s)", idle.Tag, idle.Hash, idle.Cast)
	}
	if idle.Segment != "prod" {
		t.Errorf("idle Segment = %q, want prod (hash %s in %s)", idle.Segment, idle.Hash, idle.Cast)
	}

	// Second distinct entry is the spinner frame painted by event 2, stable
	// through [0.7, 1.3].
	spinner := distinct[1]
	if spinner.Event != 2 {
		t.Errorf("spinner Event = %d, want 2 (hash %s)", spinner.Event, spinner.Hash)
	}
	if spinner.TS != 0.7 {
		t.Errorf("spinner TS = %v, want 0.7 (hash %s)", spinner.TS, spinner.Hash)
	}
}

func TestCollect_Deterministic(t *testing.T) {
	a, _, _, _, err := collect([]string{fixture()}, 0.5, false)
	if err != nil {
		t.Fatalf("collect (run a): %v", err)
	}
	b, _, _, _, err := collect([]string{fixture()}, 0.5, false)
	if err != nil {
		t.Fatalf("collect (run b): %v", err)
	}
	if len(a) != len(b) {
		t.Fatalf("distinct lengths differ across runs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Hash != b[i].Hash {
			t.Errorf("distinct[%d] hash differs across runs: %s vs %s (cast %s)", i, a[i].Hash, b[i].Hash, a[i].Cast)
		}
		if a[i].Seen != b[i].Seen {
			t.Errorf("distinct[%d] Seen differs across runs: %d vs %d (hash %s)", i, a[i].Seen, b[i].Seen, a[i].Hash)
		}
	}
}

func TestCollect_FinalEmitsLastFrameAndUnionsSources(t *testing.T) {
	distinct, _, _, gaps, err := collect([]string{fixture()}, 0.5, true)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	// The final frame (idle "…ready 999", rendered after the walk) normalizes to
	// the same hash as the gap-sampled idle screen, so it folds in rather than
	// adding a distinct entry (AC 5a/5b).
	if len(distinct) != 2 {
		t.Fatalf("distinct = %d, want 2 (final folds into the idle screen)", len(distinct))
	}
	// gaps counts quiet-gap fires only; the final-frame sample must not inflate it.
	if gaps != 3 {
		t.Errorf("gaps = %d, want 3 (final sample excluded from the gap count)", gaps)
	}

	idle := distinct[0]
	if !reflect.DeepEqual(idle.Source, []string{"final", "gap"}) {
		t.Errorf("idle source = %v, want [final gap] (hash %s in %s)", idle.Source, idle.Hash, idle.Cast)
	}
	if idle.Seen != 3 {
		t.Errorf("idle Seen = %d, want 3 (two gap fires + one final; hash %s in %s)", idle.Seen, idle.Hash, idle.Cast)
	}

	// The spinner screen is mid-run, not the ending, so final must not tag it —
	// this pins that final marked the ending frame and nothing else.
	spinner := distinct[1]
	if !reflect.DeepEqual(spinner.Source, []string{"gap"}) {
		t.Errorf("spinner source = %v, want [gap] (hash %s in %s)", spinner.Source, spinner.Hash, spinner.Cast)
	}
	if spinner.Seen != 1 {
		t.Errorf("spinner Seen = %d, want 1 (hash %s in %s)", spinner.Seen, spinner.Hash, spinner.Cast)
	}
}

func TestCollect_FinalSkipsZeroOutputCast(t *testing.T) {
	distinct, casts, events, gaps, err := collect([]string{noOutputFixture()}, 0.5, true)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if casts != 1 {
		t.Errorf("casts = %d, want 1", casts)
	}
	if events != 0 {
		t.Errorf("events = %d, want 0 (header-only cast has no output events)", events)
	}
	if gaps != 0 {
		t.Errorf("gaps = %d, want 0", gaps)
	}
	// A cast with zero output events emits no final sample even with -final on.
	if len(distinct) != 0 {
		t.Fatalf("distinct = %d, want 0 (zero-output cast yields no final sample)", len(distinct))
	}
}

func TestCollect_FinalSourceDeterministic(t *testing.T) {
	a, _, _, _, err := collect([]string{fixture()}, 0.5, true)
	if err != nil {
		t.Fatalf("collect (run a): %v", err)
	}
	b, _, _, _, err := collect([]string{fixture()}, 0.5, true)
	if err != nil {
		t.Fatalf("collect (run b): %v", err)
	}
	if len(a) != len(b) {
		t.Fatalf("distinct lengths differ across runs: %d vs %d", len(a), len(b))
	}
	// Per-sample source arrays must be byte-identical across runs (AC 4): the
	// sorted-array serialization makes order independent of discovery order.
	for i := range a {
		if a[i].Hash != b[i].Hash {
			t.Errorf("distinct[%d] hash differs across runs: %s vs %s", i, a[i].Hash, b[i].Hash)
		}
		if !reflect.DeepEqual(a[i].Source, b[i].Source) {
			t.Errorf("distinct[%d] source differs across runs: %v vs %v (hash %s)", i, a[i].Source, b[i].Source, a[i].Hash)
		}
	}
}

func TestNormalize(t *testing.T) {
	// Same-screen folds: each pair must normalize to one string (so its hash
	// collapses in dedupe). The differ pair must stay distinct.
	folds := []struct {
		name, a, b string
		wantEqual  bool
	}{
		{"digit-run-collapse", "tokens 12345", "tokens 6", true},                  // any digit run -> "0"
		{"spinner-unify", "\xe2\x9c\xb3 Thinking", "\xe2\x9c\xbb Thinking", true}, // ✳ and ✻ -> canonical
		{"right-trim", "prompt   ", "prompt", true},                               // trailing spaces stripped
		{"distinct-stays-distinct", "alpha", "beta", false},
	}
	for _, c := range folds {
		gotEqual := hashGrid(normalize(c.a)) == hashGrid(normalize(c.b))
		if gotEqual != c.wantEqual {
			t.Errorf("%s: normalize-equal = %v, want %v", c.name, gotEqual, c.wantEqual)
		}
	}

	// Direct value checks for each rule.
	if got := normalize("tokens 12345"); got != "tokens 0" {
		t.Errorf("digit collapse: got %q, want %q", got, "tokens 0")
	}
	if got := normalize("a  \nb   "); got != "a\nb" {
		t.Errorf("row right-trim: got %q, want %q", got, "a\nb")
	}
}

func TestTagFromName(t *testing.T) {
	cases := map[string]string{
		"20260707T-uuid-ok.cast":  "ok",
		"20260707T-uuid-err.cast": "err",
		"weird-name.cast":         "untagged",
	}
	for in, want := range cases {
		if got := tagFromName(in); got != want {
			t.Errorf("tagFromName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSegmentOf(t *testing.T) {
	cases := map[string]string{
		"running /private/tmp/claude-501/TestRealClaude_Foo/001": "e2e",
		"cwd /Users/x/Workspace/Projects/foo-agents/.worktree-3": "prod",
		"nothing distinctive here":                               "unknown",
	}
	for in, want := range cases {
		if got := segmentOf(in); got != want {
			t.Errorf("segmentOf(%q) = %q, want %q", in, got, want)
		}
	}
}

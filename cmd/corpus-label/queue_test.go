package main

import (
	"path/filepath"
	"testing"
)

// mkSample builds a synthetic input sample. Grids are inert placeholder text (no
// real detection anchor literals) so even a dump can never false-fire live
// detection — the self-reference discipline the whole tool is built around.
func mkSample(hash, tag string, source ...string) sample {
	return sample{
		Hash:   hash,
		Grid:   "synthetic screen " + hash + "\n",
		Cast:   "fixture-" + tag + ".cast",
		Tag:    tag,
		Source: source,
	}
}

func hashesOfSamples(ss []sample) []string {
	hs := make([]string, len(ss))
	for i, s := range ss {
		hs[i] = s.Hash
	}
	return hs
}

func TestSelectUnlabeled_SkipsLabeledAndDedupes(t *testing.T) {
	samples := []sample{
		mkSample("a", "ok"),
		mkSample("b", "ok"),
		mkSample("a", "ok"), // in-run duplicate of a
		mkSample("c", "err"),
	}
	labeled := map[string]bool{"b": true}
	got := selectUnlabeled(samples, labeled)
	want := []string{"a", "c"} // b already labeled; second a de-duped; input order kept
	if gotH := hashesOfSamples(got); !equalStrings(gotH, want) {
		t.Errorf("selectUnlabeled = %v, want %v", gotH, want)
	}
}

func TestSelectUnlabeled_EmptyLabeledIsFullRun(t *testing.T) {
	samples := []sample{mkSample("a", "ok"), mkSample("b", "ok")}
	got := selectUnlabeled(samples, map[string]bool{})
	if len(got) != 2 {
		t.Errorf("selectUnlabeled with empty labeled set = %d, want 2 (full run)", len(got))
	}
}

func TestPrioritize_FireThenErrThenRestStable(t *testing.T) {
	// Interleaved input; within each tier input order must be preserved.
	samples := []sample{
		mkSample("r1", "ok"),                 // rest
		mkSample("e1", "err"),                // err
		mkSample("f1", "ok", "gap", "fires"), // fire
		mkSample("r2", "untagged"),           // rest
		mkSample("e2", "err"),                // err
		mkSample("f2", "err", "fires"),       // fire wins over its err tag
	}
	got := hashesOfSamples(prioritize(samples))
	want := []string{"f1", "f2", "e1", "e2", "r1", "r2"}
	if !equalStrings(got, want) {
		t.Errorf("prioritize order = %v, want %v", got, want)
	}
}

func TestPrioritize_GracefulEmptyTiers(t *testing.T) {
	// No source field anywhere → fire tier empty; no err tag → err tier empty.
	// The absent provenance must leave a tier empty, never fail (AC "degrades
	// gracefully").
	samples := []sample{mkSample("a", "ok"), mkSample("b", "untagged")}
	got := hashesOfSamples(prioritize(samples))
	want := []string{"a", "b"} // all fall through to rest, order preserved
	if !equalStrings(got, want) {
		t.Errorf("prioritize with no fire/err = %v, want %v", got, want)
	}
}

func TestBatchesOf_CountsAndSizes(t *testing.T) {
	var samples []sample
	for i := 0; i < 25; i++ {
		samples = append(samples, mkSample(string(rune('a'+i)), "ok"))
	}
	batches := batchesOf(samples, 15)
	if len(batches) != 2 {
		t.Fatalf("batches = %d, want 2 (25 / 15)", len(batches))
	}
	if len(batches[0]) != 15 || len(batches[1]) != 10 {
		t.Errorf("batch sizes = %d,%d, want 15,10", len(batches[0]), len(batches[1]))
	}
}

func TestBatchesOf_EmptyQueue(t *testing.T) {
	if got := batchesOf(nil, 15); got != nil {
		t.Errorf("batchesOf(nil) = %v, want nil", got)
	}
}

func TestBatchInRange(t *testing.T) {
	cases := map[int]bool{9: false, 10: true, 15: true, 20: true, 21: false, 0: false}
	for n, want := range cases {
		if got := batchInRange(n); got != want {
			t.Errorf("batchInRange(%d) = %v, want %v", n, got, want)
		}
	}
}

func TestSelectRepass_PicksLowConfAndUnusual(t *testing.T) {
	samples := []sample{mkSample("h0", "ok"), mkSample("h1", "ok"), mkSample("h2", "ok")}
	recs := []labelRecord{
		{Hash: "h0", Label: "idle", Confidence: 0.9},            // high conf — skip
		{Hash: "h1", Label: "idle", Confidence: 0.3},            // low conf — pick
		{Hash: "h2", Label: "unusual: weird", Confidence: 0.95}, // unusual — pick
	}
	got := hashesOfSamples(selectRepass(samples, recs, 0.7))
	want := []string{"h1", "h2"}
	if !equalStrings(got, want) {
		t.Errorf("selectRepass = %v, want %v", got, want)
	}
}

func TestSelectRepass_SkipsHashAbsentFromInput(t *testing.T) {
	// A low-confidence -out record whose hash is not in -in has no grid to
	// re-label, so it is skipped (not crashed on).
	samples := []sample{mkSample("h0", "ok")}
	recs := []labelRecord{{Hash: "ghost", Label: "idle", Confidence: 0.1}}
	if got := selectRepass(samples, recs, 0.7); len(got) != 0 {
		t.Errorf("selectRepass with unmatched hash = %v, want empty", hashesOfSamples(got))
	}
}

func TestReadSamples_TolerantOfMalformedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "in.jsonl")
	// second line is garbage; it must be skipped, not abort the read.
	writeLines(t, path,
		`{"hash":"a","grid":"g","tag":"ok","source":["gap"]}`,
		`not json`,
		`{"hash":"b","grid":"g","tag":"err"}`,
	)
	got, err := readSamples(path)
	if err != nil {
		t.Fatalf("readSamples: %v", err)
	}
	if h := hashesOfSamples(got); !equalStrings(h, []string{"a", "b"}) {
		t.Errorf("readSamples hashes = %v, want [a b]", h)
	}
}

func TestReadLabels_MalformedLineIsFatal(t *testing.T) {
	// A corrupt resume source must not silently re-label everything — reading it
	// is a hard error.
	dir := t.TempDir()
	path := filepath.Join(dir, "out.jsonl")
	writeLines(t, path, `{"hash":"a","label":"idle","confidence":0.9}`, `{corrupt`)
	if _, err := readLabels(path); err == nil {
		t.Errorf("readLabels on corrupt file = nil error, want fatal")
	}
}

func TestAppendAndReadLabelsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.jsonl")
	recs := []labelRecord{
		{Hash: "a", Label: "idle", Confidence: 0.9, Model: "haiku", Cast: "x-ok.cast", Tag: "ok"},
		{Hash: "b", Label: "unusual: novel", Confidence: 0.5, Model: "haiku"},
	}
	if err := appendLabels(path, recs[:1]); err != nil {
		t.Fatal(err)
	}
	if err := appendLabels(path, recs[1:]); err != nil { // append, not truncate
		t.Fatal(err)
	}
	got, err := readLabels(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Hash != "a" || got[1].Hash != "b" {
		t.Errorf("round-trip = %+v, want a then b appended", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

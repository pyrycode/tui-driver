package main

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// TestReplayAll_ByteIdenticalAcrossWorkerCounts is the headline AC3 test: for the
// same fixture directory, a sequential (-workers 1) run and a parallel (-workers
// 8) run must produce byte-for-byte identical report output. It renders each run
// with per-cast=true so the most order-sensitive section (the per-cast table) is
// exercised; it fails if the sort-by-name is dropped and passes when present.
// The workers=8 arm also runs the pool concurrently, so this doubles as the
// -race exerciser.
func TestReplayAll_ByteIdenticalAcrossWorkerCounts(t *testing.T) {
	paths, err := castPaths("testdata", "")
	if err != nil {
		t.Fatalf("castPaths: %v", err)
	}
	r1 := replayAll(paths, 1, 1, io.Discard)
	rN := replayAll(paths, 1, 8, io.Discard)

	var b1, bN bytes.Buffer
	report(&b1, r1, "testdata", 1, true)
	report(&bN, rN, "testdata", 1, true)

	if b1.String() != bN.String() {
		t.Errorf("report output differs between -workers 1 and -workers 8:\n--- workers=1 ---\n%s\n--- workers=8 ---\n%s",
			b1.String(), bN.String())
	}
}

// TestReplayAll_ResultsSortedByName localises a sort regression before it surfaces
// as a byte diff: under the pool casts complete in arbitrary order, so the
// returned slice must be sorted ascending by cast name.
func TestReplayAll_ResultsSortedByName(t *testing.T) {
	paths, err := castPaths("testdata", "")
	if err != nil {
		t.Fatalf("castPaths: %v", err)
	}
	res := replayAll(paths, 1, 8, io.Discard)
	if len(res) == 0 {
		t.Fatal("replayAll returned no results")
	}
	for i := 1; i < len(res); i++ {
		if res[i-1].name > res[i].name {
			t.Errorf("results not sorted by name: %q before %q", res[i-1].name, res[i].name)
		}
	}
}

// TestReplayAll_SkipsUnreadableCastAndContinues proves the per-cast error branch
// stays wired and non-fatal under the pool: a bogus path makes replayCast's
// os.Open fail, so it is dropped from the results with a skip line on errOut while
// the good cast still lands.
func TestReplayAll_SkipsUnreadableCastAndContinues(t *testing.T) {
	paths := []string{
		filepath.Join("testdata", "sample-ok.cast"),
		filepath.Join("testdata", "does-not-exist.cast"),
	}
	var errBuf bytes.Buffer
	res := replayAll(paths, 1, 4, &errBuf)
	if len(res) != 1 {
		t.Fatalf("len(results) = %d, want 1 (only sample-ok)", len(res))
	}
	if res[0].name != "sample-ok.cast" {
		t.Errorf("results[0].name = %q, want sample-ok.cast", res[0].name)
	}
	if !strings.Contains(errBuf.String(), "skip") || !strings.Contains(errBuf.String(), "does-not-exist") {
		t.Errorf("errBuf missing skip line for the missing cast:\n%s", errBuf.String())
	}
}

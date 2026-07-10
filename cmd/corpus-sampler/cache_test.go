package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeGo writes content to a .go file, failing the test on error.
func writeGo(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// outBytes renders a distinct set through the exact -out path (writeSamples), so
// two runs can be compared for byte-identity without duplicating the JSONL
// marshalling. It never returns grid content to the test's failure messages —
// callers compare the bytes and report only lengths on mismatch.
func outBytes(t *testing.T, distinct []sample) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.jsonl")
	if err := writeSamples(path, distinct); err != nil {
		t.Fatalf("writeSamples: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	return data
}

// samplesEqual reports whether two sample slices marshal identically. Marshalling
// (not printing) keeps grid content out of the comparison and off the failure
// path — callers report lengths/hashes only.
func samplesEqual(t *testing.T, got, want []sample) bool {
	t.Helper()
	gb, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wb, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	return bytes.Equal(gb, wb)
}

// TestVersionHash_DeterministicAndInvalidates proves the version key is stable
// across calls and flips on any source change: an edit, an addition, or a swap of
// two files' contents (the last proving the path is folded into the hash, not just
// the multiset of contents).
func TestVersionHash_DeterministicAndInvalidates(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	writeGo(t, a, "package p\n// alpha\n")
	writeGo(t, b, "package p\n// beta\n")

	h1, err := versionHash(dir)
	if err != nil {
		t.Fatalf("versionHash: %v", err)
	}
	h2, err := versionHash(dir)
	if err != nil {
		t.Fatalf("versionHash: %v", err)
	}
	if h1 != h2 {
		t.Errorf("versionHash not deterministic: %s != %s", h1, h2)
	}

	// Editing a file's content flips the hash.
	writeGo(t, a, "package p\n// alpha edited\n")
	hEdit, err := versionHash(dir)
	if err != nil {
		t.Fatalf("versionHash: %v", err)
	}
	if hEdit == h1 {
		t.Error("editing a source did not change the hash")
	}

	// Adding a file flips the hash.
	writeGo(t, filepath.Join(dir, "c.go"), "package p\n// gamma\n")
	hAdd, err := versionHash(dir)
	if err != nil {
		t.Fatalf("versionHash: %v", err)
	}
	if hAdd == hEdit {
		t.Error("adding a source did not change the hash")
	}
}

// TestVersionHash_FoldsInPath proves the file path is part of the hash: swapping
// two files' contents (same multiset of contents, different path->content mapping)
// changes the hash, so a content swap between two files cannot go undetected.
func TestVersionHash_FoldsInPath(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	const ca = "package p\n// content A\n"
	const cb = "package p\n// content B\n"
	writeGo(t, a, ca)
	writeGo(t, b, cb)
	h1, err := versionHash(dir)
	if err != nil {
		t.Fatalf("versionHash: %v", err)
	}

	writeGo(t, a, cb)
	writeGo(t, b, ca)
	h2, err := versionHash(dir)
	if err != nil {
		t.Fatalf("versionHash: %v", err)
	}
	if h1 == h2 {
		t.Error("swapping file contents between paths did not change the hash (path not folded in)")
	}
}

// TestSampleOrLoad_Hit proves a stored entry is served without a fresh sample and
// that the returned (samples, events, gaps) round-trip identically to a fresh
// sampleCast — the load-bearing byte-identity property at the per-cast level.
func TestSampleOrLoad_Hit(t *testing.T) {
	cache := &sampleCache{dir: t.TempDir(), version: "v1", mode: "m1"}
	path := fixture()

	s0, ev0, gp0, err := sampleCast(path, 0.5, true, true, 3)
	if err != nil {
		t.Fatalf("sampleCast: %v", err)
	}
	h, err := hashCastContent(path)
	if err != nil {
		t.Fatalf("hashCastContent: %v", err)
	}
	if err := cache.store(cache.key(filepath.Base(path), h), sampleCacheEntry{Samples: s0, Events: ev0, Gaps: gp0}); err != nil {
		t.Fatalf("store: %v", err)
	}

	samples, events, gaps, hit, err := sampleOrLoad(cache, path, 0.5, true, true, 3)
	if err != nil {
		t.Fatalf("sampleOrLoad: %v", err)
	}
	if !hit {
		t.Fatal("expected a cache hit after store")
	}
	if events != ev0 || gaps != gp0 {
		t.Errorf("cached counts = (events %d, gaps %d), want (%d, %d)", events, gaps, ev0, gp0)
	}
	if !samplesEqual(t, samples, s0) {
		t.Errorf("cached samples differ from fresh sampleCast (got %d samples, want %d)", len(samples), len(s0))
	}
}

// TestSampleOrLoad_MissThenHit proves a fresh cache misses on the first lookup and
// hits on the second (the store from the miss warms it).
func TestSampleOrLoad_MissThenHit(t *testing.T) {
	cache := &sampleCache{dir: t.TempDir(), version: "v1", mode: "m1"}
	path := fixture()

	if _, _, _, hit, err := sampleOrLoad(cache, path, 0.5, false, false, 0); err != nil || hit {
		t.Fatalf("cold lookup: hit=%v err=%v, want hit=false err=nil", hit, err)
	}
	if _, _, _, hit, err := sampleOrLoad(cache, path, 0.5, false, false, 0); err != nil || !hit {
		t.Fatalf("warm lookup: hit=%v err=%v, want hit=true err=nil", hit, err)
	}
}

// TestCollectCached_VersionInvalidation is the end-to-end AC2 check: a cold cache
// misses everything, a warm re-run hits everything, and a new version key over the
// same corpus in the same dir misses everything again (every key changed).
func TestCollectCached_VersionInvalidation(t *testing.T) {
	paths, err := castPaths("testdata")
	if err != nil {
		t.Fatalf("castPaths: %v", err)
	}
	dir := t.TempDir()

	c1 := &sampleCache{dir: dir, version: "v1", mode: "m1"}
	if _, _, _, _, s, _ := collectCached(paths, 0.5, false, false, 0, c1); s.hits != 0 || s.misses != len(paths) {
		t.Fatalf("cold v1: got %d hits %d misses, want 0 hits %d misses", s.hits, s.misses, len(paths))
	}
	if _, _, _, _, s, _ := collectCached(paths, 0.5, false, false, 0, c1); s.hits != len(paths) || s.misses != 0 {
		t.Fatalf("warm v1: got %d hits %d misses, want %d hits 0 misses", s.hits, s.misses, len(paths))
	}

	c2 := &sampleCache{dir: dir, version: "v2", mode: "m1"}
	if _, _, _, _, s, _ := collectCached(paths, 0.5, false, false, 0, c2); s.hits != 0 || s.misses != len(paths) {
		t.Fatalf("v2 over v1 cache: got %d hits %d misses, want 0 hits %d misses", s.hits, s.misses, len(paths))
	}
}

// TestCollectCached_ModeInvalidation proves a mode-flag change invalidates every
// cast: a warm cache under one mode signature does not satisfy a lookup under a
// different mode signature (same dir/version), so all miss.
func TestCollectCached_ModeInvalidation(t *testing.T) {
	paths, err := castPaths("testdata")
	if err != nil {
		t.Fatalf("castPaths: %v", err)
	}
	dir := t.TempDir()

	c1 := &sampleCache{dir: dir, version: "v1", mode: "m1"}
	if _, _, _, _, s, _ := collectCached(paths, 0.5, false, false, 0, c1); s.misses != len(paths) {
		t.Fatalf("warming mode m1: got %d misses, want %d", s.misses, len(paths))
	}

	c2 := &sampleCache{dir: dir, version: "v1", mode: "m2"}
	if _, _, _, _, s, _ := collectCached(paths, 0.5, false, false, 0, c2); s.hits != 0 || s.misses != len(paths) {
		t.Fatalf("mode m2 over m1 cache: got %d hits %d misses, want 0 hits %d misses", s.hits, s.misses, len(paths))
	}
}

// TestCollectCached_OnlyNewCastReSamples proves the "adding a new cast re-samples
// only the new cast" half of AC2: warm every cast but the last, then re-sample the
// full corpus and assert exactly one miss (the new cast) and the rest hits.
func TestCollectCached_OnlyNewCastReSamples(t *testing.T) {
	paths, err := castPaths("testdata")
	if err != nil {
		t.Fatalf("castPaths: %v", err)
	}
	if len(paths) < 2 {
		t.Fatalf("need >= 2 fixtures, have %d", len(paths))
	}
	dir := t.TempDir()
	c := &sampleCache{dir: dir, version: "v1", mode: "m1"}

	warm := paths[:len(paths)-1]
	if _, _, _, _, s, _ := collectCached(warm, 0.5, false, false, 0, c); s.misses != len(warm) {
		t.Fatalf("warming subset: got %d misses, want %d", s.misses, len(warm))
	}
	if _, _, _, _, s, _ := collectCached(paths, 0.5, false, false, 0, c); s.misses != 1 || s.hits != len(warm) {
		t.Fatalf("full run after one new cast: got %d hits %d misses, want %d hits 1 miss", s.hits, s.misses, len(warm))
	}
}

// TestCollectCached_ByteIdenticalColdWarmNoCache is the headline AC3 check: the
// -out bytes AND the stdout aggregate tuple (casts, events, gaps, distinct) are
// byte-identical across a -no-cache run, a cold cache run, and a warm cache run
// over the same corpus. It additionally asserts the cold run is all-miss and the
// warm run is all-hit, so "every cast is a hit and no cast is re-sampled" is
// proven, not assumed. Rich flags (-final -fires -midstream) stress the sample
// round-trip across every source (gap/final/fire/midstream, multi-value Source
// unions, fire detector keys).
func TestCollectCached_ByteIdenticalColdWarmNoCache(t *testing.T) {
	paths, err := castPaths("testdata")
	if err != nil {
		t.Fatalf("castPaths: %v", err)
	}

	noCacheDistinct, ncCasts, ncEvents, ncGaps, ncStats, _ := collectCached(paths, 0.5, true, true, 3, nil)

	dir := t.TempDir()
	c := &sampleCache{dir: dir, version: "v1", mode: "m1"}
	coldDistinct, cCasts, cEvents, cGaps, coldStats, _ := collectCached(paths, 0.5, true, true, 3, c)
	warmDistinct, wCasts, wEvents, wGaps, warmStats, _ := collectCached(paths, 0.5, true, true, 3, c)

	noCache, cold, warm := outBytes(t, noCacheDistinct), outBytes(t, coldDistinct), outBytes(t, warmDistinct)
	if !bytes.Equal(noCache, cold) || !bytes.Equal(cold, warm) {
		t.Errorf("-out bytes differ across no-cache/cold/warm (lengths %d/%d/%d)", len(noCache), len(cold), len(warm))
	}

	// The stdout aggregate tuple must match too (Events/Gaps fold from the cache).
	agg := func(casts, events, gaps int, distinct []sample) [4]int {
		return [4]int{casts, events, gaps, len(distinct)}
	}
	nc := agg(ncCasts, ncEvents, ncGaps, noCacheDistinct)
	cd := agg(cCasts, cEvents, cGaps, coldDistinct)
	wm := agg(wCasts, wEvents, wGaps, warmDistinct)
	if nc != cd || cd != wm {
		t.Errorf("aggregate tuple (casts,events,gaps,distinct) differs: no-cache=%v cold=%v warm=%v", nc, cd, wm)
	}

	if ncStats.hits != 0 || ncStats.misses != 0 {
		t.Errorf("no-cache stats non-zero: %+v", ncStats)
	}
	if coldStats.misses != len(paths) || coldStats.hits != 0 {
		t.Errorf("cold stats: got %+v, want %d misses 0 hits", coldStats, len(paths))
	}
	if warmStats.hits != len(paths) || warmStats.misses != 0 {
		t.Errorf("warm stats: got %+v, want %d hits 0 misses", warmStats, len(paths))
	}
}

// TestSampleCache_CorruptEntryIsMiss proves a corrupt cache file is treated as a
// miss (load returns ok=false) so the cache self-heals by re-sampling and
// overwriting.
func TestSampleCache_CorruptEntryIsMiss(t *testing.T) {
	cache := &sampleCache{dir: t.TempDir(), version: "v1", mode: "m1"}
	path := fixture()
	h, err := hashCastContent(path)
	if err != nil {
		t.Fatalf("hashCastContent: %v", err)
	}
	k := cache.key(filepath.Base(path), h)
	if err := os.WriteFile(filepath.Join(cache.dir, k+".json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt entry: %v", err)
	}
	if _, ok := cache.load(k); ok {
		t.Error("corrupt entry loaded as a hit, want miss")
	}
	// sampleOrLoad re-samples past the corrupt entry rather than erroring.
	if _, _, _, hit, err := sampleOrLoad(cache, path, 0.5, false, false, 0); err != nil || hit {
		t.Fatalf("sampleOrLoad over corrupt entry: hit=%v err=%v, want hit=false err=nil", hit, err)
	}
}

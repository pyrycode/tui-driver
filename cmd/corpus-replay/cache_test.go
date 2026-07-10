package main

import (
	"bytes"
	"io"
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

// reportOf renders one result through report() with per-cast=true (the most
// field-sensitive section) so two results can be compared for byte-identity.
func reportOf(r castResult) string {
	var b bytes.Buffer
	report(&b, []castResult{r}, "testdata", 1, true)
	return b.String()
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

// TestReplayOrLoad_Hit proves a stored entry is served without a replay and that
// the returned result reports identically to a fresh replay.
func TestReplayOrLoad_Hit(t *testing.T) {
	cache := &resultCache{dir: t.TempDir(), version: "v1"}
	path := filepath.Join("testdata", "sample-ok.cast")

	r0, err := replayCast(path, 1)
	if err != nil {
		t.Fatalf("replayCast: %v", err)
	}
	h, err := hashCastContent(path)
	if err != nil {
		t.Fatalf("hashCastContent: %v", err)
	}
	if err := cache.store(cache.key(filepath.Base(path), h, 1), r0); err != nil {
		t.Fatalf("store: %v", err)
	}

	r, hit, err := replayOrLoad(cache, path, 1)
	if err != nil {
		t.Fatalf("replayOrLoad: %v", err)
	}
	if !hit {
		t.Fatal("expected a cache hit after store")
	}
	if got, want := reportOf(r), reportOf(r0); got != want {
		t.Errorf("cached result reports differently from fresh replay:\n--- cached ---\n%s\n--- fresh ---\n%s", got, want)
	}
}

// TestReplayOrLoad_MissThenHit proves a fresh cache misses on the first lookup and
// hits on the second (the store from the miss warms it).
func TestReplayOrLoad_MissThenHit(t *testing.T) {
	cache := &resultCache{dir: t.TempDir(), version: "v1"}
	path := filepath.Join("testdata", "sample-ok.cast")

	if _, hit, err := replayOrLoad(cache, path, 1); err != nil || hit {
		t.Fatalf("cold lookup: hit=%v err=%v, want hit=false err=nil", hit, err)
	}
	if _, hit, err := replayOrLoad(cache, path, 1); err != nil || !hit {
		t.Fatalf("warm lookup: hit=%v err=%v, want hit=true err=nil", hit, err)
	}
}

// TestReplayAll_VersionInvalidation is the end-to-end AC3 check: a cold cache
// misses everything, a warm re-run hits everything, and a new version key over the
// same corpus in the same dir misses everything again (every key changed).
func TestReplayAll_VersionInvalidation(t *testing.T) {
	paths, err := castPaths("testdata", "")
	if err != nil {
		t.Fatalf("castPaths: %v", err)
	}
	dir := t.TempDir()

	c1 := &resultCache{dir: dir, version: "v1"}
	if _, s := replayAll(paths, 1, 4, c1, io.Discard); s.hits != 0 || s.misses != len(paths) {
		t.Fatalf("cold v1: got %d hits %d misses, want 0 hits %d misses", s.hits, s.misses, len(paths))
	}
	if _, s := replayAll(paths, 1, 4, c1, io.Discard); s.hits != len(paths) || s.misses != 0 {
		t.Fatalf("warm v1: got %d hits %d misses, want %d hits 0 misses", s.hits, s.misses, len(paths))
	}

	c2 := &resultCache{dir: dir, version: "v2"}
	if _, s := replayAll(paths, 1, 4, c2, io.Discard); s.hits != 0 || s.misses != len(paths) {
		t.Fatalf("v2 over v1 cache: got %d hits %d misses, want 0 hits %d misses", s.hits, s.misses, len(paths))
	}
}

// TestReplayAll_OnlyNewCastReplays proves the "adding a new cast replays only the
// new cast" half of AC3: warm every cast but the last, then replay the full corpus
// and assert exactly one miss (the new cast) and the rest hits.
func TestReplayAll_OnlyNewCastReplays(t *testing.T) {
	paths, err := castPaths("testdata", "")
	if err != nil {
		t.Fatalf("castPaths: %v", err)
	}
	if len(paths) < 2 {
		t.Fatalf("need >= 2 fixtures, have %d", len(paths))
	}
	dir := t.TempDir()
	c := &resultCache{dir: dir, version: "v1"}

	warm := paths[:len(paths)-1]
	if _, s := replayAll(warm, 1, 4, c, io.Discard); s.misses != len(warm) {
		t.Fatalf("warming subset: got %d misses, want %d", s.misses, len(warm))
	}
	if _, s := replayAll(paths, 1, 4, c, io.Discard); s.misses != 1 || s.hits != len(warm) {
		t.Fatalf("full run after one new cast: got %d hits %d misses, want %d hits 1 miss", s.hits, s.misses, len(warm))
	}
}

// TestReplayAll_StrideKeyInteraction is AC5: a stride-16 warm cache does not
// satisfy a stride-1 lookup (all miss), and a second stride-1 run then hits.
func TestReplayAll_StrideKeyInteraction(t *testing.T) {
	paths, err := castPaths("testdata", "")
	if err != nil {
		t.Fatalf("castPaths: %v", err)
	}
	dir := t.TempDir()
	c := &resultCache{dir: dir, version: "v1"}

	if _, s := replayAll(paths, 16, 4, c, io.Discard); s.misses != len(paths) {
		t.Fatalf("stride-16 cold: got %d misses, want %d", s.misses, len(paths))
	}
	if _, s := replayAll(paths, 1, 4, c, io.Discard); s.hits != 0 || s.misses != len(paths) {
		t.Fatalf("stride-1 over stride-16 cache: got %d hits %d misses, want 0 hits %d misses", s.hits, s.misses, len(paths))
	}
	if _, s := replayAll(paths, 1, 4, c, io.Discard); s.hits != len(paths) || s.misses != 0 {
		t.Fatalf("stride-1 warm: got %d hits %d misses, want %d hits 0 misses", s.hits, s.misses, len(paths))
	}
}

// TestReplayAll_ByteIdenticalColdWarmNoCache is the headline AC2/AC6 check: the
// stdout report is byte-identical across a -no-cache run, a cold cache run, and a
// warm cache run over the same corpus. It additionally asserts the cold run is
// all-miss and the warm run is all-hit, so "every cast is a hit and no replay is
// performed" is proven, not assumed. The -workers 8 arm exercises the lockless
// cache path under -race.
func TestReplayAll_ByteIdenticalColdWarmNoCache(t *testing.T) {
	paths, err := castPaths("testdata", "")
	if err != nil {
		t.Fatalf("castPaths: %v", err)
	}
	render := func(res []castResult) string {
		var b bytes.Buffer
		report(&b, res, "testdata", 1, true)
		return b.String()
	}

	noCacheRes, noCacheStats := replayAll(paths, 1, 8, nil, io.Discard)

	dir := t.TempDir()
	c := &resultCache{dir: dir, version: "v1"}
	coldRes, coldStats := replayAll(paths, 1, 8, c, io.Discard)
	warmRes, warmStats := replayAll(paths, 1, 8, c, io.Discard)

	noCache, cold, warm := render(noCacheRes), render(coldRes), render(warmRes)
	if noCache != cold || cold != warm {
		t.Errorf("report output differs across no-cache/cold/warm:\n--- no-cache ---\n%s\n--- cold ---\n%s\n--- warm ---\n%s", noCache, cold, warm)
	}

	if noCacheStats.hits != 0 || noCacheStats.misses != 0 {
		t.Errorf("no-cache stats non-zero: %+v", noCacheStats)
	}
	if coldStats.misses != len(paths) || coldStats.hits != 0 {
		t.Errorf("cold stats: got %+v, want %d misses 0 hits", coldStats, len(paths))
	}
	if warmStats.hits != len(paths) || warmStats.misses != 0 {
		t.Errorf("warm stats: got %+v, want %d hits 0 misses", warmStats, len(paths))
	}
}

// TestCache_CorruptEntryIsMiss proves a corrupt cache file is treated as a miss
// (load returns ok=false) so the cache self-heals by re-replaying and overwriting.
func TestCache_CorruptEntryIsMiss(t *testing.T) {
	cache := &resultCache{dir: t.TempDir(), version: "v1"}
	path := filepath.Join("testdata", "sample-ok.cast")
	h, err := hashCastContent(path)
	if err != nil {
		t.Fatalf("hashCastContent: %v", err)
	}
	k := cache.key(filepath.Base(path), h, 1)
	if err := os.WriteFile(filepath.Join(cache.dir, k+".json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt entry: %v", err)
	}
	if _, ok := cache.load(k); ok {
		t.Error("corrupt entry loaded as a hit, want miss")
	}
	// replayOrLoad re-replays past the corrupt entry rather than erroring.
	if _, hit, err := replayOrLoad(cache, path, 1); err != nil || hit {
		t.Fatalf("replayOrLoad over corrupt entry: hit=%v err=%v, want hit=false err=nil", hit, err)
	}
}

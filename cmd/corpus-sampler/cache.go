package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// A cast's sampler result is a pure function of three inputs: the cast bytes
// (immutable per recording), the four mode inputs (-gap resolved to seconds,
// -final, -fires, -midstream — they change what a cast yields), and the
// detection + sampling code. The cache is content-addressed over all three, so
// any change to any input yields a different key -> a miss -> a fresh sample.
// That is the whole correctness argument, and it is why the version hash below
// covers BOTH source trees: the library detectors in pkg/tuidriver (IsIdle,
// DetectModalClass, the buffer, Render, ...) AND this tool's own sampling logic
// in cmd/corpus-sampler (normalize, hashGrid, activeKeys/flatDetectors, the
// gap/final/fires/midstream logic in sampleCast, segmentOf, tagFromName).
// Keying on pkg/tuidriver alone would serve stale results after an edit to any
// of those — a silent correctness bug.

// versionSourceDirs are the source trees folded into the detector-version hash.
// The tool runs by hand from the repo root (go run ./cmd/corpus-sampler ...), so
// both are reachable by relative path at runtime, same as corpus-replay.
var versionSourceDirs = []string{"pkg/tuidriver", "cmd/corpus-sampler"}

// versionHash returns hex(sha256) folding in every *.go file under each dir,
// sorted by path, path-then-content. It is deterministic; any edit to any covered
// source flips it, invalidating the whole cache. Test files and promote.go are
// NOT filtered out: over-invalidation (a test or promote edit re-samples the
// corpus) is safe and self-correcting, whereas a "which files are detectors"
// filter would risk under-invalidation — serving a stale result. Reading a source
// file's bytes to hash it renders nothing on screen, so the self-reference caution
// (anchors quoted in these files) does not apply.
func versionHash(dirs ...string) (string, error) {
	var paths []string
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			paths = append(paths, path)
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	sort.Strings(paths)

	h := sha256.New()
	for _, p := range paths {
		content, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		// Path-in-hash guards a content swap between two files; the \x00
		// separators guard boundary ambiguity between path/content/next-path.
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(content)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sampleCache is a per-cast on-disk result cache. One file per key. The sampler
// is single-threaded (collectCached walks casts sequentially), so each cache file
// is touched by exactly one goroutine at most once per run — no shared mutable
// state, no mutex. The version hash and mode signature are computed once in
// newSampleCache and read-only thereafter.
type sampleCache struct {
	dir     string // cache directory, already MkdirAll'd
	version string // detector-version hash, computed once at construction
	mode    string // mode-signature fold of the four run-level mode inputs
}

// newSampleCache creates the cache directory, computes the detector-version hash
// over sourceDirs once, and folds the four mode inputs into a mode signature. An
// empty cacheDir defaults to a corpus-sampler subdirectory under the user cache
// dir (distinct from corpus-replay's own dir).
func newSampleCache(cacheDir string, gap float64, final, fires bool, midstream int, sourceDirs ...string) (*sampleCache, error) {
	if cacheDir == "" {
		ucd, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		cacheDir = filepath.Join(ucd, "corpus-sampler")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	version, err := versionHash(sourceDirs...)
	if err != nil {
		return nil, err
	}
	// Deterministic fold of the four resolved mode inputs; any change -> a
	// different string -> a different key for every cast. gap is folded as its
	// RESOLVED seconds value (the float64 already passed to sampleCast), mirroring
	// corpus-replay folding the resolved stride, so a -fires-on / -gap 1s / ...
	// entry can never satisfy a differently-moded lookup.
	mode := strconv.FormatFloat(gap, 'g', -1, 64) + "|" +
		strconv.FormatBool(final) + "|" + strconv.FormatBool(fires) + "|" +
		strconv.Itoa(midstream)
	return &sampleCache{dir: cacheDir, version: version, mode: mode}, nil
}

// key hashes everything a result depends on — the detector version, the mode
// signature, the cast name, and the cast content hash — into one filename stem
// (mirroring the hashGrid idiom).
//
//   - version folds in the detector version -> any source edit changes every key
//     -> whole-cache invalidation.
//   - mode folds in the four resolved mode inputs -> a -fires-on entry can never
//     satisfy a -fires-off lookup.
//   - castName folds the name-derived tag into identity, so two casts with
//     identical bytes but different names never collide.
//   - contentHash enforces immutability: a new cast is a new key (miss) while
//     every existing cast keeps its key (hit).
func (c *sampleCache) key(castName, contentHash string) string {
	sum := sha256.Sum256([]byte(c.version + "\x00" + c.mode + "\x00" + castName + "\x00" + contentHash))
	return hex.EncodeToString(sum[:])
}

// hashCastContent streams the cast file through sha256 (no full-file buffering)
// and returns hex(sum). On a cache hit this is the only file access — the
// expensive sampleCast (per-event JSON decode + rolling buffer + render +
// detectors) is skipped entirely.
func hashCastContent(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sampleCacheEntry is one cast's cached sampler result: its pre-dedup sample list
// exactly as sampleCast returns it (Seen left 0 — collect owns Seen; Tag/Segment
// stamped; Source per-finder, already sorted) plus that cast's event and gap
// counts. No serializable mirror is needed (unlike corpus-replay): the sample
// type is already fully json-tagged, so a plain wrapper round-trips losslessly.
// Events/Gaps are load-bearing: without them a warm run's casts/events/gaps
// aggregate would drift from a cold run and break the byte-identity criterion.
type sampleCacheEntry struct {
	Samples []sample `json:"samples"`
	Events  int      `json:"events"`
	Gaps    int      `json:"gaps"`
}

// load reads and decodes the cache entry at key. Any error (missing file, decode
// failure) is a miss, so a corrupt entry self-heals: the cast is re-sampled and
// the entry overwritten.
func (c *sampleCache) load(key string) (sampleCacheEntry, bool) {
	data, err := os.ReadFile(filepath.Join(c.dir, key+".json"))
	if err != nil {
		return sampleCacheEntry{}, false
	}
	var e sampleCacheEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return sampleCacheEntry{}, false
	}
	return e, true
}

// store writes the entry to the key's file. Best-effort: a store error does not
// fail the cast (the fresh result is valid and already returned), it just means
// the entry is not persisted and the next run misses again.
func (c *sampleCache) store(key string, e sampleCacheEntry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.dir, key+".json"), data, 0o644)
}

// cacheStats tallies cache hits and misses across a collectCached run. Zero-valued
// and unused when the cache is nil.
type cacheStats struct {
	hits, misses int
}

// sampleOrLoad is the thin per-cast wrapper around sampleCast. With cache==nil it
// samples directly (hit is always false). Otherwise it hashes the cast content,
// looks up the key, and on a hit returns the cached result with NO sample; on a
// miss it samples and stores the result best-effort. A content-hash failure is
// propagated as the cast's error — the same skip semantics as an unreadable cast
// in sampleCast today.
func sampleOrLoad(cache *sampleCache, path string, gap float64, final, fires bool, midstream int) (samples []sample, events, gaps int, hit bool, err error) {
	if cache == nil {
		s, ev, gp, err := sampleCast(path, gap, final, fires, midstream)
		return s, ev, gp, false, err
	}
	h, err := hashCastContent(path)
	if err != nil {
		return nil, 0, 0, false, err
	}
	k := cache.key(filepath.Base(path), h)
	if e, ok := cache.load(k); ok {
		return e.Samples, e.Events, e.Gaps, true, nil // HIT — sampleCast skipped entirely
	}
	s, ev, gp, err := sampleCast(path, gap, final, fires, midstream)
	if err != nil {
		return nil, 0, 0, false, err
	}
	_ = cache.store(k, sampleCacheEntry{Samples: s, Events: ev, Gaps: gp}) // best-effort
	return s, ev, gp, false, nil
}

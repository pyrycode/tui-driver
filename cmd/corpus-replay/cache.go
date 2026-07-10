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

// A cast's replay result is a pure function of three inputs: the cast bytes
// (immutable per recording), the resolved stride, and the detection code. The
// cache is content-addressed over all three, so any change to any input yields a
// different key -> a miss -> a fresh replay. That is the whole correctness
// argument, and it is why the version hash below covers BOTH source trees: the
// library detectors in pkg/tuidriver AND this tool's own classify logic in
// cmd/corpus-replay (contentAnchors, segmentOf, classify, ...). Keying on
// pkg/tuidriver alone would serve stale results after an edit to an anchor or the
// segmentation here.

// versionSourceDirs are the source trees folded into the detector-version hash.
// The tool runs from the repo root (make corpus-replay runs there), so both are
// reachable by relative path at runtime.
var versionSourceDirs = []string{"pkg/tuidriver", "cmd/corpus-replay"}

// versionHash returns hex(sha256) folding in every *.go file under each dir,
// sorted by path, path-then-content. It is deterministic; any edit to any covered
// source flips it, invalidating the whole cache. Test files are NOT filtered out:
// over-invalidation (a test edit re-replays the corpus) is safe and
// self-correcting, whereas a "which files are detectors" filter would risk
// under-invalidation — serving a stale result. Reading a source file's bytes to
// hash it renders nothing on screen, so the self-reference caution (anchors
// quoted in these files) does not apply.
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

// resultCache is a per-cast on-disk result cache. One file per key means workers
// touch distinct paths — no shared mutable state, no mutex — so it composes with
// the #260 worker pool locklessly. The version hash is computed once in
// newResultCache and read-only thereafter.
type resultCache struct {
	dir     string // cache directory, already MkdirAll'd
	version string // detector-version hash, computed once at construction
}

// newResultCache creates the cache directory and computes the detector-version
// hash over sourceDirs once. An empty cacheDir defaults to a corpus-replay
// subdirectory under the user cache dir.
func newResultCache(cacheDir string, sourceDirs ...string) (*resultCache, error) {
	if cacheDir == "" {
		ucd, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		cacheDir = filepath.Join(ucd, "corpus-replay")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	version, err := versionHash(sourceDirs...)
	if err != nil {
		return nil, err
	}
	return &resultCache{dir: cacheDir, version: version}, nil
}

// key hashes everything a result depends on — the detector version, the resolved
// stride, the cast name, and the cast content hash — into one filename stem
// (mirroring the hashGrid idiom).
//
//   - version folds in the detector version -> any source edit changes every key
//     -> whole-cache invalidation.
//   - stride folds in the RESOLVED stride -> a stride-16 entry can never satisfy a
//     stride-1 lookup. A future -assert forces stride to 1, so its lookups key on
//     stride=1 and structurally cannot hit a coarser entry; the safety falls out
//     with no reference to -assert here.
//   - castName folds the name-derived tag into identity, so two casts with
//     identical bytes but different names never collide.
//   - contentHash enforces immutability: a new cast is a new key (miss) while
//     every existing cast keeps its key (hit).
func (c *resultCache) key(castName, contentHash string, stride int) string {
	sum := sha256.Sum256([]byte(c.version + "\x00" + strconv.Itoa(stride) + "\x00" + castName + "\x00" + contentHash))
	return hex.EncodeToString(sum[:])
}

// hashCastContent streams the cast file through sha256 (no full-file buffering)
// and returns hex(sum). On a cache hit this is the only file access — the
// expensive replayCast is skipped entirely.
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

// cacheEntry is the serializable mirror of castResult. castResult has unexported
// fields (maps included), so it cannot be JSON-marshalled directly, and exporting
// its fields would cascade through main.go and report.go; a tagged mirror beside
// it round-trips the three maps losslessly. report() sorts every map before
// printing, so JSON's sorted map-key marshalling has no bearing on byte-identity —
// only map CONTENTS matter, and those are preserved. An empty vs nil map is
// report-indistinguishable (both range zero times), so no special-casing.
type cacheEntry struct {
	Name    string          `json:"name"`
	Tag     string          `json:"tag"`
	Segment string          `json:"segment"`
	Cols    int             `json:"cols"`
	Rows    int             `json:"rows"`
	Events  int             `json:"events"`
	Fired   map[string]bool `json:"fired"`
	Edges   map[string]int  `json:"edges"`
	Anchors map[string]bool `json:"anchors"`
}

func toCacheEntry(r castResult) cacheEntry {
	return cacheEntry{
		Name:    r.name,
		Tag:     r.tag,
		Segment: r.segment,
		Cols:    r.cols,
		Rows:    r.rows,
		Events:  r.events,
		Fired:   r.fired,
		Edges:   r.edges,
		Anchors: r.anchors,
	}
}

func (e cacheEntry) toResult() castResult {
	return castResult{
		name:    e.Name,
		tag:     e.Tag,
		segment: e.Segment,
		cols:    e.Cols,
		rows:    e.Rows,
		events:  e.Events,
		fired:   e.Fired,
		edges:   e.Edges,
		anchors: e.Anchors,
	}
}

// load reads and decodes the cache entry at key. Any error (missing file, decode
// failure) is a miss, so a corrupt entry self-heals: the cast is re-replayed and
// the entry overwritten.
func (c *resultCache) load(key string) (castResult, bool) {
	data, err := os.ReadFile(filepath.Join(c.dir, key+".json"))
	if err != nil {
		return castResult{}, false
	}
	var e cacheEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return castResult{}, false
	}
	return e.toResult(), true
}

// store writes r's serializable mirror to the key's file. Best-effort: a store
// error does not fail the cast (the fresh result is valid and already returned),
// it just means the entry is not persisted and the next run misses again.
func (c *resultCache) store(key string, r castResult) error {
	data, err := json.Marshal(toCacheEntry(r))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.dir, key+".json"), data, 0o644)
}

// cacheStats tallies cache hits and misses across a replayAll run. Zero-valued
// and unused when the cache is nil.
type cacheStats struct {
	hits, misses int
}

// replayOrLoad is the thin per-cast wrapper around replayCast. With cache==nil it
// replays directly (hit is always false). Otherwise it hashes the cast content,
// looks up the key, and on a hit returns the cached result with NO replay; on a
// miss it replays and stores the result best-effort. A content-hash failure is
// propagated as the cast's error — the same skip semantics as an unreadable cast
// in replayCast today.
func replayOrLoad(cache *resultCache, path string, stride int) (res castResult, hit bool, err error) {
	if cache == nil {
		r, err := replayCast(path, stride)
		return r, false, err
	}
	h, err := hashCastContent(path)
	if err != nil {
		return castResult{}, false, err
	}
	k := cache.key(filepath.Base(path), h, stride)
	if r, ok := cache.load(k); ok {
		return r, true, nil // HIT — replayCast skipped entirely
	}
	r, err := replayCast(path, stride)
	if err != nil {
		return castResult{}, false, err
	}
	_ = cache.store(k, r) // best-effort; a store error just means the next run misses
	return r, false, nil
}

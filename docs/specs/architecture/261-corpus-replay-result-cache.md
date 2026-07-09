# Spec #261 — corpus-replay: per-cast result cache keyed by a detector-version hash

## Files to read first

The developer's turn-1 data load. Read these before writing any code.

- `cmd/corpus-replay/main.go:99-110` — `castResult` struct. This is the value the
  cache round-trips; its fields (incl. three maps) define the serializable mirror.
- `cmd/corpus-replay/main.go:112-144` — `main()`: the flag block and the
  `replayAll → report` wiring. The two new flags and the cache construction slot
  in here.
- `cmd/corpus-replay/main.go:146-217` — `replayOutcome` + `replayAll` (the #260
  worker pool). The cache threads through here; `replayOutcome` gains a `hit`
  field; the collector tallies hits/misses.
- `cmd/corpus-replay/main.go:240-314` — `replayCast`. **Unchanged.** The cache is
  a thin wrapper around it (`replayOrLoad`), never a modification of it.
- `cmd/corpus-replay/report.go:86-243` — `report()`. It reads every `castResult`
  field. Not modified — but confirms the round-trip must be lossless for those
  fields, and that report output already sorts every map it prints (so JSON
  map-key ordering is irrelevant to byte-identity).
- `cmd/corpus-replay/replayall_test.go` — the three existing `replayAll` tests
  and the byte-identity-across-workers pattern to mirror. Its 4 `replayAll` call
  sites all take the new `cache` arg (pass `nil`).
- `cmd/corpus-replay/main_test.go:12-35` — the fixture-driven `replayCast` test
  shape to mirror for the cache tests.
- `cmd/corpus-sampler/main.go:526-530` — `hashGrid`: the `hex.EncodeToString(sha256…)`
  idiom to mirror for the version hash. Line 379 shows the
  `sha256.Sum256([]byte(name + "\x00" + strconv.Itoa(idx)))` single-string key idiom.
- `cmd/corpus-sampler/promote.go:81` and `:242` — `os.WriteFile(path, data, 0o644)`
  idiom for the cache store.
- `Makefile:52-53` — the `corpus-replay` target. Confirms the tool runs from the
  repo root, so `pkg/tuidriver` and `cmd/corpus-replay` are reachable by relative
  path at runtime (the source-dir hash walk relies on this).
- `cmd/corpus-replay/testdata/*.cast` — the four existing fixtures
  (`sample-ok`, `stable-ok`, `flap-err`, `forgery-err`), reusable as the synthetic
  corpus for every cache test. No new fixtures needed.

## Context

`corpus-replay` replays the whole `.cast` corpus on every run. A cast's replay
result is a pure function of the cast bytes, the sampling stride, and the
detection code — and recordings are immutable — so on a repeat run over an
unchanged corpus with unchanged code, every result is recomputed identically for
nothing. #260 parallelised the pass across cores; the residual cost on repeat
runs is now redundant recomputation, not lack of concurrency.

A per-cast result cache removes it: the routine case (a handful of new
recordings, unchanged detectors) costs seconds, and a full re-replay fires
exactly when detection code changes — which is when it is wanted. This is the
groundwork a future `-assert` gate (#259) and casual regression checks need to
run without ceremony.

Report-only local audit tool. The cache stores classified per-cast results a
human reads; nothing is routed on them. Not security-sensitive.

## Design

One new file, `cmd/corpus-replay/cache.go`, plus edits to `main.go`. All new
identifiers are unexported (package-internal to `main`).

### The result is a pure function of three inputs

`castResult` is determined entirely by:

1. **the cast bytes** — immutable per recording;
2. **the resolved stride** — the classifier sampling rate;
3. **the detection code** — which spans *two* source trees:
   `pkg/tuidriver` (`IsIdle`, `DetectModalClass`, the buffer, …) **and**
   `cmd/corpus-replay` itself (`contentAnchors`, `segmentOf`, `classify`,
   `parseOutputEvent`, `tagFromName`).

The cache key is a content-addressed hash of all three. Any change to any input
produces a different key → a miss → a fresh replay. This is the entire
correctness argument, and it is why the version hash covers **both** source trees
(see the note below).

### Version hash — the detector-version key

```go
// versionHash returns hex(sha256) folding in every *.go file under each dir,
// sorted by path, name-then-content. Deterministic; any edit flips it.
var versionSourceDirs = []string{"pkg/tuidriver", "cmd/corpus-replay"}
func versionHash(dirs ...string) (string, error)
```

Behaviour contract:
- Walk each dir recursively (`filepath.WalkDir`), collect every `*.go` file
  (tests included — see below), collect their paths, **sort the full path list**.
- Fold into one `sha256.New()`: for each file, write the repo-relative path, a
  `\x00` separator, the file content, a `\x00` separator. Path-in-hash guards
  against a content swap between two files; the separator guards boundary
  ambiguity. Return `hex.EncodeToString(h.Sum(nil))`.
- Computed **once** at startup, stored read-only on the cache. Never recomputed
  per cast.

Two deliberate decisions, both stated so the developer does not second-guess them:

- **Cover `cmd/corpus-replay` too, not only `pkg/tuidriver`.** The ticket's
  Technical Notes point at `pkg/tuidriver`, but `castResult.anchors`/`.segment`
  and the classify logic live in `main.go`. Keying on only `pkg/tuidriver` would
  serve stale results after an edit to `contentAnchors` or `segmentOf` — a silent
  correctness bug. Including both trees is one extra dir in the walk and closes
  the gap. The ticket's AC3 test (edit a `pkg/tuidriver` source → whole cache
  invalidates) still passes: it is a subset of "edit any covered source".
- **Include `*_test.go` files (no filter).** Over-invalidation (a test edit
  re-replays the corpus) is safe and self-correcting; under-invalidation serves
  stale results. Filtering adds a branch and a judgement call about "which files
  are detectors". Simplicity + conservative correctness wins. Reading a source
  file's bytes to hash it does not render anything, so the ⚠️ self-reference
  caution (anchors quoted in these files) does not apply.

### Cache key — per (version, stride, cast)

```go
// key hashes everything the result depends on into one filename stem.
func (c *resultCache) key(castName, contentHash string, stride int) string
```

`key = hex(sha256(version + "\x00" + strconv.Itoa(stride) + "\x00" + castName +
"\x00" + contentHash))`, mirroring the `hashGrid` idiom.

- `version` folds in the detector version → any source edit changes every key →
  whole-cache invalidation (AC3).
- `stride` folds in the **resolved** stride (after the `< 1 → 1` clamp) → a
  stride-16 entry can never be served to a stride-1 lookup (AC5). Because a
  future `-assert` forces stride to 1, its lookups key on stride=1 and structurally
  cannot hit a stride-16 entry — the safety falls out, with **no** reference to
  `-assert` anywhere in this code.
- `castName` folds in the base filename → `tag` (derived from the name) is part
  of the identity, so two casts with identical bytes but different names never
  collide.
- `contentHash` folds in the cast bytes → immutability is enforced, not assumed;
  adding a new cast yields a new key (miss) while every existing cast keeps its
  key (hit) — AC3's "only the new cast replays".

`contentHash` is a stream hash of the cast file:

```go
// hashCastContent streams the file through sha256 (no full-file buffering).
func hashCastContent(path string) (string, error)
```

Open the file, `io.Copy(h, f)`, return `hex(h.Sum(nil))`. On a hit this is the
**only** file access — the expensive `replayCast` is skipped entirely, satisfying
"no replay is performed". Hashing is O(bytes) I/O and negligible against a replay
(per-event JSON decode + rolling buffer + N detectors).

### Serializable mirror

`castResult` has unexported fields (maps included), so it cannot be JSON-marshalled
directly, and renaming its fields to exported would cascade through `main.go` and
`report.go`. Instead, a tagged mirror lives beside it:

```go
type cacheEntry struct {
    Name, Tag, Segment string
    Cols, Rows, Events int
    Fired, Anchors     map[string]bool
    Edges              map[string]int
}
func toCacheEntry(r castResult) cacheEntry
func (e cacheEntry) toResult() castResult
```

(Add `json:"…"` tags per field.) The three maps round-trip losslessly through
`encoding/json`. `report()` sorts every map before printing, so JSON's sorted
map-key marshalling has no bearing on output — byte-identity depends only on map
*contents* being preserved, which they are. An empty vs nil map is
report-indistinguishable (both range zero times), so no special-casing.

### Store / load

```go
func (c *resultCache) load(key string) (castResult, bool) // miss on absent-or-corrupt
func (c *resultCache) store(key string, r castResult) error // best-effort
```

- `load`: read `<dir>/<key>.json`; on any error (missing file, unmarshal
  failure) return `(castResult{}, false)` — a corrupt entry is a miss, so the
  cache self-heals by re-replaying and overwriting.
- `store`: `os.WriteFile(<dir>/<key>.json, json.Marshal(toCacheEntry(r)), 0o644)`.
  Best-effort: a store error does **not** fail the cast (the fresh result is
  valid and returned); it just means the entry is not persisted and the next run
  misses again. Errors are swallowed (the hit/miss counts already reveal a cache
  that is not warming).

### Constructor + wiring

```go
type resultCache struct {
    dir     string // cache directory, already MkdirAll'd
    version string // detector-version hash, computed once
}
func newResultCache(cacheDir string, sourceDirs ...string) (*resultCache, error)
```

- If `cacheDir == ""`, default to `filepath.Join(userCacheDir, "corpus-replay")`
  where `userCacheDir` comes from `os.UserCacheDir()`.
- `os.MkdirAll(dir, 0o755)`; then `versionHash(sourceDirs...)`. Any error →
  return it.

Two new flags in `main()`:
- `-no-cache` (bool, default false) — bypass the cache entirely (never read,
  never write; always replay).
- `-cache-dir` (string, default "") — override the cache location.

Wiring in `main()`:
- If `!*noCache`, `cache, err := newResultCache(*cacheDir, versionSourceDirs...)`.
  On error, **degrade gracefully**: print `corpus-replay: cache disabled: <err>`
  to stderr and set `cache = nil`. The tool's core value is replay + report; the
  cache is an optimisation, and degrading keeps stdout byte-identical to a
  `-no-cache` run.
- Call the extended `replayAll(paths, *stride, *workers, cache, os.Stderr)`.
- When `cache != nil`, print the hit/miss summary line to **stderr** (AC4):
  `corpus-replay: cache %d hit(s), %d miss(es)\n` from the returned `cacheStats`.
  Stdout (the report) is untouched by this line.

### The wrapper and the pool

```go
// replayOrLoad is the thin per-cast wrapper. cache==nil ⇒ replay directly.
func replayOrLoad(cache *resultCache, path string, stride int) (res castResult, hit bool, err error)
```

- `cache == nil`: `r, err := replayCast(path, stride); return r, false, err`.
- else: `h, err := hashCastContent(path)`; on error return it (the cast is
  skipped, identical to today's unreadable-cast branch). Compute
  `k := cache.key(filepath.Base(path), h, stride)`. If `r, ok := cache.load(k); ok`
  → `return r, true, nil` (HIT — no replay). Otherwise `r, err := replayCast(path,
  stride)`; on error return it; else `cache.store(k, r)` (best-effort) and
  `return r, false, nil` (MISS).

`replayAll` changes:
- Signature: `func replayAll(paths []string, stride, workers int, cache *resultCache, errOut io.Writer) ([]castResult, cacheStats)`.
- `replayOutcome` gains `hit bool`.
- Each worker calls `replayOrLoad(cache, p, stride)` instead of `replayCast`.
- The collector (single goroutine, as today) tallies:
  `cacheStats{hits, misses int}` — `hit → hits++`, non-error miss → `misses++`,
  errored cast → neither (it is a skip). Return the stats alongside results.
- Returning stats (instead of parsing a stderr string) makes AC3's "only the new
  cast replays" assert on an integer: `misses == 1`.

`cacheStats` is `struct{ hits, misses int }`; zero-valued and unused when
`cache == nil`.

## Concurrency model

Unchanged pool from #260, extended without adding any lock:

- **Distinct keys ⇒ distinct files.** `os.ReadDir` yields unique names within a
  directory, and each cast is processed by exactly one worker, so every cache
  file at `<dir>/<key>.json` is touched by at most one worker in a run — either
  load-only (hit) or store-only (miss), never both, never by two workers. No
  shared mutable state on the cache path → no mutex.
- **Version hash is read-only.** Computed once in `newResultCache`, only read
  thereafter (in `key`). No write race.
- **Hit/miss tally stays single-writer.** Only the collector goroutine reads
  `replayOutcome.hit` and mutates `cacheStats`, exactly as it already owns the
  skip-line writes and the results append. No new goroutine, no new channel.
- The `-workers 8` arm of the existing byte-identity test doubles as the `-race`
  exerciser; the cache path is exercised under it for free.

## Error handling

- **Cache init failure** (source dir absent, cache dir uncreatable) → warn to
  stderr, run with `cache = nil`. stdout identical to `-no-cache`.
- **Unreadable cast** (`hashCastContent` fails) → propagate as the cast's error;
  the collector prints the existing `skip <name>: <err>` line and omits it — same
  semantics as today's `replayCast`-open failure.
- **Corrupt/absent cache entry** → treated as a miss in `load`; re-replayed and
  overwritten. Self-healing.
- **Store failure** → swallowed; the valid fresh result is still returned and the
  cast counts as a miss.

## Testing strategy

New `cmd/corpus-replay/cache_test.go`. Reuse the four existing testdata fixtures
as the synthetic corpus — no new fixtures. Tests inject a synthetic `version`
string and a `t.TempDir()` cache dir; they never hash the real source trees
(except the `versionHash` unit test, which uses its own temp source dir), so
`make check` stays hermetic and never renders anchors.

Scenarios (write as Go test functions in the project idiom, not copied here):

- **`versionHash` determinism + edit invalidation.** Build a temp dir with two
  `.go` files. Assert `versionHash(dir)` is stable across two calls; assert it
  changes after editing one file's content; assert it changes when a file is
  added; assert swapping the *contents* of the two files (same set of contents,
  different paths) also changes the hash (proves path is folded in).
- **Cache hit.** With a `resultCache{dir: t.TempDir(), version: "v1"}`, replay a
  fixture fresh to get `r0`, `store` it under its key, then `replayOrLoad` the
  same fixture: assert `hit == true` and that the returned result reports
  identically to `r0` (compare via `report()` over a one-element slice, or
  compare the maps directly).
- **Cache miss then warm.** Fresh cache dir: first `replayOrLoad` returns
  `hit == false`; a second call returns `hit == true`.
- **Version invalidation (end-to-end).** Warm a cache (version `"v1"`) over the
  testdata corpus via `replayAll` → `stats.misses == len(paths)`,
  `stats.hits == 0`. Re-run same version → all hits. Build a *new* cache with
  version `"v2"` over the same corpus → all misses again (every key changed).
- **Stride key interaction (AC5).** Warm the cache at stride 16 over the corpus.
  Then `replayAll` at stride 1 over the same corpus with the same cache dir →
  **all misses** (the stride-16 entries do not satisfy stride-1 lookups). A
  second stride-1 run → all hits.
- **Byte-identity (headline AC2/AC6).** Over the testdata corpus, capture the
  `report()` stdout three ways: (a) `-no-cache` (`replayAll(..., nil, ...)`),
  (b) cold cache (fresh `t.TempDir`), (c) warm cache (same dir, second run).
  Assert all three stdout buffers are **byte-identical**. Additionally assert the
  cold run's stats are all-miss and the warm run's stats are all-hit — so
  "every cast is a hit and no replay is performed" is proven, not assumed.
- **Update `replayall_test.go`.** Its four `replayAll` call sites take the new
  `cache` arg (`nil`) and the second return value (`res, _ := replayAll(...)`).

`make check` (`go vet` + `go test`) must stay green.

## README

Add the two new flags to `cmd/corpus-replay/README.md`'s flag list and a short
"Caching" note: on by default, keyed by a hash of the detector sources + stride,
`-no-cache` to bypass, `-cache-dir` to relocate; a source edit re-replays the
whole corpus, a new cast replays only itself. (Doc edit only — no code.)

## Open questions

- **Orphan accumulation.** Folding the version into every key means a source edit
  orphans the previous version's cache files (they are never read again). For a
  hand-run local tool the cache dir grows slowly and is safe to delete wholesale;
  a version-named subdirectory + pruning was considered and rejected as
  unwarranted complexity. Confirm this is acceptable, or add a one-line "stale
  entries are safe to delete" note to the README.
- **Go toolchain / stdlib version is not in the key.** The result also depends on
  `encoding/json` behaviour, which the source hash does not cover. A Go version
  bump changing parse behaviour is vanishingly unlikely to alter a result; the
  binary-hash alternative that would catch it was rejected because it cannot be
  exercised by the claude-free `make check` (it needs a rebuild, not a source
  edit). Accepted residual.
```

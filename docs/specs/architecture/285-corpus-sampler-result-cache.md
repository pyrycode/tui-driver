# Spec #285 — corpus-sampler: per-cast result cache keyed by a detector-version hash

Mirrors #261 (corpus-replay's cache), adapted for the sampler's cross-cast dedup.
Read #261's spec (`docs/specs/architecture/261-corpus-replay-result-cache.md`) and
its shipped code (`cmd/corpus-replay/cache.go`, `cache_test.go`) first — this cache
is the same shape with three documented differences (§ Design).

## Files to read first

The developer's turn-1 data load. Read these before writing any code.

- `cmd/corpus-replay/cache.go` (whole, 244 lines) — **the file to mirror.**
  `versionHash`, `newResultCache`, `key`, `hashCastContent`, `load`/`store`,
  `cacheStats`, `replayOrLoad`. Copy its shape; adapt per § Design (no serializable
  mirror, a mode-signature field, single-threaded, a `{Samples,Events,Gaps}` entry).
- `cmd/corpus-replay/cache_test.go` (whole, 271 lines) — **the test file to mirror.**
  `writeGo` helper, the `versionHash` determinism/path-fold tests, hit/miss-then-hit,
  version-invalidation, only-new-cast-replays, mode-key interaction, byte-identity
  cold/warm/no-cache, corrupt-entry-is-miss. Same scenarios, sampler subjects.
- `cmd/corpus-sampler/main.go:52-80` — the `sample` struct (already fully
  `json:"…"`-tagged) + the `source*` consts. The cache entry stores a `[]sample`
  directly — **this is why no serializable mirror is needed** (the #261 catch does
  not apply here).
- `cmd/corpus-sampler/main.go:119-177` — `main()`: the flag block and the
  `castPaths → collect → writeSamples → stdout` wiring. The two new flags, the cache
  construction (with graceful degrade), the swap to `collectCached`, and the stderr
  summary line slot in here.
- `cmd/corpus-sampler/main.go:196-249` — `collect` + `mergeSources`. **`collect`'s
  signature must NOT change** (§ Design "Do not touch collect's signature" — it has
  21 call sites). Its body moves into a new `collectCached`; `collect` becomes a
  one-line delegator. The dedup loop (the `idx` map, `mergeSources`) is exactly what
  must run over the union of cached and fresh per-cast sample lists — unchanged.
- `cmd/corpus-sampler/main.go:251-439` — `sampleCast`. **Unchanged.** Its return
  tuple `(samples []sample, events, gaps int, err error)` is the exact payload the
  cache stores and reloads. The cache is a thin wrapper around it (`sampleOrLoad`),
  never a modification.
- `cmd/corpus-sampler/main.go:526-531` — `hashGrid`: the `hex(sha256(…))` idiom to
  mirror for `versionHash`, `hashCastContent`, and `key`.
- `cmd/corpus-sampler/main_test.go:9-42` — the fixture helpers `fixture`,
  `noOutputFixture`, `noFireFixture`, `midstreamFixture` over
  `cmd/corpus-sampler/testdata/*.cast`. Reuse these as the synthetic corpus for the
  cache tests — **no new fixtures needed**, and they are already git-exempt via the
  `!cmd/corpus-sampler/testdata/*.cast` negation (#255).
- `cmd/corpus-sampler/promote.go:81` and `:242` — `os.WriteFile(path, data, 0o644)`
  idiom for the cache store.
- `cmd/corpus-sampler/README.md:26-30` and `:94` — the tool is run **by hand from
  the repo root** (`go run ./cmd/corpus-sampler …`); there is deliberately **no
  Makefile target** (#255, avoiding #253). So `pkg/tuidriver` and `cmd/corpus-sampler`
  are reachable by relative path at runtime — the source-dir walk relies on this,
  same as corpus-replay.

## Context

`corpus-sampler` (#255, #256) reads every `.cast` in `-dir` on each run and
truncates `-out` with `os.Create`, so a re-run over the immutable, additive corpus
re-does the whole single-threaded pass — a full cold pass over the current corpus
(2118 recordings, 1.1 GB) took ~6 h on the fanless MacBook Air. Recordings are
immutable and only appended, so a re-run's real work is just the handful of new
recordings.

corpus-replay solved this exact shape in #261: a per-cast result cache keyed by a
detector-version hash. Editing a detector re-processes everything; adding a cast
processes only the new cast. The sampler needs the same. This closes the "sample
only new recordings" gap — the design's incremental intent currently holds only at
the labeling stage (#257, resumable by screen hash); the sampling stage still
re-does the full corpus.

**Parallelism (`-workers`) is out of scope and rejected** — the target machine is a
fanless MacBook Air that throttles under sustained multi-core load, so caching (skip
the work) is the right lever, not racing it across cores.

Report-only local audit tool. The cache stores per-cast sample results a human
reads; nothing is routed on them (same posture as corpus-replay/#261). It does not
touch the fatal trust/network detectors. **Not security-sensitive** (the ticket
carries no `security-sensitive` label; no security-review pass runs).

## Design

One new file, `cmd/corpus-sampler/cache.go`, plus edits to `main.go`. All new
identifiers are unexported (package-internal to `main`). A `package main` cannot
import another's helpers, so `versionHash`/`hashCastContent` are copied and adapted
from `cmd/corpus-replay/cache.go` — the same established pattern by which the sampler
already copied `tagFromName`/`segmentOf` from corpus-replay.

### A cast's sampler result is a pure function of three inputs

`sampleCast`'s output for one cast is determined entirely by:

1. **the cast bytes** — immutable per recording;
2. **the four mode inputs** — `-gap` (resolved to seconds), `-final`, `-fires`,
   `-midstream`; they change *what a cast yields* (a `-fires`-on run emits fire
   samples a `-fires`-off run does not), so a `-fires`-on entry must never serve a
   `-fires`-off lookup;
3. **the detection + sampling code** — spanning *two* source trees: `pkg/tuidriver`
   (`IsIdle`, `DetectModalClass`, the buffer, `Render`, …) **and** `cmd/corpus-sampler`
   itself (`normalize`, `hashGrid`, `activeKeys`/`flatDetectors`, the gap/final/fires/
   midstream logic in `sampleCast`, `segmentOf`, `tagFromName`).

The cache key is a content-addressed hash of all three. Any change to any input
yields a different key → a miss → a fresh sample. This is the entire correctness
argument, and it is why the version hash covers **both** source trees.

### Version hash — the detector-version key

Copy `versionHash(dirs ...string) (string, error)` from `cmd/corpus-replay/cache.go`
**verbatim** (walk each dir, collect every `*.go` sorted by path, fold
path+`\x00`+content+`\x00` into one `sha256`, return hex). Only the source-dir list
changes:

```go
var versionSourceDirs = []string{"pkg/tuidriver", "cmd/corpus-sampler"}
```

Two deliberate decisions, stated so the developer does not second-guess them (both
identical to #261's rationale):

- **Cover `cmd/corpus-sampler` too, not only `pkg/tuidriver`.** The sampler's own
  logic (`normalize`, `hashGrid`, `activeKeys`, the sampling rules, `segmentOf`,
  `tagFromName`) all determine what a cast yields. Keying on `pkg/tuidriver` alone
  would serve stale results after an edit to any of those — a silent correctness
  bug. The ticket's AC (edit any detector source → whole cache invalidates) is a
  subset of "edit any covered source".
- **Include `*_test.go` and `promote.go` (no filter).** Over-invalidation (a test
  or promote edit re-samples the corpus) is safe and self-correcting; a "which files
  are detectors" filter risks under-invalidation — serving a stale result. Reading a
  source file's bytes to hash it renders nothing on screen, so the ⚠️ self-reference
  caution (anchors quoted in these files) does not apply.

Note there is **no feedback loop** from covering `cmd/corpus-sampler`: cache *files*
live under `os.UserCacheDir()`, not under the source tree, so the new `cache.go`
being hashed only means editing it invalidates the cache (over-invalidation, safe).

### Mode signature — folding the four flags

Unlike #261 (which folds a single resolved stride, passed per-call to `key`), the
sampler has four run-level mode inputs. Fold them once, at construction, into a
`mode` field on the cache (they are constant for a whole run, so precomputing mirrors
how `version` is precomputed once):

```go
// deterministic fold of the four resolved mode inputs; any change → a different
// string → a different key for every cast. Built in newSampleCache.
mode := strconv.FormatFloat(gap, 'g', -1, 64) + "|" +
        strconv.FormatBool(final) + "|" + strconv.FormatBool(fires) + "|" +
        strconv.Itoa(midstream)
```

`gap` is folded as its **resolved** `gap.Seconds()` value (the float64 already passed
to `collect`/`sampleCast`), mirroring #261's "fold the resolved stride".

### Cache struct + constructor

```go
type sampleCache struct {
    dir     string // cache directory, already MkdirAll'd
    version string // detector-version hash, computed once
    mode    string // mode-signature fold, computed once
}
func newSampleCache(cacheDir string, gap float64, final, fires bool, midstream int, sourceDirs ...string) (*sampleCache, error)
```

- Empty `cacheDir` defaults to `filepath.Join(userCacheDir, "corpus-sampler")` where
  `userCacheDir` comes from `os.UserCacheDir()` (mirror #261; note the `corpus-sampler`
  basename, distinct from corpus-replay's own dir).
- `os.MkdirAll(dir, 0o755)`; then `versionHash(sourceDirs...)`; then build `mode`.
  Any error → return it.
- `version` and `mode` are read-only after construction.

### Cache key — per (version, mode, cast)

```go
func (c *sampleCache) key(castName, contentHash string) string
```

`key = hex(sha256(version + "\x00" + mode + "\x00" + castName + "\x00" + contentHash))`,
mirroring the `hashGrid`/#261-`key` idiom. Each component's role:

- `version` → any detector-source edit changes every key → whole-cache invalidation.
- `mode` → a `-fires`-on / `-gap 1s` / … entry can never satisfy a differently-moded
  lookup.
- `castName` → the name-derived `tag` (and `segment`, via content, folded below) is
  part of identity; two casts with identical bytes but different names never collide.
- `contentHash` → enforces immutability: a new cast is a new key (miss) while every
  existing cast keeps its key (hit).

`hashCastContent(path string) (string, error)` is copied verbatim from #261 (stream
the file through `sha256` via `io.Copy`, return hex). On a hit this is the **only**
file access — the expensive `sampleCast` (per-event JSON decode + rolling buffer +
render + detectors) is skipped entirely.

### Cache entry — no serializable mirror needed

**The one place this is simpler than #261.** corpus-replay's `castResult` has
unexported fields, forcing a tagged `cacheEntry` mirror + `toCacheEntry`/`toResult`
round-trip. The sampler's `sample` type is **already fully `json:"…"`-tagged** (it is
marshalled straight to `-out`), so the entry is a plain struct wrapping a `[]sample`
plus the two aggregate counters:

```go
type sampleCacheEntry struct {
    Samples []sample `json:"samples"`
    Events  int      `json:"events"`
    Gaps    int      `json:"gaps"`
}
```

The `Events`/`Gaps` fields are **load-bearing**: without them a warm run's
`casts=… events=… gaps=… distinct=…` aggregate would drift from a cold run and break
the byte-identity AC. They store exactly what `sampleCast` returned for that cast
(§ wrapper below), so a hit folds them into the aggregate identically to a fresh
sample.

**Byte-identity of the round-trip.** `Samples` stores each `sample` exactly as
`sampleCast` returns it — `Seen` left `0` (collect owns `Seen`), `Tag`/`Segment`
stamped, `Source` already sorted (`sampleCast` sorts fire `Source` before appending).
`encoding/json` round-trips these losslessly: strings (incl. `Grid`'s control bytes)
via string escaping, the sorted `Source []string` in order, and each `TS float64`
via the shortest-round-trippable representation (`float64 → JSON → float64` is the
identity for finite values). So the `sample` fed to the final `-out` marshal is
bit-identical whether it came from a fresh `sampleCast` (cold: one marshal) or from
the cache (warm: an extra marshal-into-cache + unmarshal-from-cache before the `-out`
marshal). The extra round-trip cannot perturb the bytes.

### Store / load

```go
func (c *sampleCache) load(key string) (sampleCacheEntry, bool) // miss on absent-or-corrupt
func (c *sampleCache) store(key string, e sampleCacheEntry) error // best-effort
```

- `load`: read `<dir>/<key>.json`; on any error (missing file, unmarshal failure)
  return `(sampleCacheEntry{}, false)` — a corrupt entry is a miss, so the cache
  self-heals by re-sampling and overwriting.
- `store`: `os.WriteFile(<dir>/<key>.json, json.Marshal(e), 0o644)`. Best-effort: a
  store error does **not** fail the cast (the fresh result is valid and already
  returned); it just means the entry is not persisted and the next run misses again.
  Swallowed at the call site (the hit/miss counts already reveal a cache that is not
  warming).

### The wrapper

```go
// sampleOrLoad is the thin per-cast wrapper around sampleCast. cache==nil ⇒ sample
// directly (hit always false). A content-hash failure propagates as the cast's error
// — the same skip semantics collect already applies to an unreadable cast.
func sampleOrLoad(cache *sampleCache, path string, gap float64, final, fires bool, midstream int) (samples []sample, events, gaps int, hit bool, err error)
```

- `cache == nil`: return `sampleCast(path, gap, final, fires, midstream)` with
  `hit=false`.
- else: `h, err := hashCastContent(path)`; on error return it (cast skipped, as
  today). `k := cache.key(filepath.Base(path), h)`. If `e, ok := cache.load(k); ok`
  → return `e.Samples, e.Events, e.Gaps, true, nil` (**HIT — `sampleCast` not
  called**). Otherwise `s, ev, gp, err := sampleCast(…)`; on error return it; else
  `_ = cache.store(k, sampleCacheEntry{s, ev, gp})` (best-effort) and return
  `s, ev, gp, false, nil` (MISS).

### Do NOT change `collect`'s signature — delegate instead

`collect` has **21 call sites** across `main_test.go` and `promote_test.go`. Changing
its signature would cascade an edit through all of them — over the 10-call-site
budget and exactly the #29 refactor-cascade failure mode. Freeze it. Its current body
moves into a new cache-aware `collectCached`; `collect` becomes a one-line delegator:

```go
// unchanged 5-arg / 5-return signature — all 21 existing callers compile untouched.
func collect(paths []string, gap float64, final, fires bool, midstream int) ([]sample, int, int, int, error) {
    distinct, casts, events, gaps, _, err := collectCached(paths, gap, final, fires, midstream, nil)
    return distinct, casts, events, gaps, err
}

// collectCached is collect's body plus the cache. It swaps the single sampleCast
// call for sampleOrLoad and tallies hits/misses; the cross-cast dedup (the idx map +
// mergeSources) is byte-for-byte the existing loop, now running over the union of
// cached and fresh per-cast sample lists.
func collectCached(paths []string, gap float64, final, fires bool, midstream int, cache *sampleCache) (distinct []sample, casts, events, gaps int, stats cacheStats, err error)
```

Inside `collectCached`, for each path: call `sampleOrLoad(cache, p, …)`; on error
`skip` with the existing stderr note and `continue` (neither hit nor miss); else
`casts++`, `events += ev`, `gaps += gp`, tally `stats` (`hit → stats.hits++`, else
`stats.misses++`), and run the **unchanged** dedup loop over the returned `samples`.

**Why the union dedup "just works":** the dedup already lives in `collect` (cross-cast,
via the `idx` hash map) and `sampleCast` returns the *pre-dedup* per-cast list. Caching
that pre-dedup list and feeding it through the same loop means a screen a cached cast
contributes still dedups against an identical screen in a fresh cast, and `mergeSources`
still unions finders across casts — because every cast's list, cached or fresh, flows
through the one `idx` map. This is the ticket's "one real difference from corpus-replay"
(the sampler dedups across casts; corpus-replay's per-cast result is independent), and
it costs nothing beyond storing and reloading the list `sampleCast` already produces.

`cacheStats` is `struct{ hits, misses int }`, copied from #261; zero-valued and unused
when `cache == nil`.

### Wiring in `main()`

Two new flags (mirror #261 exactly):
- `-no-cache` (bool, default false) — bypass the cache entirely (never read, never
  write; always sample).
- `-cache-dir` (string, default "") — override the cache location.

Then:
- If `!*noCache`: `cache, err := newSampleCache(*cacheDir, gap.Seconds(), *final, *fires, *midstream, versionSourceDirs...)`.
  On error, **degrade gracefully**: `fmt.Fprintf(os.Stderr, "corpus-sampler: cache disabled: %v\n", err)` and set
  `cache = nil`. Sampling + `-out` are the tool's core value; the cache is an
  optimisation, and degrading keeps stdout/`-out` byte-identical to a `-no-cache` run.
- Call `collectCached(paths, gap.Seconds(), *final, *fires, *midstream, cache)`
  instead of `collect(...)`.
- After `writeSamples`, when `cache != nil`, print the hit/miss summary to **stderr**
  (AC4): `fmt.Fprintf(os.Stderr, "corpus-sampler: cache %d hit(s), %d miss(es)\n", stats.hits, stats.misses)`.
  The stdout aggregate line and `-out` are otherwise unchanged from a no-cache run.

Promote mode (`-promote`) is disjoint from the sampling path and is **not** touched.

## Concurrency model

**Single-threaded — simpler than #261, which had the #260 worker pool.** The sampler
processes casts sequentially in one goroutine (`collectCached`'s loop), so:

- Each cache file at `<dir>/<key>.json` is touched by exactly one goroutine, at most
  once per run (load-only on a hit, store-only on a miss). No shared mutable cache
  state, no mutex — trivially, because there is only one worker.
- `version`/`mode` are computed once in `newSampleCache` and read-only thereafter.
- The hit/miss tally is owned by the single loop.

`-workers` parallelism is explicitly **not added** (fanless Air throttles under
sustained all-core load — see § Context).

## Error handling

- **Cache init failure** (source dir absent, cache dir uncreatable, `UserCacheDir`
  fails) → warn to stderr, run with `cache = nil`. stdout + `-out` identical to
  `-no-cache`.
- **Unreadable cast** (`hashCastContent` fails, or `sampleCast`'s own open fails) →
  propagated as the cast's error; `collectCached` prints the existing
  `skip <name>: <err>` line and omits it — the cast counts as neither hit nor miss,
  exactly as today.
- **Corrupt/absent cache entry** → treated as a miss in `load`; re-sampled and
  overwritten. Self-healing.
- **Store failure** → swallowed; the valid fresh result is still returned and the
  cast counts as a miss.

## Testing strategy

New `cmd/corpus-sampler/cache_test.go`, mirroring `cmd/corpus-replay/cache_test.go`.
Reuse the four existing testdata fixtures (`stable-sample-ok`, `no-output-ok`,
`no-fire-ok`, `midstream-ok`) as the synthetic corpus — **no new fixtures**. Tests
inject a synthetic `version`/`mode` via a directly-constructed
`&sampleCache{dir: t.TempDir(), version: "v1", mode: "m1"}` and never hash the real
source trees (except the `versionHash` unit test, which builds its own temp source
dir), so `make check` stays hermetic and renders no anchors.

Write these as Go test functions in the project idiom (bullet scenarios, not copied
code):

- **`versionHash` determinism + invalidation + path-fold.** Copy #261's two
  `versionHash` tests verbatim (temp dir of `.go` files): stable across calls; flips
  on edit; flips on add; flips on swapping two files' contents (proves path is folded
  in).
- **`sampleOrLoad` hit.** Fresh-sample a fixture to `(s0, ev0, gp0)`; `store` under
  its key; `sampleOrLoad` the same fixture → assert `hit == true` and the returned
  `(samples, events, gaps)` equal `(s0, ev0, gp0)` (compare via a small helper that
  marshals the sample slice, or field-compare — reference sample *hashes*, never grid
  content).
- **Miss then hit.** Fresh cache dir: first `sampleOrLoad` → `hit == false`; second →
  `hit == true`.
- **Version invalidation (end-to-end, AC2).** Warm the corpus via
  `collectCached(paths, …, c1)` where `c1.version == "v1"` → `stats.misses == casts`,
  `stats.hits == 0`. Re-run same cache → all hits. New cache `c2.version == "v2"`,
  same dir → all misses again.
- **Mode invalidation (AC2).** Warm with `c1.mode == "m1"`; a second cache same
  `dir`/`version` but `mode == "m2"` → all misses (proves a mode-flag change
  invalidates every cast).
- **Only new cast re-samples (AC2).** Warm every cast but the last; re-run the full
  set → `stats.misses == 1`, `stats.hits == casts-1`.
- **Byte-identity cold/warm/no-cache (headline AC3).** Over the corpus, capture three
  ways: (a) `-no-cache` (`collectCached(…, nil)`), (b) cold cache (fresh `t.TempDir`),
  (c) warm cache (same dir, second run). Assert the **`-out` bytes** (marshal the
  returned `distinct` via the same path `writeSamples` uses, into a buffer) **and**
  the stdout aggregate tuple `(casts, events, gaps, len(distinct))` are byte-identical
  across all three. Additionally assert cold = all-miss and warm = all-hit, so "every
  cast is a hit and no cast is re-sampled" is proven, not assumed.
- **Corrupt entry is a miss.** Write `{not json` at a cast's key; assert `load`
  returns `ok == false` and `sampleOrLoad` re-samples past it (`hit == false`,
  `err == nil`).

`make check` (`go vet` + `go test`) must stay green. The existing `collect(...)` call
sites in `main_test.go`/`promote_test.go` are **untouched** (the delegator preserves
the signature).

## README

Add the two new flags to `cmd/corpus-sampler/README.md`'s flag list and a short
"Caching" note: on by default, keyed by a hash of the detector sources
(`pkg/tuidriver` + `cmd/corpus-sampler`) plus the mode flags (`-gap`/`-final`/
`-fires`/`-midstream`); `-no-cache` bypasses, `-cache-dir` relocates; a source or
mode-flag change re-samples the whole corpus, a new recording samples only itself;
stale entries under the cache dir are safe to delete wholesale. (Doc edit only — no
code.)

## Open questions

- **Orphan accumulation.** Folding version + mode into every key means a source edit
  or a flag change orphans the previous key's cache files (never read again). For a
  hand-run local tool the cache dir grows slowly and is safe to delete wholesale; a
  version/mode-named subdirectory + pruning was considered and rejected as unwarranted
  complexity (identical to #261's resolution). The README note above states "safe to
  delete"; confirm that is sufficient.
- **Cache entry size.** Each entry stores the cast's full pre-dedup `[]sample`,
  including raw `Grid` strings — larger than corpus-replay's per-cast entry (which
  stores only counts + small maps). For a local tool over a 1.1 GB corpus this is
  bounded (a handful of samples per cast, one file per cast) and dwarfed by the
  recordings themselves; accepted. Flagged only so it is a conscious choice, not a
  surprise.
- **Go toolchain / stdlib version is not in the key.** Same accepted residual as
  #261: the result also depends on `encoding/json` / `regexp` behaviour, which the
  source hash does not cover. A Go bump altering a result is vanishingly unlikely, and
  the binary-hash alternative that would catch it cannot be exercised by the
  claude-free `make check`.

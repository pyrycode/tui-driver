# Spec: corpus-replay — parallelize cast replay across cores (#260)

## Files to read first

- `cmd/corpus-replay/main.go:109-146` — `main()`: the flag block and the **sequential replay loop** (lines 135-143) that this ticket replaces. This is the only seam that changes.
- `cmd/corpus-replay/main.go:174-243` — `replayCast(path, stride)`: confirm it is a **pure function** over `(path, stride)` — opens its own file, allocates its own `tuidriver.NewBuffer` and `strings.Builder`, owns its own `castResult` maps, returns them. No package-level or shared mutable state. This purity is the entire licence for parallelising.
- `cmd/corpus-replay/report.go:87-104` — `report()` signature and the header lines; unchanged by this ticket.
- `cmd/corpus-replay/report.go:132-243` — the **order-dependent** report sections: false-positive-suspect `hits` (`first(hits,3)`), the `-per-cast` table (`for _, r := range results`). These walk `results` in slice order, which is *why* `results` must be sorted by cast name before `report()`. (Note: `bucketFlappers` at `report.go:55-77` re-sorts internally, so it is already order-independent — the sort-by-name fixes the other two.)
- `cmd/corpus-replay/report_test.go:13-32` — the `report(&buf, []castResult{...}, dir, stride, perCast)`-into-a-`bytes.Buffer` + `strings.Contains` pattern the new byte-identical test mirrors.
- `cmd/corpus-replay/main_test.go:12-100` — existing fixtures (`sample-ok.cast`, `forgery-err.cast`, `flap-err.cast`, `stable-ok.cast`) and test style; the new tests reuse these fixtures via `castPaths("testdata", "")`.
- `cmd/corpus-replay/README.md:24-32` — the flags list; add a `-workers` bullet (see § Docs).

## Context

A full-fidelity corpus pass is single-threaded today: `main()` loops over sorted `paths` calling `replayCast` one at a time (`main.go:135-143`). At ~1.3ms/classify-tick over ~4.5M events that extrapolates to ~100 minutes, which keeps the assert-mode gate from being routine. `replayCast` is already independent per cast (pure over path+stride, no shared state), so the pass parallelises trivially. This ticket confines the change to `main()`'s replay loop plus one new flag — no detector, buffer, or report logic changes.

Sibling of the corpus-replay tool family (harness #227, transition-edges #247). Not `security-sensitive` (confirmed: no such label; a report-only audit tool — a forged count routes no grant/answer). Security-review step skipped.

## Design

### 1. New flag

Add to the flag block in `main()`:

```go
workers := flag.Int("workers", runtime.NumCPU(), "number of concurrent cast-replay workers")
```

- Default `runtime.NumCPU()` means `make corpus-replay` parallelises with **no Makefile change** (AC: default from available CPU count).
- Clamp defensively after `flag.Parse()`, mirroring the existing `*stride < 1` clamp: `if *workers < 1 { *workers = 1 }`. `runtime.NumCPU()` is always ≥ 1, so the clamp only guards an explicit `-workers 0`/negative.
- New import: `runtime` (and `sync` for the pool). `flag.Int` auto-prints the default in `-h`; the shown number varies by machine — cosmetic, acceptable.

### 2. Extract a testable worker-pool function

The parallelism must be reachable from a test without going through `main()` (which owns `os.Args`/`os.Exit`). Extract the replay loop into:

```go
// replayAll replays every path through the detectors using `workers` concurrent
// workers and returns the results sorted by cast name — so report output is
// identical across worker counts. A cast that fails to replay prints one skip
// line to errOut and is omitted from the results (unchanged from the sequential
// path; stderr ordering under parallelism is out of scope for byte-identity).
func replayAll(paths []string, stride, workers int, errOut io.Writer) []castResult
```

`main()` collapses to: `castPaths` → `results := replayAll(paths, *stride, *workers, os.Stderr)` → `report(os.Stdout, results, *dir, *stride, *perCast)`. The `len(paths) == 0` guard stays in `main()` before the call. New import in `main.go`: `io` (for the `errOut io.Writer` param) alongside `runtime`, `sync`.

### 3. Concurrency model

Streaming worker pool with a single collector. One unexported outcome type carries a per-cast result or error across the channel:

```go
type replayOutcome struct {
    path string
    res  castResult
    err  error
}
```

Goroutine topology inside `replayAll`:

```
   paths (sorted by castPaths)
        │  feeder goroutine: for _, p := range paths { jobs <- p }; close(jobs)
        ▼
   jobs (chan string)
        │
   ┌────┴──── … ────┐   N = workers goroutines, each:
   ▼                ▼     for p := range jobs {
 worker 0    …   worker N-1   out <- replayOutcome{p, replayCast(p, stride)…} }
   │                │
   └────┬──── … ────┘
        ▼
   out (chan replayOutcome)
        │  closer goroutine: wg.Wait(); close(out)
        ▼
   collector (replayAll's own goroutine — the SINGLE reader of out):
        for o := range out {
            o.err != nil ? Fprintf(errOut, "corpus-replay: skip %s: %v", base(o.path), o.err) : append
        }
        sort results by name; return
```

- **Single reader of `out`** = the `replayAll` goroutine itself. Because only one goroutine ever writes `errOut` and appends to `results`, no mutex is needed and skip lines never interleave mid-byte.
- **Ownership transfer:** each `castResult` (and its maps) is created and mutated by exactly one worker, then handed off via the channel; after the send the collector is the sole owner. No concurrent map access → `-race` clean.
- **Channels unbuffered** is correct and simplest (feeder and collector run concurrently with the workers, so no side blocks indefinitely — see shutdown/deadlock note). Buffering `jobs`/`out` to `len(paths)` is an acceptable alternative but not required.

**Shutdown sequence:** feeder drains `paths` → `close(jobs)` → each worker finishes its in-flight cast, sees `jobs` closed, exits its `range`, calls `wg.Done()` → closer's `wg.Wait()` returns → `close(out)` → collector's `range out` ends → sort → return. No goroutine leak.

**Deadlock-freedom:** the collector reads `out` continuously and the workers read `jobs` continuously, so neither the workers (sending to `out`) nor the feeder (sending to `jobs`) can block permanently. The feeder must be its own goroutine (not inline before collection) — otherwise workers would fill `out` with no reader and wedge.

### 4. Ordering — the byte-identical guarantee (AC3)

`castPaths` already `sort.Strings`es the paths, and within a single directory sorting by full path equals sorting by base name — so the pre-existing sequential output was already in name order. Under parallelism, casts *complete* in arbitrary order, so `replayAll` **re-sorts `results` by `r.name` before returning**:

```go
sort.Slice(results, func(i, j int) bool { return results[i].name < results[j].name })
```

`os.ReadDir` yields unique names within one directory, so cast names are unique → the comparator is a strict total order and no secondary tie-break is needed; `sort.Slice` (non-stable) is sufficient and deterministic. This restores exactly the sequential (`-workers 1`) order, which is the baseline AC3 preserves. `report()` is untouched: feeding it a name-sorted slice makes every order-dependent section (per-cast table, false-positive `hits`) deterministic regardless of worker count.

## Error handling

- Per-cast replay error → collector prints the existing skip line to `errOut` and omits the cast (identical semantics to `main.go:138-141`). Relative ordering of skip lines under parallelism is explicitly out of scope for the byte-identical criterion (stderr is not report output).
- `-workers` ≤ 0 → clamped to 1 (sequential).
- Empty `paths` (guarded in `main` already, but `replayAll` is still safe): feeder closes `jobs` immediately, workers exit, collector returns an empty slice.
- `workers > len(paths)`: surplus workers read from a soon-closed `jobs`, get nothing, exit. Harmless.

## Testing strategy

Bullet scenarios (developer writes them in the project's `_test.go` idiom; reuse the four `testdata` fixtures via `castPaths("testdata", "")`):

- **Byte-identical across worker counts (AC3, the headline test).** Gather `paths, _ := castPaths("testdata", "")`. Run `r1 := replayAll(paths, 1, 1, io.Discard)` and `rN := replayAll(paths, 1, 8, io.Discard)`. Render each into its own `bytes.Buffer` via `report(&buf, rX, "testdata", 1, true)` — use `perCast=true` so the most order-sensitive section (the per-cast table) is exercised. Assert the two buffers are **byte-equal** (`bytes.Equal` / `buf1.String() == buf2.String()`). With workers=8 completing four distinct-named casts in arbitrary order, this fails if the sort is dropped and passes when present. This test doubles as the `-race` exerciser (AC4): its workers=8 arm runs the pool concurrently.
- **Ordering determinism (supports AC2).** `res := replayAll(paths, 1, 8, io.Discard)`; assert `res` is non-empty and `res[i].name <= res[i+1].name` for all `i`. Localises a sort regression before it surfaces as a byte diff.
- **Skip path drops errors and continues.** Call `replayAll([]string{"testdata/sample-ok.cast", "testdata/does-not-exist.cast"}, 1, 4, &errBuf)` with a `bytes.Buffer` for `errBuf`. Assert `len(results) == 1` (only `sample-ok`), and `errBuf` contains `skip` + `does-not-exist`. No new fixture needed — the bogus path makes `replayCast`'s `os.Open` fail. Proves the error branch is wired and non-fatal under the pool.
- **`-race` (AC4).** `go test -race ./cmd/corpus-replay/...` must be clean; the byte-identical test's concurrent arm provides the coverage. No dedicated test beyond running the suite under `-race`.
- The four **existing** `replayCast`/`report` tests are unchanged — `replayCast`'s signature and behaviour do not move; only `main()`'s loop is extracted.

## Wall-clock before/after (AC5) — hand-run bullet

AC5 asks the PR description to record a wall-clock before/after. **This is a hand-run measurement, not a `make check` gate**, and cannot come from the 4-tiny-cast `testdata` (finishes in microseconds — parallelism won't register). Two honest ways to produce a real number, both self-contained (no external corpus, **no live claude** — running corpus-replay replays static `.cast` files through detectors; there is no claude session involved):

1. **Synthetic corpus (reproducible in the sandbox).** Duplicate the fixtures into a temp dir to get measurable work, then time both worker counts **with stdout discarded** (do not display the report — see self-reference below):
   ```sh
   TMP=$(mktemp -d)
   for i in $(seq 1 400); do for c in cmd/corpus-replay/testdata/*.cast; do cp "$c" "$TMP/$i-$(basename "$c")"; done; done
   time go run ./cmd/corpus-replay -dir "$TMP" -workers 1 >/dev/null
   time go run ./cmd/corpus-replay -dir "$TMP" -workers "$(getconf _NPROCESSORS_ONLN)" >/dev/null
   ```
   Record the two `real` times in the PR description. Bump the multiplier until `-workers 1` takes a few seconds so the ratio is meaningful.
2. **Operator's real recorder dir** (`~/.local/share/pyry-recordings`) if the operator runs it by hand — the truest number, but external to the sandbox.

Either satisfies AC5. The committed *code* deliverable (pool + flag + sort + tests) is fully verifiable in `make check`; the wall-clock line is filled in from the hand-run above.

**⚠️ Self-reference (load-bearing, carried from #152/#154/#155/#227/#247).** The tool's source and `testdata` quote detector anchors (`"Quick safety check"`, `"Do you want to proceed"`), and the tool prints matched content. Build and run it **by hand**. When measuring wall-clock, **redirect the report stdout to `/dev/null`** (as above) — never render the report or `cat` the source/testdata onto a live claude screen, or the recorder captures an anchor and false-fires live detection.

## Docs

Add one bullet to `cmd/corpus-replay/README.md` flags list (after `-stride`, `README.md:29`):

- `-workers N`: number of concurrent replay workers (default: the machine's CPU count). Casts replay independently, so a full corpus pass scales roughly linearly with cores; `-workers 1` is the sequential baseline. Report output (stdout) is byte-identical across worker counts.

The README is the tool's own in-tree usage doc (lives under `cmd/corpus-replay/`, not `docs/`), so this bullet is in scope for the developer. Do **not** add or touch any `docs/knowledge/**` file — that is the documentation phase's job.

## Open questions

None blocking. One judgement call left to the developer: whether to buffer `jobs`/`out` (to `len(paths)`) or leave them unbuffered. Unbuffered is correct and simplest; buffering is a micro-optimisation with no correctness impact. Recommend unbuffered.

## Acceptance criteria (restated for the developer)

- [ ] `-workers N` flag on `cmd/corpus-replay`, default `runtime.NumCPU()`, clamped to ≥ 1.
- [ ] Replay runs in a worker pool (`replayAll`); results aggregated after workers join and **sorted by cast name** before `report()`.
- [ ] Report stdout is **byte-identical** for `-workers 1` vs `-workers N` on the same fixture dir (regression test asserts it).
- [ ] `go test -race ./cmd/corpus-replay/...` clean.
- [ ] PR description records a wall-clock before/after (hand-run per § Wall-clock; stdout discarded).
- [ ] `README.md` `-workers` bullet added.
- [ ] `make check` green.

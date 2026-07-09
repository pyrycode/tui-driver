# #274 — corpus-sampler: mid-stream sample source

Add a `-midstream N` flag to `cmd/corpus-sampler` that emits up to N deterministically
chosen mid-stream frames per cast, so streaming-state variety that lives between output
bursts (where the quiet-gap rule rarely fires) is represented in the exemplar set for the
#257 labeling pass. Additive to the single-file binary; last child of the #256 split.

## Files to read first

- `cmd/corpus-sampler/main.go:50-77` — `sample` struct + `source` consts (`sourceGap`/`sourceFinal`/`sourceFire`). Extract: add `sourceMidstream = "midstream"` to this const block; do **not** add a field to `sample`.
- `cmd/corpus-sampler/main.go:116-153` — `main()`: flag registration and the `collect(...)` call. Extract: the `-final`/`-fires` `flag.Bool` shape to mirror for `-midstream` (an `flag.Int`), and the arg-threading into `collect`.
- `cmd/corpus-sampler/main.go:178-203` — `collect()`: signature and the single `sampleCast(...)` call site. Extract: thread the new param through; the dedupe loop (`mergeSources` union on hash collision) is reused **unchanged**.
- `cmd/corpus-sampler/main.go:246-369` — `sampleCast()`: the event walk. Extract: the **`fires` block (303-336)** is the closest structural analog — a per-event block after `buf.Append`; the **`final` block (348-360)** is the analog for post-walk emission. The mid-stream block borrows from both.
- `cmd/corpus-sampler/main.go:205-225` — `mergeSources`: the sorted-union-on-collision path AC4 requires. Reused as-is; no change.
- `cmd/corpus-sampler/main.go:417-437` — `normalize` / `hashGrid`: the dedupe-key path every sample uses. Reused as-is.
- `pkg/tuidriver/buffer.go:56-64` — `Snapshot()` returns a **fresh copy** (`make` + `copy`), so a retained snapshot is **safe to hold across later `Append`s**. This is the correctness anchor for the single-pass retention design below.
- `cmd/corpus-sampler/main_test.go:34-67` — `TestCollect_StableScreensDedupeAndCounts`: the AC1 byte-identical-when-off pin (its `[gap]`-only assertions must still hold after the param is threaded with `0`).
- `cmd/corpus-sampler/main_test.go:226-270` — `TestCollect_FireSourceEmitsWithDetectorAttribution`: the closest test analog — a new-source-surfaces-a-mid-stream-frame assertion table. Mirror its shape for the mid-stream tests.
- `cmd/corpus-sampler/testdata/stable-sample-ok.cast` — reused as the union / emit-all fixture (see Testing strategy).
- `.gitignore` (root) — `*.cast` is globally ignored; `!cmd/corpus-sampler/testdata/*.cast` un-ignores this dir. A new fixture placed there is committed automatically. `.cast` fixtures are hand-authored with `` JSON escapes (see the three existing fixtures) — no raw ESC byte, no Go helper needed.

## Context

Streaming-state variety lives mid-stream, where the quiet-gap rule (#255, the core) rarely
fires: the dot-spinner frame #243 fixed went uncounted in the original glyph census because
it appears between output bursts, not at a quiet wait-state. `-fires` (#273) surfaces frames
where a *known* detector newly fires; `-midstream` surfaces a thin, fixed sample of the
streaming frames themselves, so *unknown* variety (the next uncounted #243) reaches the
labeling pass without exploding the sample set.

Determinism is the load-bearing property: the corpus test suite depends on a stable exemplar
set, so re-runs over the same input directory must produce the identical distinct set. This
mirrors #260's parallel-replay work — byte-identity rested on a deterministic ordering, not
incidental behavior.

## Design

Single file (`cmd/corpus-sampler/main.go`). Four touch points, all mirroring the shipped
`-final`/`-fires` shape:

**1. Source const.** Add to the existing const block (main.go:73):

```go
sourceMidstream = "midstream"
```

**2. Flag.** In `main()`, register an int flag (default 0 = off) and thread it into `collect`:

```go
midstream := flag.Int("midstream", 0, "also emit up to N deterministically chosen mid-stream frames per cast (source \"midstream\")")
```

The stdout report line is **unchanged** (`casts=… events=… gaps=… distinct=…`). Mid-stream
samples fold into `distinct`; adding no stdout field is what makes AC1's byte-identical-when-off
hold automatically (N=0 ⇒ no mid-stream samples ⇒ `distinct` unchanged), and matches the
`-final`/`-fires` precedent (neither added a counter).

**3. Signature threading.** Add `midstream int` as the last positional param to both
`collect` and `sampleCast` (continuing the positional-flag convention #272/#273 established;
this is the last #256 child, so the param list does not grow further). `collect`'s dedupe
loop is untouched — the `mergeSources` union already handles a mid-stream sample whose hash
collides with an existing entry.

**4. Mid-stream selection in `sampleCast`.** Contract:

- **Eligible candidates** = every output-event index `i ∈ [0, eventCount)` in the cast. (The
  ticket blesses not excluding endpoints — dedupe folds any overlap with gap/final; the
  `midstream` source simply unions onto the existing entry.)
- **Deterministic key:** `key(i) = sha256(name + "\x00" + strconv.Itoa(i))` — a pure function
  of the cast base name and event index. No `time.Now()`, no unseeded `math/rand`, no
  dependence on processing order.
- **Selection:** the N candidates with the **smallest keys** (lexicographic byte compare of
  the 32-byte digest). Fewer than N eligible ⇒ select all (no padding, no error — AC3).
- **Frame:** the buffer rendered at event `i`'s **post-append** state
  (`tuidriver.Render(snapshotAfter(i), cols, rows)`) — the same after-append snapshot the
  `fires` source uses. `Source = []string{sourceMidstream}`, `Event = i`, `TS = ts[i]`.
- **Emission order:** selected samples appended after the walk (like `final`), **sorted by
  event index ascending** — a deterministic within-cast discovery order.

Because the retained set is exactly the N smallest keys over the eligible set, it is a pure
function of `(name, eligible indices)` and independent of insertion order — the two-run
identity (AC2/AC5b) follows structurally, not incidentally.

**Recommended implementation — single-pass, bounded retention.** During the existing walk,
after `buf.Append(data)` (guarded by `midstream > 0`), retain a slice of ≤ N candidates, each
holding `{index, ts, key, snap}` where `snap` is `buf.Snapshot()` (already a fresh copy —
buffer.go:56-64 — safe to keep across later appends). Insert when `len < N`; otherwise, if the
new key is smaller than the current max-key in the retained set, evict that max and insert.
This caps memory at ≤ N snapshots and does exactly ≤ N `Render` calls (at end). Render-on-entry
(render immediately, discard on eviction) is an acceptable alternative — determinism is
identical either way, since selection is by key alone — but it can render up to O(eligible)
frames in a pathological improving-key order, so retention is preferred.

Do **not** retain a snapshot per event unconditionally: a cast can hold tens of thousands of
output events; retention must stay bounded at ≤ N.

## Concurrency model

None. `corpus-sampler` is a single-threaded offline audit tool (one cast at a time, in
`castPaths`-sorted order). No goroutines, no channels. Determinism comes from the pure key
function and the sorted emission order, not from any synchronization.

## Error handling

- **Fewer than N eligible frames** — select and emit all of them; no padding, no error (AC3).
  Falls out of the "N smallest keys, or all if fewer than N" rule with no special case.
- **N = 0 / flag off** — no mid-stream samples, `distinct` and stdout unchanged (AC1).
- **Bad cast** (open/scan failure) — already handled by `collect` (skip + stderr note, not
  counted); the mid-stream block is inside the same `sampleCast` the existing skip guards.
- **Zero output events** — eligible set empty ⇒ nothing selected ⇒ nothing emitted (no
  special case needed).

## Testing strategy

Scenarios (developer writes them in the existing table-driven idiom; failure messages
reference sample **hashes and cast names, never grid content** — the self-reference
discipline). Add one hand-authored fixture `cmd/corpus-sampler/testdata/midstream-ok.cast`:
a tight burst of ~6 output events at sub-threshold gaps (e.g. 0.0/0.1/…/0.5s), each painting a
**distinct** streaming frame (distinct words, no digits/spinner glyphs so no hash-fold), so no
gap sample fires and all 6 are eligible mid-stream candidates — making N=3 meaningfully less
than the eligible count.

- **AC5a/AC5b — stable positions + two-run identity (new fixture, N=3).**
  `collect([midstream-ok.cast], 0.5, false, false, 3)` emits exactly 3 distinct mid-stream
  samples, each `Source` containing `midstream`; two `collect` runs produce the identical set
  of hashes and `Event` indices. The 3 winning indices are a **stability pin** (recorded from
  a first run), not a golden validated against ground truth.

- **AC1 — byte-identical when off (existing + new fixture).** The existing tests, updated to
  pass `midstream = 0`, must still pass unchanged (their `[gap]`-only / distinct-count
  assertions are the regression pin). Plus: `collect([midstream-ok.cast], 0.5, false, false, 0)`
  yields 0 samples.

- **AC3 + AC4 — emit-all + source union (reuse `stable-sample-ok.cast`, N ≥ eligible).**
  `collect([stable-sample-ok.cast], 0.5, false, false, 10)` (10 > 5 eligible) selects all 5
  events. Expect distinct = 3: the idle screen carries `Source = [gap, midstream]` (event 0 is
  both gap-sampled and mid-stream-selected), the Thinking spinner carries `[gap, midstream]`,
  and the **Working spinner (event 1) — which no quiet gap sampled — appears with
  `Source = [midstream]`** (the point of the source; parallels the `-fires` surfacing). This
  exercises `mergeSources` union (AC4) and emit-all-when-fewer-than-N (AC3) without depending
  on which specific indices a sub-N key selection would pick.

- **Determinism guard (new fixture).** Two `collect` runs with `midstream = 3` produce
  byte-identical per-sample `Source` arrays and identical distinct ordering — the analog of
  `TestCollect_FinalSourceDeterministic` / `TestCollect_FireSourceDeterministic`.

`make check` green.

## Open questions

- **Pre- vs post-append frame for a selected index.** Pinned to **post-append** (the frame
  event `i` painted), matching the `fires` source. If the developer finds a strong reason to
  prefer the pre-append (gap-style) state, either is deterministic — but keep it consistent
  with `fires` absent that reason.
- **Endpoint exclusion.** Left as "include all output events"; dedupe + `mergeSources` handle
  overlap with gap/final. No eligibility bookkeeping is warranted unless a later ticket shows
  the endpoint overlap distorts the exemplar counts.

## Sizing note (auditable)

Sized **S**, not split, despite the edit-fan-out red line tripping on raw count. Recorded so
the decision is reviewable:

- Production source files touched: **1** (`main.go`). New `.go` files: 0. New exported types: 0.
  Reject/error branches: 0. Projected total written LOC ~150–200 (< 600).
- **`collect` fan-out: 14 call sites** (1 production + 13 test) — over the >10 line. But all 13
  test sites live in **one file** and cluster into exactly **3 arg-patterns**
  (`,0.5,false,false)`×5, `,0.5,false,true)`×4, `,0.5,true,false)`×4), collapsing to **3
  `replace_all` + 1 production edit**.
- **Decisive evidence:** #272 (`-final`) and #273 (`-fires`) each made the identical
  `collect`/`sampleCast` signature change with these exact 13 test call sites and both shipped
  clean at S. This is observed outcome on the identical change, twice — not a "mechanical edits
  are cheap" estimate. Splitting would defend a fan-out-exhausts-budget failure disproven twice
  on this code, and would create an artificial seam (dead-flag child + whole-feature child).
  Per Evidence-Based Fix Selection: not split.

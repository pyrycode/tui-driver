# Spec #273 — corpus-sampler: detector-fire sample source

## Files to read first

Read these before touching code — line ranges point at the exact contracts this
change rides on. The whole change lives in `cmd/corpus-sampler/`; the detector
set is copied out of `cmd/corpus-replay/`.

- `cmd/corpus-sampler/main.go:53-76` — the `sample` struct + `sourceGap`/`sourceFinal`
  consts. The struct comment (`:64-68`) and the const comment (`:71-76`) are the
  seam #272 pre-wired for you: **follow-up sources add *values* to `Source`, not
  new fields.** Add `sourceFire` here.
- `cmd/corpus-sampler/main.go:198-286` — `sampleCast`. This is the one function
  that changes materially. Note the exact ordering per output event: the **gap
  emit renders `buf.Snapshot()` BEFORE `buf.Append(data)`** (`:236-252`, frame
  `idx-1`); the fire emit must render **AFTER** the append (see Design). `:265-277`
  is the `-final` precedent for how a bool flag threads through and appends an
  extra sample.
- `cmd/corpus-sampler/main.go:139-164` — `collect`: the whole-run dedupe on `Hash`
  with `mergeSources` unioning `Source` on collision (`:151-156`). Fire samples
  reuse this path untouched. `collect`'s signature grows one `bool` (see Design).
- `cmd/corpus-sampler/main.go:170-186` — `mergeSources` contract: **sorted** union,
  dedup. Why sorting matters for byte-identity across runs.
- `cmd/corpus-replay/main.go:64-78` — `detector` struct + `flatDetectors`. **Copy
  verbatim** (package `main` can't import it), the same way #255 copied
  `tagFromName`/`segmentOf`.
- `cmd/corpus-replay/main.go:316-332` — `activeKeys(snap []byte) map[string]bool`:
  the flat-detector loop **plus the `modal:<class>` folding** off `DetectModalClass`.
  Copy this verbatim; it is the exact predicate set + key vocabulary this source
  must match.
- `cmd/corpus-replay/main.go:286-296` — where corpus-replay runs its classifier:
  `buf.Append(data)` **then** `classify(buf.Snapshot(), …)`. This after-append
  placement is the alignment anchor — the fire emit must render the same snapshot
  corpus-replay classifies.
- `cmd/corpus-sampler/main_test.go:9-27` — the `stable-sample-ok.cast` fixture doc
  (event→frame→gap table) and `no-output-ok.cast`. **This fixture already fires
  real detectors** (verified — see Testing strategy); reuse it as the positive.
- `cmd/corpus-sampler/main_test.go:142-219` — #272's `-final` tests: the pattern
  for threading a bool through `collect`, asserting unioned `Source` arrays with
  `reflect.DeepEqual`, and the cross-run determinism pin. Mirror these for `-fires`.
- `cmd/corpus-replay/main_test.go:12-100` — how corpus-replay asserts fires on
  fixtures (`r.fired["idle"]`, `r.fired["modal:trust-folder"]`). Confirms the key
  strings your `Source` values must use.

## Context

The sampler's core (#255) captures **quiet-gap** stable screens — frames that sat
unchanged through a `-gap` window. Those are wait-states. The direct
false-positive-triage candidates for the offline labeling pass (#257) are the
frames where a **structural detector actually fired** (idle / thinking / an
mcp- or network-failure banner / an unknown dialog / a modal class) — the exact
screens a live run would have acted on. Those often sit mid-stream where quiet
gaps are rare, so the gap source misses them.

`cmd/corpus-replay` (#227) already runs this predicate set over the corpus and
reports *which* detectors fired — but not the *screens* at those fires. This
ticket adds a `-fires` selection source to the sampler that emits those screens,
deduped, with provenance naming the firing detector, feeding the same #257 pass.

This rides the multi-valued `Source []string` / `mergeSources` union seam landed
by #272 (PR #275, on `main`). Not security-sensitive: a selection source picks
which frames land in an offline file; it gates no grant, answer, or route (same
posture as #255 and the corpus-tooling family).

## Design

Additive to the single-file `cmd/corpus-sampler` binary. Four edits, no new
files except one fixture.

### 1. New source const + copied detector set

Add alongside the existing consts (`main.go:73-76`):

```go
sourceFire = "fire"
```

Copy **verbatim** from `cmd/corpus-replay/main.go` into `corpus-sampler/main.go`:

- the `detector` struct (`replay:64-67`),
- `flatDetectors` (`replay:72-78`) — `idle`, `thinking`, `mcp-failure`,
  `network-failure`, `unknown-dialog`,
- `activeKeys(snap []byte) map[string]bool` (`replay:321-332`) — runs each flat
  detector, and adds `"modal:"+string(mc)` when `DetectModalClass(snap)` returns
  a non-`ModalClassUnknown` class.

Do **not** copy `contentAnchors`, `classify`, `castResult`, or the worker pool —
none are used here. The library predicates (`tuidriver.IsIdle`, `IsThinking`,
`HasMcpFailureBanner`, `HasNetworkFailure`, `HasUnknownDialog`,
`DetectModalClass`) are exported and called through the copied `activeKeys`.

The copied names (`"idle"`, `"modal:trust-folder"`, …) are detector *keys*, not
rendered claude anchors, so listing them as string literals does not violate the
screen-literal discipline (corpus-replay lists them the same way).

### 2. Flag + signature threading (mirror `-final`)

- `main()`: add `fires := flag.Bool("fires", false, "…")` next to `final`
  (`main.go:82`). Pass `*fires` into `collect`.
- `collect(paths, gap, final, fires bool)` — add the 4th param, forward it to
  `sampleCast`. **No other change to `collect`** — dedupe/merge already handle
  whatever `sampleCast` returns (AC3).
- `sampleCast(path, gap, final, fires bool)` — add the 4th param; use it to gate
  the fire block (below).

Edit fan-out: `collect` has 10 call sites (1 in `main()`, 9 in `main_test.go`).
Append `, false` to each existing call that isn't exercising fires — mechanical,
same as #272's `final` rollout. Keep the positional order `(…, final, fires)`.

### 3. Fire emission inside the `sampleCast` walk

Emit on the **fire edge** — a detector key that is active this event but was not
active the previous event (`{} → {key}` or a re-fire after it cleared). Rationale
(the ticket left this to the architect): dedupe collapses repeats by hash either
way, but edge emission keeps each sample's `Event`/`TS` meaningful (the onset
moment) instead of stamping an arbitrary mid-plateau tick, and it mirrors
corpus-replay's own edge accounting. Every key in corpus-replay's per-cast
`fired` set gets at least one onset sample here, so the two align.

**Placement (load-bearing).** corpus-replay classifies over the snapshot **after**
appending the event's bytes (`replay:291-294`). To align, the fire check runs
**after** `buf.Append(data)` — i.e. below `:252-253` in the loop, distinct from
the gap check which renders **before** the append. Per output event, when `fires`:

- `cur := activeKeys(buf.Snapshot())` on the post-append snapshot.
- Collect the keys in `cur` not in `prev` (a per-cast `map[string]bool`, empty
  at the first event) into a slice; **`sort.Strings` it** (map iteration is
  randomized — unsorted keys would break byte-identity across runs, see Error
  handling).
- If that slice is non-empty: render `grid := tuidriver.Render(buf.Snapshot(),
  cols, rows)` of that **same** snapshot, and append one `sample` with
  `Source = append([]string{sourceFire}, sortedNewKeys...)` (then `sort.Strings`
  the whole `Source` slice — see Error handling), `Event = idx`, `TS = ts`, the
  cast's `cols`/`rows`, `Hash = hashGrid(normalize(grid))`. **One sample per
  fire event carrying all newly-fired keys** — not one sample per key — so `Seen`
  counts screen occurrences, not key occurrences.
- `prev = cur`.

When `fires` is false, skip the entire block (no `activeKeys` call, no `prev`) —
this is what keeps `-fires` off byte-identical (AC1).

`Event = idx` here is the index of the event whose bytes were just appended (the
event **at which** the detector fired), distinct from the gap sample's `Event =
idx-1` (the frame that sat stable *before* the current event). Both are correct
for their respective sources.

No separate final-frame fire classification is needed: unlike corpus-replay
(which samples at a `-stride` and re-classifies the last frame to cover a skipped
tick), this walk runs the fire check on **every** output event, so the last event
is already covered.

### 4. stdout unchanged

Keep the stdout line exactly `casts=%d events=%d gaps=%d distinct=%d`. Fire
samples fold into `distinct`; do **not** add a `fires=` counter (not required by
any AC, and leaving stdout alone keeps `-fires` off trivially byte-identical for
stdout as well as the `-out` file). `gaps` continues to count quiet-gap fires
only.

### Data flow (one cast, `-fires` on)

```
per output event i (ts, data):
  ├─ [gap check]  if gap(ts,prevTS): emit gap sample  (renders buf BEFORE append → frame i-1)
  ├─ buf.Append(data)
  └─ [fire check] cur = activeKeys(buf.Snapshot())     (renders buf AFTER append  → frame i)
                  newKeys = sort(cur \ prev)
                  if newKeys: emit fire sample Source=[fire]+newKeys, Event=i, TS=ts
                  prev = cur
→ collect(): dedupe all samples (gap+final+fire) on Hash; mergeSources unions Source on collision
```

A frame found by more than one source (e.g. an idle frame that both sat through a
quiet gap and tripped `IsIdle`) collapses to one distinct entry whose `Source`
is the sorted union, e.g. `["fire","gap","idle"]` — exactly the intended
multi-source provenance.

## Concurrency model

None. `corpus-sampler` is single-threaded (unlike corpus-replay's worker pool);
this change adds no goroutines. Determinism comes from sorted `castPaths` +
in-order event walk + first-occurrence-wins dedupe, unchanged.

## Error handling

- **Map-iteration nondeterminism (the one real hazard).** `activeKeys` returns a
  `map`; iterating it yields keys in random order. Both the per-event new-key
  slice and the final `Source` slice **must** be `sort.Strings`-ed before the
  sample is stored, because `collect` stores the first occurrence's `Source`
  **as-is** (`mergeSources` only re-sorts on a later collision). Skip the sort and
  a fire sample that is the first to introduce two keys serializes in random order
  → non-byte-identical re-runs (AC3). The determinism test (below) is the guard.
- A cast that fails to open/scan is already skipped-with-stderr-note by `collect`
  (`main.go:143-146`); the fire path adds no new failure modes.
- Grids still go to `-out` only; stdout stays counts-only; failure messages
  reference hashes + cast names, never grid content (self-reference discipline).

## Testing strategy

All tests drive `collect(...)` directly, as the existing suite does — no process
spawn. **Verified empirically against the real library detectors** (so these are
facts, not guesses):

**Positive — reuse `testdata/stable-sample-ok.cast` (already committed).** Its
frames genuinely fire real detectors: event 0 → `idle`, events 1–2 → `thinking`,
events 3–4 → `idle`. With edge emission, the fire samples are event 0 `[fire,idle]`,
event 1 `[fire,thinking]`, event 3 `[fire,idle]`. Combined with the existing gap
samples, `collect([]string{fixture()}, 0.5, false /*final*/, true /*fires*/)`
yields exactly **3 distinct entries**:

| # | screen | Source (sorted) | Seen | Event | TS |
|---|--------|-----------------|------|-------|-----|
| 0 | idle (`…ready 12`/`999` fold to one hash) | `["fire","gap","idle"]` | 4 | 0 | 0.0 |
| 1 | spinner `Working` (mid-stream, **only** the fire source catches it) | `["fire","thinking"]` | 1 | 1 | 0.6 |
| 2 | spinner `Thinking` (the gap-sampled spinner) | `["gap"]` | 1 | 2 | 0.7 |

Assert against this table. Entry #1 is the point of the source — a mid-stream
frame the quiet-gap rule never sampled, surfaced because a detector fired on it.

Scenarios to cover (bullet points, not full bodies — write in the suite's idiom):

- **Fire source emitted with detector attribution:** with `fires=true, final=false`,
  a distinct entry carries `Source` containing both `"fire"` and `"idle"`
  (assert the full 3-row table above, incl. the `["fire","thinking"]` entry that
  proves a modal/flat key rides `Source`).
- **`-fires` off is byte-identical (AC1):** `collect(fixture, 0.5, false, false)`
  reproduces the existing 2-entry gap-only result with every `Source == ["gap"]`
  — the existing `TestCollect_StableScreensDedupeAndCounts` assertions must still
  hold verbatim (add `, false` for the new param; do not otherwise change it).
- **No-fire cast emits no fire sample (AC4 negative):** add a small committed
  fixture with plain build-output frames (e.g. two `compiling package …` /
  `ok  …` lines) — **verified to fire no detector**. Plain text has no control
  bytes, so it can be Written directly (the `.cast` ESC-escaping hazard only
  bites frames with `\x1b`). Assert no distinct entry's `Source` contains
  `"fire"`. Use a sub-threshold or absent gap if you want `distinct` empty, or
  just assert the `"fire"`-absence directly (a `[gap]`-only sample is fine — the
  AC is about the *fire* source, not emptiness). Failure message references the
  cast name + sample hash, never grid content.
- **Determinism (AC3):** run `collect(fixture, 0.5, false, true)` twice; assert
  equal length, per-index equal `Hash`, and `reflect.DeepEqual` on each `Source`
  — the guard for the map-iteration sort above. Mirror
  `TestCollect_FinalSourceDeterministic`.

`make check` green (build + vet + full test) is the final AC.

## Open questions

- **README courtesy (out of AC scope):** `cmd/corpus-sampler/README.md` lists the
  flags; adding a one-line `-fires` entry keeps the tool self-documenting and is
  in-tool (not an outside-`src` doc), but it is **not** a blocking AC. Developer's
  call whether to include the one line; do not let it grow.
- **Multi-key fire events** (two detectors newly firing on the same event) are
  handled by the one-sample-per-event design (all new keys in one `Source`), but
  no committed fixture exercises that path and no AC requires it — left untested
  by design rather than fabricating a synthetic multi-fire frame.

## Acceptance Criteria (restated for the developer)

1. `-fires` flag (default off). On: emit a sample at each event where a structural
   detector newly fires, over the same predicate set corpus-replay exercises
   (`flatDetectors` + `DetectModalClass`→`modal:<class>`). Off: `-out` **and**
   stdout byte-identical to prior behavior.
2. Each fire sample carries the `fire` source **and** the firing detector key
   (`modal:<class>` for a modal-class fire), through `Source []string` — no new
   struct field.
3. Dedupe/merge reuse the core's sorted-union path: a fire screen whose normalized
   hash already exists unions its sources onto the existing sample (no new line,
   no second merge path). Re-runs are byte-identical.
4. A unit test on committed fixtures asserts (a) a known detector-fire frame is
   emitted with the firing detector recorded, and (b) a cast where no detector
   fires emits no fire sample. Failure messages reference hashes + cast names,
   never grid content.
5. `make check` green.

# Spec: corpus-sampler — final-frame source + multi-valued `source` provenance (#272)

Split child of #256. Adds the first extra sample source (`-final`) to the shipped
`cmd/corpus-sampler` core (#255), together with the shared multi-valued `source`
provenance that the two remaining #256 children (`-fires` #273, `-midstream` #274,
both blocked-by this ticket) build on.

Size: **S**. Not security-sensitive (report-only exemplar audit feeding offline
#257/#258 — a forged sample routes nothing; same posture as #255 / corpus-replay).
Single production file (`cmd/corpus-sampler/main.go`), additive to the sample shape.

## Files to read first

- `cmd/corpus-sampler/main.go:50-64` — the `sample` struct; this is where the
  provenance field is added (JSON key `source`). Note the field ordering/tag idiom
  to match.
- `cmd/corpus-sampler/main.go:66-101` — `main`: flag wiring (`-dir`/`-out`/`-gap`)
  and the counts-only stdout line. `-final` is registered here and threaded into
  `collect`.
- `cmd/corpus-sampler/main.go:120-148` — `collect`: the cross-run dedupe. The
  merge on a hash collision is `distinct[pos].Seen++` (line 139) — this is the
  exact site that must also union sources. Signature grows a `final` param.
- `cmd/corpus-sampler/main.go:150-225` — `sampleCast`: the per-cast walk. The gap
  sample is built at lines 199-207; the final render reuses the identical
  `tuidriver.Render(buf.Snapshot(), cols, rows)` call (line 198) once the walk
  completes. The post-loop Tag/Segment stamping (lines 218-223) already exists;
  the final sample must be appended *before* that stamping so it inherits
  Tag/Segment. Note `gaps` is currently returned as `len(samples)` (line 224) —
  that must change (see § Design).
- `cmd/corpus-sampler/main_test.go:22-127` — the four existing `collect(...)` call
  sites (lines 25, 52, 70, 108-112) that need the new `final` argument, and the
  fixture doc-comment (lines 8-22) describing the committed cast's event timeline.
- `pkg/tuidriver/grid.go:39` — `Render(snap []byte, cols, rows int) string`
  (confirm signature; unchanged use).
- `pkg/tuidriver/buffer.go:58` — `Buffer.Snapshot() []byte` returns a copy; safe to
  render at any point including after the walk.
- `.gitignore:27-32` — `*.cast` is globally ignored; the
  `!cmd/corpus-sampler/testdata/*.cast` negation (line 32) is **already present**
  from #255. No gitignore edit needed whether you reuse the existing fixture or add
  a new one under that directory.
- Memory landmine `cast-fixture-esc-escaping` — only relevant if you add a *new*
  fixture: raw ESC bytes must be generated via a throwaway Go program
  (`string(rune(0x1b))` + `json.Marshal`), never hand-typed through an editor.

## Context

`corpus-sampler` core (#255) emits only quiet-gap stable screens — a frame that sat
unchanged through an inter-event gap ≥ `-gap`. It misses how a run *ended* when the
final events arrive in a burst (no trailing quiet gap), so the ending screen — a
clean idle, a wedged error frame, a half-drawn modal — never gets sampled.

This ticket adds:

1. **`-final`** — emit each cast's final rendered frame as one sample.
2. **`source`** — a provenance field on every sample naming which rule found it
   (`gap`, `final`). Multi-valued from the start: when one normalized screen is
   found by more than one source, the written sample keeps the full set. This is
   the shared mechanism #273 (`-fires`) and #274 (`-midstream`) extend by adding
   *values*, not new machinery — hence the serialized shape must be stable now.

## Design

All changes are in `cmd/corpus-sampler/main.go`. No new files (fixture reuse is the
default — see § Testing strategy). No exported types (package main).

### 1. `source` provenance on the sample shape

Add a provenance member to `sample`, serialized under the JSON key **`source`**
(mandated by AC 2). Contract — describe behavior, not representation:

- Holds a **set of distinct source-name strings**. Values in this ticket: `"gap"`,
  `"final"`. Follow-up children add more values (`"fires"`, `"midstream:N"`, …) —
  no new field, no shape change.
- **Always serialized as a JSON array**, even for a single source. A scalar-then-
  array shape would break when a screen picks up a second source; the follow-ups
  depend on it already being an array. (This is the one shape decision worth
  pinning: array always.)
- **Serialized in a stable, sorted order** so re-runs are byte-identical (AC 4).
  Discovery order across casts is already deterministic (sorted `castPaths` +
  in-order walk), but sorting the set at marshal time makes the array order
  independent of *which* source happened to find the screen first — the robust way
  to satisfy "identical per-sample source sets."

Internal Go representation is the developer's call (a sorted `[]string`, or a
`map[string]struct{}` folded to a sorted slice at marshal). Recommend the simplest
that keeps the JSON sorted and distinct.

### 2. `-final` flag + final-frame emission

- Register `-final` as a bool flag, **default false**, in `main`; thread it into
  `collect`, which threads it into `sampleCast`. (Signature growth: `collect(paths,
  gap, final)` and `sampleCast(path, gap, final)`. ~6 call sites total — 1 in
  `main`, 4 existing test calls, 1 internal — well under the fan-out line.)
- In `sampleCast`, after the scan loop completes and `sc.Err()` is clear, **if
  `final` is true and the cast had ≥1 output event**, render
  `tuidriver.Render(buf.Snapshot(), cols, rows)` (the buffer now holds every output
  event) and append one sample with:
  - source = `final`
  - `Event` = index of the last output event (`idx-1`)
  - `TS` = that event's timestamp (`prevTS`)
  - `Grid`/`Hash`/`Cast`/`Cols`/`Rows` built exactly like the gap sample.
- **Zero-output cast → no final sample** (AC 1): guard on the output-event count
  (`idx >= 1`), not on buffer content.
- Append the final sample **before** the existing Tag/Segment stamping loop so it
  inherits `Tag`/`Segment` like every other sample.
- With `-final` off, `sampleCast` appends nothing new → output is byte-identical to
  core (AC 1, AC 5c).

### 3. Set `source: gap` on gap samples

The existing gap sample (built inside the loop) gains source = `gap` at
construction. Additive to the sample shape — **not** a rename of any field.

### 4. Union-on-merge in `collect`

The collision branch currently reads:

```
if pos, ok := idx[s.Hash]; ok {
    distinct[pos].Seen++
    continue
}
```

Extend it to **also union `s`'s source set into `distinct[pos]`'s source set**
(distinct, sorted). `Seen++` stays — seen still counts total occurrences (AC 3).
The first-occurrence insert keeps `s.Source` as-is (already a one-element set).

This is the single behavioral seam #273/#274 reuse: they emit samples carrying
their own source, and the same union folds them into the kept entry. Do not add a
per-source mechanism — one union path serves all sources.

Worked trace (existing fixture, `-final` on): the idle screen is emitted by gap at
events 0 and 3 and by final at the last event; all three collide on one hash →
kept entry ends with source `{gap, final}`, `Seen == 3`. The spinner screen is
gap-only → `{gap}`, `Seen == 1`. `distinct` stays 2.

### 5. `gaps` count must exclude the final sample

`sampleCast` currently returns the gap count as `len(samples)` (correct only while
every sample is a gap). Once a final sample can be appended, that would
over-count. Track gap emissions with an **explicit counter** incremented in the gap
branch and return that; the stdout `gaps=` stat keeps meaning "quiet-gap fires,"
final-frame samples excluded. (`events`/`distinct` counts are unaffected — `events`
is the output-event count, `distinct` is `len(distinct)`.)

## Concurrency model

None. `collect` walks casts sequentially; `sampleCast` walks one cast's events
sequentially. Single-writer throughout. Determinism (AC 4) follows from sorted
`castPaths` + in-order walk + first-occurrence-wins dedupe + sorted source arrays —
no goroutines, no shared state.

## Error handling

Unchanged from core. A cast that fails to open/scan is skipped with a stderr note
and not counted (`collect` lines 130-133). The final render cannot fail (in-memory
`Render` over a snapshot). `writeSamples` failure still aborts with a non-zero exit
and never falls back to stdout.

## Testing strategy

One fixture-backed test file change plus the four call-site updates. Failure
messages reference **sample hashes and cast names only — never grid content**
(self-reference discipline; the existing tests already model this).

**Fixture reuse (default).** The committed `testdata/stable-sample-ok.cast` already
ends on the idle screen (event 4 repaints idle "…ready 999"), which the gap rule
also samples (events 0, 3). Its final frame therefore folds into a gap-sampled
screen — exactly the gap+final union AC 5b needs — with **no new fixture**. The
developer must confirm this empirically by running the new test; only if the final
frame does *not* fold as expected should a minimal dedicated fixture be authored
(per the ESC-escaping landmine).

**Scenarios (bullet-pointed; developer writes the Go in the file's idiom):**

- **`-final` off matches core (AC 5c).** `collect(fixture, 0.5, false)`: `distinct
  == 2`, `gaps == 3`, idle `Seen == 2`, spinner `Seen == 1`, and **no** sample
  carries `final` in its source set (every source set is exactly `{gap}`). This is
  the regression pin that `-final` off changed nothing.
- **`-final` on emits the last frame + records both sources (AC 5a, 5b).**
  `collect(fixture, 0.5, true)`: `distinct == 2` (final folds), `gaps == 3`
  (unchanged — gap count excludes the final sample), the idle entry's source set
  `== {final, gap}` (sorted) with `Seen == 3`, and the spinner entry's source set
  `== {gap}` with `Seen == 1`. Asserting spinner stays `{gap}`-only pins that
  `final` tagged the *ending* (idle) screen and not some mid-run frame.
- **Serialized source order is deterministic (AC 4).** Two `collect(..., true)`
  runs produce identical source arrays per hash (extend the existing
  `TestCollect_Deterministic` shape, or add source-array equality to it).
- **(Optional, cheap) zero-output guard (AC 1).** A cast with no `o` events yields
  no final sample even with `-final` on. Can be a table row driven through
  `sampleCast` or a tiny synthetic fixture; include if it doesn't cost a new
  finicky `.cast`.

**Existing tests to update:** the four `collect(...)` calls
(`main_test.go:25,52,70,108-112`) grow a `false` final argument to keep asserting
core behavior. That mechanical change *is* the AC 5c guarantee in situ.

## Open questions

- **Reuse vs. new fixture.** Spec recommends reusing `stable-sample-ok.cast`
  (empirically it folds final→idle). If the developer's run shows the final frame
  does *not* fold into a gap screen, author a minimal dedicated fixture whose last
  event repaints an already-gap-sampled screen. Either way is within S.
- **Flag threading vs. options struct.** #273 and #274 each add another flag
  (`-fires`, `-midstream N`). This ticket threads a single `final bool` — do **not**
  pre-build a config struct now (Simplicity First; speculative). If the param list
  becomes unwieldy, the child that trips it can refactor then.
- **README.** `cmd/corpus-sampler/README.md` documents the core flags; adding
  `-final` and the `source` field is a nice-to-have doc touch, not a production
  deliverable and not gated by any AC. Leave to developer discretion.

# Spec #255 — corpus-sampler: deduped stable screens from the recording corpus, with provenance

## Files to read first

- `cmd/corpus-replay/main.go:148-167` — `castPaths`: the `.cast` directory listing + sorted-for-determinism pattern. Copy shape (drop the `only` filter — #255 has no `-only` flag).
- `cmd/corpus-replay/main.go:195-225` — the scanner setup (`sc.Buffer(..., 4*1024*1024)`), header-dims parse, and the per-event `buf.Append(data)` walk skeleton. Mirror this loop; the divergence is timestamp gating (below).
- `cmd/corpus-replay/main.go:287-335` — `parseOutputEvent`, `tagFromName`, `segmentOf`. **`tagFromName` and `segmentOf` copy verbatim.** `parseOutputEvent` is the base for the timestamp-returning fork (Technical Note divergence).
- `cmd/corpus-replay/main_test.go:1-13,102-126` — test idiom: `filepath.Join("testdata", "<fixture>.cast")`, table tests for `tagFromName`/`segmentOf`. Reuse.
- `cmd/corpus-replay/testdata/stable-ok.cast` — the "`[2J[H` clear-home per frame" fixture pattern. Mirror it so each frame renders to a *predictable* grid, and copy the `` ESC escaping (Go `json` rejects a raw `0x1b` in a string literal — see the `.cast` ESC landmine in project memory).
- `pkg/tuidriver/grid.go:39-49` — `Render(snap, cols, rows) string`: VT100 render, **each row already right-trimmed, trailing empty rows dropped**. This is the raw grid text.
- `pkg/tuidriver/buffer.go:35-64` — `NewBuffer(0)` (→ `DefaultBufferCap` rolling window) and `Snapshot()` (returns a copy). The sampler feeds one buffer per cast.
- `pkg/tuidriver/state.go:54-60` — the exact five sparkle spinner glyphs (`✻ ✳ ✢ ✶ ✽`, with their `\xNN` byte encodings). `normalize` unifies these; the set is **unexported (`spinnerGlyphs`)**, so the tool re-lists the runes locally (copy the five from here). The middle-dot `U+00B7` (state.go:108) is deliberately *not* in the unify set — same reason the library keeps it out: it doubles as an in-region separator.
- `Makefile:14,52-53` — `TOOLS`/`corpus-replay` target. **Do not add a `corpus-sampler` target** (see Open Questions); the tool builds by hand and `make check` (`go test -race ./...`, line 44) auto-runs the new package's test.

## Context

The PTY recording corpus behind `SpawnOpts.RecordTo` (~1900 casts, ~4.5M output events) is the regression baseline, but there is no mechanical way to pull the *distinct situations* out of it. Whole-grid dedupe fails: 96.9% of per-event grids are unique (76.0% even after collapsing digits + unifying spinner glyphs) because the rolling window shifts on every output event, so almost every render differs *somewhere*.

The situations worth extracting are the **stable screens** — a grid that sat unchanged through a quiet gap (a screen waiting for input is a quiet screen). Measured: quiet gaps ≥ 0.5s average 7.6/cast (~14 500 corpus-wide before cross-cast dedupe). This ticket builds the sampler that walks every event, emits the grid at each quiet gap, dedupes on a normalized hash, and writes a low-thousands exemplar set with provenance. That set feeds the offline model-labeling pass (#257) and fixture promotion (#258).

**Scope boundary.** #255 is quiet-gap stable screens *only*. The other candidate sources — a random mid-stream slice, every detector-fire moment, and final frames — are #256 ("extra sources"). Do not build them here. In particular, **do not emit a final-frame sample** after the last event (that is #256).

## Design

New single-concern binary `cmd/corpus-sampler` (its own `package main`; it cannot import `corpus-replay`'s helpers, so the three reused helpers are copied). No library changes — `Render`, `NewBuffer`, the glyph set are all already exported/available.

### Data flow

```
-dir ──> castPaths (sorted) ──> for each cast: sampleCast(path, gap)
                                     │  walk "o" events; NewBuffer(0)
                                     │  on inter-event gap ≥ threshold:
                                     │     Render(snap) ──> raw grid
                                     │     normalize+hash ──> stable-screen sample
                                     ▼
                          collect(): cross-run dedupe on hash
                             first hash ──> append to discovery-ordered slice, Seen=1
                             repeat hash ──> Seen++ on the existing entry (no new line)
                                     │
              ┌──────────────────────┴───────────────────────┐
        -out: JSONL (one line per distinct screen)      stdout: aggregate
        (grids written here ONLY)                       counts ONLY, never a grid
```

### The stable-screen rule (pin this exactly)

Output events (code `"o"` only) are indexed `0,1,2,…` in encounter order. For consecutive output events `i-1` and `i` with `ts[i] - ts[i-1] >= gap`, the frame painted by event `i-1` sat stable through the quiet `[ts[i-1], ts[i]]` window. Emit **one** sample for it:

- render **the buffer holding events `[0 … i-1]`** — i.e. render *before* appending event `i`;
- `Event = i-1`, `TS = ts[i-1]` (when the stable frame was last painted).

Event `0` has no predecessor → no gap. Non-`"o"` events (input, resize, parse miss) are skipped for both gap timing and the buffer, so "consecutive output events" means consecutive `"o"` events.

### Key types & functions (contracts, not bodies)

```go
// sample is one distinct stable screen with full provenance. Grid is the RAW
// rendered grid (un-normalized); Hash is over the normalized form. Written to
// -out only — never to stdout.
type sample struct {
    Grid    string  `json:"grid"`    // Render() output at the gap moment
    Hash    string  `json:"hash"`    // hashGrid(normalize(Grid))
    Cast    string  `json:"cast"`    // filepath.Base(path)
    Event   int     `json:"event"`   // output-event index of the stable frame (i-1)
    TS      float64 `json:"ts"`      // ts[i-1], seconds
    Cols    int     `json:"cols"`    // from cast header (0 → Render default)
    Rows    int     `json:"rows"`
    Tag     string  `json:"tag"`     // tagFromName: "ok" | "err" | "untagged"
    Segment string  `json:"segment"` // segmentOf: "prod" | "e2e" | "unknown"
    Seen    int     `json:"seen"`    // total occurrences across the run (first = 1)
}

// parseOutputEventTS forks corpus-replay's parseOutputEvent to also return the
// event timestamp ev[0]. ok=false for any non-"o" event or malformed line.
func parseOutputEventTS(line []byte) (ts float64, data []byte, ok bool)

// sampleCast walks one cast and returns its stable-screen samples (Tag/Segment
// already stamped; Seen left 0 — dedupe owns it), plus the cast's output-event
// count and gap count. gap is the threshold in seconds.
func sampleCast(path string, gap float64) (samples []sample, events, gaps int, err error)

// normalize maps a raw grid to its dedupe key form: unify the five sparkle
// spinner glyphs to one canonical rune, collapse each run of ASCII digits to a
// single "0", right-trim every row. Order: unify → collapse → trim.
func normalize(grid string) string

// hashGrid returns hex(sha256(normalized)). Deterministic; the JSONL/​failure
// messages reference this, never the grid.
func hashGrid(normalized string) string

// collect walks paths in order, dedupes stable screens across the whole run on
// Hash (first occurrence kept in discovery order, repeats bump Seen), and
// returns the distinct set plus aggregate counts.
func collect(paths []string, gap float64) (distinct []sample, casts, events, gaps int, err error)
```

`sampleCast` internals: open → `bufio.Scanner` with the 4 MB buffer → parse header dims into `cols,rows` → `buf := tuidriver.NewBuffer(0)` + `var full strings.Builder`. Per output event: if `idx >= 1 && ts-prevTS >= gap`, append a partial sample (`Grid = tuidriver.Render(buf.Snapshot(), cols, rows)`, `Hash`, `Cast`, `Event=idx-1`, `TS=prevTS`, `Cols`, `Rows`) — then `buf.Append(data)`, `full.Write(data)`, `prevTS=ts`, `idx++`. `Segment` needs the whole cast's text, so compute `segmentOf(full.String())` + `tagFromName(name)` **after** the walk and stamp them onto every sample from this cast.

`collect` dedupe (avoid pointer-aliasing a range variable): keep `distinct []sample` in discovery order + `idx map[string]int` (hash → position). For each incoming sample: if `pos, ok := idx[s.Hash]; ok` → `distinct[pos].Seen++`; else `s.Seen = 1; idx[s.Hash] = len(distinct); distinct = append(distinct, s)`. `casts` counts casts processed; `events`/`gaps` sum the per-cast returns. Determinism follows from sorted `castPaths` + in-order events + first-occurrence-wins → identical distinct set on re-run.

`main()`: flags `-dir` (required), `-out` (required output file), `-gap` (`flag.Duration`, default `500ms`; pass `gap.Seconds()` down) → `castPaths` → `collect` → write each `distinct` entry as one `json.Marshal` + `\n` to `-out` → print the aggregate line to stdout. Missing `-dir` or `-out` → message to stderr, `os.Exit(2)` (mirror corpus-replay's `-dir` handling).

## Concurrency model

None. Single-threaded, deterministic offline walk — the same shape as `corpus-replay`. `Buffer`'s mutex is irrelevant here (one goroutine). No context, no channels.

## Error handling

- Missing `-dir`/`-out` → stderr + `os.Exit(2)`.
- `-dir` unreadable / no `.cast` files → stderr + `os.Exit(1)` (mirror corpus-replay:125-133).
- A cast that fails to open/scan → `fmt.Fprintf(os.Stderr, "corpus-sampler: skip %s: %v", …)` and `continue` (one bad cast never aborts a 1900-cast run; mirror corpus-replay:138-141). It is **not** counted in `casts`.
- A malformed/header-less line yields `cols,rows = 0` → `Render` falls through to its 120×40 default (grid.go:40-45). Acceptable; the corpus is recorded at 120×40.
- `-out` open/write failure → stderr + `os.Exit(1)`. Write grids to the file only; a failed write must not fall back to stdout.

## Self-reference discipline (load-bearing)

Sample grids contain detection-anchor literals; echoing them on screen mid-run can false-fire live detection (as with #152/#154/#155). Enforce:

- **stdout carries aggregate counts only** — `casts, events, gaps, distinct screens` — and **never** any grid content or any per-sample field derived from grid text other than counts.
- Grids go to `-out` only.
- **Test failure messages reference `Hash` and `Cast` (and numeric fields), never `Grid`.** A `t.Errorf` that prints a grid is a defect, not a debugging aid.

## Testing strategy

`make check` (`go test -race ./...`) auto-includes the new package. Add `cmd/corpus-sampler/main_test.go` with one committed fixture `cmd/corpus-sampler/testdata/<name>.cast`.

**Fixture requirements** (author the `.cast` bytes; mirror `stable-ok.cast`'s `[2J[H`-per-frame form so each frame renders to a predictable grid, and its `` escaping):

- header `{"version":2,"width":120,"height":40}`;
- crafted timestamps so some inter-event gaps are `≥ 0.5s` (emit a sample) and at least one is `< 0.5s` (no sample) — proves gap gating, not event-stride;
- a screen `S` that renders to the same normalized grid at **two** separated `≥0.5s` gaps (e.g. an idle prompt that returns after a spinner frame) so the two collapse to one `distinct` entry with `Seen == 2`;
- optionally a second occurrence of `S` that differs only by a digit run or spinner glyph, to prove `normalize` folds them to the same hash.

**Test scenarios** (bullet form — write in the project idiom; assert on hashes/counts, never grids):

- `collect([]string{fixture}, 0.5)` returns the expected number of `distinct` samples; `gaps` equals the number of `≥0.5s` inter-event gaps; the sub-threshold gap produced **no** sample.
- The repeated screen `S` appears **once** in `distinct` with `Seen == 2` (or N). Assert by looking up its `Hash`; if the assertion fails, print the hash and cast name.
- The first sample's provenance matches the stable-frame rule: `Event == i-1`, `TS == ts[i-1]`, `Cols/Rows == 120/40`, `Tag`/`Segment` as expected from the filename/content.
- `collect` on the same input twice yields an identical `distinct` slice (determinism) — compare the hash sequences.
- `normalize`: unit table — a digit run collapses (`"tokens 12345"` and `"tokens 6"` → same), a sparkle glyph unifies (`✳`-frame and `✻`-frame of the same screen → same normalized string), rows right-trimmed.
- Copy `corpus-replay`'s `TestTagFromName`/`TestSegmentOf` tables (the helpers are copied verbatim; re-pin them here).

## Open questions

- **No Makefile target (decided).** The AC says build/run by hand like `corpus-replay`; `make check` already covers the test via `go test ./...`. A `corpus-sampler` Make target would edit the `Makefile`, which in-flight #253 also touches → a needless merge conflict for zero AC benefit. If a convenience target is wanted later, it is a trivial follow-up once #253 lands. **Do not add it in this ticket.**
- **README optional.** `corpus-replay` ships a README; a short one for `corpus-sampler` is welcome but not an AC and not required for `make check`. Keep it to usage + the self-reference warning if added.
- **Hash width.** Full `sha256` hex is specified for zero-ambiguity dedupe. Truncating for readability is unnecessary (the JSONL is machine-read); do not truncate.

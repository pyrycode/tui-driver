# Spec #150 — Rendered-grid refactor 1/6: addressable rendered-grid accessor

**Ticket:** pyrycode/tui-driver#150 · **Size:** S · **Dep:** none · **Chain:** slice 1 of 6 (Cross-Repo Code Review 2026-07-03, fixes CRITICAL A + B)

## Files to read first

- `pkg/tuidriver/grid.go:36-63` — `Render` + `trimGridText`. The exact rendering contract `NewGrid` wraps: renders a `[]byte` snapshot through vt10x, right-trims every row, drops trailing empty rows, joins with `"\n"`. **Reuse it verbatim; do not re-implement rendering or trimming.**
- `pkg/tuidriver/grid.go:12-15` — `DefaultGridRows` / `DefaultGridCols` (alias `DefaultPtyRows`=40 / `DefaultPtyCols`=120). These are the zero-dimension fallthrough values; `Render` already applies them for `cols<=0` / `rows<=0`, so `NewGrid` inherits the behaviour for free by delegating.
- `pkg/tuidriver/grid_test.go` (whole file, 129 lines) — the test idiom for this file: plain `got != want` with `t.Errorf`, fixtures built as inline `[]byte`, one behaviour per function. `TestRenderDefaultsForZeroDims:79-86` is the exact pattern to mirror for the `NewGrid` zero-dims-parity test.
- `pkg/tuidriver/buffer.go:52-60` — `Buffer.Snapshot()` returns the raw history `[]byte` that `NewGrid` consumes in production; it already returns a fresh copy, so the grid never aliases live buffer memory (no defensive copy needed in `NewGrid`).
- `pkg/tuidriver/agents.go:41-73` (`ParseAgentList`) — **context only, not changed in this slice.** The canonical `strings.Split(Render(...), "\n")` + per-line-normalise shape that slices 2-6 will migrate onto `Grid`. Confirms the accessor's `Rows()` output must be the same `[]string` these detectors produce ad hoc today.
- `pkg/tuidriver/mcp.go:71-80` (`ParseMcpStatus`) and `pkg/tuidriver/permission.go:63-88` (`ParseModalContent` / `modalLines`) — **context only.** The other two future-migration call sites; they establish that "split rendered text into lines, then predicate over regions" is the shared pattern this type consolidates.

## Context

`Render` already interprets a PTY snapshot as a VT100 grid and returns the correct on-screen text — but flattened to one joined string. Every structure-aware detector (`agents.go:42`, `mcp.go:72`, `permission.go:73`) then re-splits or re-scans that string ad hoc, and other predicates `strings.Contains` the *raw append-only history buffer* directly. Both criticals from the Cross-Repo Code Review share one root cause: **text that has scrolled off the top of the visible grid still lives in the history buffer, so a substring match over that buffer classifies stale/off-screen text as if it were on screen.**

This slice ships the foundation the rest of the chain builds on: a small addressable value over the rendered rows, with region-scoped predicates. **It wires no detector to it and changes no behaviour** — slices 2-6 migrate the detectors. Shipping the accessor alone keeps this slice mechanically verifiable (pure function of its input, no detector regression surface) and lets the migration slices review cleanly against a stable API.

## Design

### Placement

Add the new type and its methods to the existing **`pkg/tuidriver/grid.go`** (alongside `Render`) — it is the same "rendered grid" concern, keeps the reading surface tight, and adds zero new production files. Tests go in the existing `pkg/tuidriver/grid_test.go`.

### Type + constructor

```go
// Grid is an addressable view over a rendered VT100 screen. Construct once per
// snapshot with NewGrid; it renders the snapshot via Render and caches the
// resulting rows. Every accessor operates on the rendered *screen* rows — the
// text a terminal would actually display — never the raw append-only history
// buffer. That distinction is what lets later detectors avoid classifying
// scrolled-off text as on-screen.
type Grid struct {
    rows []string // rendered rows; see NewGrid for the empty-render contract
}

// NewGrid renders snap at the given grid dimensions and returns a Grid over the
// resulting rows. Passing 0 for either dimension falls through to
// DefaultGridCols / DefaultGridRows (same convention as Render); non-zero
// dimensions override them. An empty render (whitespace-only or empty snapshot)
// yields a Grid with zero rows.
func NewGrid(snap []byte, cols, rows int) *Grid
```

- **Body is a one-liner over `Render`:** `text := Render(snap, cols, rows)`. Dimension fallthrough is entirely `Render`'s job — do not re-check `cols<=0` here.
- **Empty-render contract (pin in a test).** `Render` returns `""` for an empty/whitespace-only snapshot. `strings.Split("", "\n")` returns `[]string{""}` (one empty row), which is wrong for an empty screen. `NewGrid` must special-case: `if text == "" { rows = nil } else { rows = strings.Split(text, "\n") }`. An empty screen has **zero** rendered rows, consistent with "trailing empty rows dropped."
- Returns `*Grid` to match the package's constructor convention (`NewBuffer`, `NewTracker` → pointer).

### Accessors

```go
// Rows returns the grid's rendered rows in top-to-bottom order: one entry per
// row, each right-trimmed, trailing empty rows dropped (exactly what Render
// emits per line). The returned slice is the grid's own backing store —
// read-only; callers must not mutate it.
func (g *Grid) Rows() []string

// ContainsInLastRows reports whether sub appears within the last n rendered
// rows. n<=0 returns false; n greater than the row count is clamped to all
// rows. A match must fall inside a single row — sub is never matched across a
// row boundary (that is the point: off-screen/above-region text does not count).
func (g *Grid) ContainsInLastRows(sub string, n int) bool

// RowHasPrefix reports whether row i (0-based, top-down) starts with prefix.
// An out-of-range i returns false rather than panicking.
func (g *Grid) RowHasPrefix(i int, prefix string) bool
```

Behavioural contracts (each is one guard + one stdlib call — no loops beyond the region scan):

- `Rows()` → return `g.rows` directly.
- `ContainsInLastRows(sub, n)` → `n <= 0` → false; clamp `n = min(n, len(g.rows))` (Go 1.26 builtin `min`); scan `g.rows[len(g.rows)-n:]` with `strings.Contains`; false if none. Empty grid (`len==0`) returns false via the clamp (window is empty).
- `RowHasPrefix(i, prefix)` → `i < 0 || i >= len(g.rows)` → false; else `strings.HasPrefix(g.rows[i], prefix)`.

### Data flow

```
Buffer.Snapshot() ──[]byte──▶ NewGrid(snap, cols, rows)
                                   │ Render(snap, cols, rows)  (vt10x grid, right-trim, drop trailing empties)
                                   │ strings.Split(text,"\n")  ("" ⇒ nil)
                                   ▼
                                 *Grid{rows}
                                   ├─ Rows() []string
                                   ├─ ContainsInLastRows(sub, n) bool   (region-scoped)
                                   └─ RowHasPrefix(i, prefix) bool
```

Nothing consumes `*Grid` in this slice — the arrows into the detectors are slices 2-6.

## Concurrency model

None. `Grid` is an immutable value fully computed at construction from a caller-owned `[]byte` (`Buffer.Snapshot` already hands back a copy). No goroutines, no shared mutable state, no locks. A `*Grid` is safe to read from multiple goroutines and safe to pass by value of the pointer; the intended lifecycle is construct-per-snapshot, query, discard.

## Error handling

No error returns anywhere — mirrors `Render`, which swallows the vt10x write error and cannot fail a caller. Degenerate inputs are absorbed, not rejected:

| Input | Result |
|---|---|
| empty / whitespace-only snapshot | `Rows()` len 0; predicates return false |
| `cols` or `rows` == 0 | defaults applied by `Render` |
| `n <= 0` in `ContainsInLastRows` | false |
| `n` > row count | clamped to all rows |
| `i` out of range in `RowHasPrefix` | false (no panic) |

Out-of-range access returning false rather than panicking is a hard contract (AC), not a convenience — it lets migrated detectors probe rows without length guards.

## Testing strategy

All in `grid_test.go`, same `got != want` / `t.Errorf` idiom as the existing tests. Scenarios (developer writes the bodies):

- **Zero-dims parity** — mirror `TestRenderDefaultsForZeroDims`: `NewGrid(snap,0,0).Rows()` equals `NewGrid(snap,DefaultGridCols,DefaultGridRows).Rows()` for a non-empty snapshot. Confirms fallthrough is delegated, not reimplemented.
- **Rows shape** — for a snapshot rendering to content on row 0, a blank row 1, content on row 2: `Rows()` is `["...","","..."]` (3 entries, middle empty preserved, trailing empties already dropped by `Render`). Ties `Rows()` to `strings.Split(Render(...),"\n")`.
- **Empty render ⇒ zero rows** — `NewGrid([]byte(""),0,0).Rows()` has `len == 0` (not `[""]`). Pins the special-case.
- **Region distinction (headline AC — the whole point of the slice).** Two distinct byte snapshots, both containing the modal string in their raw bytes (so `strings.Contains(snap, modal)` is true for both):
  - Snapshot A renders the modal string into the grid's **bottom** region → `ContainsInLastRows(modal, k)` is **true**.
  - Snapshot B is A plus extra content rendered **below** the modal, pushing it **above** the last-`k` window → `ContainsInLastRows(modal, k)` is **false**, even though the raw `strings.Contains` over B still matches.
  Construct both as deterministic inline `[]byte` (newline-separated rows; no live PTY). This is the test that demonstrates screen-region vs. history-buffer semantics.
- **`ContainsInLastRows` boundaries (AC: pin both).** With a known multi-row grid: `n = 0` → false; `n = -1` → false; a string in row 0 with `n` == exactly the row count → true; the same with `n` >> row count → true (clamp). Include the "string is above the last-n window" negative to guard the region math.
- **`RowHasPrefix`** — in-range matching prefix → true; in-range non-matching → false; `i = -1` and `i = len(rows)` → false with no panic.

`make check` (build + vet + full test) must stay green; no existing test changes since no detector is touched.

## Open questions

- **Unexported field name** (`rows` vs `lines`) — cosmetic; `rows` reads naturally against the row-oriented API. Developer's call.
- **`Rows()` returning the backing slice vs a copy** — spec chooses the backing slice (documented read-only), matching how `AgentList`/`McpStatus` expose their slices directly. If a later slice ever needs a defensive copy, that is a cheap additive change then, not now — do not pre-build it (evidence-based: no mutating consumer exists).
- **Method-set placement of future region predicates** — slices 2-6 may want `FirstRowWithPrefix`, `ContainsInFirstRows`, etc. Out of scope here; add them when a detector migration actually needs them, not speculatively.

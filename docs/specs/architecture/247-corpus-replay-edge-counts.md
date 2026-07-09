# Spec #247 — corpus-replay: per-detector transition-edge counts

**Ticket:** [#247](https://github.com/pyrycode/tui-driver/issues/247)
**Size:** S — additive `edges` field threaded through the replay loop + one new
report section following the existing per-bucket table pattern. 2 production
files modified (`main.go`, `report.go`), 1 test file + 2 tiny fixtures added.
Not security-sensitive (report-only audit; a forged edge count routes nothing —
see memory `corpus-replay-tool-ticket-family`).

## Files to read first

- `cmd/corpus-replay/main.go:96-106` — `castResult`; add one field `edges
  map[string]int` (detector key → transition-edge count for this cast).
- `cmd/corpus-replay/main.go:168-237` — `replayCast`; the **two** classify points
  (in-loop at `:218-220` every `stride`, plus the unconditional final classify at
  `:226-227`) are where edge tracking threads in. Note `buf.Snapshot()` is the
  rolling `DefaultBufferCap` window the live detector sees.
- `cmd/corpus-replay/main.go:239-250` — `applyDetectors`; the active-key
  membership to reuse verbatim — flat detectors that fired, plus `modal:<class>`
  when `DetectModalClass != ModalClassUnknown`.
- `cmd/corpus-replay/report.go:47-148` — `report`; mirror the `fires
  map[string]map[string]int` + `tabwriter` + `buckets` iteration for the edge
  table. `sortedDetectors` (`:150-169`) gives the row ordering. The
  false-positive-suspects, anchor-in-content, and `-per-cast` blocks must stay
  byte-for-byte unchanged (AC3).
- `cmd/corpus-replay/report.go:23-45` — `bucketOf` / `buckets` / `isSuspect`.
  Edges use `bucketOf` for column assignment, but **do not** apply `isSuspect`'s
  idle/thinking exclusion — see the busy-axis decision below.
- `cmd/corpus-replay/main_test.go:12-65` — the `replayCast` fixture-test pattern
  to mirror for the new edge test. Leave `TestReplayCast_RealTrustDialogFires`
  and `TestReplayCast_ForgedHeaderAndRetiredTokenSuppressed` untouched (they
  assert `fired`, which is preserved).
- `cmd/corpus-replay/testdata/sample-ok.cast` — fixture format. Its frame-2
  content (`Quick safety check` header + a pointer-marked numbered option row)
  classifies as `modal:trust-folder` — proven by the existing passing test. The
  new fixtures reuse this exact modal content.
- `pkg/tuidriver/grid.go:39-73` — `Render`/`NewGrid` run the snapshot through
  **vt10x, a full VT100 emulator**, on every render. `\x1b[2J\x1b[H`
  (clear-display + cursor-home) resets the rendered grid, so a frame that opens
  with it fully repaints — the deterministic lever for flapping a detector on and
  off across sampled frames.
- `pkg/tuidriver/modal.go:219-257` — `DetectModalClass`; `trust-folder` is a
  full-panel class (`gridHasTrustDialog` matches anywhere in the visible grid),
  so the reused modal content classifies after a clear+repaint, and a cleared
  frame that shows only `❯ ready` returns `ModalClassUnknown`.
- `pkg/tuidriver/state.go:152-164` — `IsIdle`/`isIdleGrid`; idle fires on a
  bottom `❯` prompt and is false while a modal overlays. Confirms the non-modal
  frames of the flap fixture also fire (and flap) the `idle` key.
- `cmd/corpus-replay/README.md` — the self-reference warning. The edge report
  prints detector **keys** (`modal:permission`) and integer counts, not the
  literal claude anchors (`Do you want to proceed`), so it adds no new
  anchor-exposure class beyond the existing fires table.

## Context

The corpus-replay harness records only whether each detector fired *at least
once* per recording (`castResult.fired`, `applyDetectors`). The 2026-07-07
detection review needed **transition counts** to see two real defects — a
permission modal flapping shown/hidden six times in one recording, and the busy
axis flipping hundreds of times per run — and had to hand-build a throwaway
probe.

Flapping is a distinct failure signature from a single fire: a real dialog
detects once and stays; content forgeries and animation gaps flap. This ticket
makes that signature visible in the standard `make corpus-replay` output so the
next flapping regression is caught without re-deriving a one-off probe. Related
#259 (assert mode) may later consume per-key edge totals as a gate input; this
ticket is **report-only** — no new flags, no exit-code change.

## Two architect decisions the ticket defers

**1. The busy/idle axis IS counted.** The second named recording's defect is the
busy axis flipping hundreds of times; excluding `idle`/`thinking` would make that
defect invisible and defeat the ticket. So edge counting includes **every**
detector key — flat detectors (incl. `idle`, `thinking`) plus the active modal.
Note the deliberate asymmetry with the fires table: `idle`/`thinking` are
non-suspects for the false-positive section (a healthy run is idle), but they are
valid *flapping* axes for edges. Flapping ≠ false-positive; the false-positive
section keeps its `isSuspect` exclusion unchanged.

**2. Edges key on the existing `modal:<class>` key space**, not a single `modal`
key. Rationale: (a) reuses the key convention `fired` and the report's per-class
breakout already use; (b) attributes flapping to the specific class (permission
vs trust), which is exactly what the 2026-07-07 review needed to distinguish; (c)
a direct class→class transition (e.g. permission→trust with no idle between)
registers an edge on **both** keys — still the correct "both classes unstable"
signal. A single `modal` key would lose per-class attribution and not fit the
existing per-class report layout.

Both decisions collapse into one uniform mechanism: an **active-key-set diff**
between consecutive sampled ticks, over the same key space `applyDetectors`
already populates.

## Design

### Edge semantics

An **edge** on a key increments when that key's membership in the active-detector
set changes between two consecutive *sampled* ticks within one cast. Formally,
per cast, maintain the previous tick's active-key set `prev` (initialised
**empty**). At each sampled classification compute the current active set `cur`;
for every key in the symmetric difference `cur △ prev`, increment
`edges[key]`; then set `prev = cur`.

Consequences that satisfy the AC directly:

- **Fires once and stays → exactly 1.** First appearance is `{} → {key}` (one
  edge); staying present adds no further edges. Total = 1.
- **Flaps → > 1.** Each disappear or reappear adds an edge, so a key that toggles
  climbs past 1.
- **Duplicate final classify is idempotent.** When `stride` lands on the last
  event, the unconditional final classify (`main.go:226-227`) re-renders the same
  frame → `cur == prev` → empty symmetric difference → no spurious edge. So both
  classify points can run through the same helper unguarded.
- **Empty/edge-free casts.** An empty snapshot renders to a zero-row grid →
  empty `cur`; a cast with no output events never classifies → `edges` stays
  empty. No new failure modes.

### `main.go` changes

- `castResult` gains one field: `edges map[string]int` (key → edge count).
  Initialise it alongside `fired`/`anchors` in `replayCast` (`:181-186`).
- Extract the active-key membership into a helper that **returns** the set rather
  than mutating in place — contract:
  `func activeKeys(snap []byte) map[string]bool` — same membership
  `applyDetectors` computes today (flat detectors that fired, plus
  `modal:<class>` when not Unknown).
- Replace the two `applyDetectors(...)` call sites with a single per-cast helper
  that folds fires + edges — contract, one line of behaviour each:
  `func (r *castResult) classify(snap []byte, prev map[string]bool) map[string]bool`
  — computes `cur := activeKeys(snap)`; ORs `cur` into `r.fired` (preserves
  existing fires semantics); for each key in `cur △ prev` increments `r.edges`;
  returns `cur` as the next `prev`. `replayCast` threads `prev` (starting empty)
  through the loop and the final call.

This keeps the two classify points DRY and the fires behaviour byte-identical.
`applyDetectors`'s old signature disappears; both its call sites (only two, both
in `main.go`) move to `classify`. No other package references it.

### `report.go` changes — one new section, appended after anchor-in-content and
before the `-per-cast` block

Aggregate once, mirroring the existing `fires` loop:
`edgeSums map[string]map[string]int` — `for key, n := range r.edges {
edgeSums[key][bucketOf(r)] += n }` over all results.

Then print two things (AC2):

1. **Per-key edge totals table** — a `tabwriter` with the same column layout as
   the fires table (`DETECTOR`, `prod-ok`, `prod-err`, `e2e-ok`, `e2e-err`,
   `other`) plus a trailing `total` column so the "per-key total" is explicit.
   Rows ordered by `sortedDetectors` (idle/thinking lead, rest sorted). Section
   header names the semantic, e.g.:
   `transition edges (a detector flipping — or the active modal class changing —
   between consecutive sampled ticks; a real dialog fires once and stays at 1,
   flapping climbs):`

2. **Top-flappers list, broken out per (segment, tag) bucket** — for each bucket
   in `buckets`, collect `(shortName(cast), key, edgeCount)` for casts in that
   bucket with `edgeCount > 1` (i.e. more than a single fire-and-stay), sort
   descending by count, print the top **N = 5** per bucket. Skip buckets with no
   flappers; if none anywhere, print `  none`. The sub-header states the cap
   (`top 5 by edge count`) so the truncation is not silent. This is the line that
   surfaces "recording X, `modal:permission`, ×6" in the standard report.

The flap threshold `> 1` aligns exactly with AC4's synthetic test (flap > 1,
stable == 1).

`report`'s signature is unchanged — edges live inside `castResult`. Every
existing section and the `-per-cast` line format are untouched (AC3).

## Concurrency model

None. The tool is single-goroutine and sequential: `main` iterates casts, each
`replayCast` scans its file line-by-line into a rolling buffer. Edge tracking is
per-cast local state (`prev` set threaded through the classify calls). No shared
state, no goroutines, no shutdown sequence.

## Error handling

Edge tracking adds no new failure modes. `edges` is initialised at `castResult`
construction (nil-map writes avoided the same way `fired`/`anchors` are). The
duplicate final classify is idempotent (symmetric-difference empty). Empty
snapshots and zero-event casts produce empty `edges`. File/scan errors keep their
existing `replayCast` handling.

## Testing strategy

Add one test using **two new tiny synthetic fixtures** under `testdata/` (data,
not the external corpus — AC4). Both reuse `sample-ok.cast`'s proven
trust-folder modal content, and open each frame with `\x1b[2J\x1b[H` (written in
the `.cast` JSON string as `[2J[H`) so vt10x fully repaints per frame,
making the modal presence deterministic:

- `flap-err.cast` — sampled frames alternate: idle prompt → trust modal → idle
  prompt → trust modal. Assert `r.edges["modal:trust-folder"] > 1` (it appears
  and disappears repeatedly) and, to pin the busy-axis decision,
  `r.edges["idle"] > 1` (idle flaps inversely).
- `stable-ok.cast` — the trust modal is present from the first frame and stays
  for every sampled frame. Assert `r.edges["modal:trust-folder"] == 1` (one
  appearance, never toggles).

Test as bullet scenarios in the project's `replayCast`-fixture idiom (call
`replayCast(path, 1)`, assert on `r.edges[...]`), mirroring
`main_test.go:12-65`. Leave the two existing `replayCast` tests unchanged to
prove AC3's fires semantics are preserved.

Optionally (not required for AC): a light report-level smoke test that captures
`report(&buf, []castResult{flapResult}, ...)` output and asserts it contains the
`transition edges` header and the flapping cast name — confirms the section
renders. Keep it minimal.

`make check` (= `vet test`, claude-free) runs these via `go test ./...` and must
be green.

## Open questions

- **Top-flappers cap N.** Spec says 5 per bucket with the cap stated in the
  sub-header. If the real corpus proves 5 too shallow for a bucket, bump the
  constant — it is a display constant, no semantic weight.
- **README bullet.** Adding a "transition edges" bullet to `README.md`'s "What it
  reports" is a nice-to-have but **not an AC** and not required for `done` — the
  developer's worktree should stay to code + tests + this spec; the doc phase can
  fold it in post-merge.

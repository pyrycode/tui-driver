# corpus-sampler

Extracts the distinct **stable screens** out of the PTY recording corpus, with
provenance, so the offline model-labeling pass (#257) and fixture promotion
(#258) work from a low-thousands exemplar set instead of the corpus's ~4.5M raw
render events (#255).

A stable screen is a grid that sat unchanged through a quiet gap: whenever the
timestamp gap between two consecutive output events is at least `-gap`, the frame
painted by the earlier event waited out the quiet window and is emitted as one
sample. Whole-grid dedupe fails on the raw corpus (96.9% of per-event grids are
unique) because the rolling buffer window shifts on every output event; gating on
the inter-event time gap and deduping on a normalized hash (digit runs collapsed,
spinner glyphs unified, rows right-trimmed) pulls the distinct situations out.

Sibling of [`corpus-replay`](../corpus-replay) (#227) — it reuses the same
cast-directory listing, asciinema header parse, `-ok`/`-err` tag read, and
prod/e2e segment cues, and (for `-fires`) its detector predicate set. **Scope:**
quiet-gap stable screens plus, with `-final`, each cast's ending frame, plus,
with `-fires`, each frame a structural detector fired on; random mid-stream
slices are `-midstream` (#274).

## Run

```sh
go run ./cmd/corpus-sampler -dir /path/to/casts -out screens.jsonl
go run ./cmd/corpus-sampler -dir /path/to/casts -out screens.jsonl -gap 1s
go run ./cmd/corpus-sampler -dir /path/to/casts -out screens.jsonl -final
go run ./cmd/corpus-sampler -dir /path/to/casts -out screens.jsonl -fires
```

Flags:

- `-dir` (required): directory of `.cast` files.
- `-out` (required): output JSONL file. One line per distinct stable screen.
- `-gap` (default `500ms`): minimum inter-event quiet gap that marks a stable
  screen. The tool walks **every** event and gates on the time gap, never an
  event stride — a quiet dialog emits almost no events, so a stride steps over
  the exact stable screens this tool exists to catch.
- `-final` (default `false`): also emit each cast's final rendered frame as one
  sample (`source: ["final"]`). Catches how a run ended when the last events
  arrive in a burst with no trailing quiet gap. A cast with zero output events
  emits no final sample.
- `-fires` (default `false`): also emit a sample at each event where a structural
  detector newly fires — the same predicate set `corpus-replay` reports on (`idle`,
  `thinking`, the mcp-/network-failure banners, `unknown-dialog`, and each modal
  class). The sample's `source` carries `"fire"` plus the firing detector key
  (e.g. `["fire","idle"]`, `["fire","modal:trust-folder"]`). These are the direct
  false-positive-triage candidates for the #257 labeling pass — the exact screens
  a live run would have acted on, often mid-stream where quiet gaps are rare.

Each `-out` line carries full provenance: the raw rendered grid, its
normalized-grid hash, cast filename, output-event index, timestamp, cols, rows,
`ok`/`err` tag, prod/e2e segment, a cross-run `seen` count, and a `source` array
naming every rule that found this screen (`"gap"`, `"final"`, `"fire"` plus the
firing detector key; a screen found by more than one rule keeps the full sorted,
distinct set). `stdout` prints a single aggregate line — `casts events gaps
distinct` — and nothing else; `gaps` counts quiet-gap fires only, excluding any
`-final` or `-fires` sample.

Local audit tool only — no CI workflow (org rule).

## ⚠️ Self-reference

Sample grids quote detection anchor literals. Displaying them on screen mid-run
can false-fire the live detection (as with #152 / #154 / #155). This tool writes
grids to `-out` **only**; `stdout` carries aggregate counts only, and the tests
reference sample hashes and cast names, never grid content. Build and run it
**by hand**, off the agent pipeline.

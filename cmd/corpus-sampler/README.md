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
with `-fires`, each frame a structural detector fired on, plus, with
`-midstream N`, N deterministically chosen mid-stream frames per cast.

## Run

```sh
go run ./cmd/corpus-sampler -dir /path/to/casts -out screens.jsonl
go run ./cmd/corpus-sampler -dir /path/to/casts -out screens.jsonl -gap 1s
go run ./cmd/corpus-sampler -dir /path/to/casts -out screens.jsonl -final
go run ./cmd/corpus-sampler -dir /path/to/casts -out screens.jsonl -fires
go run ./cmd/corpus-sampler -dir /path/to/casts -out screens.jsonl -midstream 5
```

To promote a sampled screen to a committed fixture + manifest entry:

```sh
go run ./cmd/corpus-sampler -promote -in screens.jsonl -hash <hex> -dir /path/to/casts
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
- `-midstream N` (default `0`, off): also emit up to N deterministically chosen
  mid-stream frames per cast (`source: ["midstream"]`). Each output-event index
  is keyed by `sha256(cast-name + index)`; the N smallest keys are selected, so
  re-running over the same input directory reproduces the identical distinct
  set — no wall-clock time, no RNG. A cast with fewer than N eligible events
  emits all of them, no padding. Surfaces streaming-state variety that lives
  between output bursts, where the quiet-gap rule rarely fires (the #243
  dot-spinner frame is the motivating case — it went uncounted in the original
  glyph census because it appears mid-stream, not at a quiet wait-state).

## Promote mode (`-promote`)

`-promote` turns one triaged screen into a permanent regression: a committed
raw-snapshot fixture under `pkg/tuidriver/testdata/corpus/` plus an entry in
`testdata/corpus/manifest.json` recording the expected per-axis classification
(modal class, busy, idle, and the mcp-/network-failure and unknown-dialog
banners) and provenance (cast, event index, hash). A single table-driven test
(`pkg/tuidriver/corpus_fixtures_test.go`) then classifies every manifest entry
through the same detector path the existing `testdata/*.bin` fixtures use — a new
fixture needs zero new test code.

The sampler JSONL stores only the *rendered* grid + its hash, never the raw
buffer bytes the detectors take, so promote **reconstructs** the raw snapshot by
replaying the source cast to the recorded event index and taking the buffer
snapshot — hence it needs `-dir` (the recordings) as well as `-in` (the JSONL).
It verifies the reconstruction against the recorded hash before writing anything,
so a stale recordings dir aborts cleanly.

Flags (promote mode):

- `-promote`: select promote mode.
- `-in` (required): the sampler JSONL to read the screen's provenance from.
- `-hash` (required): the normalized-grid hash of the screen to promote (the
  `hash` field of an `-out` line).
- `-dir` (required): the recordings directory holding the source cast.
- `-outdir` (default `pkg/tuidriver/testdata/corpus`): target fixture directory,
  so a bare run from the repo root writes into the tree you then `git add`.

Re-running promote for a hash already in the manifest is a no-op: the fixture is
rewritten byte-identical and the manifest is left untouched, so a
human-confirmed axis value is never clobbered.

### ⚠️ Mandatory human review before committing a promoted fixture

The promoter **pre-fills** each axis with the detector's *current* verdict as a
convenience, but the committed value is the **human's confirmed call** — that is
the review step. Before `git add`-ing a promoted fixture, a human MUST eyeball
it for:

- **Privacy** — a raw snapshot can contain paths or prompt content from the
  source recording. Do not commit anything that should not be public.
- **Anchor content** — the fixture embeds detection-anchor literals by design
  (exactly like the existing committed fixtures). The test harness references
  fixtures by path and never prints their bytes for this reason.

The seed screens are chosen so detector and human agree, so `make check` stays
green. Promoting a *disagreement* screen (an independent label vs. the detector
verdict — the tool's eventual purpose) would legitimately turn the suite **red**
as a documented detector bug; recording the human's call and pinning it is how
that gets tracked, and is out of scope for the seed set.

Each `-out` line carries full provenance: the raw rendered grid, its
normalized-grid hash, cast filename, output-event index, timestamp, cols, rows,
`ok`/`err` tag, prod/e2e segment, a cross-run `seen` count, and a `source` array
naming every rule that found this screen (`"gap"`, `"final"`, `"fire"` plus the
firing detector key, `"midstream"`; a screen found by more than one rule keeps
the full sorted, distinct set). `stdout` prints a single aggregate line —
`casts events gaps distinct` — and nothing else; `gaps` counts quiet-gap fires
only, excluding any `-final`, `-fires`, or `-midstream` sample.

Local audit tool only — no CI workflow (org rule).

## ⚠️ Self-reference

Sample grids quote detection anchor literals. Displaying them on screen mid-run
can false-fire the live detection (as with #152 / #154 / #155). This tool writes
grids to `-out` **only**; `stdout` carries aggregate counts only, and the tests
reference sample hashes and cast names, never grid content. Build and run it
**by hand**, off the agent pipeline.

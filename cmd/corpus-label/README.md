# corpus-label

Runs a resumable, subscription-login **labeling queue** over the
[`corpus-sampler`](../corpus-sampler) output JSONL (#255, enriched by #272–#274):
a judge **independent of the structural detectors** classifies each sampled screen
ahead of fixture promotion (#258, #257).

A presweep that classified with the code under test could only re-discover what the
detectors already see — a blind spot would dedupe into the boring bucket. So the
judge is a headless `claude` reading the raw grids, and each screen gets exactly one
label from the known-situation taxonomy plus a free-text `unusual: <description>`
escape hatch. That escape hatch is the whole point: it surfaces situations no enum
value covers (the dot-frame spinner gap #243 was found exactly this way, by reading
frames rather than by any detector).

Sibling of [`corpus-sampler`](../corpus-sampler) (#255) and
[`corpus-replay`](../corpus-replay) (#227): a self-contained leaf CLI, local audit
tool, no CI workflow (org rule), run **by hand** off the agent pipeline.

## Operator run

> **Subscription login only — never set `ANTHROPIC_API_KEY`.** The child `claude`
> runs on the operator's ambient subscription login. If `ANTHROPIC_API_KEY` is
> present in the environment, `claude` flips billing to **metered** — the exact
> outcome this design avoids. corpus-label **removes** the variable from the child
> environment as a safety net, but the operator must not rely on that: run it in a
> shell where the key is unset. Operator decision 2026-07-09: **no API spend.**

```sh
# First (and resumed) passes — default model haiku, over all unlabeled screens:
go run ./cmd/corpus-label -in screens.jsonl -out labels.jsonl -park parked.jsonl

# A subscription window can cap mid-run. Just re-run the same command — it
# continues from the first unlabeled screen, re-labeling nothing:
go run ./cmd/corpus-label -in screens.jsonl -out labels.jsonl -park parked.jsonl

# Re-pass the low-confidence and unusual screens with a stronger model:
go run ./cmd/corpus-label -in screens.jsonl -out labels.jsonl -park parked.jsonl -model sonnet
```

Flags:

- `-in` (required): `corpus-sampler` output JSONL to label.
- `-out` (required): labels JSONL — one line per labeled screen, **and** the resume
  source. Stores hashes (never grids); #258 joins labels back to grids on `hash`.
- `-park` (required): park JSONL for batches that fail to parse twice (below).
- `-batch` (default `15`): screens per `claude -p` call. Must be in `[10,20]`.
- `-model` (default `haiku`): model passed to `claude -p`. A **non-default** value
  switches to **re-pass mode**: instead of the unlabeled screens, it selects the
  existing `-out` records that are low-confidence (`< -min-confidence`) or `unusual:`,
  re-labels them, and overwrites their entries. High-confidence records are untouched.
- `-min-confidence` (default `0.7`): the re-pass selection threshold.

## How it works

Sequential — one batch at a time, no goroutines. The subscription login is a single
serialized resource, and resume correctness wants ordered, flush-per-batch
persistence; a parallel pool (unlike [`corpus-replay`](../corpus-replay)'s
`-workers`) could leave a mid-run gap resume can't reason about.

```
read -in ─► resume/re-pass select ─► priority order ─► batch ─► [ label ─► persist | park ] loop ─► counts
```

- **Resume** walks `-in`, skipping any screen whose normalized hash already appears
  in `-out`, and de-dupes within the run (first wins) so each hash is queued at most
  once. Labels are flushed **per batch**, so a mid-run kill loses at most the
  in-flight batch — no completed batch's results are lost, no screen is re-labeled.
- **Priority** orders each pass into three tiers, stable within each: detector-fire
  samples first (`source` contains `"fires"`, #273), then error-tagged samples
  (`tag == "err"`), then the rest. Provenance is read tolerantly — an absent field
  leaves that tier empty rather than failing.
- **Label** builds one prompt per batch (allowed labels + the `unusual:` instruction
  + a JSON-array output contract, each screen tagged with its hash and raw grid on
  the child's stdin), spawns `claude -p --model <model>`, and validates the response:
  every batch hash present exactly once, each label in the taxonomy or `unusual:`,
  each confidence in `[0,1]`.
- **Park** — a malformed batch is retried once; still malformed (or a spawn / non-zero
  exit) → the batch's screens are written to `-park` with the verbatim model
  response, for manual review. Never dropped, never crashing the run. Parked screens
  are **not** written to `-out`, so a later run re-attempts them.

`stdout` prints a single aggregate line — `mode in queued batches labeled parked` —
and nothing else.

## Taxonomy

The enum is the situation classes the detectors already name, kept in sync with
`pkg/tuidriver`: the modal classes (`permission`, `trust-folder`, `mcp`,
`slash-picker`, `ask-user-question`, `model-select`, `permissions-config` — built
from the `ModalClass` constants, excluding the retired `agents`), plus `idle`,
`busy`, `mcp-failure`, `network-failure`, and `startup` (the pre-first-prompt
family). A label is valid iff it is one of those **or** begins with `unusual:`.

## ⚠️ Self-reference

Sample grids quote detection anchor literals. Displaying them on screen mid-run can
false-fire the live detection (as with #152 / #154 / #155). This tool sends grids
**only** to the child `claude`'s stdin; `-out` stores hashes (never grids), `stdout`
carries aggregate counts only, and the tests reference sample hashes, never grid
content. Fixture grids in `testdata` are synthetic (no real anchor literals). Build
and run it **by hand**, off the agent pipeline.

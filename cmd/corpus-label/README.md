# corpus-label

Runs a resumable, subscription-login **labeling queue** over the
[`corpus-sampler`](../corpus-sampler) output JSONL (#255, enriched by #272–#274):
a judge **independent of the structural detectors** classifies each sampled screen
ahead of fixture promotion (#258, #257).

A presweep that classified with the code under test could only re-discover what the
detectors already see — a blind spot would dedupe into the boring bucket. So the
judge reads the raw grids, and each screen gets exactly one label from the
known-situation taxonomy plus a free-text `unusual: <description>` escape hatch. That
escape hatch is the whole point: it surfaces situations no enum value covers (the
dot-frame spinner gap #243 was found exactly this way, by reading frames rather than
by any detector).

Sibling of [`corpus-sampler`](../corpus-sampler) (#255) and
[`corpus-replay`](../corpus-replay) (#227): a self-contained leaf CLI, local audit
tool, no CI workflow (org rule), run **by hand** off the agent pipeline.

## Why `pyry agent-run`, not `claude -p`

Each batch is judged by one fresh `pyry agent-run`. That verb spawns interactive
claude through pyry's pseudo-terminal, delivers the batch, returns the result, and
the session ends. It is the one blessed way to run claude on the operator's
subscription without metered spend — the same path the agent dispatchers use every
day.

A print-mode `claude -p` call bills per token against the metered API. Over the full
~4000-batch run that is thousands of metered calls, the exact outcome the design set
out to avoid. Unsetting `ANTHROPIC_API_KEY` does **not** fix it — print mode itself is
the metered path. So corpus-label never runs `claude -p`; it shells out to
`pyry agent-run`, whose default path is subscription-billed.

corpus-label depends on an installed `pyry` binary that supports `agent-run` with the
current flag set (`--prompt-file`, `--system-prompt-file`, `--allowed-tools`,
`--max-turns`, `--effort`, `--model`, `--workdir`, `--output-format stream-json`).
That contract is the dispatcher's core and is stable.

## Operator run

> **Subscription login only — never meter.** Run under `op run` with the durable
> 1Password token in `CLAUDE_CODE_OAUTH_TOKEN`, in a shell where `ANTHROPIC_API_KEY`
> is unset, against a `pyry` binary that has `agent-run`. corpus-label removes
> `ANTHROPIC_API_KEY` and `PYRY_USE_STREAMJSON` from the child environment as a
> safety net (either would flip billing off the subscription), but the operator must
> not rely on that. Use the **durable** token, not the rotating Keychain login: the
> Keychain access token expires after about 8 hours and 401s, which would break a
> multi-day run. Operator decision 2026-07-09: **no API spend.**

```sh
# First (and resumed) passes — default model haiku, over all unlabeled screens:
op run -- go run ./cmd/corpus-label -in screens.jsonl -out labels.jsonl -park parked.jsonl

# A subscription window can cap mid-run, or Ctrl-C stops it cleanly. Just re-run the
# same command — it continues from the first unlabeled screen, re-labeling nothing:
op run -- go run ./cmd/corpus-label -in screens.jsonl -out labels.jsonl -park parked.jsonl

# Re-pass the low-confidence and unusual screens with a stronger model:
op run -- go run ./cmd/corpus-label -in screens.jsonl -out labels.jsonl -park parked.jsonl -model sonnet
```

`op run` resolves `CLAUDE_CODE_OAUTH_TOKEN` from the durable 1Password item at launch
and freezes it for the process, so a rotating Keychain login expiring mid-run cannot
401 the job. If a transient 401 does hit, the batch fails, parks, and re-attempts on
the next relaunch — no screen is lost.

Flags:

- `-in` (required): `corpus-sampler` output JSONL to label.
- `-out` (required): labels JSONL — one line per labeled screen, **and** the resume
  source. Stores hashes (never grids); #258 joins labels back to grids on `hash`.
- `-park` (required): park JSONL for batches that fail to parse twice (below).
- `-batch` (default `15`): screens per `pyry agent-run` batch. Must be in `[10,20]`.
- `-model` (default `haiku`): model passed to `pyry agent-run`. A **non-default** value
  switches to **re-pass mode**: instead of the unlabeled screens, it selects the
  existing `-out` records that are low-confidence (`< -min-confidence`) or `unusual:`,
  re-labels them, and overwrites their entries. High-confidence records are untouched.
- `-min-confidence` (default `0.7`): the re-pass selection threshold.
- `-effort` (default `low`): thinking effort passed to `pyry agent-run`. A
  read-and-label task needs no more.
- `-workdir` (default `.`): working directory for `pyry agent-run`. Must exist. pyry
  pre-marks it trusted in `~/.claude.json`; defaulting to the already-trusted current
  directory keeps that a no-op.
- `-pyry` (default `pyry`): the `pyry` binary path. A test injection seam.

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
  + a JSON-array output contract, each screen tagged with its hash and raw grid),
  writes it to a temp `--prompt-file`, and runs one fresh `pyry agent-run`. It reads
  the judge's reply from the trailing `type:"result"` stream-json line and validates
  it: every batch hash present exactly once, each label in the taxonomy or `unusual:`,
  each confidence in `[0,1]`.
- **Park** — a malformed batch is retried once; still malformed (or a non-zero pyry
  exit, a per-batch timeout, or an error-typed result) → the batch's screens are
  written to `-park` with the model's verbatim reply, for manual review. Never
  dropped, never crashing the run. Parked screens are **not** written to `-out`, so a
  later run re-attempts them.

Ctrl-C (SIGINT/SIGTERM) cancels the in-flight batch and exits cleanly; the cancelled
batch is neither persisted nor parked, and resume re-does it next run.

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
**only** to the judge's `--prompt-file` (a temp file removed after each batch);
`-out` stores hashes (never grids), `stdout` carries aggregate counts only, and a
park record carries only the model's reply or a grid-free error — never grid content.
pyry's stream-json stdout **does** echo the delivered prompt (grids) as a
`type:"user"` line, so corpus-label parses only the `result` line and never surfaces
raw pyry stdout into any record. Fixture grids in `testdata` are synthetic (no real
anchor literals). Build and run it **by hand**, off the agent pipeline.

## Real-pyry smoke run

The one new real-stack risk is that the `pyry agent-run` flags and result parsing are
wired correctly. Prove it once before the big job, so the ~4000-batch run is never the
first real execution:

```sh
# 1. Build a pyry that has agent-run (from the pyrycode repo):
#      (cd ../pyrycode && go build -o /tmp/pyry ./cmd/pyry)
# 2. Label a 3-screen synthetic fixture with the real pyry under the durable token:
op run -- go run ./cmd/corpus-label \
  -in cmd/corpus-label/testdata/smoke-3.jsonl \
  -out /tmp/smoke-labels.jsonl \
  -park /tmp/smoke-park.jsonl \
  -pyry /tmp/pyry
```

Expect the aggregate line to report `labeled=3 parked=0`, and `/tmp/smoke-labels.jsonl`
to hold three records, each with a `hash` and **no** grid content. `make check` stays
fast and claude-free; this smoke is opt-in and operator-run.

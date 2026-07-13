# 287 — corpus-label: run each batch via `pyry agent-run` (subscription-billed)

**Ticket:** #287 · **Size:** S · **Security-sensitive:** no (offline operator tool; a forged
label routes nothing live — same posture as sibling corpus tools). **Supersedes** the print-mode
(`claude -p`) labeling mechanism from #257.

## Context

`corpus-label` (#257) classifies the sampled corpus screens with a model judge that reads raw grids
independent of the detectors. As built it ran each batch as `claude -p`, which is print mode. Print
mode bills per token against the metered API, so the full ~4000-batch run would be ~4000 metered
calls — the exact outcome the design set out to avoid. Unsetting `ANTHROPIC_API_KEY` does not fix it;
print mode itself is the metered path.

`pyry agent-run` is the one blessed way to run claude on the subscription without metered spend. Its
default path spawns interactive claude through pyry's pseudo-terminal, delivers the prompt, tails the
session, and re-emits stream-json — the same path the dispatchers bill correctly on every day. The
rework swaps the per-batch call for a fresh `pyry agent-run`; everything downstream of "raw model
text" is unchanged.

## Files to read first

- `cmd/pyry/agent_run.go` (pyrycode repo) — the `pyry agent-run` flag surface. Required flags:
  `--prompt-file`, `--system-prompt-file`, `--allowed-tools` (non-empty), `--max-turns` (>0),
  `--effort` (`low|medium|high|xhigh|max`), `--model`, `--workdir` (must exist),
  `--output-format stream-json`. Default path is subscription-billed; `PYRY_USE_STREAMJSON=1` selects
  the metered print-mode fallback. The verb pre-marks the workdir trusted and writes the deny-default
  settings itself, so corpus-label synthesizes none of that.
- `internal/agentrun/streamjson/emitter.go` (pyrycode repo) — the `type:"result"` trailer. The
  model's final text is its `result` field; `is_error`/`subtype` classify a wedge. `Emit` re-emits
  every session entry verbatim, including the `type:"user"` prompt echo (the grids) — the reason
  corpus-label must parse only the result line and never surface raw stdout.

## Design

Same package (`cmd/corpus-label`, `package main`). The queue, resume, re-pass, park, priority,
taxonomy, and validation code do not change. Only the per-batch call changes.

### The one-method seam (`label.go`)

`labeler` calls a `runner` whose sole method turns a prompt into the model's raw reply:

```go
type runner interface { run(ctx context.Context, prompt string) (string, error) }
```

`labelBatch` keeps its build-prompt / retry-once / park logic; the only edits are threading a
`context.Context` and swapping the old `spawn` call for `l.runner.run(ctx, prompt)`.

### Production runner (`pyryrun.go`)

`pyryRunner.run` per call: write the batch prompt to a temp `--prompt-file`; invoke `pyry agent-run`
with the flags below and the child environment scrubbed; capture stdout; parse the trailing result
line; return its `result` text. A per-batch deadline (`batchTimeout`) bounds a hung call. A non-zero
exit, a timeout, an error-typed trailer, or a missing result line returns an error, which the
existing `labelBatch` already treats as a failed attempt → retry once → park.

Small pure helpers, each unit-tested:

- `buildAgentRunArgs` — the flag list, pinning `--output-format stream-json`, `--model`,
  `--max-turns`, and a fixed minimal `--allowed-tools`.
- `parseResultText` — the `result` field of the last `type:"result"` line; errors if absent or
  error-typed; ignores every non-result line (init, per-turn events, the grid-bearing user echo).
- `childEnv` — strips `ANTHROPIC_API_KEY` and `PYRY_USE_STREAMJSON`, keeps everything else including
  `CLAUDE_CODE_OAUTH_TOKEN`. Replaces the old single-variable `scrubEnv`.

`--max-turns` is `labelMaxTurns = 4`. pyry counts logical assistant turns and fires the max-turns
stop at the cap even on the end-of-turn entry, so a budget of 1 would misclassify a clean single-turn
answer as an error. A read-and-label reply is one logical turn; the slack lets it complete while still
bounding a runaway.

### Minimal system prompt

A fixed system prompt is written once to a temp file at labeler init and reused: it tells the judge to
use no tools and respond only with the requested JSON array. This keeps the taxonomy prompt in
`buildPrompt` unchanged and steers claude to a pure text answer; the small `--max-turns` backs it up.
The minimal `--allowed-tools` only satisfies pyry's non-empty requirement — the judge is told not to
use it.

### Context and signals (`main.go`)

`main` builds the root context with `signal.NotifyContext` for SIGINT/SIGTERM, so Ctrl-C cancels the
in-flight batch and exits cleanly. The cancelled batch is neither persisted nor parked; resume
continues from the first unlabeled screen, which suits the fanless-Air multi-day run.

### Flags

`-claude` becomes `-pyry` (path to the pyry binary, default `pyry`). New: `-effort` (default `low`)
and `-workdir` (default `.`, the already-trusted current dir). `-batch` stays `[10,20]`; `-model`
still switches to re-pass mode on a non-default value.

### Resumability across a multi-day run

The run is kill-and-relaunch safe. On relaunch, resume reads `-out`, skips every already-labeled hash,
and continues from the first unlabeled screen; labels flush per batch, so a crash loses at most the
in-flight batch (which is re-done, not lost). A batch that fails twice parks to `-park` and never
reaches `-out`, so the next run re-attempts it — this is how a transient auth 401 is absorbed. Ctrl-C
cancels the in-flight batch cleanly (neither persisted nor parked).

The re-pass rewrite is made crash-atomic: `rewriteLabels` writes to a temp file in the same directory,
fsyncs, then renames over `-out`. A kill mid-write leaves the temp file and the previous `-out`
intact, so re-pass never truncates the labels file. The bulk haiku pass is append-only and already
safe. One residual edge left as-is: a kill mid-append can leave a torn final line in `-out`, and
`readLabels` treats a corrupt line as fatal (deliberately — a garbled resume source must not silently
re-label everything), so resume then needs that one torn line removed by hand.

## Billing invariant

The billing invariant is: run the default `pyry agent-run` path with `ANTHROPIC_API_KEY` and
`PYRY_USE_STREAMJSON` both absent from the child environment. `childEnv` enforces it, and the fake
`pyry agent-run` in tests asserts it (either variable present → non-zero exit → the batch parks and
the driving test fails loudly). Auth is ambient: run under `op run` with the durable 1Password token
in `CLAUDE_CODE_OAUTH_TOKEN`, since the rotating Keychain login expires after ~8h and would 401 a
multi-day run.

## Self-reference discipline

pyry's stream-json stdout echoes the delivered prompt (grids) as a `type:"user"` line, so grid content
IS present in the captured stdout. Enforce, same as before: `run` never returns raw stdout — only the
parsed `result` text (grid-free) on success, or a grid-free error otherwise. `-out` stores hashes, a
park record carries only the model's reply or an error, `stdout` carries aggregate counts, and tests
reference hashes, never grid content. The prompt temp file (which carries grids) lives only in the OS
temp dir for the call and is removed after.

## Testing strategy

The re-exec fake ports from a fake `claude -p` to a fake `pyry agent-run`: driven by `-pyry = os.Args[0]`,
it asserts the billing invariant, reads the prompt from `--prompt-file`, echoes it as a user line (so
every scenario proves grids stay out of park records), and emits a stream-json result line whose
`result` is the scenario response. Every prior scenario survives (valid, retry-then-valid, malformed,
bad label, bad confidence, missing hash, low confidence, unusual, and the resume / absent-out /
parked-batch / re-pass end-to-end cases), plus new direct unit tests for `buildAgentRunArgs`,
`parseResultText`, `childEnv`, and a park-never-leaks-grids assertion. `make check` stays fast,
claude-free, and pyry-free.

## Real-stack proof

A documented smoke run in the README labels a 3-screen synthetic fixture
(`testdata/smoke-3.jsonl`) with the real pyry binary under the durable token, expecting
`labeled=3 parked=0` and three grid-free records. The operator runs it once before the ~4000-batch
job, so the big run is never the first real execution.

## Out of scope

The two-pass run (haiku over the full set, then a sonnet re-pass on the below-0.7 and unusual screens),
the disagreement triage against `corpus-replay`, and fixture promotion via `corpus-sampler -promote`
are later tasks on a separate go-ahead.

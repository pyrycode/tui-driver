# 257 — corpus-label: resumable operator-run labeling queue over sampled screens

**Ticket:** #257 · **Size:** S (confirmed) · **Security-sensitive:** no (offline operator tool; a
forged label routes nothing live — same posture as sibling corpus tools #247/#255).

## Files to read first

Load these before writing. Each entry says what to extract; you do not need to read whole files.

- `cmd/corpus-sampler/main.go:53-76` — the `sample` struct. This is the **exact input JSONL schema**
  corpus-label decodes (`hash`, `grid`, `cast`, `tag`, `source []string`, plus `segment`/`seen`/… we
  ignore). Copy the field subset we consume; a `package main` cannot import another's types.
- `cmd/corpus-sampler/main.go:78-114` — `main()`: flag declare → required-flag check → `os.Exit(2)` on
  usage error, `os.Exit(1)` on runtime error, single aggregate `stdout` line at the end. Mirror this shape.
- `cmd/corpus-sampler/main.go:356-395` — `writeSamples` (JSONL marshal-one-object-per-line + buffered
  flush, deferred `Close` that surfaces the close error) and `tagFromName` (the `tag` values are
  `"ok"`/`"err"`/`"untagged"`). Reuse the JSONL-write idiom for the labels and park files.
- `pkg/tuidriver/modal.go:22-48` — the `ModalClass` constants. These are the **modal half of the
  taxonomy**; import `pkg/tuidriver` and build the enum from `string(tuidriver.ModalClass…)` so the label
  vocabulary stays in sync with the detectors. **Exclude** `ModalClassAgents` (retired #245 — cannot
  render on the pinned claude) and `ModalClassUnknown` (empty string).
- `pkg/tuidriver/state.go:152` (`IsIdle`), `:174` (`IsThinking`) — the concepts behind the `idle`/`busy`
  labels. **Reference only — do NOT call any detector.** Running the code-under-test to pre-classify would
  defeat the independent-judge premise (see Context).
- `pkg/tuidriver/network.go:43` (`HasNetworkFailure`), `pkg/tuidriver/mcp_banner.go:61`
  (`HasMcpFailureBanner`) — concepts behind the `network-failure` / `mcp-failure` labels (not string
  constants in the library, so corpus-label names them).
- `pkg/tuidriver/ready.go:1-40` — `Readiness`: the concept behind the single `startup` label (the
  pre-first-prompt family).
- `cmd/corpus-replay/main_test.go:1-60` — the table/fixture test idiom for this repo (`fired`/`anchors`
  maps; assertions reference sample **name/hash, never grid content**). Follow it.
- `cmd/spike-one-turn/main.go:112` and `cmd/e2e-runner/main.go:593-594` — the `exec.Command("claude", …)`
  spawn and `cmd.Env = append(os.Environ(), …)` idiom the **scrubbed-env** spawn mirrors (our variant
  *filters* rather than appends).
- `CLAUDE.md` "Scope discipline" + the Drop-In Contract bullet — why the API-key env scrub is
  load-bearing (subscription billing flips to metered when the key is present in the child env).
- `.gitignore:27-32` — the `*.cast` global ignore + `!cmd/*/testdata/*.cast` negation. **NOTE:**
  corpus-label's fixtures are `*.jsonl` (sampler-output-shaped), which are **not** gitignored — do **not**
  add a `.cast` negation for this tool. Keep fixture grids synthetic (no real anchor literals).

## Context

The sampled screens (`cmd/corpus-sampler` output JSONL, #255, enriched by #272–#274) need a one-time
classification by a judge **independent of the structural detectors**, ahead of fixture promotion (#258).
A presweep that classifies with the code under test can only re-discover what the detectors already see —
a blind spot dedupes into the boring bucket. So the judge is a headless `claude` reading the raw grids,
and the whole point of the free-text `unusual:` escape hatch is to surface situations no enum value covers
(the dot-frame spinner gap #243 was found exactly this way — by reading frames, not by any detector).

Operator decision 2026-07-09: **no API spend.** Labeling runs headless on the operator's `claude`
subscription login. A subscription window can cap mid-run, so the queue must be **resumable**: a capped
run just pauses and the next run continues from the first unlabeled screen. The tool is a self-contained
leaf CLI, sibling to `corpus-sampler` (#255) and `corpus-replay` (#227): local audit tool, no CI workflow
(org rule), run by hand off the agent pipeline.

**This is a data-schema soft dependency on #272/#273, not a build dependency.** corpus-label reads a JSON
file, not a Go API, so it is fully buildable and testable now against synthetic JSONL. The detector-fire
priority tier simply populates once the operator re-samples with the richer `-fires` source. No native
blocker is set (and none should be).

## Design

### Package layout (3 production files, one `package main`)

The ticket names the natural seam: a **deterministic queue** (read / resume / priority / batch /
persist / park) versus the **claude batch-labeler** (spawn / prompt / parse / retry / park-decision).
Split along it:

- `cmd/corpus-label/main.go` — flags, wiring, aggregate `stdout` progress. (~70 LOC)
- `cmd/corpus-label/queue.go` — input decode, resume filter, priority ordering, batching, labels/park
  persistence. Pure, deterministic, no subprocess — fully unit-testable without a fake binary. (~160 LOC)
- `cmd/corpus-label/label.go` — taxonomy set, prompt build, env scrub, `claude -p` spawn, response parse +
  validate, retry-once/park orchestration. (~175 LOC)

This file split maps 1:1 to the test split: `queue_test.go` needs no fake claude; `label_test.go` drives
the fake. If the developer's realized total blows past the corpus-replay envelope (~690 prod / ~920 total
LOC, which shipped clean as one S), that is the pre-identified route-back seam — but the sketch lands
inside that envelope, so ship as one ticket.

### Data types (all unexported — `package main` exports nothing)

- `sample` — the decoded input record. Fields consumed: `Hash string`, `Grid string`, `Cast string`,
  `Tag string`, `Source []string`. Copy the json tags from `corpus-sampler`'s `sample`
  (`main.go:53-76`). Tolerant decode is free: `json.Unmarshal` leaves missing fields zero — an absent
  `source` → `nil` → fire tier empty; an absent `tag` → `""` → err tier empty. **This is the AC's
  "degrades gracefully" mechanism — no explicit presence checks needed.**
- `labelRecord` — one persisted label: `Hash`, `Label`, `Confidence float64`, `Model string`, `Cast`,
  `Tag`. **Grid is deliberately NOT stored** — #258 joins labels to grids on `hash` against the sampler
  file; duplicating grids bloats the file and widens the self-reference surface.
- `parkRecord` — one parked batch: `Hashes []string`, `Raw string` (verbatim model response),
  `Attempts int`, `Model string`.

### Taxonomy (single source of truth = the library)

Build one `map[string]bool` (or sorted slice for the prompt) at startup from `pkg/tuidriver`:

- Modal half: `string(tuidriver.ModalClassPermission)`, `…TrustFolder`, `…MCP`, `…SlashPicker`,
  `…AskUserQuestion`, `…ModelSelect`, `…PermissionsConfig`. Excluding `…Agents` and `…Unknown`.
- Non-modal half (corpus-label's own constants, each commented with the library concept it maps to):
  `idle` (`IsIdle`), `busy` (`IsThinking` — library term is "thinking"; label is our vocabulary),
  `mcp-failure` (`HasMcpFailureBanner`), `network-failure` (`HasNetworkFailure`), `startup` (`Readiness`
  pre-first-prompt family — one label collapses the family; splitting sub-states is deferred, the
  `unusual:` bucket catches any pre-first-prompt screen the judge finds distinct).

**Validation contract:** a label is valid iff `taxonomy[label]` OR `strings.HasPrefix(label, "unusual:")`.
Confidence is valid iff present and in `[0,1]`.

### CLI contract

| Flag | Default | Meaning |
|------|---------|---------|
| `-in` | (required) | sampler output JSONL to label |
| `-out` | (required) | labels JSONL (also the resume source) |
| `-park` | (required) | park JSONL for batches that fail twice |
| `-batch` | `15` | screens per `claude -p` call (AC range 10–20; reject outside) |
| `-model` | `haiku` | model passed to `claude -p`; a non-default value switches to **re-pass mode** (below) |
| `-min-confidence` | `0.7` | re-pass selection threshold |
| `-claude` | `claude` | claude binary path — **injection seam for the fake-binary tests** |

`main` validates required flags and `-batch` ∈ [10,20] with `os.Exit(2)` (mirror
`corpus-sampler/main.go:85-89`); runtime errors `os.Exit(1)`.

### Data flow (sequential — no goroutines)

```
read -in ──► resume filter ──► priority order ──► batch ──► [ label batch ──► persist | park ] loop ──► counts
```

1. **Read** `-in` line by line into `[]sample` (skip blank/malformed lines with a stderr note, tolerant —
   one bad line never aborts, mirroring `corpus-sampler`'s cast-skip).
2. **Resume filter.** Read `-out` if it exists, collect the set of already-labeled `hash`es, drop any
   input sample whose hash is in the set. De-dupe within the run by hash too (first wins) so each hash is
   queued at most once → "exactly one label per hash". A missing `-out` file = empty set = full run.
3. **Priority order** into three tiers, **stable within each tier** (preserve input order for
   determinism):
   - fire: `source` contains the fires marker (const `sourceFires = "fires"`, commented as the #273
     `-fires` source value — confirm against #273 when it merges; empty tier today is the graceful case);
   - err: not fire AND `tag == "err"`;
   - rest: everything else.
   A screen that qualifies for fire is not also placed in err (highest tier wins).
4. **Batch** the ordered list into chunks of `-batch`.
5. **Label loop** — for each batch, in order: label it, then **persist-or-park before moving on**
   (per-batch flush is what makes resume lose at most the in-flight batch).

### The batch labeler (`label.go`)

`labelBatch(batch []sample) (labels []labelRecord, parked *parkRecord)` — signature; behavior:

- **Prompt.** Build a single prompt: the allowed-label list + the `unusual:` instruction + a request to
  emit **only** a JSON array `[{"hash","label","confidence"}, …]`, one object per screen, each screen
  presented tagged with its `hash` and its raw `grid`. Grids flow to the child's **stdin only** (never
  argv, never stdout).
- **Spawn.** `exec.Command(claudePath, "-p", "--model", model)` (confirm the exact print/model flags
  against the installed claude; `-p`/`--print` reading the prompt on **stdin** is the contract), stdin =
  the prompt, capture stdout. `cmd.Env = scrubEnv(os.Environ())`.
- **Env scrub** (`scrubEnv`) — return `os.Environ()` with every entry whose key is `ANTHROPIC_API_KEY`
  **removed** (not blanked), so the child's `os.LookupEnv("ANTHROPIC_API_KEY")` returns `ok == false`.
  Scrub **only** this one var (AC-specified; over-scrubbing risks breaking the subscription login).
- **Parse + validate.** Tolerantly extract the outermost JSON array from stdout (models may wrap it in
  prose), unmarshal, then require: every batch hash present exactly once, no extra hashes, each label
  valid per the taxonomy contract, each confidence in `[0,1]`. Any failure = malformed.
- **Retry/park.** Malformed → retry the same batch **once**. Still malformed (or spawn/non-zero exit
  after retry) → return a `parkRecord{Hashes, Raw: lastResponse, Attempts: 2, Model}`; the run continues.
  Never drop, never crash.

Persistence: on success, append each `labelRecord` to `-out` and **flush/sync before the next batch**; on
park, append the `parkRecord` to `-park` and continue. Parked screens are **not** written to `-out`, so a
later run (e.g. a sonnet re-pass) re-attempts them — the operator's retry path. (Re-park accumulation is
acceptable for a rare event; a skip-set is a deferred, evidence-gated refinement, not built now.)

### Re-pass mode (`-model` ≠ default)

The AC couples model and mode ("`-model` flag to force a `sonnet` re-pass over low-confidence and unusual
screens"). Honor it literally with **one** flag:

- default (`-model haiku`): selection = **unlabeled** screens (resume-style, above).
- `-model <non-default>`: selection = existing `-out` records with `Confidence < -min-confidence` **OR**
  `Label` prefixed `unusual:`; re-label those and **overwrite** their prior `-out` entry (rewrite the
  labels file with the updated records — the file is low-thousands lines, a full rewrite is fine).

Priority ordering, batching, retry/park are identical in both modes. (Alternative — a separate `-repass`
bool that keeps `-model` purely the model — is noted in Open Questions; the spec follows the AC's single
`-model` coupling.)

## Concurrency model

**None — fully sequential, one batch at a time.** Deliberate, and a departure from `corpus-replay`'s
`-workers` parallelism (#260): the subscription login is a single serialized resource (parallel `claude`
calls contend on it), and resume correctness wants **ordered, flush-per-batch persistence** — a parallel
pool would let batch N+1 finish and persist before batch N, so a mid-run kill could leave a gap that
resume can't reason about. There is no throughput pressure (operator tool, low-thousands screens). Keep it
sequential; it is both simpler and more correct here.

## Error handling

| Failure | Handling |
|---------|----------|
| `-in` open fails | `os.Exit(1)` with stderr note |
| `-in` line malformed | skip with stderr note (tolerant), continue |
| `-out` unreadable (resume) | `os.Exit(1)` — a corrupt resume source must not silently re-label everything |
| `-out` absent | empty labeled-set, full run |
| nothing to label (empty selection) | print counts, exit 0 |
| claude spawn error / non-zero exit | treated as malformed for that attempt → retry, then park |
| response not valid JSON / hash mismatch / bad label / bad confidence | malformed → retry once → park |
| `-out` / `-park` append fails | `os.Exit(1)` (a lost persist breaks resume — fail loud, don't continue) |

## Testing strategy

All tests use a **fake `claude`** — no real model call anywhere. Recommended idiom: **re-exec the test
binary as the fake** (the standard `os/exec` `TestMain` pattern), so it is deterministic and cross-platform
with no shell dependency:

- `TestMain` checks a marker env var (e.g. `CORPUS_LABEL_FAKE=1`); when set, the binary acts as the fake
  claude — reads stdin, consults scenario env knobs, emits a canned response, exits — instead of running
  tests. Tests set `-claude = os.Args[0]` and set the marker + scenario knobs via `t.Setenv` (these ride
  through `scrubEnv`, which only strips `ANTHROPIC_API_KEY`).

Scenarios (write as focused tests; assert on hashes/counts/labels, **never grid content**):

- **Resume:** given `-out` pre-populated with some hashes, selection skips exactly those; a run that
  labels batch 1, then a simulated kill before batch 2, then a restart, labels no screen twice and loses
  no completed batch. (Drive resume by pre-writing `-out` and re-invoking selection — no real signal
  needed.)
- **Batching:** N unlabeled screens with `-batch=15` → correct batch count and sizes; `-batch` outside
  [10,20] exits 2.
- **Priority:** a mix of fire (`source∋"fires"`) / err (`tag=="err"`) / plain screens orders fire → err →
  rest, stable within tier; a fixture with no `source` field leaves the fire tier empty (graceful); one
  with no `tag` leaves err empty.
- **Parse — happy:** valid JSON array → each screen labeled, appended to `-out`.
- **Parse — retry-then-succeed:** fake emits malformed on call 1, valid on call 2 (scenario via a
  call-count marker file the fake bumps) → screens labeled, not parked.
- **Park:** fake emits malformed twice → batch's screens written to `-park` with the raw response, run
  does not crash, `-out` unchanged for those hashes.
- **Validity:** a label outside the taxonomy and outside the `unusual:` prefix → malformed → park; a
  well-formed `unusual: <text>` label → accepted; confidence outside [0,1] → malformed.
- **Env scrub (the billing invariant):** the test sets `ANTHROPIC_API_KEY` in its own env via `t.Setenv`;
  the fake asserts `_, ok := os.LookupEnv("ANTHROPIC_API_KEY"); ok == false` and exits non-zero (failing
  the test loudly) if the key is present. This is why the invariant is an AC, not a comment — it is
  deterministically checkable.
- **Re-pass:** given `-out` with a low-confidence and an `unusual:` record, `-model sonnet` selects
  exactly those and overwrites their entries; high-confidence records are untouched.

`make check` (`vet test`) builds and tests the package automatically — **no Makefile target** (mirrors
corpus-sampler; avoids the #253-class target conflict). `go build ./cmd/corpus-label` drops a stray
`corpus-label` binary at the repo root — **`rm` it before `git add`** (recurring corpus-tool landmine).

## Self-reference discipline

Sample grids quote detection anchor literals; echoing them mid-run can false-fire live detection (#152 /
#154 / #155). Enforce, same as corpus-sampler:

- grids flow **only** to the child claude's stdin, to `-out`… **no** — grids do **not** go to `-out`
  (labels store hashes only). Grids reach exactly one place: the child's stdin.
- `stdout` progress carries **hashes and counts only** — never grid content, never a label's free text
  echoed alongside a grid.
- test failures reference sample **hashes and cast names**, never grid content.
- fixture grids are synthetic (no real anchor literals) so even a fixture dump is inert.

## Open questions

- **`-model` coupling vs. a separate `-repass` flag.** Spec follows the AC (one `-model`; non-default
  ⇒ re-pass). If the operator finds the implicit mode-switch surprising in use, a `-repass` bool is the
  low-cost follow-up — flag for the operator, don't build speculatively.
- **Exact `claude -p` flags / `--output-format`.** The contract is "prompt on stdin, model via `--model`,
  emit a JSON array." The developer confirms the precise print flag (`-p` vs `--print`) and whether
  `--output-format json` (structured envelope) is worth using over tolerant array extraction against the
  installed claude; either satisfies the parse contract.
- **`sourceFires` value.** Named const in corpus-label = `"fires"`, tied to #273's `-fires` source. #273
  is unmerged; the empty-tier fallback is graceful today. Confirm the literal against #273 when it lands.

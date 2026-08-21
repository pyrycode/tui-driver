# corpus-replay

Replays asciinema `.cast` recordings through tui-driver's screen-state detectors
and reports which detectors fired in which runs. It turns the ad-hoc corpus
scans that cracked the 2.1.199 recalibration, the wedge taxonomy, and the
2026-07-07 detection review into a repeatable make target (#227).

Recordings accumulate wherever a consumer points `SpawnOpts.RecordTo` (the pyry
agent-run flight recorder writes them to `~/.local/share/pyry-recordings`,
tagged `-ok` / `-err`).

## Run

```sh
# Default: replays the operator's recorder directory.
make corpus-replay

# Any directory of casts, with options.
go run ./cmd/corpus-replay -dir /path/to/casts
go run ./cmd/corpus-replay -dir /path/to/casts -per-cast
go run ./cmd/corpus-replay -dir /path/to/casts -only 20260707T094901
```

Flags:

- `-dir` (required): directory of `.cast` files.
- `-stride N`: run the classifier every Nth output event (default 1). A modal is
  up for many frames, so a small stride still catches it while cutting cost on a
  large corpus.
- `-workers N`: number of concurrent replay workers (default: the machine's CPU
  count). Casts replay independently, so a full corpus pass scales roughly
  linearly with cores; `-workers 1` is the sequential baseline. Report output
  (stdout) is byte-identical across worker counts.
- `-per-cast`: print one line per cast (fires, anchors, segment, tag).
- `-only SUBSTR`: replay only casts whose filename contains SUBSTR — for
  drilling into a specific recording.
- `-no-cache`: bypass the result cache entirely (always replay; never read or
  write the cache).
- `-cache-dir DIR`: override the cache directory (default: a `corpus-replay`
  subdirectory under the user cache dir).
- `-assert`: assert mode — evaluate the detection-health invariants and exit
  non-zero on any violation. Forces `-stride 1` (see below). This is the gate
  behind `make corpus-assert`.
- `-assert-edge-ceiling N`: the per-key transition-edge ceiling for the flapping
  invariant (default is a documented placeholder; it is a baseline-derived
  tunable, see below).

## Caching

The result cache is **on by default**. A cast's replay result is a pure function
of the cast bytes, the resolved stride, and the detection code, and recordings
are immutable — so on a repeat run over an unchanged corpus with unchanged
detectors every result is recomputed identically for nothing. The cache removes
that: the routine case (a handful of new recordings, unchanged detectors) costs
seconds, while a full re-replay fires exactly when the detection code changes.

Each cast's result is stored in one file, keyed by a hash of the detector
sources (`pkg/tuidriver` **and** `cmd/corpus-replay`) plus the resolved stride
plus the cast's name and content. So:

- editing any detector source re-replays the **whole** corpus (every key flips);
- adding a new cast to an otherwise-unchanged corpus replays **only** the new
  cast (the rest stay hits);
- a result recorded at one stride is never served to a lookup at a different
  stride.

A hit/miss summary is printed to stderr (it does not affect the stdout report).
`-no-cache` bypasses the cache; `-cache-dir` relocates it. Stale entries from a
previous detector version are never read again and are safe to delete wholesale.

## What it reports

For each cast it feeds the recorded output through a rolling buffer (the same
window the live detector sees), runs the classifier at the stride, and records:

- **structural detector fires** — idle, thinking, each modal class, the
  mcp-failure and network-failure banners, the unknown-dialog shape;
- **transition edges** — how many times each detector key's membership flips
  between consecutive sampled ticks within a cast (a modal class shown/hidden
  repeatedly, a busy axis flapping). A real dialog fires once and stays at an
  edge count of 1; content forgeries and animation gaps climb past it. Printed
  as a per-key total table plus a top-5-per-bucket flappers list naming the
  specific cast and detector (#247);
- **anchor-in-content** — whether a detection anchor literal appeared in the
  run's content at all, whether or not the structural detector classified;
- the **`-ok` / `-err`** tag and a **prod / e2e / unknown** segment (the corpus
  mixes production agent runs with the `make e2e` suite; they are told apart by
  content cue — a `TestRealClaude_*` temp-dir path for e2e, a git worktree under
  Workspace for production).

How to read it:

- A structural detector firing in **production `-ok`** runs is a
  **false-positive suspect**: a healthy run should not trip a modal/banner
  detector.
- A cast where an anchor appears in content but the matching structural detector
  did **not** fire is a **correctly-suppressed forgery** — the shape the #219 /
  #220 co-signals reject, and the shape of the two 2026-07-07 aborts.
- A chrome anchor that never fires where it should, or a **retired** anchor that
  still appears in content, is a **drift suspect**.

Local audit tool only — no CI workflow (org rule).

## Assert mode — the detection gate (#259)

The report above is read by a human. Assert mode turns the same replay into a
pass-fail gate, so a detection regression fails automatically instead of
depending on someone reading the aggregate correctly.

```sh
# The operator-run gate: replay the corpus and exit non-zero on any violation.
make corpus-assert

# Tune the flapping ceiling for a run.
make corpus-assert EDGE_CEILING=20
```

`-assert` runs the normal replay and report, then evaluates three invariants over
the **production `-ok`** casts and exits `0` when all hold, non-zero on any
violation. Each violation line names only the cast short-name, the detector key,
and a category label — never the matched content — so the gate output cannot
itself forge live detection.

The invariants:

- **(a) no stray modal or banner fire.** No modal-class, mcp-failure,
  network-failure, or unknown-dialog detector may fire in a production `-ok`
  run, unless the cast is named in the committed allowlist.
- **(b) idle must fire.** Every production `-ok` cast must reach the idle prompt
  at least once. A run that never does means the idle detector went blind.
- **(c) no flapping past the ceiling.** Every detector key's transition-edge
  count must stay at or below `-assert-edge-ceiling`, so a class shown/hidden
  repeatedly or a busy axis toggling fails loudly.

**Assert always runs at stride 1.** Event-stride sampling counts events, not
seconds, so a dialog on an otherwise-quiet screen emits almost no events and can
drop out of a strided sample. The gate forces stride 1 whatever `-stride` said,
which is why it is the tens-of-minutes full-fidelity pass, run by hand and not in
`make check`.

### Tunables the operator owns

Two values are **baseline-derived** and out of a claude-free change's scope. The
mechanism ships with safe placeholders; the real values come from a baseline
`make corpus-assert` run over the operator's external corpus.

- **The allowlist** (`assertAllowlist` in `assert.go`) names production `-ok`
  casts whose modal or banner fire is a known, accepted exception, each with the
  reason. It ships **empty**. Add an entry only once a baseline run shows a
  genuinely benign fire.
- **The edge ceiling** (`defaultEdgeCeiling` in `assert.go`, overridable with
  `-assert-edge-ceiling`) ships as a documented placeholder. Set it from the
  edge-count distribution a baseline run reports, high enough to clear healthy
  runs and low enough to catch a real flapping regression.

The synthetic fixtures under `testdata/assert/` prove the gate mechanism under
`make check` — a clean corpus, one fixture per violation kind, and an
allowlisted-exception corpus — not any empirical constant.

## ⚠️ Self-reference

This tool's source, its `testdata` fixtures, and its output quote detector
anchors. Rendering those on screen mid-run can false-fire the live detection, so
build and run it **by hand**, off the agent pipeline, as the detection tickets
(#152 / #154 / #155) were.

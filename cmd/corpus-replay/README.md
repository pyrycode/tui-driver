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
- `-per-cast`: print one line per cast (fires, anchors, segment, tag).
- `-only SUBSTR`: replay only casts whose filename contains SUBSTR — for
  drilling into a specific recording.

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

## ⚠️ Self-reference

This tool's source, its `testdata` fixtures, and its output quote detector
anchors. Rendering those on screen mid-run can false-fire the live detection, so
build and run it **by hand**, off the agent pipeline, as the detection tickets
(#152 / #154 / #155) were.

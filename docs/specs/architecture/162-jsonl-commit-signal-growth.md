# Spec: Base the JSONL commit signal on file growth, not existence (#162)

## Files to read first

- `pkg/tuidriver/deliver.go:182-210` — `promptDidCommit`, the function to change. The stat check at `:197-200` (`os.Stat(jsonlPath) == nil ⇒ committed`) becomes a size-growth check. Extract: the poll-loop shape (spinner check → JSONL check → `select` on ctx/deadline/ticker) that the growth check slots into unchanged.
- `pkg/tuidriver/deliver.go:95-111` — `DeliverPrompt`. This is where the pre-delivery baseline is stat'd once, before `deliverPrompt` runs, and threaded into the `promptDidCommit` closure at `:106-108`. Extract: the closure currently forwards `(ctx, opts.JSONLPath, timeout)`; it gains a captured `baseline` argument.
- `pkg/tuidriver/deliver.go:34-39` — `DeliverOpts.JSONLPath` godoc. "treats the file's appearance as a commit signal" is the existence-based wording to correct to growth.
- `pkg/tuidriver/deliver.go:76-79` — `DeliverPrompt` godoc, "Commit confirmation" bullet. "the per-session JSONL has appeared" is the second existence-based wording to correct.
- `pkg/tuidriver/deliver_test.go:253-293` — `TestPromptDidCommit`, the direct-against-`promptDidCommit` test this change extends. Extract: the four subtests each construct a `&Session{buffer: NewBuffer(0)}`, optionally `Append(SpinnerGlyph)`, and write a temp JSONL under `t.TempDir()`. Every call site gains the new `baseline` argument; the "jsonl file present" subtest is rewritten as a growth pair.
- `pkg/tuidriver/deliver_test.go:69-223` — the `TestDeliverPrompt_*` table tests. Read only to confirm they inject a fake `didCommit func(time.Duration) bool` and therefore need **no** change (the seam signature is preserved). Extract: `deliverDeps` is the injection boundary; the growth logic lives below it, in the real `promptDidCommit`.
- `pkg/tuidriver/jsonl.go:148` — the "rare on an append-only file" note. Extract: confirmation the file only ever grows, which is the invariant that makes `size > baseline` a sound commit signal.

## Context

`promptDidCommit` (`deliver.go:197-200`) treats the mere existence of the per-session JSONL as a commit signal: `os.Stat(jsonlPath) == nil ⇒ committed`. claude writes that file the first time a turn commits and appends to it thereafter (jsonl.go is explicit it is append-only), so from turn two onward "the file exists" is vacuously true and `promptDidCommit` returns committed instantly — before the current turn has landed.

The effect: paste-recovery (the corrupted-paste re-deliver loop in `deliverPrompt`) is silently disabled for every turn after the first. A turn-two paste that never commits is reported committed, the recovery loop is skipped, and the turn wedges downstream.

The fix bases the signal on *growth*: capture the JSONL size immediately before delivery and require the file to grow past that baseline. Because the file is append-only, a fresh append for the current turn always pushes the size up, and a stale pre-existing file that does not grow no longer counts as a commit. The spinner (`IsThinking`) path is the primary reliable in-flight anchor (per CLAUDE.md's spinner caveat) and stays untouched.

Filed from the Cross-Repo Code Review 2026-07-03 (`MEDIUM`).

## Design

The change is confined to one production file, `pkg/tuidriver/deliver.go`. No public surface changes: `DeliverOpts`, `DeliverResult`, the `DeliverPrompt` signature, and the `deliverDeps.didCommit` seam (`func(time.Duration) bool`) are all preserved. Growth is an internal refinement of one commit signal; the consumer (pyrycode's `pyry agent-run`) sees identical types and only observes more-correct `Committed` behaviour on turn two onward.

### 1. `promptDidCommit` gains a `baseline` parameter

New signature:

```go
func (s *Session) promptDidCommit(ctx context.Context, jsonlPath string, baseline int64, timeout time.Duration) bool
```

`baseline` is the JSONL byte size captured immediately before this delivery. The JSONL branch of the poll loop (currently `:197-200`) changes from existence to growth:

- Before: `if _, err := os.Stat(jsonlPath); err == nil { return true }`
- After: stat the path; report committed only when `err == nil && info.Size() > baseline`.

Everything else in the loop is unchanged: the `IsThinking(s.Snapshot())` spinner check stays first and still short-circuits to `true` regardless of JSONL state (AC 4); the `jsonlPath != ""` guard stays; the `select` on ctx/deadline/ticker is untouched. A stat error (missing file) is treated as "no growth yet, keep polling" — identical to the existing behaviour where a stat error simply did not return true.

Rationale for `int64`: `os.FileInfo.Size()` returns `int64`; matching the type avoids a conversion and reads naturally against `info.Size()`.

### 2. `DeliverPrompt` captures the baseline once, before delivery

In `DeliverPrompt` (`:95-111`), before the `deliverPrompt(...)` call, stat `opts.JSONLPath` once and record its size:

- If `opts.JSONLPath == ""` or the stat errors (file absent), the baseline is `0`.
- Otherwise the baseline is the current file size.

Thread this captured value into the closure at `:106-108`:

```go
didCommit: func(timeout time.Duration) bool {
    return s.promptDidCommit(ctx, opts.JSONLPath, baseline, timeout)
},
```

**Capture-once is load-bearing.** The baseline must be stat'd in `DeliverPrompt` (outside the retry loop), not inside `promptDidCommit`. `deliverPrompt` may invoke the `didCommit` closure multiple times across re-delivery attempts; the closure captures the single pre-delivery baseline so every attempt is measured against the file state *before this turn began*. If the baseline were re-stat'd inside `promptDidCommit`, attempt 2 would fold attempt 1's growth into its own baseline and the check would regress. A missing file at capture time reads as size 0, so the turn-one "file first appears with content" case still satisfies `size > 0` (AC 3).

### 3. Doc-comment corrections (same file)

Three godoc blocks encode the existence assumption and must be corrected to describe growth. Prose only — no behavioural weight, but leaving them stale would re-teach the bug:

- `:34-39` (`DeliverOpts.JSONLPath`) — "treats the file's appearance as a commit signal" → describe it as the file *growing past its pre-delivery size*.
- `:76-79` (`DeliverPrompt` godoc, "Commit confirmation" bullet) — "the per-session JSONL has appeared" → "the per-session JSONL has grown past its pre-delivery size".
- `:182-187` (`promptDidCommit`'s own godoc) — "the per-session JSONL has appeared (claude writes it only once input lands)" → describe growth past the captured baseline. This block is not named in the ticket but belongs to the function being changed; keeping its doc truthful is part of the edit, not scope creep.

## Concurrency model

Unchanged. No new goroutines, channels, or mutexes. The added `os.Stat` in `DeliverPrompt` runs synchronously on the caller's goroutine before delivery; the growth stat inside `promptDidCommit` runs on the same poll loop that already stat'd the file. `promptDidCommit` remains observe-only — it never writes to the PTY or the JSONL — so a false negative still costs at most one extra re-delivery, never a corrupted live turn (the existing safety posture).

## Error handling

- **Stat error at baseline capture** (`DeliverPrompt`): file absent → baseline `0`. Correct: turn one starts from an absent/zero file, and any real commit grows it past 0.
- **Stat error during the poll** (`promptDidCommit`): treated as "not grown yet", loop continues until spinner appears, the file grows, the deadline fires, or ctx cancels. Identical control flow to today's `err == nil` guard failing.
- **Empty file at capture, never grows**: baseline 0, size stays 0, `0 > 0` is false → not committed → recovery runs. An empty JSONL is correctly not a commit.
- **Append-only invariant**: the file only grows (jsonl.go), so `size > baseline` is monotonic — once true within a poll window it stays true. No shrink/rotation case to defend (none observed; do not add speculative handling).

## Testing strategy

All new coverage lives in `pkg/tuidriver/deliver_test.go`, extending `TestPromptDidCommit` (the function is exercised directly against a temp JSONL, mirroring the existing subtest pattern). The `deliverDeps.didCommit` seam signature is unchanged, so **every `TestDeliverPrompt_*` table test stands as-is** — confirm this rather than editing them.

Update the four existing `TestPromptDidCommit` call sites to pass the new `baseline` argument, and restructure the JSONL subtests to cover growth (AC 5). Scenarios (developer writes them in the house table/subtest idiom):

- **spinner visible → committed (baseline irrelevant):** buffer has `SpinnerGlyph`, `jsonlPath == ""`, `baseline` any (0) → true. Preserves the existing spinner subtest; keeps AC 4's "unchanged" guarantee for the no-JSONL case.
- **spinner wins over a non-growing JSONL → committed (AC 4):** buffer has `SpinnerGlyph`; JSONL exists with size `N`; `baseline == N` (no growth). Expect true — the spinner short-circuits before the growth check even runs. This is the AC-4 case the current suite does not cover (today's spinner test uses `jsonlPath == ""`).
- **JSONL grows past baseline → committed via file signal (AC 3 / turn one):** no spinner; write the file with content of size `N > 0`; call with `baseline == 0`. Expect true (`N > 0`). Models turn one where the file first appears with content, or grows from empty.
- **pre-existing JSONL does NOT grow → not committed (AC 1, 2, 5 / turn two):** no spinner; write the file with content of size `N`; call with `baseline == N` and a short timeout (~120 ms, mirror the existing "no signal times out to false" subtest). Expect false — size equals baseline, not greater, so no file signal; times out. This is the regression the ticket exists to prevent: turn-two recovery is restored.
- **no signal times out to false:** keep the existing subtest; add the `baseline` argument (0, absent file).
- **ctx cancel returns false promptly:** keep the existing subtest; add the `baseline` argument (0).

The "does not grow" subtest must honour the timeout (it never returns early), so use a short bounded timeout as the existing timeout subtest does — do not use `time.Second`, which would slow the suite.

## Open questions

- **Should the growth check use `>=` with a stored line count instead of byte size?** No. Byte size is what `os.FileInfo.Size()` gives for free, the file is strictly append-only, and any real commit appends at least one JSONL record (many bytes). Strict `>` on bytes is the simplest sound signal; a line-count variant would re-read file contents for no gain.
- **Does the consumer (pyrycode) need any change?** No. `DeliverOpts`/`DeliverResult`/`DeliverPrompt` signatures are identical; the consumer keeps passing `JSONLPath` and reading `Committed`. This is a pure internal correctness fix.

# Spec: `spike-multi-turn` turn-complete predicate — PTY-quiescence to clear stuck `✻` glyph

**Ticket:** [#73](https://github.com/pyrycode/tui-driver/issues/73)
**Size:** S
**Status:** ready for development
**Posture:** predicate swap. The current `gotEndTurn ∧ IsIdle for 250ms`
conjunction wedges against `claude 2.1.148` because a `✻ Brewed for Ns`
glyph stays painted in the 4 KB rolling buffer after a fast assistant
response, keeping `IsIdle` false forever. Replace with the
PTY-quiescence predicate that `spike-cancel` already proves out for the
same stuck-glyph-in-rolling-buffer failure mode. No new package symbols;
no behavioral change to `IsIdle`; one binary touched.

## Files to read first

Load these before touching anything. They cover the failing predicate,
the proven sibling pattern this spec adopts, and the buffer/state
primitives the new predicate composes.

- `cmd/spike-multi-turn/main.go:347-382` — the failing predicate and its
  load-bearing rationale block. Lines 347-353 document the safety
  property the AC pins; lines 369-382 implement the wedging conjunction
  + 250 ms stability gate. Both blocks get rewritten by this spec.
- `cmd/spike-multi-turn/main.go:47-66` — constants for the spike. Drop
  `idleStableWindow = 250ms` (no longer used); add
  `ptyQuietWindow = 1500ms` (the empirical value spike-cancel validated).
- `cmd/spike-cancel/main.go:82-91` — the rationale block for
  `ptyQuietWindow`. Same failure mode (stuck `✻` glyph that
  post-cancel claude can't roll out of the 4 KB rolling buffer); same
  fix shape. This spec is reusing the same calibrated window. Cite this
  file:line in the new comment block.
- `cmd/spike-cancel/main.go:513-568` — `waitReappeared`, the proven
  PTY-quiescence predicate (`IsIdle(snap) AND rb.QuietFor() >=
  ptyQuietWindow`). The new `check` function in `runTurn` mirrors this
  shape but composed differently (no preCancel state to track; gate on
  `gotEndTurn` instead).
- `pkg/tuidriver/state.go:38-44` — `IsIdle` definition. The new
  predicate **does NOT** require `IsIdle`'s spinner-absent half (that's
  the wedging clause). It does require `IsIdle`'s `❯`-glyph-present
  clause. The new predicate is therefore weaker than `IsIdle` on the
  spinner-glyph axis but **stronger** than `IsIdle for 250ms` on the
  rendering-activity axis (1500 ms of zero PTY bytes vs. 250 ms of
  glyph-absence). The AC's safety property holds — see § Safety.
- `pkg/tuidriver/buffer.go:62-71` — `QuietFor()`. Returns time since
  last `Append`. Reading is mutex-protected and side-effect-free; safe
  to call from `runTurn`'s tick loop alongside `Snapshot()`.
- `pkg/tuidriver/state.go:9-13` — `IdleGlyph` exported byte constant.
  The new predicate calls `bytes.Contains(StripANSI(snap), IdleGlyph)`
  directly rather than going through `IsIdle`. `spike-multi-turn`
  already imports `bytes` (line 26), so no new import.
- `cmd/spike-cancel/README.md:40-44` — empirical derivation: post-cancel
  claude emits "~1.4 KB of redraw + title-bar updates, then silence."
  Same shape post-`end_turn`; 1.5 s window covers the tail.
- `cmd/spike-one-turn/main.go:226` — sibling predicate
  `gotEndTurn && (!thinkingObserved || spinnerGone)`. Untouched by this
  spec. Confirms `spike-one-turn`'s independence (AC 4 — no regression).
- `docs/knowledge/architecture/system-overview.md` § *Key signals* — the
  current "Turn-complete predicate (multi-turn)" entry will be stale
  after this lands. Documentation phase owns the update; do NOT edit
  this file from the developer's worktree.

## Context

### Symptom

Reproducible against `claude 2.1.148`:

```
TUIDRIVER_STRICT_MCP_CONFIG=1 bin/spike-multi-turn -trust-folder=accept
```

Three of three trials wedge on turn 1 (`say hello`), 35–74 s wall time,
exit 1. The watchdog fires with either "spinner counter frozen at 2 s
for 30 s" or "PTY quiet for 1m1s (last state: turn=1 prompt-written)"
— both are downstream symptoms of the same predicate wedge.

### Root cause

After a fast assistant response, claude writes the assistant message
with `stop_reason=end_turn` to the session JSONL within ~2 s. The TUI
emits the spinner one or two times during processing, then renders the
final response and stops redrawing. The `✻ Brewed for Ns` glyph is
left painted in the rolling buffer and never rolls out — the buffer is
4 KB and post-`end_turn` claude emits a small redraw + title-bar updates
totalling well under 4 KB.

`runTurn`'s predicate (`cmd/spike-multi-turn/main.go:369-382`) is:

```
gotEndTurn ∧ IsIdle(snap) continuously for idleStableWindow = 250ms
```

`IsIdle` requires `❯` present AND `✻` absent
(`pkg/tuidriver/state.go:38-44`). The stuck `✻` glyph keeps `IsIdle`
false forever; `gotEndTurn` flips to true within ~2 s; the conjunction
never holds; the watchdog fires. The 35–74 s spread is the race between
the two watchdog rules.

### Why `spike-one-turn` is unaffected

`spike-one-turn` (`cmd/spike-one-turn/main.go:226`) uses
`gotEndTurn && (!thinkingObserved || spinnerGone)`, where `spinnerGone`
is driven by the `matchSpinner` REGEX (which has known coverage gaps
already documented as out of scope). For `What is 2+2?` the spinner
regex never matches (class B/C verbs the regex misses), so
`thinkingObserved` stays false and the disjunction short-circuits the
moment `gotEndTurn` is true. This is **predicate-shape luck**, not a
fix — it papers over the same buffer-residue problem by not asking
about spinner state at all.

### Why this is the same problem spike-cancel already solved

`spike-cancel/main.go:82-91` documents the identical failure mode for
the post-cancel predicate:

> The spec's first-cut predicate ("hasSpinnerGlyph becomes false AND
> isIdle true") fails empirically because the spinner glyph the
> wait-for-kickoff step observed sits in the 4096-byte rolling buffer
> and post-cancel claude doesn't emit enough bytes to roll past it
> (observed empirically: ~1.4 KB of redraw + title-bar updates, then
> silence). The PTY-quiescence proxy detects that silence directly.

The fix shape spike-cancel adopted (and validated across 5 green
end-to-end runs) is the predicate this spec adopts for spike-multi-turn:

> `❯ idle glyph present AND PTY has been quiet for at least
> ptyQuietWindow = 1500ms`.

### Why fix shapes 1, 2, 4 are not chosen

The ticket lists four plausible fix shapes. The architect picks a fifth
that is a near-clone of fix shape 3:

- **Shape 1 (bigger / different buffer model)** — overlaps with the
  deferred library-extraction decision flagged at
  `pkg/tuidriver/buffer.go:8-13` and changes the substrate for every
  consumer. Out of proportion for one wedged predicate.
- **Shape 2 (swap to `(!thinkingObserved || spinnerGone)` like
  spike-one-turn)** — would regress safety in spike-multi-turn. The
  `matchSpinner` regex has documented coverage gaps (class B/D verbs;
  ellipsis-form without `for Ns`). If a future claude version produces
  a fast turn where `matchSpinner` happens to match — and then stops
  redrawing while leaving the glyph — `spinnerGone` would never fire.
  spike-one-turn tolerates this because it's the single-turn path with
  no follow-up keystroke; spike-multi-turn cannot.
- **Shape 4 (drive a redraw via no-op keystroke)** — sends bytes
  unprompted into the claude process to force buffer churn. Every
  multi-turn use site would have to do this. Couples the consumer
  contract to an implementation detail of claude's renderer.

### Chosen fix shape (PTY-quiescence; close kin of shape 3)

Replace the predicate with: "`❯` glyph present in the rolling buffer
AND `rb.QuietFor() >= ptyQuietWindow`." This is fix shape 3 ("stuck
spinner detector") expressed as "no PTY bytes for N seconds" instead of
"no counter advance for N seconds." The PTY-quiescence variant is
strictly more general — it doesn't require parsing the spinner counter,
which the `matchSpinner` regex already fails to match on multiple
observed verbs.

The predicate is the same one `spike-cancel/main.go:545-549` already
validates empirically. Adopting it removes the failure mode AC 1 pins
and preserves the safety property AC 2 pins (see § Safety).

## Design

### Predicate shape

```
turn-complete (multi-turn, post-fix) =
    gotEndTurn
  ∧ ❯ glyph present in StripANSI(rb.Snapshot())
  ∧ rb.QuietFor() ≥ ptyQuietWindow
```

`gotEndTurn` is unchanged: the JSONL tailer flips it on the first
assistant event with `stop_reason == "end_turn"` for the current turn.

The "❯ present" half is the surviving clause from `IsIdle`. The "✻
absent" half is removed — that's the wedging clause. The new
"PTY-quiet for ≥ 1500 ms" half subsumes the safety property the removed
clause was meant to provide: when claude has stopped emitting bytes
entirely for 1.5 s, it has by definition stopped redrawing the spinner
(or anything else), so further input is safe.

### Code changes

**File: `cmd/spike-multi-turn/main.go`**

Constants block (lines 47-66):

- Remove `idleStableWindow = 250 * time.Millisecond` and the explanatory
  comment.
- Add `ptyQuietWindow = 1500 * time.Millisecond` with a comment block
  explaining the empirical derivation (cite
  `cmd/spike-cancel/main.go:82-91` and the README finding).

Rationale block (lines 347-353):

- Rewrite to describe the new predicate. Mandatory content per AC 3:
  (a) what the predicate is, (b) why the conjunction `❯-present ∧
  PTY-quiet` is safe (no race window), (c) reference to
  `cmd/spike-cancel/main.go:513-568` as the proven sibling and the
  4 KB-buffer constraint at `pkg/tuidriver/buffer.go:8-13`.

`check` closure (lines 369-382):

- Drop the `idleSince` time tracking (no longer needed; PTY-quiet
  carries the temporal dimension).
- New body:

  ```
  check := func() bool {
      if !gotEndTurn { return false }
      stripped := tuidriver.StripANSI(rb.Snapshot())
      if !bytes.Contains(stripped, tuidriver.IdleGlyph) { return false }
      return rb.QuietFor() >= ptyQuietWindow
  }
  ```

  This is the contract sketch, not the implementation — the developer
  writes the actual closure in the spike's idiom (the existing closure
  already lives inside `runTurn` and uses the same locals).

- Remove the `idleSince` variable declaration above the closure.

No other functions change. No new imports (`bytes`, `tuidriver` already
imported). No new exported package symbols.

### Why no `pkg/tuidriver` helper

This spec deliberately keeps the predicate inside the spike binary,
matching the project's standing pattern: "All four spikes share ~600
LOC of helpers under `// copied from cmd/spike-...` attribution
comments. The duplication is deliberate — every spike binary deletes
when `pkg/tuidriver/` lands."
([system-overview.md § Current state](../../knowledge/architecture/system-overview.md))

A second consumer of the same PTY-quiescence pattern is integration
pressure — but the post-spike library-extraction work (#58–#62) is the
right place to lift this into `pkg/tuidriver`, alongside spike-cancel's
`waitReappeared` and any other consumers that surface during
extraction. Doing it here would prematurely commit an API shape that
will need to coordinate with spike-cancel's needs.

### What does NOT change

- `pkg/tuidriver/state.go`'s `IsIdle` is untouched. All other consumers
  (`spike-one-turn`, `spike-cancel`, `spike-long-prompt`,
  `spike-permission`, `spike-ask-user`, `spike-multiselect`,
  `probe-first-prompt-hang`) keep their existing behaviour. The 25+
  call sites do not cascade.
- `pkg/tuidriver/buffer.go` is untouched. `QuietFor` and `Snapshot` are
  already public and used by `spike-cancel`.
- `spike-multi-turn`'s session-level idle wait (line 202-206) is
  untouched. That wait is for the FIRST idle observation pre-prompt-1,
  before any spinner has been painted — no stuck-glyph risk.
- The session-scoped event-channel drain (lines 300-307) is untouched.
- The watchdog (60 s PTY-quiet, 30 s spinner-counter-freeze) is
  untouched. PTY-quiescence at 1.5 s is the predicate firing; 60 s
  PTY-quiet is the watchdog rule — different scales, different
  purposes.

## Safety

AC 2 pins: the next prompt is only written when (a) `stop_reason=end_turn`
has been observed AND (b) the TUI is verifiably ready to receive input.
The new predicate satisfies both:

(a) `gotEndTurn` is unchanged.

(b) "TUI verifiably ready" means two things in practice:
- claude has stopped emitting bytes — `rb.QuietFor() >= 1500ms`
  observes this directly (the rolling buffer is the byte sink, so
  "buffer has been quiet for 1.5 s" ↔ "claude has emitted no bytes for
  1.5 s").
- the input prompt is on screen — `❯` glyph present in the stripped
  buffer establishes this.

Compared to the OLD predicate (`IsIdle for 250 ms`), the NEW predicate
is:
- **Stronger** on the rendering-activity axis: 1500 ms of zero bytes is
  a much higher bar than 250 ms of "✻ glyph not in buffer." The 250 ms
  window is a debounce, not a quiescence proof — a transient
  glyph-rolled-out moment could fool it. Quiescence cannot be faked.
- **Weaker** on the spinner-glyph axis: we no longer require the glyph
  to be absent. This is the entire point — the OLD requirement is
  exactly what wedges, and an absent ✻ glyph is neither necessary
  (PTY-quiescence covers "claude is done") nor sufficient (a glyph
  rolling out doesn't mean claude is done) for "TUI ready."

There is no race window where claude could emit a fresh prompt-relevant
byte between the predicate firing and `typePrompt` running: the
predicate fires only on observing 1500 ms of silence, so by
construction claude has been doing nothing for 1.5 s when `typePrompt`
runs. If claude were going to redraw, it would have done so within the
~50 ms paint cadence the previous redraws established.

`typePrompt`'s own 10 ms inter-byte pacing
(`cmd/spike-multi-turn/main.go:463-475`) is an independent guard against
"input handler still transitioning from prior turn" — it remains in
place and continues to absorb any micro-redraw that lands between
predicate-fires and first-prompt-byte.

## Concurrency model

Unchanged. Same three goroutines (`main`, PTY reader, JSONL tailer)
coordinated by the single `context.WithCancelCause`. The `check`
closure runs inside `main`'s `runTurn` loop; it reads `rb.Snapshot()`
and `rb.QuietFor()` (both mutex-protected) and reads `gotEndTurn`
(local). No new shared state.

## Error handling

Unchanged. The watchdog continues to fire on `PTYQuietLimit = 60s` or
`SpinnerFreezeLimit = 30s` if the new predicate fails to converge for
any reason. Acceptable failure modes:

- If claude post-`end_turn` keeps redrawing indefinitely (e.g. a
  livelocked title-bar updater), `QuietFor()` never reaches 1500 ms,
  the watchdog fires at 60 s. Same failure surface as today, with the
  same recovery (read the watchdog log, escalate).
- If `gotEndTurn` is never observed (e.g. the tailer dies or claude
  never writes the end_turn line), the predicate never fires, the
  watchdog fires at 60 s. Identical to today.

The current bug (`gotEndTurn=true, IsIdle wedged false, watchdog fires
at 30/60s`) is gone because the new predicate doesn't gate on `IsIdle`.

## Testing strategy

Per AC 1 + AC 4 — both verified by running the actual spike binaries.

Scenarios the developer must observe:

- **AC 1, baseline run.** Build, then run the binary three consecutive
  times against `claude 2.1.148`:
  ```
  go build -o bin/spike-multi-turn ./cmd/spike-multi-turn
  TUIDRIVER_STRICT_MCP_CONFIG=1 bin/spike-multi-turn -trust-folder=accept
  ```
  Each run must exit 0; turn 1 (`say hello`) must complete without the
  watchdog firing. Recommended cadence: run, capture wall time, repeat.
  Expected per-turn budget post-fix: ≤ 5 s for turn 1; total wall time
  for all five prompts in the same ballpark as the pre-#64 baseline
  (16–21 s on the 3-turn shape; 5-turn shape never re-baselined — this
  run establishes the new baseline).
- **AC 4, sibling regression.** Build and run spike-one-turn once
  against the same `claude 2.1.148`:
  ```
  bin/spike-one-turn -trust-folder=accept
  ```
  Must exit 0 with `SUCCESS:` line and complete in roughly the same
  ~2.5 s wall time it did before. (No code in spike-one-turn changes,
  but explicitly confirm the sibling is green.)

No new unit tests in `pkg/tuidriver/`. `IsIdle` is unchanged and its
existing tests at `pkg/tuidriver/state_test.go` still apply. The new
predicate composition lives in a single binary and is verified by the
binary's own e2e behaviour against the canonical reproduction case.

The README at `cmd/spike-multi-turn/README.md` may be updated with a
post-fix empirical-timing observation by the developer if they choose,
following the existing per-turn-timing finding pattern — but the
documentation phase owns the system-overview update, not the developer.

## Open questions

None. The predicate shape, window length, and migration path are all
copied from spike-cancel's validated implementation. The architect-side
decision was choosing PTY-quiescence over the other three plausible fix
shapes (§ Context); the developer-side decision surface is empty.

## Acceptance criteria mapping

- AC 1 (3/3 trials exit 0, turn 1 completes without watchdog) — covered
  by § Design's predicate swap and § Testing strategy's baseline run.
- AC 2 (safety: end_turn AND TUI verifiably ready) — covered by
  § Safety. The new predicate is strictly stronger on rendering-activity
  than the old one and weaker only on the broken (wedging) clause.
- AC 3 (rationale documented at predicate site, old block replaced) —
  covered by § Design's "Rationale block" bullet. The developer writes
  the new comment at `cmd/spike-multi-turn/main.go:347-353` referencing
  this spec and `cmd/spike-cancel/main.go:513-568` + 82-91.
- AC 4 (no spike-one-turn regression) — covered by § Testing strategy's
  sibling run. spike-one-turn is not touched.

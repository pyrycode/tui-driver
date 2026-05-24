# spike-one-turn slow-path predicate: swap to PTY-quiescence

Ticket: [#97](https://github.com/pyrycode/tui-driver/issues/97)
Sibling fix this mirrors: [#73 / PR #74](https://github.com/pyrycode/tui-driver/pull/74)

## Files to read first

- `cmd/spike-one-turn/main.go:42-50` — current constants block; new `ptyQuietWindow` constant lands here.
- `cmd/spike-one-turn/main.go:212-252` — the turn-complete loop being modified. The predicate `!(gotEndTurn && (!thinkingObserved || spinnerGone))` at line 226 and the probe-tick branch maintaining `thinkingObserved` / `thinkingVerb` / `spinnerGone` (lines 237-250) are what this spec replaces.
- `cmd/spike-one-turn/main.go:122-147` — watchdog goroutine. **Do NOT modify.** It still calls `matchSpinner` to feed `tr.ObserveSpinner` + `tr.CheckWatchdog`; the spinner-freeze safety net stays in place for genuinely wedged sessions. The new loop predicate exits well before the 30 s `spinnerFreezeLimit`, so the watchdog cannot trip on the normal slow path.
- `cmd/spike-multi-turn/main.go:59-71` — the calibrated `ptyQuietWindow = 1500ms` constant and its rationale-by-citation comment block. Copy the citation pattern (point at spike-cancel's empirical derivation, do not re-derive).
- `cmd/spike-multi-turn/main.go:353-417` — the reference fix shape from PR #74: the rationale block (`main.go:353-381`), the `check` closure (`main.go:391-400`), and the `for !check() { select { ... } }` loop shape (`main.go:402-417`). The new spike-one-turn loop is a structurally identical port.
- `cmd/spike-cancel/main.go:81-90` — the original empirical derivation of the 1500 ms window. Cite this in the new comment block; do not paraphrase the derivation.
- `pkg/tuidriver/buffer.go:62-71` — `(*Buffer).QuietFor()` semantics. Returns `0` if no bytes have ever been appended (irrelevant on the post-end_turn path — the prompt write has already produced PTY output by then).
- `pkg/tuidriver/state.go:9-13` — `tuidriver.IdleGlyph` (the exported `❯` byte sequence the new predicate `bytes.Contains`-tests).
- `docs/knowledge/codebase/73.md` — lessons-learned from the multi-turn port: why the IsIdle wrapper is the exact piece that wedges; why "spinner-absent" and "rolling-buffer churn" are not independent; why the new conjunction's three clauses are.
- `docs/knowledge/architecture/system-overview.md` § Key signals — "Turn-complete predicate (multi-turn)" and "PTY-quiescence readiness" entries. The new spike-one-turn predicate is the same shape; the system-overview entry "Idle" (line 91) is unchanged.

## Context

`spike-one-turn`'s slow path uses the predicate `gotEndTurn && (!thinkingObserved || spinnerGone)` at `cmd/spike-one-turn/main.go:226`. Against `claude 2.1.150`, the slow path (~1/5 of runs) wedges because:

1. `thinking-detected` fires (the `✻ Churned for Ns` regex matches once).
2. ~100 ms later `end-turn-detected` fires (assistant `stop_reason=end_turn` lands in the JSONL).
3. The TUI never repaints — the stuck `✻ Churned for 2s` glyph stays in the 4 KB rolling buffer (`pkg/tuidriver/buffer.go:8-13`) because post-end_turn claude doesn't emit enough bytes to roll it past the cap.
4. `matchSpinner` keeps matching the same `for 2s`, so `spinnerGone` never flips.
5. The watchdog at `pkg/tuidriver/tracker.go:144` observes the spinner counter frozen at the same total for 31 s and trips, returning `watchdog: spinner counter frozen at 2s for 31s`.

The fast path (4/5 runs) succeeds because the spinner regex never matches on `What is 2+2?` before `end_turn` arrives — `thinkingObserved` stays false and the disjunction short-circuits. That is luck-of-the-prompt, not a structural guarantee; #73's lessons-learned anticipated this exact failure ("Fixing the regex would expose spike-one-turn to the same buffer-residue class spike-multi-turn just escaped"). The shift from `claude 2.1.148` to `2.1.150` surfaced it without any spike code change.

This is the **same root cause #73 / PR #74 fixed for spike-multi-turn**: a small post-end_turn redraw budget combined with a 4 KB rolling buffer cap leaves the spinner glyph painted indefinitely, and any predicate composed against "spinner absent" wedges. The fix shape from PR #74 is already validated across three other binaries (spike-multi-turn `runTurn`, spike-cancel `waitReappeared` + `runRecovery`, spike-permission `runAutoRespond`) against the identical failure mode. This ticket extends it to the fourth spike binary.

## Design

### Predicate

Replace the current predicate with a three-clause conjunction matching the spike-multi-turn shape:

> `gotEndTurn ∧ bytes.Contains(StripANSI(rb.Snapshot()), tuidriver.IdleGlyph) ∧ rb.QuietFor() >= ptyQuietWindow`

- **`gotEndTurn`** — JSONL `stop_reason=end_turn` has been observed for this turn. (Unchanged from today.)
- **`bytes.Contains(StripANSI(snap), tuidriver.IdleGlyph)`** — the `❯` input-prompt glyph is currently visible. This is the **`❯`-present half of `IsIdle` without the spinner-absent half** — the wrapper's spinner-absent clause is the piece that wedges, so call the two exported primitives directly. Same shape as `cmd/spike-multi-turn/main.go:391-400`.
- **`rb.QuietFor() >= ptyQuietWindow`** — the rolling buffer has not received bytes for at least 1500 ms. Substitutes for "TUI has stopped redrawing" by observing silence directly, sidestepping the buffer-residue problem entirely.

Implement as a closure `check func() bool` returning the conjunction, then drive the loop as `for !check() { select { ... } }` — mirrors `cmd/spike-multi-turn/main.go:391-417` verbatim modulo the per-turn / single-turn shape.

### Constants

Add to the existing `const (` block at `cmd/spike-one-turn/main.go:42-50`:

```go
// ptyQuietWindow: see cmd/spike-cancel/main.go:81-90 for the empirical
// derivation. Reused unchanged across cmd/spike-multi-turn (#73),
// cmd/spike-permission (#70), and cmd/spike-cancel itself (#11/#69).
ptyQuietWindow = 1500 * time.Millisecond
```

Citation-only. Do **not** re-derive the value; do not paraphrase spike-cancel's rationale. Cross-binary calibration sharing pattern established in [#73](../knowledge/codebase/73.md).

Leave the existing `spinnerFreezeLimit = 30 * time.Second` and other constants untouched — they belong to the watchdog, which is unaffected by this change.

### State to delete

- `thinkingObserved bool` (line 218)
- `thinkingVerb string` (line 219)
- `spinnerGone bool` (line 220)
- The `probe` ticker and its branch (lines 224–225, 237–250) that toggles those flags via `matchSpinner` on the rolling buffer.

The PO note suggests "thinking-detected log can remain as instrumentation." We delete it instead, for three reasons:

1. The system-overview already notes the regex misses every observed verb in practice (`docs/knowledge/architecture/system-overview.md:92`); the log line is dead instrumentation.
2. Removing it deletes the `thinkingObserved` / `spinnerGone` state cleanly, leaving the loop's local variables to just `gotEndTurn` and `assistantText`. Matches the multi-turn shape exactly.
3. PR #74 set the precedent in the sibling binary — the dormant slow-path log was dropped there too.

The watchdog goroutine's `matchSpinner` call (line 137) **must stay** — it feeds `tr.ObserveSpinner` + `tr.CheckWatchdog` and is the structural safety net for a genuinely wedged session (e.g. claude itself hangs indefinitely). The `spinnerRe` regex and the `matchSpinner` function stay because of this call site.

### Loop shape (final)

```go
var (
    gotEndTurn    bool
    assistantText string
)

check := func() bool {
    if !gotEndTurn {
        return false
    }
    stripped := tuidriver.StripANSI(rb.Snapshot())
    if !bytes.Contains(stripped, tuidriver.IdleGlyph) {
        return false
    }
    return rb.QuietFor() >= ptyQuietWindow
}

ticker := time.NewTicker(statePollInterval)
defer ticker.Stop()

for !check() {
    select {
    case <-rootCtx.Done():
        return fmt.Errorf("wait termination: %w", context.Cause(rootCtx))
    case ev := <-eventCh:
        if !gotEndTurn && isEndTurn(ev) {
            assistantText = extractAssistantText(ev)
            gotEndTurn = true
            tr.RecordTransition("end-turn-detected")
            logger.Printf("end-turn-detected")
        }
    case <-ticker.C:
    }
}
```

(Sketch only — exact variable layout is the developer's call. The contract is: three-clause conjunction, JSONL event-only state, no spinner-driven gating.)

### Rationale comment block

Add a comment block above the `check` closure that documents why the predicate's three clauses are independent and why `IsIdle` is deliberately not used. Pattern-match PR #74's rationale block at `cmd/spike-multi-turn/main.go:353-381` (cited in AC 2). Key points to cover:

- JSONL `end_turn` says "model done speaking."
- `❯`-present says "input prompt is on screen."
- PTY-quiescence says "claude has stopped redrawing."
- Why `tuidriver.IsIdle` is **not** called: its spinner-absent half is exactly what wedges this spike on `claude 2.1.150` (cite the failure mode — stuck `✻ Verb for Ns` glyph in the 4 KB rolling buffer).
- Why PTY-quiescence is strictly stronger than a `IsIdle`-stable debounce: 1500 ms of zero bytes cannot be faked by a transient buffer state.
- Cite #73 / PR #74 as the sibling that proved the predicate shape against the identical failure mode.

This block is what AC 2 requires ("documents in a comment block why it is equivalent in safety — pattern matches PR #74's `main.go:353-381` rationale block").

## Concurrency model

Unchanged. The three-goroutine layout from `docs/knowledge/architecture/system-overview.md` § Concurrency model still applies:

- `main` orchestrator (now waits on the new three-clause predicate instead of the old disjunction).
- PTY reader (unchanged; still appends bytes to the rolling buffer, which is what `rb.QuietFor()` measures).
- JSONL tailer (unchanged).
- Watchdog (unchanged; still observes the rolling buffer and tracker, still trips at 30 s of spinner-counter freeze for genuinely-wedged sessions).

Shutdown sequence unchanged. The deferred `cancelCause` + `session.Close` pair already runs in the correct order.

## Error handling

No new error paths. Existing failure modes:

- `ctx.Done()` during the loop → `fmt.Errorf("wait termination: %w", context.Cause(rootCtx))`. Unchanged.
- JSONL tailer error → propagates via `cancelCause`. Unchanged.
- Watchdog trip (PTY-quiet for 60 s OR spinner-counter-frozen for 30 s) → propagates via `cancelCause`. Unchanged in mechanism; in practice the new predicate exits the loop within ~1.5 s of `gotEndTurn` for healthy turns, so the 30 s threshold isn't reached on the normal slow path.

There is no new edge case where the predicate could spin forever without tripping a watchdog: if `gotEndTurn` never fires, the existing 60 s PTY-quiet watchdog catches it; if `gotEndTurn` fires but claude keeps emitting bytes indefinitely, the 60 s PTY-quiet watchdog still catches it (`QuietFor()` resets on every Append).

## Testing strategy

Spike binaries are validated end-to-end against a live `claude` install, per the project convention. There are no unit tests to add.

Per AC 1: `bin/spike-one-turn -trust-folder=accept` against `claude 2.1.150` must exit 0 across 5 consecutive trials. The slow path is the regressing case today; the trial set should observe both fast-path and slow-path exits to confirm coverage. Trial cadence and sampling are the developer's call — the relevant observation is "all 5 exit 0" plus a note on which run(s) took the slow path (visible via the `end-turn-detected` log line without a preceding `❯`-glyph-present check).

Per AC 3: spike-multi-turn and spike-cancel must continue to pass against `claude 2.1.150`. These binaries are untouched by this change, so the regression check is structural rather than substantive — just rebuild and re-run each once.

## Open questions

None at spec time. The PO note's "architect confirms predicate shape during spec-stage" is confirmed above: mirror PR #74's three-clause conjunction, with the documented variance that the dormant `thinking-detected` log is deleted rather than retained.

## Why not lift PTY-quiescence into the library now

Tempting and out of scope. This ticket's AC 4 explicitly forbids changes to `pkg/tuidriver/`. The library extraction work (#58–#62 + follow-ups) is the right home for a `ReadyForInput`-shaped primitive that captures `❯-present ∧ QuietFor() >= N`; #73's lessons-learned anticipated that lift ("the PTY-quiescence helper's public shape should be the minimal common substrate"). After this ticket lands, the pattern has **four** consumer call sites across four binaries (`spike-cancel`, `spike-multi-turn`, `spike-permission`, `spike-one-turn`), which is enough integration pressure to inform the library API but not enough reason to widen scope here. The extraction stays its own ticket.

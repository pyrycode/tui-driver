# Spec: fix(spike) — make `thinking-detected` optional in the state machine

**Ticket:** [#4](https://github.com/pyrycode/tui-driver/issues/4)
**Size:** XS
**Status:** ready for development

## Files to read first

Load these in order; each entry says what to extract.

- `cmd/spike-one-turn/main.go:198-263` — the run() region from `prompt-written` through the existing BOTH-wait loop; this is the exact block being rewired. The JSONL tailer at 199–208 already starts immediately after `prompt-written` (no rewiring needed there).
- `cmd/spike-one-turn/main.go:210-222` — current standalone `waitUntil(...)` thinking-gate. This block is deleted in full.
- `cmd/spike-one-turn/main.go:224-254` — current BOTH-wait loop (`for !(spinnerGone && gotEndTurn)`). The termination condition, state variables, and ticker case are restructured here.
- `cmd/spike-one-turn/main.go:148-171` — watchdog goroutine. **Read only** — confirm to yourself why the 30 s spinner-freeze watchdog cannot false-positive on the fast path (`observeSpinner(false, …)` keeps `spinnerActive=false`, so `checkWatchdog`'s freeze branch never fires).
- `cmd/spike-one-turn/main.go:340-374` — `observeSpinner` and `checkWatchdog`. Read-only; confirms AC #4 is structurally already true and needs no code change.
- `cmd/spike-one-turn/README.md:60-71` — *Required state log lines (in order)* table; `thinking-detected verb=...` and `spinner-gone` rows become conditional on the slow path.
- `cmd/spike-one-turn/README.md:141-152` — *Surprise #2* — the empirical record of why this ticket exists (trivial prompts may produce zero spinner output).
- `cmd/spike-one-turn/README.md:176-188` — *Surprise #8* (post-#3) — the ANSI-strip-eats-the-cursor-forward observation; explains why even runs that *do* render a spinner have `thinking-detected` never firing today.
- `docs/specs/architecture/1-spike-one-turn.md:73-86` — *State machine* § step 4 (`waitThinking`) and step 5 (`waitTerminationBoth`); these are amended.
- `docs/specs/architecture/1-spike-one-turn.md:144-156` — *State log lines* section; `thinking-detected` and `spinner-gone` are reclassified as slow-path-only.
- `docs/knowledge/codebase/3.md` — predecessor's per-ticket notes; reaffirms the JSONL side is healthy and the only blocker is the spinner gate this ticket removes.

## Context

Ticket #3 fixed JSONL discovery and end-turn detection. Post-#3 run 3 confirmed the JSONL side is now fully healthy — the `assistant` event with `stop_reason=end_turn` arrives within ~1 s of `prompt-written` and is queued on `eventCh` — but the state machine still wedges at `prompt-written` because the standalone `waitUntil(...)` at `main.go:211-220` blocks on the spinner regex matching before the BOTH-wait loop is reached. For trivial prompts, either (a) the spinner never visibly renders (response too fast — finding #2) or (b) the spinner renders but the ANSI strip eats the cursor-forward whitespace so the regex misses it (finding #8). Either way, `thinking-detected` never fires and the 60 s inactivity watchdog trips.

This ticket makes `thinking-detected` optional. The fix is local to one block in `run()`. The spinner regex itself is **not** touched — improving it is a separate, smaller change and out of scope here.

## Design

### Overview of the change

All production-code edits are inside the `run()` function in `cmd/spike-one-turn/main.go`. The reader goroutine, watchdog, shutdown sequence, JSONL tailer, JSONL parser, and assistant-text extraction are all unchanged. Two things move:

1. The standalone `waitUntil(...)` thinking-gate (current lines 210–222) is **deleted**.
2. The existing BOTH-wait loop (current lines 224–254) is rewired to handle three transitions instead of two: thinking-detected (optional), spinner-gone (optional, only if thinking-detected fired), end-turn-detected (always).

Net effect: the JSONL channel is now consumed from the moment after `prompt-written`, and spinner-state transitions are observed opportunistically inside the same loop.

### State variables (replace the existing block at main.go:226-230)

The loop now tracks four booleans plus the verb capture:

- `thinkingObserved bool` — flips true the first tick the spinner regex matches.
- `thinkingVerb string` — captured on that same tick, logged as `verb=...`.
- `spinnerGone bool` — flips true on the first tick *after* `thinkingObserved` when the spinner regex no longer matches.
- `gotEndTurn bool` — flips true when an `end_turn` event is dequeued from `eventCh`.
- `assistantText string` — populated when `gotEndTurn` flips.

### Termination condition

```
for !(gotEndTurn && (!thinkingObserved || spinnerGone))
```

The two paths fall out of this condition naturally:

- **Fast path:** `gotEndTurn=true`, `thinkingObserved=false` (spinner never seen). Loop exits because `!thinkingObserved` is true. No `thinking-detected` or `spinner-gone` line ever emitted.
- **Slow path:** spinner observed (`thinkingObserved=true`) → spinner disappears (`spinnerGone=true`) → end_turn arrives (`gotEndTurn=true`). Loop exits because both `gotEndTurn` and `spinnerGone` are true. All three lines emitted in the natural sequence.

### Probe case (replace the existing block at main.go:244-253)

The ticker case absorbs both spinner transitions. It runs the existing ANSI strip + `matchSpinner` once per tick and branches on:

- `matchSpinner==true && !thinkingObserved` → capture verb, log `thinking-detected verb=%q`, `tr.recordTransition("thinking-detected")`, set `thinkingObserved=true`.
- `matchSpinner==false && thinkingObserved && !spinnerGone` → log `spinner-gone`, `tr.recordTransition("spinner-gone")`, set `spinnerGone=true`.

Both branches can run in the same tick only across separate ticks (the spinner can't be simultaneously matching and not matching). The order `thinking-detected → spinner-gone` is enforced by the `thinkingObserved && !spinnerGone` guard — `spinner-gone` cannot fire before `thinking-detected`.

### Channel case (replace the existing block at main.go:237-243)

Unchanged in shape:

- On every receive from `eventCh`, if `!gotEndTurn && isEndTurn(ev)` → extract text, log `end-turn-detected`, `tr.recordTransition("end-turn-detected")`, set `gotEndTurn=true`.

### Race ordering (per ticket's technical note)

If the spinner appears in the same tick that `end_turn` is dequeued, Go's `select` is non-deterministic: either case may win. The ticket asks for "preserve the slow-path log lines — emit `thinking-detected` then `spinner-gone` then `end-turn-detected` in their natural order." Achieving full ordering on the same tick is not possible without breaking the select's fairness, but the termination condition guarantees the slow-path lines are not *skipped*:

- If `thinkingObserved` is true at any point before termination, the loop will *not* exit until `spinnerGone` is also true.
- Therefore `thinking-detected` always precedes `spinner-gone` in stderr (enforced by the `thinkingObserved && !spinnerGone` guard).
- The relative position of `end-turn-detected` against the spinner pair may be either before or after, but all three lines will be emitted on the slow path.

This is the strongest ordering achievable with the existing concurrency model and is what the ticket actually needs. The README's state-log assertion is "all required lines present in stderr," not "lines in fixed total order across goroutines."

### Watchdog (no code change required)

Re-confirm by reading `observeSpinner` (main.go:340–359) and `checkWatchdog` (main.go:361–374):

- `observeSpinner(visible=false, ...)` returns early with `spinnerActive=false`. If the spinner is never observed, `spinnerActive` stays false for the entire run.
- `checkWatchdog` only evaluates the 30 s freeze deadline `if t.spinnerActive && ...`. Therefore the spinner-freeze watchdog cannot fire on the fast path — AC #4 is structurally already true.
- The 60 s inactivity watchdog bumps `lastTransitionAt` on every `tr.recordTransition(...)` call. On the fast path, the sequence is `prompt-written → end-turn-detected → assistant-text-extracted`; the gap from `prompt-written` to `end-turn-detected` is the only opportunity for the inactivity watchdog to fire, and at <1 s observed it's far below 60 s. The backstop still applies and still trips correctly if `end_turn` never arrives.

No edits to `observeSpinner` or `checkWatchdog`. No edits to the watchdog goroutine at main.go:148–171.

### Doc updates (required, in this same ticket)

The Required state log lines are now conditional. Two files need amendment by the developer:

1. **`cmd/spike-one-turn/README.md`** § *Required state log lines (in order)* (currently lines 60–71):
   - Mark `thinking-detected verb="<captured>"` and `spinner-gone` as **slow-path only** — i.e., present iff the spinner was visible at any point during the turn.
   - Add a one-line note: *On the fast path (trivial prompts where the spinner never renders), `thinking-detected` and `spinner-gone` are skipped; `end-turn-detected` fires directly after `prompt-written`.*
   - No change to `idle-detected`, `session-jsonl-opened …`, `prompt-written`, `end-turn-detected`, `assistant-text-extracted`, `shutdown-signalled` — these still fire on every run.

2. **`docs/specs/architecture/1-spike-one-turn.md`**:
   - § *State machine* step 4 (currently line 80: `waitThinking(ctx) error — poll the rolling buffer for the spinner regex; on first match, log thinking-detected verb=<captured>...`) — reclassify as "opportunistic, not blocking." Rephrase to: *Inside `waitTerminationBoth`, on the first tick the spinner regex matches, log `thinking-detected verb=<captured>` and start tracking the spinner's time-tail for the freeze watchdog. If the spinner never matches during the turn, this step is skipped.*
   - § *State machine* step 5 (`waitTerminationBoth`) — update to match the new termination condition: *Wait until a JSONL `end_turn` event has arrived AND (the spinner was never observed during this turn OR the spinner regex has stopped matching). Log `spinner-gone` only if `thinking-detected` was previously logged.*
   - § *State log lines* (currently lines 144–156) — annotate `thinking-detected` and `spinner-gone` as "slow path only."

These doc edits land in the same commit as the code change so the canonical state-machine description matches the code.

### Constants

No constants added, removed, or renamed.

### Code blocks summary

The diff is a localized restructure of one block in `run()`. Reference shape (not literal — match house style):

- Delete: the `var thinkingVerb string` declaration + `waitUntil(...)` + `tr.recordTransition("thinking-detected")` + `logger.Printf("thinking-detected verb=%q", thinkingVerb)` at lines 210–222.
- Modify: the BOTH-wait loop at lines 224–254 — new termination condition, new `thinkingObserved` / `thinkingVerb` locals, ticker case that handles both spinner transitions.
- Unchanged: everything else (declarations of `probe`, the `<-rootCtx.Done()` case, the `<-eventCh` case, the post-loop `assistant-text-extracted` log + `SUCCESS:` print + watchdog goroutine + `observeSpinner` + `checkWatchdog`).

## Concurrency model

Unchanged. Same three goroutines, same shutdown sequence, same context propagation, same JSONL tailer startup precondition. The only change is that the orchestrator goroutine consumes from `eventCh` immediately after `prompt-written` instead of waiting for spinner detection first.

## Error handling

Unchanged. The watchdog still trips on 60 s inactivity (the only event that can leave the loop forever is `end_turn` failing to arrive — same as today, just no longer gated behind `thinking-detected`). The 30 s spinner-freeze still trips if and only if the spinner is visible and frozen.

## Testing strategy

No automated tests (consistent with spike #1 and #3 policy). Verification is by execution:

1. `go build ./cmd/spike-one-turn`
2. Run the binary at least once from the worktree root.
3. **Expected fast-path outcome (most likely for `What is 2+2?`):** stdout shows `SUCCESS: 4` (or whatever claude responds). Stderr contains, in order: `idle-detected`, `session-jsonl-opened path=… offset=…`, `prompt-written`, `end-turn-detected`, `assistant-text-extracted len=…`, `shutdown-signalled`. **No `thinking-detected` line, no `spinner-gone` line.**
4. **Acceptable slow-path outcome** (if claude happens to render the spinner long enough for the regex to match — observed verbs to date are `Skedaddling` (ellipsis form, won't match) and `Baked` (cursor-forward strip eats whitespace, won't match), so this is unlikely until the spinner regex is itself fixed — but the path must work if/when it triggers): stderr contains `thinking-detected verb="..."` followed by `spinner-gone` followed by `end-turn-detected` (in that relative order between the spinner pair; `end-turn-detected` may interleave).
5. Confirm no watchdog trips. If the spike trips a watchdog, record the new failure shape in the README's *Surprises / findings* section rather than expanding scope.
6. Record the new timing row in the README's *Observed timings* table (fill in the previously-empty columns).
7. Verify the clean-exit invariant per README: `pgrep -lf 'claude$'` returns nothing after the spike exits.

AC #5's verification clause is satisfied by step 3 (fast-path SUCCESS with no `thinking-detected` line).

## Open questions

- **What happens if claude's response actually does take long enough to render a visible spinner that the regex matches?** Today this is empirically not happening (findings #2 and #8 explain why), but the slow-path code branch should work the moment finding #8 is independently fixed (separate ticket, not this one). The new loop's `thinkingObserved && !spinnerGone` guard preserves the slow-path log ordering when that day comes. No test coverage today; document the first observed slow-path run in the README's *Observed timings* table.
- **Should the spike's verb-capture log be retained at all if it never fires?** Yes — the moment the spinner regex is fixed (separate ticket), verb capture starts producing data. Keeping the log line in slow-path mode preserves that telemetry channel without cost on the fast path.

## Out of scope (reminder, mirrors ticket)

- The spinner regex / ANSI strip fix (finding #8) — separate, smaller ticket.
- Multi-turn / tool-use behavior.
- Extracting reusable primitives into `pkg/tuidriver/`.
- Updating `docs/knowledge/architecture/system-overview.md` or `jsonl-layout.md` — documentation phase owns these and will reconcile from the new spike behavior post-merge.
- Updating `docs/knowledge/codebase/` per-ticket notes — documentation phase owns these.

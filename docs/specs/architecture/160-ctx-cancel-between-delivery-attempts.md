# Spec #160 — Honour context cancellation between prompt-delivery attempts

Size: **XS**. One production file (`pkg/tuidriver/deliver.go`), one test file
(`pkg/tuidriver/deliver_test.go`). No new exported types; no signature change to
any public API. Not `security-sensitive` (verified on the issue labels).

## Files to read first

- `pkg/tuidriver/deliver.go:94-110` — `DeliverPrompt`; where `ctx` currently
  enters and is captured only into the `didCommit` closure. This is the one
  production call site of `deliverPrompt` and the wiring point for the threaded
  `ctx`.
- `pkg/tuidriver/deliver.go:123-171` — `deliverPrompt`, the pure driver. The
  retry loop (138-166) is the whole change surface. Note the exact decision
  points: `didCommit` (151), the `#227` no-chip guard (155-164), the backstop
  log (167-170).
- `pkg/tuidriver/deliver.go:179-201` — `promptDidCommit`; already honours
  `ctx.Done()` and returns `false` fast on cancel (the reason `didCommit`
  returns `false` on a cancelled poll). No change here — it is the upstream that
  makes the loop-level check sufficient.
- `pkg/tuidriver/deliver.go:87-93` — `DeliverPrompt` doc-comment; states the
  cancel-is-not-an-error contract. Extend one clause (see Design); do not change
  the contract.
- `pkg/tuidriver/deliver_test.go:69-215` — the six existing `deliverPrompt(...)`
  fakes-only tests. Each call site gains a leading `context.Background()` arg
  (mechanical). Model the new cancel test on `TestDeliverPrompt_ChipPresentReDeliver`
  (130-158), which is the exact behaviour the cancel must suppress.
- `pkg/tuidriver/deliver_test.go:249-256` — `promptDidCommit`'s existing
  "ctx cancel returns false promptly" subtest; shows the real-`context.WithCancel`
  idiom the new test reuses (the fakes-only seam already admits a real ctx).

## Context

`deliverPrompt`'s retry loop re-delivers up to `MaxAttempts` (default 3) without
inspecting cancellation between attempts. `promptDidCommit` honours `ctx.Done()`
and returns `false` fast on cancel — but on that `false` the loop falls through
to `hasChip()` (`deliver.go:155`), and if a "Pasted text" chip is still present
(plausible when a paste is cut short mid-cancel) the loop re-enters and does
`clear()` + `write()` again. A single cancel can drive up to 3 back-to-back
writes into the live PTY.

This breaks the ctx-bounded delivery contract the consumer (`pyry agent-run`)
relies on for prompt shutdown unblock, and re-opens the destructive re-paste
failure mode the `#227` no-chip guard exists to prevent: a cancelled re-delivery
re-pastes into an already-in-flight or abandoned turn.

## Design

**One decision: thread `ctx` into `deliverPrompt` and add a single
between-attempts cancellation check after the commit poll.**

`ctx` today lives only inside the `didCommit` closure, invisible to the loop.
Promote it to `deliverPrompt`'s first parameter — the canonical Go move when a
function must observe cancellation, and directly testable with a real
`context.WithCancel` (the fakes-only seam for the PTY operations —
`write`/`clear`/`didCommit`/`hasChip` — is untouched; `ctx` is a plain value,
exactly as `promptDidCommit`'s own test already passes one).

Rejected alternative: adding a `cancelled func() bool` seam to `deliverDeps`.
It would merely wrap `ctx.Err()` behind a bool closure — hiding a genuine
context dependency instead of expressing it. The function now truly needs
cancellation semantics, so it should take the context.

### Signature + wiring

```
// deliver.go:123
func deliverPrompt(ctx context.Context, opts DeliverOpts, deps deliverDeps) (DeliverResult, error)

// deliver.go:102 — DeliverPrompt passes its own ctx through
return deliverPrompt(ctx, opts, deliverDeps{ ... })
```

The `didCommit` closure keeps capturing the same `ctx` for the poll — no change
to the closure; the loop now references the same `ctx` it was handed.

### The check (contract sketch, not full body)

Immediately **after** the `if deps.didCommit(...) { ... return }` block and
**before** the `hasChip()` check, add:

```
if ctx.Err() != nil {
    // Commit poll ended because ctx was cancelled (not a genuine timeout):
    // stop. Do not consult hasChip, do not begin a fresh re-delivery.
    logger.Debug("tuidriver: context cancelled between delivery attempts; not re-delivering")
    return res, nil // Committed stays false; caller re-observes ctx downstream
}
```

Why this is the single sufficient location:

- `promptDidCommit` (the poll) is the **only** blocking point in an iteration,
  and it returns `false` on cancel. A non-blocking `ctx.Err() != nil` check
  right after it distinguishes a genuine commit-timeout (`ctx.Err() == nil` →
  proceed to `hasChip` unchanged) from a cancelled poll (`ctx.Err() != nil` →
  stop).
- Placing it before `hasChip()` means a cancelled attempt neither evaluates the
  chip nor logs a misleading "re-delivering" marker, and cannot reach the next
  iteration's `clear()` + `write()`. `writes` therefore stays at the in-flight
  attempt's count.
- A top-of-loop guard would be redundant: the after-poll return makes the next
  iteration unreachable, so there is no second place cancellation can slip
  through.

### Log level

`Debug`, not `Warn`. The existing three markers (re-deliver, committed-but-slow,
gave-up) are `Warn` because they flag anomalies; a cancel is the **expected**
shutdown/abort path, so it should not add `Warn` noise. Developer may keep it a
one-liner.

### Doc-comment touch

Extend the `DeliverPrompt` cancel clause (`deliver.go:91-93`) from "ends the
current attempt early" to also note it "stops before any further re-delivery."
Contract unchanged — cancel remains a non-error.

## Concurrency model

No new goroutines, channels, locks, or shared state. The change is a single
synchronous, non-blocking `ctx.Err()` read inside the existing loop, on the
same goroutine that already runs `deliverPrompt`. Cancellation is *observed*,
not *coordinated*.

## Error handling

- Cancel between attempts → `return res, nil` with `res.Committed == false` and
  `res.Attempts` = the attempt whose poll was cancelled. This preserves the
  existing "cancel is not an error here" contract (`deliver.go:91-93`); the
  caller re-observes `ctx` via the downstream JSONL wait, which honours
  `ctx.Done()` and unblocks cleanly. `Committed == false` is the safe report —
  the doc already says `false` means the caller falls through to the JSONL wait
  + watchdog.
- Write/clear PTY failures still return their wrapped errors, unchanged.
- Live (uncancelled) ctx: `ctx.Err() == nil` on every check, so the loop's
  corrupted-paste retry and the `#227` no-chip short-circuit behave exactly as
  before.

## Testing strategy

Add one focused test; update the six existing call sites mechanically.

- **New — `TestDeliverPrompt_CtxCancelledMidLoop`** (fakes-only, no PTY):
  - `ctx, cancel := context.WithCancel(context.Background())`.
  - `didCommit` fake: `cancel(); return false` — simulates ctx cancelled during
    the commit poll (mirrors real `promptDidCommit` returning false on cancel).
  - `hasChip` fake: `return true` — chip present, so *without* the fix the loop
    would re-deliver (this is the `TestDeliverPrompt_ChipPresentReDeliver` path).
  - `MaxAttempts: 3`.
  - Assert: `writes == 1`, `clears == 0`, `res.Attempts == 1`,
    `res.Committed == false`, `err == nil`. The `writes == 1` assertion is AC #4
    — the write count does not increase past the in-flight attempt.
  - Optionally assert the logs do **not** contain `"re-delivering"` (the cancel
    suppressed the re-delivery decision).
- **Mechanical — existing tests:** prepend `context.Background()` to the six
  `deliverPrompt(...)` calls (deliver_test.go:78, 109, 139, 174, 197, 211). No
  assertion changes; the live-ctx path is unchanged, so
  `TestDeliverPrompt_ChipPresentReDeliver` still expects `writes == 3` /
  `clears == 2`, `TestDeliverPrompt_CommitSlowNoChip` still short-circuits, etc.
- `promptDidCommit` tests are untouched (that function does not change).

Maps to AC:
- AC1 (cancel during poll → loop exits, no further clear/write) → new test,
  `clears == 0` and `writes == 1`.
- AC2 (at most the in-flight write even with chip present) → new test,
  `hasChip == true` yet `writes == 1`.
- AC3 (live ctx unchanged: retries + `#227` guard intact) → the six existing
  tests pass unchanged after the mechanical ctx arg.
- AC4 (regression test on write count) → new test's `writes == 1`.

## Open questions

- None blocking. Log level (`Debug` vs `Warn`) is settled above as `Debug`;
  developer may adjust if a code-review reviewer prefers parity with the other
  `Warn` markers, but the shutdown-path rationale stands.
- Intentional non-goal: the guard suppresses *re-delivery*, not the very first
  `write()`. A caller passing an already-cancelled ctx still gets one delivery
  attempt — within AC2's "at most the already-in-flight write." No top-of-loop
  pre-write guard is added.

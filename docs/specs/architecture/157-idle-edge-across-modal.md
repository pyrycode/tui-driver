# #157 — emit the idle/thinking rising edge even when a modal was active

Fixes the deferred `prev*`-update leak from **#61** (PR #94 review, PASS-with-flag),
re-deferred by **#100**. The idle rising edge is silently swallowed across a
modal-active window, so a consumer running "answer the modal, then wait for idle"
hangs forever. Chosen fix: **symmetric suppression of the `prev.idle` / `prev.thinking`
trackers while a modal is active** (candidate (a) from the ticket's Technical Notes).

Size: **XS** — 1 production file (`events.go`), ~3 lines + comment in the `mergeEvents`
ticker arm; one additive test. No new exported symbols, no signature changes, no
edit fan-out. Branch-overlap check clean (no in-flight branch touches `events.go` /
`events_test.go`).

## Files to read first

- `pkg/tuidriver/events.go:253-359` — the ticker arm. Three regions matter: the
  **modal-dominance block** (266-287, emits `ModalHidden` before `ModalShown`), the
  **idle/thinking suppression guard** (288-307, `if cur.modal == ModalClassUnknown`),
  and the **unconditional `prev = cur`** at 358. Line 358 is the fix site.
- `pkg/tuidriver/events.go:363-390` — `ptyState` struct + `classify`. Confirm `idle`
  and `thinking` are pure snapshot-derived axes (set by `classify`); `stalled` is the
  only axis `classify` does **not** set (computed inline at 260-262, before any
  mutation of `cur`). This is why the fix can safely rewrite `cur.idle` / `cur.thinking`
  at line 357 without disturbing the stall arm.
- `pkg/tuidriver/state.go:83-115` — `IsIdle` / `IsThinking`. Confirm a permission modal
  renders `❯` in the status region with no busy anchor → `IsIdle` returns **true**
  throughout the modal's lifetime (region-scoped since #153). This is the classification
  that leaks into `prev.idle`.
- `pkg/tuidriver/events_test.go:14-77` — `testSnap` (mutable snapshot source supporting
  `Set`-style byte replacement), `mustReceiveEvent`, `assertEventChClosed`, `neverQuiet`.
  The new test reuses all four verbatim.
- `pkg/tuidriver/events_test.go:78-118` — `TestMergeEvents_PtyIdleAndThinkingTransitions`
  — the byte-literal idioms the new test copies: `❯` = `[]byte("\xe2\x9d\xaf input")`,
  `✻` = `[]byte("\xe2\x9c\xbb Baked for 2s\n\xe2\x9d\xaf input")`.
- `pkg/tuidriver/events_test.go:120-178` — `TestMergeEvents_ModalShowAndHide` — the
  **masking test**. Must stay green unchanged (AC 6). It transitions Permission → MCP →
  idle; the MCP snapshot (`ManageMCPservers`, no `❯`) is why it passes today. Verify the
  fix keeps it green for a *different* reason (frozen `prev.idle`, traced below).
- `pkg/tuidriver/events_test.go:464-523` — `TestMergeEvents_BannerCoexistsWithIdleAndModal`
  — the idle-under-modal **suppression** assertion (phase 2 expects exactly one
  `ModalShown`, no spurious idle). Must stay green unchanged (AC 4 + AC 6).
- `docs/knowledge/codebase/61.md` § "Lessons learned" #1 — the full mechanism, the two
  candidate fixes, and the recommendation of (a) ("minimal and symmetric").
- `docs/knowledge/codebase/100.md` (Deferred item, § Lessons) — why the two banner axes
  were deliberately kept **non-suppressed** so they would not reproduce this leak shape.

## Context

**The bug.** `mergeEvents` suppresses idle/thinking rising-edge emissions while any
modal is active — the modal axis dominates, because a permission prompt paints `❯`
in the input region and `IsIdle` would otherwise fire a false "ready for next prompt"
signal (`events.go:288`, the `if cur.modal == ModalClassUnknown` wrapper). But
`prev = cur` runs unconditionally at the end of every tick (`events.go:358`). A
permission / trust / MCP modal renders `❯` with no busy anchor for its whole
lifetime, so `IsIdle` classifies the buffer idle throughout — which means `prev.idle`
quietly becomes `true` *while the emission is being suppressed*. When the modal then
clears with claude still idle, the tick computes `cur.idle=true && !prev.idle=true`
→ `false`, and **no `EventKindPtyIdle` fires after `EventKindPtyModalHidden`**. The
consumer (e.g. the permission auto-answer daemon) watches the modal go away and never
gets the "ready for next prompt" signal it is blocked on.

**Why now.** The fix was flagged on #61's PR #94 review (decision PASS-with-flag) and
consciously re-deferred by #100, which kept the banner axes non-suppressed specifically
to avoid reproducing this shape. #157 is the separate fix ticket both lessons point to.
The consuming flow (permission auto-answer) is a live pyrycode-side pattern; the hang is
not hypothetical.

**Scope boundary.** This ticket fixes the **event-ordering leak** only. The reviewer's
deeper root-cause note (state classified by substring-matching a raw history buffer
rather than the rendered grid) is tracked by the rendered-grid chain #150–#156 and is
explicitly out of scope here. The fix must not depend on or anticipate that chain.

## Design

### Chosen approach — (a) symmetric `prev*` suppression

The invariant #61 identified: *when you suppress emissions on one axis based on another
axis's state, you must either suppress the `prev*` updates symmetrically **or** force-emit
the suppressed axes on the suppressing-axis-clear transition.* Updating `prev*` without
emitting is the exact failure mode. Approach (a) takes the first compensation: while a
modal is active, freeze the `prev.idle` / `prev.thinking` trackers so the pre-modal
rising-edge state survives the whole modal window and fires on the clear tick.

The fix is a single conditional immediately before the existing `prev = cur` at line 358.
Contract sketch (developer writes the final form; this defines the behaviour, not the
prose):

```go
// Symmetric modal suppression (fixes #61 deferred → #157): while a modal is
// active the idle/thinking emissions above are suppressed (modal axis
// dominates), so their prev trackers must be frozen too. Otherwise prev.idle
// silently tracks the ❯-under-modal classification; when the modal clears with
// claude still idle, cur.idle && !prev.idle evaluates false and the idle rising
// edge is lost. Freezing preserves the pre-modal edge across the modal window so
// it fires on the clear tick — after EventKindPtyModalHidden, which the modal
// block above already emitted first.
if cur.modal != ModalClassUnknown {
    cur.idle, cur.thinking = prev.idle, prev.thinking
}
prev = cur
```

**Why rewriting `cur` (rather than a partial field-wise `prev` update) is correct and
minimal.** `prev = cur` is the last statement in the ticker arm; nothing reads `cur`
after it. Every emission decision on this tick — modal (266-287), idle/thinking
(288-307), banners (312-345), stall (349-357) — has already run against the *classified*
`cur` before line 357. So overwriting `cur.idle` / `cur.thinking` at 357 only affects
what gets carried into `prev` for the *next* tick, which is exactly the intent. It keeps
the clean single `prev = cur` assignment and confines the change to two fields under one
guard. The `stalled` axis is untouched: `cur.stalled` was computed at 260-262 (before
this mutation) and consumed at 349; freezing idle/thinking does not perturb it.

**Behaviour across the modal window (the fix's whole point):**

- Modal appears while `prev.idle=false` (claude was thinking, or transition-blind start):
  `prev.idle` stays `false` through every modal-active tick → on the clear tick
  `cur.idle && !prev.idle` = `true && !false` → `EventKindPtyIdle` fires. **Hang fixed.**
- Modal appears while `prev.idle=true` (idle already emitted, consumer already notified):
  `prev.idle` stays `true` → clear tick computes `true && !true` → `false` → no re-emit.
  Correct: the consumer already has its idle signal; the modal did not introduce a new
  low→high transition, so no spurious duplicate edge. The rising-edge invariant (an event
  marks a real `false → true` transition) is preserved.
- Same reasoning holds symmetrically for `thinking` (AC 2): a modal that clears to reveal
  an in-flight spinner (`✻` present) fires `EventKindPtyThinking`, because `prev.thinking`
  was frozen at `false` across the window.

### Ordering (AC 3) — unchanged, already correct

The modal-dominance block (266-287) runs *before* the idle/thinking block (288-307)
within the ticker arm. On the clear tick, `EventKindPtyModalHidden` is therefore sent
before `EventKindPtyIdle`/`EventKindPtyThinking`. The fix does not touch either block, so
the ordering contract is preserved by construction.

### Suppression-while-active (AC 4) — unchanged

The emission guard at 288 is untouched. On a tick where a modal remains active with no
class transition, no idle/thinking event is emitted (and now, additionally, the trackers
don't advance). The banner axes (312-345) remain independent of modal state, per #100.

### Considered and rejected — (b) force-emit on the clear transition

Candidate (b) — on `prev.modal != ModalClassUnknown && cur.modal == ModalClassUnknown`,
force-emit the current idle/thinking state — also restores the hang-fix, but is rejected:

1. **Spurious duplicate edges.** (b) emits idle on *every* modal clear where `cur.idle`
   holds, including the "idle already emitted before the modal" case — a "rising edge"
   that is not a real `false → true` transition. Consumers treating `EventKindPtyIdle`
   as a strict edge would see a phantom. (a) never double-emits.
2. **More surface.** (b) adds a second idle/thinking emission path that must be reasoned
   about against the normal block running on the same (now `modal==Unknown`) tick, to
   avoid a genuine double-send. (a) adds no emission path — it only changes what advances
   into `prev`.
3. **#61's own recommendation** is (a): "minimal and symmetric."

(a) is the smaller diff, preserves the rising-edge semantics, and needs no new reasoning
about emission ordering.

## Concurrency model

Unchanged. The single merge goroutine remains the sole reader of `jsonlCh`, sole writer
of `out`, and sole owner of `prev` (loop-local). No new goroutine, channel, lock, or
shared state. The fix is a two-field local assignment inside the existing ticker arm.
`defer close(out)` and the three-arm select shutdown path are untouched.

## Error handling

Not applicable — no new failure modes, no I/O, no new branches that can error. The change
is a data-flow correction inside an existing pure-in-memory loop step.

## Testing strategy

One additive test function (AC 5). Reuse `testSnap` / `mustReceiveEvent` /
`assertEventChClosed` / `neverQuiet` and the `❯` / `✻` byte literals from the existing
tests — no new helpers. Scenarios (developer writes the Go in the project's test idiom;
these are the phases + assertions, not code):

**New test — direct Permission → idle, no MCP detour (the reproduction the masking test
misses).** Drives the merge loop with `testSnap`:
- Phase 1: `Set` a Permission-modal snapshot that also renders `❯` (e.g.
  `"\xe2\x9d\xaf Do you want to proceed"`). Assert the first event is
  `EventKindPtyModalShown` with `Modal == ModalClassPermission`, and assert **no**
  `EventKindPtyIdle` follows while the modal is up (idle suppressed).
- Phase 2: `Set` a plain-idle snapshot (`❯` present, no modal anchor, no spinner —
  e.g. `"\xe2\x9d\xaf ready"`). Assert two events **in order**:
  `EventKindPtyModalHidden` (`Modal == ModalClassPermission`) then `EventKindPtyIdle`.
- This sequence **times out waiting for `EventKindPtyIdle` against the current code**
  (only `ModalHidden` arrives) and **passes after the fix**.

**Thinking-axis symmetry (AC 2).** Cover in the same test (a third phase) or a sibling
test: a modal shown while idle, then cleared to a spinner-present snapshot
(`"\xe2\x9c\xbb ... \xe2\x9d\xaf ..."`) → assert `EventKindPtyModalHidden` then
`EventKindPtyThinking`. Confirms the freeze applies symmetrically to `prev.thinking`.

**Regression — existing tests unchanged (AC 6).** `TestMergeEvents_ModalShowAndHide` and
`TestMergeEvents_BannerCoexistsWithIdleAndModal` must pass **without edits**. Traced:

- `ModalShowAndHide` (Permission → MCP → idle): under the fix, `prev.idle` is frozen at
  `false` across both modal phases (instead of being reset to `false` by the MCP snapshot
  as it is today). Phase 3 still computes `cur.idle=true && !prev.idle=false` →
  `EventKindPtyIdle`. Green, for a different internal reason. Do not edit the test.
- `BannerCoexistsWithIdleAndModal`: phase 2 (modal on, banner unchanged) still emits only
  `ModalShown` — the freeze does not add an idle event under an active modal (emission
  guard at 288 unchanged); phase 3 still emits only `McpFailureHidden`. Green unchanged.

Run the whole `TestMergeEvents_*` suite plus `TestEvents_TailJSONLErrorBubbles` — all
should stay green; the stall-axis tests are unaffected (stall is computed before the
mutation and reads `prev.stalled`, not `prev.idle`).

## Open questions

- **Thinking-symmetry test placement** — fold the thinking-axis phase into the new idle
  test, or add a small sibling test. Developer's call in the project's test idiom; either
  satisfies AC 2. (Non-blocking.)
- None affecting the fix contract. The choice between (a) and (b) is resolved above in
  favour of (a).

## Security review

**Verdict:** PASS

The `security-sensitive` label is present because the signal this ticket repairs
(idle/thinking rising edge) feeds the permission auto-answer gate. Adversarial re-read of
the spec against every applicable category below; default-FAIL discipline applied.

**Findings:**

- **[Trust boundaries]** No MUST FIX. The one untrusted input the change touches is
  `snapshot()` — an ANSI-stripped, region-scoped view of claude's terminal output, which
  is tool-/prompt-influenceable. The boundary is the pre-existing `classify` →
  `IsIdle`/`IsThinking`/`DetectModalClass` predicates; the fix adds **no new consumption**
  of those bytes — it only copies already-classified goroutine-local `bool`s
  (`prev.idle, prev.thinking = ...`). The security-relevant selector for what the
  auto-answer child *does* (keystroke, grant) is `ModalContent.Index` on the answer path,
  documented in `system-overview.md:110` — **not** the idle/thinking edge this ticket
  gates. This change controls *when the consumer is told claude is ready*, never *what
  grant a keystroke applies*.
- **[Threat model alignment]** No MUST FIX. Relevant threat: forged claude output tricking
  the consumer into treating an open permission prompt as dismissed (false "ready for
  input"). The fix cannot do this — the emission guard at `events.go:288`
  (`if cur.modal == ModalClassUnknown`) is untouched, so no idle/thinking event can fire
  while a modal is active (AC 4 re-verified by trace of `BannerCoexistsWithIdleAndModal`
  phase 2). New idle/thinking emissions occur **only on the clear tick**, when
  `DetectModalClass` already reports no modal. The change tightens `prev` bookkeeping
  rather than loosening emission. OUT OF SCOPE (named): the deeper substrate concern —
  state classified by substring-matching a history buffer rather than the rendered grid —
  is tracked by the rendered-grid chain **#150–#156**, not this ticket.
- **[Concurrency]** No MUST FIX. This is the only structurally-relevant category. The fix
  reads/writes only the loop-local `prev`/`cur` `ptyState` — no new goroutine, channel,
  lock, or shared state; the merge goroutine remains `jsonlCh`'s sole reader and `out`'s
  sole writer. No lock-ordering surface (no locks). Shutdown path (`defer close(out)`,
  three-arm select) untouched → no leak. Adversarial sub-question — *does freezing
  `prev.idle`/`prev.thinking` suppress a security-relevant signal?* No: the banner axes
  (MCP/network failure) and the stall axis advance **unconditionally** every tick,
  unaffected by the modal-gated freeze; modal show/hide uses the separate `prev.modal`
  field. The only behavioural delta is delivering a previously-*lost* readiness edge,
  which cannot weaken the gate.
- **[Error messages, logs, telemetry]** No findings — N/A. The change adds no logs, error
  messages, or telemetry; the idle/thinking events are payload-free (Source/Time only), so
  nothing attacker-controlled is emitted onto the stream or into any sink.
- **[Network & I/O]** No findings — N/A. No sockets, no reads from external input. The one
  resource-exhaustion sub-question: the fix is a single conditional two-field copy per
  tick and *reduces* emissions relative to today's `prev` churn — no runaway-emission or
  wedge path.
- **[Tokens/secrets/credentials]** No findings — N/A. No tokens, secrets, or credentials
  anywhere in the merge loop or this change.
- **[File operations]** No findings — N/A. No filesystem access; the merge loop is pure
  in-memory. (`TailJSONL`'s file open is upstream and untouched.)
- **[Subprocess / external command execution]** No findings — N/A. No `exec`, no shell,
  no environment manipulation.
- **[Cryptographic primitives]** No findings — N/A. No crypto, no RNG (security-relevant
  or otherwise) in this change.

No MUST FIX and no SHOULD FIX. The change is confined to goroutine-local `prev`
bookkeeping, tightens rather than loosens the readiness signal, touches no
security-relevant selector, adds no parsing/injection surface, and leaves every existing
suppression / independence / ordering contract intact (AC 3, 4, 6 re-verified in the
Testing strategy traces).

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-07-04

# Spec #161 — Harden AnswerModal dismissal re-read against a false 'still present'

**Ticket:** [#161](https://github.com/pyrycode/tui-driver/issues/161) · **Size:** XS · **Labels:** `security-sensitive`, `size:xs`
**Blocked by:** #152 (region-scoped `DetectModalClass`) — **merged to `main`** (`a3807ac`, PR #202, 2026-07-06). Blocker satisfied; this ticket can proceed.

## TL;DR

Add **one** new sub-case to `TestModalDismissed` that locks #152's region-scoping fix at the `modalDismissed` seam: a permission anchor lingering only in scrolled-up / above-region history must read as **dismissed** (return `true`), so a stale history anchor never fires the spurious `"… modal still present after answering"` error. **Zero production lines** — the fix already ships in #152's `DetectModalClass`; this is regression coverage only.

Verified during spec authoring: the sub-case (composed against `main`) **passes green today** and short-circuits before the first poll tick — see § Testing strategy.

## Files to read first

- `pkg/tuidriver/answer_test.go:127-170` — `TestModalDismissed`. The four existing sub-cases (`dismissed immediately when class is gone` / `still present times out to false` / `different modal counts as dismissed` / `ctx cancelled returns false promptly`). Mirror the table-free `t.Run` shape and the fake-`snapshot` injection. **Add the new sub-case here.** Note the file already imports `context`, `strings`, `time` — no new imports needed.
- `pkg/tuidriver/answer.go:117-134` — `modalDismissed(ctx, class, snapshot func() []byte, timeout)`. The seam under test. Returns `true` the instant `DetectModalClass(snapshot()) != class` (top-of-loop check, before any `select`), so an `Unknown` read short-circuits before the first `answerConfirmPoll` tick. **Do not modify this function** (AC #2).
- `pkg/tuidriver/modal_test.go:113-140` — `TestDetectModalClassPermissionRegion`, #152's own regression. **Copy its forged-above-region fixture shape** (`prompt = "Do you want to proceed?"` + `strings.Repeat("transcript body line\r\n", 25)`) but assert at a **different layer** (see § Design). Do **not** re-file its `DetectModalClass == Unknown` assertion.
- `pkg/tuidriver/modal.go:139-170` — region-scoped `DetectModalClass`. Permission uses `g.ContainsInLastRows(anchorPermissionSpaced, permissionRegionRows=12)`; the grid excludes scrolled-off history and the last-12-rows constraint excludes an above-region on-screen anchor. This is why the history-forged buffer classifies `Unknown`.
- `pkg/tuidriver/grid.go:88` — `(*Grid).ContainsInLastRows(sub, n)` — the region primitive #152 keys permission on. Read only if you need to convince yourself why the forged fixture reads `Unknown`.
- Memory / lesson: `codebase/150` fixture rule — synthetic grid fixtures use `\r\n` rows so `vt10x` renders flat lines. The forged fixture already follows this.

## Context

After `AnswerModal` sends the selecting keystroke, `modalDismissed` (`answer.go:117`) confirms dismissal by re-running `DetectModalClass(snapshot())` and checking the class no longer matches. Pre-#152, `DetectModalClass` substring-matched anchors over the whole stripped append-only buffer, so the answered permission modal's anchor (`"Do you want to proceed"`) lingered in scrolled-up history → the re-read reported the class **still present** → `AnswerModal` returned a spurious `"… modal still present after answering"` error → the consumer re-drove an already-committed turn (the destructive re-paste class the cross-repo review 2026-07-03 flagged downstream at `deliver.go:155-208`).

**#152 delivers the production fix, not this ticket.** #152 ported `DetectModalClass` onto the region-scoped rendered grid *in place*, keeping the `func(snap []byte) ModalClass` signature and explicitly keeping `answer.go:123` as an untouched caller. `modalDismissed` inherits region-scoping for free. `TestModalDismissed`'s four existing sub-cases never place the answered class's anchor in history-only, so none would have caught the original bug or would notice a future regression of the fix.

**What remains here** is the dismissal-seam regression that locks the behaviour at the call site whose false positive causes consumer churn — one layer above #152's detection-level regression.

## Design

Test-only. No package structure, types, or interfaces change.

**New sub-case in `TestModalDismissed`** (name suggestion: `history-only permission anchor counts as dismissed`):

- Build the forged buffer with the **same shape as `modal_test.go:124`**: `prompt` (`"Do you want to proceed?"`) followed by a long transcript body (`strings.Repeat("transcript body line\r\n", 25)`) that pushes the anchor above the bottom `permissionRegionRows` window. Build it inline in `answer_test.go` — do **not** extract a shared helper across the two test files (cross-file coupling for a one-line fixture is scope creep the AC does not ask for).
- Guard the fixture with a `strings.Contains(..., "Do you want to proceed")` sanity assertion (mirrors #152's `t.Fatal("fixture lost the forged phrase")`) so the test can never silently pass on a fixture that lost the anchor — the whole point is that the anchor *is* present in the buffer but not in the live region.
- Drive `modalDismissed(context.Background(), ModalClassPermission, snap, time.Second)` where `snap := func() []byte { return forged }`. Assert it returns **`true`** (the answered class is gone from the live region).
- Assert the short-circuit: `time.Since(start) < answerConfirmPoll` (mirrors the existing `dismissed immediately when class is gone` sub-case). Because `DetectModalClass(forged) == Unknown` on the first top-of-loop check, `modalDismissed` returns before the first poll tick — this pins that the `true` comes from the class-gone signal, not from an accidental timeout-to-`true` (which cannot happen: timeout returns `false`).

### The seam contract this asserts

```
modalDismissed(ctx, ModalClassPermission, () -> <permission anchor only above the region>, timeout)  ==  true
```

### Distinct from #152's regression — do not duplicate

| | #152 `TestDetectModalClassPermissionRegion` (modal_test.go) | #161 new sub-case (answer_test.go) |
|---|---|---|
| Layer | detection | dismissal seam |
| Call | `DetectModalClass(forged)` | `modalDismissed(ctx, Permission, ()->forged, t)` |
| Assert | `== ModalClassUnknown` | `== true` (dismissed) |

Same fixture *shape*, different call and different assertion target. The two are complementary, not redundant: #152 proves detection excludes the forgery; #161 proves the seam that consumes detection reports *dismissed* rather than *still present*.

## Concurrency model

None introduced. `modalDismissed` runs a `time.Timer` + `time.Ticker` on the caller's goroutine and spawns nothing; the new sub-case exercises only its top-of-loop short-circuit (returns before the first `select`). The existing `ctx cancelled` and `times out` sub-cases already cover the blocking exit paths — unchanged.

## Error handling

The behaviour under test is the **absence** of a false-positive error. When the answered modal is genuinely gone from the region, `modalDismissed` returns `true` and `AnswerModal` returns `nil` — no `"still present"` error, no consumer re-drive. The complementary genuine-still-present path stays asserted by the existing `still present times out to false` sub-case (AC #3): a live in-region modal must still return `false` so `AnswerModal` still surfaces the error. Both assertions must coexist — the positive (history-forged → dismissed) alone would pass even if region-scoping regressed into "every permission modal reads dismissed"; the retained negative case is what forbids that.

## Testing strategy

The deliverable *is* a test. Scenarios for the new `TestModalDismissed` sub-case:

- **Input:** `snap()` returns a synthetic buffer where `"Do you want to proceed?"` sits at the top, pushed above the bottom `permissionRegionRows` window by ~25 lines of transcript body (`\r\n`-terminated). No live PTY, no `Session`.
- **Expected:** `modalDismissed(ctx, ModalClassPermission, snap, time.Second) == true`, returning in `< answerConfirmPoll` (short-circuit).
- **Guard:** `strings.Contains` sanity check that the anchor is present in the buffer (else the forgery contrast is void).

**No regressions permitted** (AC #3): `still present times out to false` (real `permission-snapshot.bin`, live modal in region) stays green — the `"still present after answering"` error still fires for a genuinely-present modal.

**Determinism / no live PTY** (AC #4): synthetic `\r\n` bytes per the `codebase/150` fixture rule. No new `.bin` file, no filesystem access, no PTY. Chose synthetic bytes over a captured `.bin` because the buffer geometry (anchor above the 12-row region) must be exact and legible in the test; a captured `.bin` would obscure why it reads `Unknown`.

**Verification performed during spec authoring:** composed the exact sub-case against `main` (post-#152) in a throwaway in-package test and ran it — `PASS`, short-circuited before the first poll tick. `TestModalDismissed` (4 cases) and `TestDetectModalClassPermissionRegion` are green on `main`. The developer therefore ships green with zero production change. If the sub-case instead fails after implementation, that is a #152 region-scoping gap → route back to #152 per AC/ticket; **do not patch `answer.go` here.**

## Open questions

None. The seam is injectable, the fixture shape is proven by #152, and the composed sub-case is empirically green on `main`.

## Security review

**Verdict:** PASS

This ticket is `security-sensitive` because it sits on the attacker-influenceable path the cross-repo review 2026-07-03 flagged: claude's rendered screen output feeds the modal-state classification that gates the permission answer/confirm cycle. The change is **test-only** (zero production lines); the review below asks whether the test correctly locks — and does not weaken — the trust boundary #152 established.

**Findings:**

- **[Trust boundaries]** No MUST FIX. The boundary is `claude screen output (untrusted, tool-/prompt-influenceable) → the dismissal-confirmation decision in modalDismissed`. #152 relocated the boundary to the region-scoped grid render inside `DetectModalClass`; this ticket adds a regression that pins the boundary at the `modalDismissed` seam — verifying the exact attacker technique the review named (a `"Do you want to proceed"` anchor forged in scrolled-up history) can no longer force a spurious "still present". The test does not create, move, or widen any boundary; it asserts the existing one. It cannot mask a genuine still-present modal because AC #3 retains the live-in-region `false` assertion (see [Threat model alignment]).
- **[Tokens, secrets, credentials]** N/A — no tokens, secrets, or credentials touched. Synthetic in-memory bytes only.
- **[File operations]** No findings. The fixture is synthetic in-memory bytes (§ Testing strategy), not a file; no path derives from user input, so no path-traversal / TOCTOU / symlink surface. AC #4 permits a captured `.bin`, but the design deliberately chooses in-memory bytes → zero filesystem surface.
- **[Subprocess / external command execution]** N/A — no `exec`, no PTY. The test drives `modalDismissed` through a fake `snapshot func() []byte`; the ticket explicitly forbids a live PTY.
- **[Cryptographic primitives]** N/A — no randomness (fixture is a fixed literal), no secret comparison.
- **[Network & I/O]** N/A — no sockets, no reads from untrusted I/O. The "untrusted input" is a deterministic in-memory literal representing hostile screen content; there is no unbounded read to cap.
- **[Error messages, logs, telemetry]** No findings. The test adds no error text and no logging; it asserts the *absence* of the `"… modal still present after answering"` false-positive error path. No token/path/state leak surface is introduced.
- **[Concurrency]** No findings. `modalDismissed` spawns no goroutine and the new sub-case exercises only its top-of-loop short-circuit (returns before the first `select`); no lock, no shared mutable state, no goroutine-leak surface. The retained `ctx cancelled` / `times out` sub-cases cover the blocking exit paths.
- **[Threat model alignment]** In scope and addressed. The review's root-cause criticals — *state classified by substring-matching a raw append-only history buffer* — have their downstream consequence at `deliver.go:155-208` (destructive re-paste of an already-committed turn). This ticket's assertion targets precisely the seam where the history-forgery would otherwise trigger that re-paste, and pairs it with the retained negative case (genuine in-region modal → `false` → error still fires) so the fix cannot silently invert into "all permission modals read dismissed". The complementary detection-level assertion is owned by #152's `TestDetectModalClassPermissionRegion` and is intentionally not duplicated here.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-07-06

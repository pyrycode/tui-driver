# #164 — Retire the spinner-freeze watchdog arm

**Size:** XS (override down from PO's `size:s`). Deletion-dominated, single package, 3 production files touched (one doc-only), ~5 tests removed/trimmed. Not security-sensitive (no `security-sensitive` label; liveness/timeout detector, not a permission/auth gate).

**Decision: RETIRE the freeze arm.** Remove its firing logic from `CheckWatchdog` and its plumbing (`ObserveSpinner`, the `spinnerActive`/`lastSpinner*` bookkeeping, the `ParseSpinner → ObserveSpinner` wiring in the watchdog loop). Keep `ParseSpinner` (independent verb-telemetry consumer). Keep the public `SpinnerFreezeLimit` opts field and `DefaultSpinnerFreezeLimit` const as documented, inert no-ops so the change stays library-internal and cascades zero edits to the 8 spike binaries.

---

## Files to read first

- `pkg/tuidriver/tracker.go:56-148` — `Tracker` struct fields, `NewTracker`, `ObserveSpinner` (101-125), `CheckWatchdog` (freeze arm at 143-146). This is the bulk of the change.
- `pkg/tuidriver/watchdog.go:34-94` — `runWatchdogLoop` per-tick work list doc (34-47) and body (82-93); the `ParseSpinner → ObserveSpinner` lines at 87-88 are removed. Also the `DefaultWatchdogTick` doc at 8-13 name-checks `SpinnerFreezeLimit`.
- `pkg/tuidriver/state.go:197-229` — `ParseSpinner` + its "Consumer paths" doc (204-217). **Keep the function and its `(verb, totalSeconds, ok)` signature unchanged**; only rewrite the doc claim about feeding `ObserveSpinner`/the freeze arm.
- `pkg/tuidriver/tracker_test.go:9-131` — the two defaults tests (9-30, both reference the internal `spinnerFreezeLimit` field) and the three freeze-arm tests (73-131). The PTY-quiet test (47-71) stays untouched.
- `pkg/tuidriver/watchdog_test.go:42-175` — `TestRunWatchdogSpinnerFreezeWedge` (68-124) is removed; three sibling tests set `SpinnerFreezeLimit: 1*time.Hour // disable spinner arm` (47, 134, 158) — those comments become false claims.
- `cmd/spike-long-prompt/main.go:241` — `v, _, ok := tuidriver.ParseSpinner(session.Snapshot())`. The one production consumer of `ParseSpinner`; reads verb + ok, **discards seconds**. Must keep compiling → do not touch the signature.
- `CLAUDE.md:23` — the "Spinner caveat (claude 2.1.158)" paragraph. In scope per AC #2.
- `docs/knowledge/architecture/system-overview.md:95,101,102` — carry freeze-arm / `ParseSpinner → ObserveSpinner` claims. **Documentation-phase-owned; NOT a developer AC.** Listed only so the developer doesn't try to "helpfully" fix them (see § Out of scope).

## Context

`CheckWatchdog`'s spinner-freeze arm (`tracker.go:143-146`) fires only when `ParseSpinner`'s class-A `✻ Verb for Ns` seconds-counter stops incrementing while the spinner stays visible. The pinned Claude (2.1.158) renders class-B/C ellipsis forms, for which `ParseSpinner` returns `ok=false` (0/667 frames match — root bug #124). So in production `ObserveSpinner(false, 0)` is the only call ever made: `spinnerActive` never becomes true, and the arm can never fire. It nonetheless ships as a working, documented API with a `SpinnerFreezeLimit` knob and per-spike calibration.

**Why retire, not rework** (Evidence-Based Fix Selection + belt-and-suspenders-different-fabric): the freeze case is already covered by two *deterministic*, *live* detectors:

1. **PTY-quiet arm** (`CheckWatchdog`, same function) — a genuinely frozen claude stops repainting, `Buffer.QuietFor()` exceeds `PTYQuietLimit`, and this arm fires. Unchanged by this ticket.
2. **`EventKindStallDetected`** (since #141, `Events()` path) — fires mid-turn when `Buffer.QuietFor()` exceeds the limit AND no JSONL entry arrived in that window. This is the JSONL-correlated form of the exact "spinner visible without progress" semantics a rework would target.

A reworked "spinner-visible-without-PTY-progress" arm would be a redundant third detector reading the same PTY-quiet signal arm #1 already reads. There is no observed failure mode it would catch that the two live detectors miss. Building it would violate "don't ship a defense for a failure mode that hasn't been observed."

## Design

Package `tuidriver`, no new files, no new exported types. All changes are removals + doc rewrites, except the two public symbols explicitly retained as no-ops.

### `tracker.go`

- **`CheckWatchdog`** — delete the freeze-arm branch (143-146). The function keeps only the PTY-quiet arm and its `return nil`. `now := time.Now()` becomes unused once the branch is gone — remove it too. Update the error-format doc block (131-134) to drop the `"spinner counter frozen ..."` example line.
- **`ObserveSpinner`** — delete the entire method (101-125). Its only non-test caller is `runWatchdogLoop`, edited below.
- **`Tracker` struct** — delete the `lastSpinnerProgressAt`, `lastSpinnerTotal`, `spinnerActive`, and `spinnerFreezeLimit` fields (60-64). None have a reader after the arm and `ObserveSpinner` are gone.
- **`NewTracker`** — stop copying/defaulting `spinnerFreezeLimit` (72, 77-79). `opts.SpinnerFreezeLimit` is now accepted and ignored.
- **`TrackerOpts.SpinnerFreezeLimit`** (30-34) — **keep the field.** Rewrite its doc to state it is retained for source compatibility and has had no effect since #164 (the freeze arm was retired; use the PTY-quiet arm and, for a JSONL-correlated stall signal, `Events()`'s `EventKindStallDetected`). Retaining it is what keeps the 8 spikes (`SpinnerFreezeLimit: spinnerFreezeLimit`) compiling with zero edits.
- **`DefaultSpinnerFreezeLimit`** (14) — **keep the const exported** to avoid an API break for the external `pyry` consumer. Rewrite its doc comment to mark it a retained no-op (paired with the field above). It is now internally unreferenced; Go allows an unused exported package-level const, so this compiles.
- **`Tracker` type doc** (37-53) — rewrite from "two-arm watchdog (PTY-heartbeat + spinner-freeze)" to a **single-arm** (PTY-quiet) description. Delete the numbered arm-2 description (45-49).

Contract after the change: `CheckWatchdog(buf *Buffer) error` returns non-nil iff `buf.QuietFor() > ptyQuietLimit`. Signature unchanged.

### `watchdog.go`

- **`runWatchdogLoop`** (82-93) — delete lines 87-88 (`ParseSpinner` + `ObserveSpinner`). The tick body reduces to `if err := tr.CheckWatchdog(buf); err != nil { return err }`. `buf.Snapshot()` was called only to feed `ParseSpinner`, so it drops out of the loop entirely (`CheckWatchdog` takes `buf` directly and reads `buf.QuietFor()`). `ParseSpinner` stays exported and imported package-internally — only this *call site* is removed.
- **`RunWatchdog` per-tick work-list doc** (34-47) — collapse steps 1-4 to the single remaining step (`CheckWatchdog`). Remove the ParseSpinner/ObserveSpinner narration and the "class-B/C/D return ok=false" aside.
- **`runWatchdogLoop` doc** (70-74) and **`DefaultWatchdogTick` doc** (8-13) — drop the `ParseSpinner → ObserveSpinner` / `SpinnerFreezeLimit` references; leave the PTY-quiet framing.

### `state.go`

- **`ParseSpinner`** — function and `(verb, totalSeconds, ok)` signature **unchanged** (spike-long-prompt + `state_test.go` depend on it). Rewrite the "Consumer paths" doc (204-217): keep bullet 1 (verb is empirical telemetry). Replace bullet 2 — the total-seconds counter no longer feeds any in-library consumer now that `ObserveSpinner`/the freeze arm are retired (#164); note the counter is still returned for callers that want it but has no live consumer, and the spike reads only the verb. Keep the "Sibling extractor: ParseSpinnerTokens" note.

### `CLAUDE.md:23`

Rewrite the Spinner-caveat paragraph. Current text says the arm is "effectively dead"; after this change it is *removed*. State: `ParseSpinner` matches 0/667 class-A frames on 2.1.158, so the spinner-freeze watchdog arm was **retired** in #164 (the PTY-quiet arm and `Events()`'s `EventKindStallDetected` cover freeze). `ParseSpinner` is retained for verb telemetry; `SpinnerFreezeLimit` is a retained no-op. Keep the surviving guidance (prefer the `"esc to interrupt"` hint as the in-flight anchor).

## Concurrency model

Unchanged. `runWatchdogLoop` remains a single ticker-driven select on `ctx.Done()` / `ticker.C`, one CheckWatchdog per tick, `defer ticker.Stop()`. Removing the two lines removes no goroutines, channels, or shutdown paths. `Tracker` stays mutex-guarded; removing fields removes no locking.

## Error handling

`CheckWatchdog` now has exactly one failure mode (`"watchdog: PTY quiet for %s (last state: %s)"`). The `"spinner counter frozen ..."` string and its call site are deleted. No new error paths.

## Testing strategy

After the change, `go test ./...` must pass and no test may assert the dead class-A counter path "works" (AC #3). The PTY-quiet arm's tests must be untouched and still green (AC #4).

**Remove entirely** (they exercise `ObserveSpinner` / the freeze arm):
- `tracker_test.go`: `TestTrackerCheckWatchdogSpinnerFreeze`, `TestTrackerSpinnerProgressResetsFreeze`, `TestTrackerSpinnerDisappearedClearsState`.
- `watchdog_test.go`: `TestRunWatchdogSpinnerFreezeWedge` (including its buffer warm-keep goroutine).

**Trim** (they reference the now-removed internal field or a now-false comment):
- `tracker_test.go` `TestTrackerDefaultsApplied` — drop the `spinnerFreezeLimit == DefaultSpinnerFreezeLimit` assertion; keep the `ptyQuietLimit` assertion.
- `tracker_test.go` `TestTrackerExplicitLimitsRespected` — drop the `SpinnerFreezeLimit`/`tr.spinnerFreezeLimit` half; keep the `PTYQuietLimit` half.
- `watchdog_test.go` `TestRunWatchdogPTYQuietWedge`, `TestSessionRunWatchdogMethod`, `TestRunWatchdogDefaultTickApplied` — remove the `SpinnerFreezeLimit: 1 * time.Hour // disable spinner arm` lines; the field no longer exists as an arm to disable, so the comment lies. (Setting the retained no-op field is harmless, but the "disable spinner arm" comment must go — cleanest is to drop the line.)

**Keep untouched** (AC #4 — the PTY-quiet arm and its behaviour must be unchanged):
- `tracker_test.go` `TestTrackerCheckWatchdogQuietBuffer`, `TestTrackerRecordTransition`.
- `watchdog_test.go` `TestRunWatchdogContextCancellation`, `TestRunWatchdogPTYQuietWedge` body (minus the trimmed line), `TestSessionRunWatchdogMethod` body, `TestRunWatchdogDefaultTickApplied` body.

**No new test needed.** The retire path removes a detector; there is no new positive behaviour to assert. `ParseSpinner`'s own tests (`state_test.go:379-414`) are unaffected — the signature is unchanged.

Developer must also confirm the 8 spike binaries still build (`go build ./...`), proving the retained `SpinnerFreezeLimit` field kept the call sites compiling.

## Out of scope (documentation phase owns these)

- `docs/knowledge/architecture/system-overview.md` lines 95, 101, 102 and `docs/knowledge/codebase/*.md` carry freeze-arm / `ParseSpinner → ObserveSpinner` claims. These are **documentation-phase-owned** and are updated from this spec + the merged diff after the PR lands. The developer must **not** edit them (per architect constraint: the developer's worktree mutates only code, tests, and this spec file — plus `CLAUDE.md`, which AC #2 names explicitly).

## Open questions

- **`DefaultSpinnerFreezeLimit` — retain vs remove.** Spec retains it exported (no-op) to avoid an external-API break for `pyry`. If the developer confirms via the merged consumer that nothing outside this repo references it, a follow-up could remove it — but that is not in scope here and must not be done speculatively (it would be a gratuitous API break for a symbol that costs nothing to keep). Leave retained.

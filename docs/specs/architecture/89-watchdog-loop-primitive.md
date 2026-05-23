# Spec: Promote watchdog goroutine pattern to `pkg/tuidriver` (#89)

## Files to read first

- `pkg/tuidriver/tracker.go:101-148` — `ObserveSpinner` + `CheckWatchdog` contract. The new primitive feeds the former and short-circuits on the latter; the library code added here is the consumer-side glue that ties them at 1 Hz.
- `pkg/tuidriver/buffer.go:53-71` — `Buffer.Snapshot` + `QuietFor` (read by `CheckWatchdog`). The new loop snapshots once per tick and passes the same `*Buffer` to `CheckWatchdog`.
- `pkg/tuidriver/state.go:56-96` — `ParseSpinnerTokens` precedent. `ParseSpinner` (new) is the sibling extractor for the spike-local regex, factored the same way (package-private regex var, exported pure function over `StripANSI(snap)`, godoc names which spinner-class renderings it matches and which it skips). Both extractors live next to `IsThinking` / `SpinnerGlyph`.
- `pkg/tuidriver/state_test.go` — table-test shape for the new `ParseSpinner` tests. Use the existing `TestParseSpinnerTokens*` cases as the structural template (table-test of stripped snapshots → expected tuple).
- `pkg/tuidriver/events.go:90-117` — precedent for a library primitive that owns a goroutine + ctx-cancellation termination. The new primitive is *blocking* (caller spawns the goroutine), so it does not need the channel/buffered-output shape — but the ctx-shutdown semantics and the godoc explanation of the merge-loop's tick cadence are the model to mirror for tone and contract clarity.
- `pkg/tuidriver/tracker_test.go:47-97` — `TestTrackerCheckWatchdogQuietBuffer` and `TestTrackerCheckWatchdogSpinnerFreeze` use short limits + real wall-clock sleeps to drive synthetic wedges. The new tests reuse this idiom — no clock seam needed because the loop period is configurable (a 20 ms tick + 50 ms limit drives a sub-100 ms test).
- `cmd/spike-long-prompt/main.go:43-65` — local `watchdogTick`, `ptyQuietLimit`, `spinnerFreezeLimit` constants + `spinnerRe` var (the 7-spike-duplicated regex). After migration: `watchdogTick` and `spinnerRe` are deleted; `ptyQuietLimit` + `spinnerFreezeLimit` stay (still passed into `TrackerOpts`).
- `cmd/spike-long-prompt/main.go:137-161` — the inline ticker goroutine to replace. The ~22 lines from `wg.Add(1)` through the closing `}()` collapse to ~7 lines.
- `cmd/spike-long-prompt/main.go:235-280` — Events handler. Line 250's `matchSpinner(tuidriver.StripANSI(rb.Snapshot()))` call also gets the rewrite to `tuidriver.ParseSpinner(rb.Snapshot())` (the new function strips ANSI internally, matching `ParseSpinnerTokens`).
- `cmd/spike-long-prompt/main.go:300-313` — local `matchSpinner` function. Deleted in full after migration; the `strconv` import becomes unused and is dropped.
- `docs/knowledge/architecture/system-overview.md:98` — Events / `EventKindPtyThinking` consumer-side gotcha already documents that "spike-long-prompt's `matchSpinner` regex demanding the full `✻ Verb for Ns` capture" is the spike's stricter local predicate. This is the call site at main.go:250 — after migration the prose still holds, just the regex now lives in the library as `ParseSpinner`. No doc edit required from architect; the documentation phase owns that note.
- `CLAUDE.md` — "This library owns: PTY allocation, byte-stream parsing, state detection, modal handling, keystroke injection, session lifecycle, watchdog." Watchdog is in-scope; `ParseSpinner` is "byte-stream parsing." Both additions are within library scope.

## Context

The 1 Hz watchdog goroutine is copy-pasted inline in 7 spike binaries (`spike-one-turn`, `spike-multi-turn`, `spike-cancel`, `spike-permission`, `spike-ask-user`, `spike-multiselect`, `spike-long-prompt`) alongside an identical local `matchSpinner` function and `spinnerRe` regex. Same shape as #87 (PTY-input helpers) and the #58–#62 family (JSONL helpers): the spikes proved the calibrated behaviour, and now the calibrated behaviour wants to live in the library so the next consumer (the `pyry acp` rewrite) reuses it without re-deriving it from spike source.

The loop is short, but the de-dup goal is real: 7 watchdog loops + 7 `matchSpinner` copies = 14 deletion sites once the follow-up sweep runs across the other 6 spikes. This ticket lands the library primitive and migrates `spike-long-prompt` as the proof-of-usability consumer; the other 6 spikes are explicitly out of scope (follow-up tickets, one per spike, mechanical templates of the spike-long-prompt migration once it lands).

## Design

### Design choice: bundle (option a)

The architect picks design (a) from the ticket's Technical Notes — promote both the watchdog loop AND `matchSpinner` into the library. Rationale:

1. **The regex is identical across all 7 spikes** (verified: `grep -n spinnerRe cmd/*/main.go` confirms `^\xe2\x9c\xbb\s+(\S+(?:\s+\S+)?)\s+for\s+(?:(\d+)m\s+)?(\d+)s` in every spike). De-duping the watchdog loop without de-duping the regex leaves 7 future deletion sites that the next ticket has to chase — design (b)'s callback shape would require every migrating consumer to keep importing the regex locally or to write a separate ticket to promote it.
2. **Precedent for the pure-extractor shape exists** in `state.go:65-96` (`spinnerTokensRe` + `ParseSpinnerTokens`). The new code reuses the same template — package-private regex, exported pure function over `StripANSI(snap)`, godoc naming which spinner-class renderings it does and does not match.
3. **The spike consumer's downstream-of-the-loop call site also benefits.** `spike-long-prompt/main.go:250` calls `matchSpinner` from inside the Events handler (the spike's local stricter thinking-detected predicate). Under design (b) the spike keeps its local copy alive solely for that call. Under design (a) both call sites use `tuidriver.ParseSpinner` and the spike file loses 15 lines (the regex var + helper) instead of 8.
4. **Library surface stays small.** The bundle adds three exported names (`RunWatchdog`, `WatchdogOpts`, `ParseSpinner`) plus one exported constant (`DefaultWatchdogTick`). Below the 5-new-exported-types size budget.

### Package placement

- `ParseSpinner` lives in `pkg/tuidriver/state.go` next to `ParseSpinnerTokens`. Same domain (spinner-class snapshot extraction), same template, same test file. Adding a sibling extractor next to an existing one is the obvious placement.
- `RunWatchdog` lives in a new file `pkg/tuidriver/watchdog.go`. Rationale: it is not a method on any existing type (the ticket's Technical Notes call this out — neither `*Tracker` nor `*Buffer` is the natural owner of a goroutine-driven 1 Hz loop), and the surface is narrow enough that mixing it into `tracker.go` would conflate the "synchronous predicates a consumer calls" file with the "blocking loop a consumer spawns" file. A dedicated file keeps the godoc focused on the loop semantics and the spawn pattern.
- New file: `pkg/tuidriver/watchdog_test.go`. Tests for `RunWatchdog` live in their own file matching the source-file convention used by `tracker_test.go`, `events_test.go`, etc.

### `ParseSpinner` (added to `pkg/tuidriver/state.go`)

Signature: `func ParseSpinner(snap []byte) (verb string, totalSeconds int, ok bool)`

Behaviour: strips ANSI from `snap` internally (matching `ParseSpinnerTokens`), runs the long-form spinner regex `✻\s+(\S+(?:\s+\S+)?)\s+for\s+(?:(\d+)m\s+)?(\d+)s`, returns the verb (group 1), `minutes*60 + seconds` (groups 2 + 3), and `true`. On no match: `("", 0, false)`.

Godoc must:
- Name which spinner-class renderings this matches: class A (`✻ Baked for 2s`) and the time-tail half of class C (`✻ Actualizing… (2s · …)` — note the regex requires the literal `for` so class C with parens does NOT match; this is the documented class-C gap, intentional, matching `ParseSpinnerTokens`'s class-A/B gap).
- Name which spinner-class renderings this skips: class B (`✻ Channeling…` — no `for Ns` tail) and class D (full-sentence aphorisms — still unobserved as of 2026-05-23). The "miss" semantics are the same as `ParseSpinnerTokens`: returns `(_, _, false)`, not an error.
- Name the consumers and their dependence on the verb (the empirical log in spike READMEs) vs. the total seconds (the spinner-freeze arm of `CheckWatchdog`).
- Cross-reference `ParseSpinnerTokens` — sibling extractor over the same snapshot, different field, both classes-incomplete by construction.

Package-private regex: `var spinnerForRe = regexp.MustCompile(...)`. Lowercase to keep the regex internal; the exported function is the API. Name `spinnerForRe` rather than `spinnerRe` to disambiguate from the existing `spinnerTokensRe` and from "the spinner glyph itself."

### `RunWatchdog` (added to new `pkg/tuidriver/watchdog.go`)

Constants:

```go
// DefaultWatchdogTick is the loop period RunWatchdog uses when
// WatchdogOpts.Tick is zero. Matches the 1 Hz cadence used by every
// spike binary.
const DefaultWatchdogTick = 1 * time.Second
```

Options struct:

```go
// WatchdogOpts configures RunWatchdog.
type WatchdogOpts struct {
    // Tick is the loop period. 0 → DefaultWatchdogTick.
    Tick time.Duration
}
```

Signature: `func RunWatchdog(ctx context.Context, buf *Buffer, tr *Tracker, opts WatchdogOpts) error`

Behaviour:

- Allocate a `time.Ticker` at `opts.Tick` (or `DefaultWatchdogTick`). `defer ticker.Stop()`.
- Loop: `select` on `ctx.Done()` and `ticker.C`.
- On `ctx.Done()`: return `nil`. Cancellation is requested, not detected — same convention `Spawn`/`Session.Wait` use for their "caller asked us to stop" paths.
- On `ticker.C`: snapshot the buffer, strip ANSI, `ParseSpinner(snap)`, `tr.ObserveSpinner(ok, total)`, `tr.CheckWatchdog(buf)`. If `CheckWatchdog` returns non-nil, return that error verbatim. Otherwise continue.
- Returns at most once (either ctx-cancel or wedge); subsequent ticks do not fire.

Godoc must:
- Name the per-tick work list (snapshot → StripANSI → ParseSpinner → ObserveSpinner → CheckWatchdog) so consumers reading the API can decide whether their workload needs any of those steps customised. Customisation is a "no" today — same calibration as every spike — but the doc names the steps so future divergence is a deliberate decision.
- Name the two return conditions and their distinguishing return values (nil on ctx-cancel; the `CheckWatchdog` error verbatim on wedge). Consumers wrap the call in a goroutine and treat non-nil as fatal.
- Cite the recommended consumer pattern (log the non-nil return; cancel the parent context with the error as cause). Show the 3-line snippet as a godoc example:
  ```
  go func() {
      if err := tuidriver.RunWatchdog(ctx, sess.Buffer, tr, tuidriver.WatchdogOpts{}); err != nil {
          log.Printf("%v", err)
          cancelCause(err)
      }
  }()
  ```
- Cross-reference `Tracker.ObserveSpinner` + `Tracker.CheckWatchdog` as the two `*Tracker` calls per tick, and `Buffer.Snapshot` + `Buffer.QuietFor` (the latter indirectly via `CheckWatchdog`) as the two `*Buffer` reads per tick.
- Note that `WatchdogOpts.Tick` defaults to 1 Hz; non-default ticks are honoured (tests use 20 ms ticks to drive synthetic wedges).

### Why returned-error rather than callback or cancel-cause

The AC offers three error-surfacing shapes; the spec picks returned-error. Rationale:

- **Callback shape** (`onWedge func(error)`) couples the library to the consumer's logging/cancellation strategy. The library would have to define whether the callback runs inline (blocking the loop) or in a fresh goroutine (race window between callback return and loop exit). Returned-error pushes both decisions to the consumer.
- **Cancel-cause integration** (library takes `context.CancelCauseFunc`) couples the library to `context.WithCancelCause` specifically. Consumers using `context.WithCancel` (no cause) would need a no-op wrapper. Returned-error is API-agnostic — the consumer plugs in whichever context shape they use.
- **Returned-error** is the idiomatic Go signature for "blocks until done, may fail." The consumer wraps the call in a goroutine, reads the return value, logs + cancels as they see fit. Exactly mirrors the spike's existing inline shape; the only thing the library hides is the ticker bookkeeping.

The AC constraint "fires exactly once per loop, then exits" is satisfied trivially: the function returns on the first `CheckWatchdog` error and the ticker stops on the deferred `ticker.Stop()`.

### Concurrency model

- `RunWatchdog` runs synchronously on the calling goroutine. Consumers spawn it.
- One ticker, one select loop, no internal goroutines. The function holds no state of its own — it reads from `buf` (thread-safe per the Buffer's documented contract) and calls `*Tracker` methods (also thread-safe per the Tracker doc).
- `ticker.Stop()` releases the ticker's resources on every return path via `defer`.
- The function never panics on the documented inputs (nil `buf` or nil `tr` would panic on first dereference; this matches the rest of the library's "construct or you get a nil-deref" posture and is not worth a defensive check).

### Error-handling envelope

Two error paths:

1. **`CheckWatchdog` error** — returned verbatim. The error string already names the failure mode (`watchdog: PTY quiet for ...` or `watchdog: spinner counter frozen ...`) and the last recorded transition state. No wrapping; the consumer's `log.Printf("%v", err)` is what the spike already does and what the godoc example reproduces.
2. **No error path for ticker creation or buffer access.** `time.NewTicker` panics on non-positive duration, but the default-injection (`if opts.Tick <= 0 { opts.Tick = DefaultWatchdogTick }`) at the function entry guarantees a positive value. `Buffer.Snapshot` cannot fail. `Tracker.CheckWatchdog` returns nil-or-error, no panic.

## Consumer migration: `cmd/spike-long-prompt/main.go`

Edits, in source-line order:

1. **Line 44 `watchdogTick` constant** — delete (no longer referenced; library owns the default).
2. **Line 61-64 `spinnerRe` var + godoc** — delete (library owns the regex internally).
3. **Lines 139-161 inline watchdog goroutine** — replace the entire `go func()` block (the 22 lines from `wg.Add(1)` through the closing `}()`) with:
   ```go
   wg.Add(1)
   go func() {
       defer wg.Done()
       if err := tuidriver.RunWatchdog(rootCtx, rb, tr, tuidriver.WatchdogOpts{}); err != nil {
           logger.Printf("%v", err)
           cancelCause(err)
       }
   }()
   ```
4. **Line 250 inline `matchSpinner(tuidriver.StripANSI(rb.Snapshot()))` call** — replace with `tuidriver.ParseSpinner(rb.Snapshot())`. The library function strips ANSI internally; the explicit `StripANSI` wrapper goes.
5. **Lines 300-313 local `matchSpinner` function** — delete in full.
6. **Imports** — the `strconv` import (only used by `matchSpinner`) becomes unused; drop it. The `regexp` import (only used by `spinnerRe`) also becomes unused; drop it.

`go build ./cmd/spike-long-prompt` and `go vet ./cmd/spike-long-prompt` must pass. The consumer's `make e2e-spike-long-prompt` integration test exercises the new path against real `claude`; the spec assumes that target stays green (it is the same byte-shape on the wire, same `CheckWatchdog` cadence, same calibrations).

Out of scope: the other 6 spikes (`spike-one-turn`, `spike-multi-turn`, `spike-cancel`, `spike-permission`, `spike-ask-user`, `spike-multiselect`). Their `matchSpinner` copies and inline watchdog loops stay. Follow-up sweep tracked separately per the ticket body.

## Testing strategy

### `ParseSpinner` — table tests in `state_test.go`

Add a single `TestParseSpinner` table test, mirroring the shape of `TestParseSpinnerTokens` (or `TestIsIdle` if simpler). Cases:

- Class A — `✻ Baked for 2s` → `("Baked", 2, true)`.
- Class A with two-word verb — `✻ Quick witted for 13s` → `("Quick witted", 13, true)`.
- Class A with minutes — `✻ Baked for 2m 5s` → `("Baked", 125, true)`.
- Class B — `✻ Channeling…` → `("", 0, false)` (no `for Ns` tail).
- Class C — `✻ Actualizing… (2s · ↓1 tokens)` → `("", 0, false)` (parens, not `for`).
- No spinner glyph — `❯ ready` → `("", 0, false)`.
- Empty snapshot — `""` → `("", 0, false)`.
- Snapshot with ANSI escapes wrapping the spinner — assert the ANSI stripping happens internally (one case: prepend `"\x1b[2K\x1b[1G"` then the class-A rendering; expect the same `("Baked", 2, true)` as the plain class-A case).

All cases are pure byte-string assertions — no PTY, no clock, no goroutine.

### `RunWatchdog` — three tests in new `watchdog_test.go`

The AC names three sub-properties (ctx-cancel cleanly, wedge surfaced exactly once, configurable tick). One test per property; all three use synthetic wedges driven by short Tracker limits + short ticks, matching the wall-clock pattern `tracker_test.go` already uses (no clock seam needed — a 20 ms tick + 50 ms limit drives a sub-100 ms test).

Test sketches (scenarios, NOT implementation):

1. **`TestRunWatchdogContextCancellation`** — covers AC 6(a).
   - Setup: fresh `Buffer` with a recent `Append` (not stale), `Tracker` with default limits, `context.WithCancel`.
   - Action: spawn `RunWatchdog` in a goroutine; wait one tick (50 ms with default 1 s tick is too long — use `WatchdogOpts{Tick: 20 * time.Millisecond}`); cancel ctx; wait for the goroutine to return.
   - Assert: return value is `nil`; goroutine returns within one tick of cancel (use a `select` with a `time.After(100 * time.Millisecond)` guard against hang). No leaked goroutine (verified implicitly by the test exiting cleanly; if explicit verification is wanted, capture `runtime.NumGoroutine` before/after).
2. **`TestRunWatchdogPTYQuietWedge`** — covers AC 6(b) via the PTY-quiet arm.
   - Setup: `Buffer` with one `Append` of `"x"` then no more writes; `Tracker` with `TrackerOpts{PTYQuietLimit: 50 * time.Millisecond}`; `WatchdogOpts{Tick: 20 * time.Millisecond}`.
   - Action: call `RunWatchdog` synchronously (no goroutine — the test blocks until wedge or timeout). Wrap in a goroutine if the test wants a hang-guard via `select`.
   - Assert: return value is a non-nil error; `err.Error()` contains `"PTY quiet"`. Total elapsed < 200 ms (i.e. the loop exited promptly, not after many ticks).
3. **`TestRunWatchdogSpinnerFreezeWedge`** — covers AC 6(b) via the spinner-freeze arm; also covers AC 6(c) (custom tick).
   - Setup: `Buffer` with periodic `Append` writes from a helper goroutine that keeps it warm (write `"x"` every 10 ms via a separate goroutine for the test's duration; cancel that goroutine via `t.Cleanup`); `Tracker` with `TrackerOpts{PTYQuietLimit: 1 * time.Hour, SpinnerFreezeLimit: 50 * time.Millisecond}`; `WatchdogOpts{Tick: 20 * time.Millisecond}`.
   - Pre-load: `Buffer.Append(...)` a class-A spinner rendering (`"✻ Baked for 2s"`) and ensure subsequent appends keep the spinner-frozen snapshot stable (the same rendering — `ParseSpinner` will keep returning `(_, 2, true)`; `ObserveSpinner` sees no progress; freeze arm fires after 50 ms).
   - Action: call `RunWatchdog`; wait for return.
   - Assert: return value is a non-nil error; `err.Error()` contains `"spinner counter frozen"`. Custom tick (20 ms) was honoured — the wedge fired within ~70-100 ms of start (which is only possible with a sub-100 ms tick; the default 1 s tick would not have fired in that window).

The freeze-arm test is the densest because it has to keep the buffer warm (defeating the PTY-quiet arm) while the spinner snapshot stays frozen (driving the freeze arm). A single helper goroutine that appends `"\x1b[?25h"` (or any other ANSI-noise byte) every 10 ms is sufficient — the noise gets stripped before `ParseSpinner` runs, so the spinner rendering stays stable.

No clock-seam needed. The Tracker tests already use the wall-clock + short-limit pattern; the same idiom works here. The 20 ms tick keeps tests fast (sub-200 ms each).

### Library-side `go test ./...` must pass

The full test suite is part of the developer's verification loop. No new external dependencies.

## Open questions

- **Should `RunWatchdog` take `*Session` instead of `*Buffer`?** No. The function only needs the buffer (for `Snapshot` and via-`CheckWatchdog`-for `QuietFor`); taking the full `*Session` would suggest the library owns the watchdog-session coupling, which it doesn't (the consumer composes them). The spike's call site reads `rb := session.Buffer` already; passing `rb` to `RunWatchdog` is one extra word at the call site.
- **Should `WatchdogOpts` also accept `TrackerOpts`?** No. The Tracker is constructed separately, before the watchdog runs, because consumers `RecordTransition` against it during the linear state machine (see spike-long-prompt lines 117, 170, 181, 192, 200, etc., all called from outside any goroutine). Folding TrackerOpts into the watchdog primitive would invert that relationship — the watchdog would own the Tracker — and break the existing Tracker API which is explicitly excluded from changes in AC 4.
- **Should the spike consumer keep its local `watchdogTick` constant for documentation?** No. The library's `DefaultWatchdogTick` is the documented value; the spike does not customise it; `WatchdogOpts{}` (zero value) makes the default explicit at the call site. Adding a local constant just to set `WatchdogOpts{Tick: watchdogTick}` would be ceremony without benefit.
- **Does the library need a separate `WatchdogTickOptioned` constructor sketch for testability?** No. The `WatchdogOpts.Tick` field is the testability seam and is also the public knob the AC requires. One field, one purpose, no duplicate API.
- **Should `ParseSpinner` be renamed to disambiguate from `ParseSpinnerTokens`?** No. The naming pair mirrors how `IsIdle` / `IsThinking` coexist next to each other — both refer to "the spinner," and the suffix tells you which field is extracted. `ParseSpinner` extracts verb + seconds; `ParseSpinnerTokens` extracts the token count.

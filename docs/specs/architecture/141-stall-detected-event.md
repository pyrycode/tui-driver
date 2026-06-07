# Spec #141 — `stall_detected` typed Event off the existing watchdog arms

Part of pyrycode#596 (Phase 2 structured streaming). Implements the
safe-degrade marker from ADR 025: *"Stall (safe-degrade): not-idle +
PTY quiet + no JSONL progress → `stall_detected`."*

## Files to read first

- `pkg/tuidriver/events.go:15-117` — the `EventKind` enum, the
  per-kind doc convention, and the `Event` struct's "Field population
  by Kind" list. The new kind is added here; its doc comment follows
  the same convention as the existing kinds.
- `pkg/tuidriver/events.go:142-150` — `Events()` signature + the
  `mergeEvents` spawn line. Both change: `Events` gains a `*Tracker`
  param, and the spawn forwards `s.buffer.QuietFor` + the tracker's
  limit.
- `pkg/tuidriver/events.go:164-304` — the `mergeEvents` loop. The
  JSONL arm (lines 192-214) is where `lastJsonlAt` is recorded; the
  ticker arm (215-301) is where the stall rising-edge is computed and
  emitted, just before `prev = cur`.
- `pkg/tuidriver/events.go:306-328` — `ptyState` + `classify`. Extend
  `ptyState` with a `stalled bool`; **do NOT** compute it in
  `classify` (classify sees only the snapshot, not quiet timings).
- `pkg/tuidriver/tracker.go:56-81` — `Tracker` struct + `NewTracker`.
  `ptyQuietLimit` is set once in `NewTracker` and never mutated →
  immutable post-construction → safe to read unlocked from the merge
  goroutine. This is the single source of truth for the limit (AC4).
- `pkg/tuidriver/tracker.go:127-148` — `CheckWatchdog`'s **fatal**
  PTY-quiet arm. We reuse its `ptyQuietLimit` value but do **not**
  modify this method or its contract (the ticket's split-guard).
- `pkg/tuidriver/buffer.go:62-71` — `QuietFor` semantics: returns `0`
  before the first append, else `time.Since(lastAppendAt)`. The `0`
  pre-append return is why no false stall fires at session start
  (condition (b) cannot hold before any PTY byte).
- `pkg/tuidriver/state.go:29-54` — `IsIdle` / `IsThinking`. `IsIdle`
  (❯ present AND ✻ absent) is condition (a)'s discriminator; the merge
  loop already computes it as `cur.idle`.
- `pkg/tuidriver/events_test.go:71-111` and `:173-260` — the existing
  `mergeEvents` test patterns (`testSnap`, `mustReceiveEvent`,
  `assertEventChClosed`). The 9 `go mergeEvents(ctx, snap.Snapshot,
  jsonlCh, out, DefaultPollInterval)` call sites are **identical** →
  one `replace_all` updates them all (see Testing strategy).
- `cmd/spike-long-prompt/main.go:111-114` and `:208` — `tr` is already
  constructed at 111 and in scope at 208, the sole production caller of
  `Events()`. The update is a one-line arg addition.
- pyrycode `docs/knowledge/decisions/025-mobile-remote-head-interactive-session.md`
  § "Safe degradation" — the authoritative stall definition and the
  fact that the consumer wraps the library event as a wire
  `stall_detected {conversation_id}` (conversation_id is consumer
  state, so the library event carries no payload).

## Context

The stall signals exist today but are split across two mechanisms that
don't talk to each other:

- `Events()` (`events.go`) is the only library-owned goroutine that
  sees JSONL arrivals (it tails the file) **and** polls PTY state.
- `RunWatchdog` / `Tracker.CheckWatchdog` (`watchdog.go`,
  `tracker.go`) run a separate loop whose PTY-quiet arm is **fatal**
  (returns an error, terminates the loop) and reads only the buffer —
  never JSONL.

The combined stall condition needs both inputs, so the merge loop is
its only correct home. This ticket adds a single **non-fatal** typed
event to the `Events()` stream. It does not touch `RunWatchdog` or the
fatal arm; it reuses the `Tracker`'s configured PTY-quiet limit so
there is exactly one timeout in play.

## Design

### The new event kind

Add one `EventKind` constant, `EventKindStallDetected`, after
`EventKindJsonlEndOfTurn` in the enum (`events.go:20-80`). Document it
in the same per-kind style. Payload-free, mirroring the idle/thinking
and banner edges:

```go
// EventKindStallDetected is the safe-degrade marker: on a poll tick
// the session is mid-turn (not idle), the PTY has been quiet beyond
// the watchdog's PTYQuietLimit, AND no JSONL entry has arrived within
// that same window. Rising-edge: fires once on entry into the stalled
// condition, not every tick. No payload fields — the kind is the
// signal; consumers wanting the quiet duration call Session.QuietFor.
EventKindStallDetected
```

Add it to the `Event` struct's "Field population by Kind" doc block
(`events.go:94-105`): `EventKindStallDetected: no payload fields.`

**Source tag:** `EventSourcePty`. The event is emitted from the
ticker (poll) arm and its dominant input is PTY-quiet; a consumer that
dispatches on `Source` first groups it with the other tick-derived
state transitions (idle / thinking / modal / banner). No new
`EventSource` value.

**Why payload-free:** ADR 025 has the consumer emit
`stall_detected {conversation_id}` on the wire — `conversation_id` is
consumer state, not derivable from the library event. The only
diagnostic the library could add (quiet duration) is already reachable
via `Session.QuietFor()` at the instant the consumer receives the
event. Keeping it payload-free matches the dominant precedent (idle /
thinking / banner kinds) and leaves the `Event` struct unchanged.
Adding a duration field later is additive if a real need surfaces
(deferred — evidence-based).

### The three-condition predicate

Maps 1:1 onto ADR 025's "PTY quiet while not-idle and no JSONL
progress":

| AC condition | Predicate in the ticker arm |
|---|---|
| (a) not parked at idle prompt (mid-turn) | `!cur.idle` (i.e. `!IsIdle(snap)`, already computed) |
| (b) PTY quiet beyond the limit | `quietFor() > ptyQuietLimit` |
| (c) no JSONL within the same window | `now.Sub(lastJsonlAt) > ptyQuietLimit` |

`stalled := !cur.idle && quietFor() > ptyQuietLimit && now.Sub(lastJsonlAt) > ptyQuietLimit`

- **Single limit, single window** (AC4): both (b) and (c) compare
  against the same `ptyQuietLimit`, sourced from the `Tracker`. No new
  constant, no new `TrackerOpts` field, no second cadence — the check
  runs on the existing `DefaultPollInterval` (50 ms) merge tick.
- **`lastJsonlAt` zero value:** before any JSONL has arrived
  `lastJsonlAt` is the zero `time.Time`, so `now.Sub(lastJsonlAt)` is
  huge → (c) holds. Correct: "no JSONL has arrived" ⊃ "none within the
  window." It cannot fire prematurely because (b) still requires the
  PTY to have gone quiet for the full limit, and `QuietFor` returns `0`
  until the first PTY byte.
- **Modals suppress naturally in the common case:** at a *detected*
  permission / ask-user / picker / mcp modal the snapshot renders ❯
  with no spinner → `IsIdle` true → (a) false → no stall. No explicit
  `cur.modal` guard is needed for the AC. (See Open questions for the
  broken-modal residual.)

### Rising edge (AC5)

Store the result on `ptyState` so the existing `prev`/`cur` machinery
carries it forward:

- Extend `ptyState` with a `stalled bool` field. Leave `classify`
  unchanged — it cannot see quiet timings.
- In the ticker arm, after `cur := classify(snapshot())` and
  `now := time.Now()`, set `cur.stalled = <the predicate above>`.
- Emit only on the rising edge, consistent with the idle/thinking/
  banner edges, placed after the banner block and before `prev = cur`:

  ```go
  if cur.stalled && !prev.stalled {
      if !send(Event{Kind: EventKindStallDetected, Source: EventSourcePty, Time: now}) {
          return
      }
  }
  ```

- The existing `prev = cur` at the end of the arm propagates
  `stalled`, so the event will not repeat while the condition persists,
  and re-arms once the condition clears (PTY byte, JSONL entry, or
  return to idle) and re-enters.

### Recording JSONL arrivals

In the JSONL arm (`events.go:192-214`), record the receive instant so
condition (c) has its reference point. Use the `now` already computed
there:

```go
case e, ok := <-jsonlCh:
    if !ok { return }
    now := time.Now()
    lastJsonlAt = now        // NEW: any JSONL entry is "progress"
    // ... existing JsonlEntry / JsonlEndOfTurn sends unchanged ...
```

Declare `var lastJsonlAt time.Time` alongside `var prev ptyState` near
the top of `mergeEvents`. **Every** JSONL entry (user / assistant /
delta / end_turn) counts as progress — any structured output means
claude is responding, per the safe-degrade ladder.

### Plumbing the inputs

`mergeEvents` gains two parameters (the PTY-quiet seam, kept decoupled
from `*Tracker` so tests can drive it directly):

```go
func mergeEvents(
    ctx context.Context,
    snapshot func() []byte,
    quietFor func() time.Duration,   // NEW
    ptyQuietLimit time.Duration,     // NEW
    jsonlCh <-chan JSONLEntry,
    out chan<- Event,
    pollInterval time.Duration,
) { ... }
```

`Events` gains a `*Tracker` and forwards the two values:

```go
func (s *Session) Events(ctx context.Context, jsonlPath string, startOffset int64, tr *Tracker) (<-chan Event, error) {
    // ... TailJSONL unchanged ...
    go mergeEvents(ctx, s.buffer.Snapshot, s.buffer.QuietFor, tr.ptyQuietLimit, jsonlCh, out, DefaultPollInterval)
    return out, nil
}
```

- `tr.ptyQuietLimit` is read directly (same package, immutable field)
  — no getter is added, keeping the change to `events.go` only on the
  production-library side. This makes the limit provably the *same*
  value `CheckWatchdog` uses (AC4): one field, two readers.
- `tr` is **required (non-nil)**, matching `RunWatchdog`'s contract
  ("Nil tr will panic on first dereference") and the library's
  construct-or-nil-deref posture. A nil `tr` panics synchronously in
  `Events` — an immediate, clear failure at the call site.
- Update the `Events` and `mergeEvents` doc comments to mention the
  stall arm and the new param.

### Relationship to the fatal watchdog (the core seam)

This event is purely additive and independent of `RunWatchdog`:

- The fatal PTY-quiet arm in `CheckWatchdog` is **unchanged**. A
  consumer that runs `RunWatchdog` still gets a fatal error at the
  limit.
- The stall event reuses only the *limit value*, not the watchdog
  loop. It evaluates a stricter predicate (adds `!idle` and
  `no-JSONL-progress`) on the faster 50 ms merge tick, so it typically
  fires slightly before the 1 Hz fatal arm would.
- Composition is the consumer's choice (ADR 025): a consumer that
  wants "degrade, don't die" relies on the stall event and either does
  not run the fatal `RunWatchdog` or sets a larger `PTYQuietLimit` for
  it. This ticket does not legislate that — it only surfaces the
  signal.

## Concurrency model

No new goroutine, channel, lock, or shared mutable state. The stall
arm runs inside the existing `mergeEvents` goroutine on its existing
ticker. `lastJsonlAt` and `prev.stalled` are goroutine-local to
`mergeEvents`. The merge goroutine reads `tr.ptyQuietLimit` (immutable
after `NewTracker`) and `s.buffer.QuietFor()` (thread-safe per
`Buffer`'s contract). When the same `*Tracker` is also passed to
`RunWatchdog`, the merge goroutine touches only the immutable limit
field — never the mutex-guarded spinner/transition state — so there is
no race with the watchdog goroutine. Shutdown is unchanged: the single
`defer close(out)` exit path still closes the stream on ctx-cancel or
`jsonlCh` closure.

## Error handling

No new failure modes. `Events`'s only error path
(`TailJSONL` open/seek) is unchanged. A nil `tr` is a programming
error that panics at the call site (documented), consistent with
`RunWatchdog`.

## Testing strategy

All scenarios drive `mergeEvents` directly with a `testSnap` plus a
controllable `quietFor` closure and a small `ptyQuietLimit`, following
the existing `events_test.go` idiom (real wall-clock, generous
margins). Describe inputs + expected behaviour; write the bodies in the
project's test idiom.

**Migrate the existing tests (mechanical):**
- Add a `neverQuiet` helper: `func() time.Duration { return 0 }`. With
  `quietFor` returning 0, condition (b) never holds → no stall fires →
  every existing assertion is unchanged.
- One `replace_all` over `events_test.go`:
  `mergeEvents(ctx, snap.Snapshot, jsonlCh, out, DefaultPollInterval)`
  → `mergeEvents(ctx, snap.Snapshot, neverQuiet, DefaultPTYQuietLimit, jsonlCh, out, DefaultPollInterval)`
  (covers all 9 call sites at once).
- `TestEvents_TailJSONLErrorBubbles` constructs a `*Session` and calls
  `s.Events(ctx, missing, 0)`; add a `tr := NewTracker(TrackerOpts{})`
  arg. The error path fires before the tracker is dereferenced, so a
  zero-opts tracker is fine.

**New stall scenarios:**
- **Rising-edge fire + no-repeat (AC2, AC5):** not-idle snap +
  `quietFor` returning > limit + no JSONL sent → exactly one
  `EventKindStallDetected` (Source `EventSourcePty`, non-zero `Time`),
  then assert no further stall across several subsequent ticks.
- **Idle suppresses (AC3-a):** idle snap (❯, no ✻) + `quietFor` >
  limit + no JSONL → no stall within N ticks.
- **Recent-PTY suppresses (AC3-b):** not-idle snap + `quietFor`
  returning < limit + no JSONL → no stall.
- **Recent-JSONL suppresses, then fires (AC3-c):** not-idle snap +
  `quietFor` > limit; send a JSONL entry → assert no stall while within
  the (modest, e.g. ~150 ms) limit window; then with no further JSONL,
  once the window elapses assert the stall fires. Exercises both the
  suppression and the `lastJsonlAt` reset.
- **Reuse-limit (AC4):** drive `mergeEvents` with a small custom
  `ptyQuietLimit` and assert detection keys off that value (fires when
  the fakes cross it), confirming the limit is a parameter, not a
  hardcoded constant. Optionally assert at the `Events` seam that a
  `Tracker` built with a custom `PTYQuietLimit` produces stall timing
  consistent with that value.
- **Composition unchanged:** the migrated existing tests
  (idle/thinking/modal/banner/jsonl) still pass with `neverQuiet`,
  proving the stall arm doesn't perturb the other axes.

**Spike compile fix:** `cmd/spike-long-prompt/main.go:208` →
`session.Events(rootCtx, jsonlPath, 0, tr)` (`tr` already in scope at
line 111). `make check` / `make e2e` must stay green.

## Open questions

- **Broken-modal-looks-idle residual.** A *broken* (anchor-undetected)
  permission modal can render ❯ with no spinner → `IsIdle` true → (a)
  false → stall suppressed, even though that is exactly the "dangerous
  hang" ADR 025 cites. Both the AC ("being at the idle prompt
  suppresses it") and the ADR ("PTY quiet while **not-idle**") specify
  the not-idle discriminator, so implementing `!IsIdle` is faithful to
  both. A more robust mid-turn signal (turn-state tracking via
  `end_turn`) is a larger design and out of scope for this S ticket.
  Defer unless a real broken-modal hang is observed escaping the stall;
  in the common case the retained ✻ glyph (the documented 4 KB
  buffer-retention behaviour) keeps `IsIdle` false and the stall does
  fire.
- **Explicit modal guard.** Should the predicate also require
  `cur.modal == ModalClassUnknown`, so a stale ✻ glyph under a
  *detected* modal can't fire a redundant stall the consumer already
  knows about? Implicitly handled by the idle condition in the common
  case; add the guard only if a consumer reports false stalls at
  modals. Deferred (evidence-based).
- **Diagnostic payload.** Left payload-free per the precedent and the
  consumer's `{conversation_id}` wrapping. If a consumer needs the
  quiet duration in-band rather than via `Session.QuietFor()`, add an
  additive field then.

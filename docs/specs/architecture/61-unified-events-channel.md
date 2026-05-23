# Spec: library API — unified `Session.Events()` merging PTY + JSONL

**Ticket:** [#61](https://github.com/pyrycode/tui-driver/issues/61)
**Size:** S
**Status:** ready for development

## Files to read first

Load these in order; each entry says what to extract.

- `cmd/spike-long-prompt/main.go:222-275` — the canonical select-loop pattern this API consolidates. The library version is "this loop, lifted to the library, with the PTY-state edge detector generalised to idle/thinking/modal axes, and with the eventCh receive arm carrying `JSONLEntry` (not `map[string]any`) plus a synthetic end-of-turn emission when `IsEndTurn(e)` holds." Read the full loop body — the `ticker.NewTicker(statePollInterval)` cadence, the three-arm `select` (`ctx.Done` / `eventCh` / `probe.C`), and the rising/falling edge bookkeeping (`thinkingObserved`, `spinnerGone`) are all reused in spirit.
- `pkg/tuidriver/jsonl.go:80-322` — existing `JSONLEntry` / `EntryMessage` / `ContentBlock` types, plus `TailJSONL`, `IsEndTurn`, `AssistantText`. The new code composes over these — no shape changes here, just consumption.
- `pkg/tuidriver/session.go:1-150` — `Session` struct, `SpawnOpts`, `Spawn`, `Write`, `WritePrompt`, `Close`. The new `Events` method hangs off `*Session` and reads `s.Buffer.Snapshot()`. Note: `Session` has no `ctx` field and no JSONL-path field — both flow in via `Events`'s signature, by design.
- `pkg/tuidriver/buffer.go:14-79` — `Buffer.Snapshot()` semantics: returns a fresh copy of the rolling-buffer bytes, mutex-protected, safe to call concurrently with `Append`. The merge loop calls this every poll tick.
- `pkg/tuidriver/state.go:1-97` — `IsIdle`, `IsThinking` predicates over a `snap []byte`. The merge loop's PTY classifier is `(IsIdle(snap), IsThinking(snap))`; the conjunction with modal-class detection forms the unified PTY state.
- `pkg/tuidriver/modal.go:1-122` — `ModalClass` enum + `DetectModalClass(snap) ModalClass`. The merge loop calls this every tick; transitions from `ModalClassUnknown` to a class (or class→class change) drive show/hide events.
- `pkg/tuidriver/wait.go:8-43` — `DefaultPollInterval = 50ms` and the `WaitUntil` precedent (50 ms is also the JSONL tail's EOF-sleep cadence). The merge loop reuses `DefaultPollInterval` as its ticker cadence — same value, same justification (fine-grained enough to feel instant; negligible CPU).
- `pkg/tuidriver/tuidriver.go:1-67` — package doc comment. One sentence updates in § "Doc-comment update" below — read the current in-scope phrasing first.
- `docs/specs/architecture/59-jsonl-tail-entry-channel.md:280-287` — channel-buffer capacity decision (32, blocking on full). The new output channel reuses the same capacity and behaviour.
- `docs/specs/architecture/60-end-of-turn-discriminator-and-assistant-text.md:128-148` — `IsEndTurn` semantics. The synthetic end-of-turn event is `if IsEndTurn(e) { emit endOfTurn }` immediately after emitting the `Entry` event — same per-entry rule, no msg_id grouping.
- `docs/knowledge/architecture/system-overview.md:44-90` — current data-flow diagram + "Three goroutines per spike" concurrency table. The library version collapses the consumer's select loop into one merge goroutine that owns the unified channel; spike-level orchestration stays in the consumer.

## Context

The library already exposes the two halves the consumer wants to merge:

- **JSONL side:** `TailJSONL(ctx, path, startOffset) <-chan JSONLEntry` (#59) for parsed entries, `IsEndTurn(e)` / `AssistantText(e)` (#60) for per-entry classification.
- **PTY side:** `IsIdle(snap)`, `IsThinking(snap)`, `DetectModalClass(snap)` — snapshot-based predicates, no event channel.

Every consumer (the four `spike-*` binaries, future pyry acp, future Claudian fork) writes the same merge: one goroutine tailing JSONL → channel, one ticker polling `Buffer.Snapshot` for PTY state, one select loop reconciling them in arrival order. The reference shape is `cmd/spike-long-prompt/main.go:247-275` — a `time.NewTicker(statePollInterval)` probe alongside `range eventCh` over the JSONL tail.

That ceremony is exactly what the library exists to eliminate. This ticket promotes the merge loop into the library as a single method on `*Session`:

```go
events, err := session.Events(ctx, jsonlPath, 0)
if err != nil { /* … */ }
for ev := range events { /* dispatch on ev.Kind */ }
```

One channel, both sources, arrival order preserved, library owns the merge.

**Scope:** purely additive. The existing PTY predicates and JSONL primitives stay public — consumers that prefer to compose the streams themselves can keep doing so. `Events()` is the one-call shortcut for the majority of consumers that want both.

What is explicitly **out of scope** for this slice:

- **PTY transition channel separate from the unified `Events()`.** No standalone `Session.PtyEvents()` shipping diff events alongside the merged stream — would duplicate the same polling work. If a future consumer wants PTY-only, the snapshot predicates already cover it.
- **`Buffer.Append`-driven notification.** The merge loop polls at `DefaultPollInterval` (50 ms); it does not subscribe to buffer writes. Buffer instrumentation is a separate concern with its own design tradeoffs (lock-free signal, coalescing, backpressure on the reader goroutine) — deferred until a real consumer surfaces a real latency problem.
- **`msg_id` grouping / cross-line assistant-text aggregation.** End-of-turn is per-entry (mirrors `IsEndTurn`); turns whose text spans multiple JSONL lines under one `message.id` need a separate aggregator. Same out-of-scope reasoning as #60.
- **Migration of the four `cmd/spike-*/main.go` select loops** to consume `Events()`. Mechanical follow-up; tracked separately (#62).
- **Cancellation-marker detection** (`user(text="[Request interrupted by user]")` JSONL line) as a typed kind. Consumers writing one-line predicates over the `Entry` payload already cover it; not worth a typed event today.
- **`SpawnOpts.JSONLPath`** so `Events()` doesn't need a path argument. Considered; rejected because the JSONL path is not known until after `Spawn` (the file is created by claude post-first-prompt, see `WaitForSessionJSONL`'s doc comment). Passing path at `Events()` time matches the actual lifecycle.

## Design

### Files touched

```
pkg/tuidriver/events.go        (NEW — Event types, Events method, mergeEvents loop)
pkg/tuidriver/events_test.go   (NEW — TestEvents_* / TestMergeEvents_* set)
pkg/tuidriver/tuidriver.go     (MODIFIED — one-sentence doc-comment edit)
```

Two new files plus a 1-line doc-comment edit. Scope-check: 2 production source files (`events.go` + `tuidriver.go`). Well under the 5-file red line.

### Public surface

One method on `*Session`, three new exported types, plus one EventKind enum + one EventSource enum.

```go
// EventKind classifies an Event on the unified Session.Events() stream.
// Payload-field population is kind-dependent — see Event for the rules.
type EventKind int

const (
    EventKindUnknown EventKind = iota

    // PTY-side rising edges. Emitted when the buffer-derived PTY state
    // crosses INTO the named state from any other state. No falling-edge
    // event is emitted; the next opposite event implies the previous edge.
    EventKindPtyIdle        // claude returned to the input prompt (no spinner, no modal)
    EventKindPtyThinking    // claude's thinking spinner appeared

    // PTY-side modal transitions. Both carry the ModalClass on the Modal field.
    // On a class→class change (rare but possible), Hidden(old) fires before Shown(new).
    EventKindPtyModalShown
    EventKindPtyModalHidden

    // JSONL-side events. Both carry the JSONLEntry on the Entry field.
    EventKindJsonlEntry      // a parsed JSONL line was forwarded
    EventKindJsonlEndOfTurn  // the immediately-preceding JsonlEntry satisfied IsEndTurn
)

// EventSource tags an Event with its origin. Redundant with Kind but
// convenient for consumers that switch on source first.
type EventSource int

const (
    EventSourcePty EventSource = iota
    EventSourceJsonl
)

// Event is one element of the unified Session.Events() stream.
//
// Field population by Kind:
//   - EventKindPtyIdle, EventKindPtyThinking: no payload fields.
//   - EventKindPtyModalShown, EventKindPtyModalHidden: Modal carries the class
//     (the appearing class on Shown; the just-hidden class on Hidden).
//   - EventKindJsonlEntry, EventKindJsonlEndOfTurn: Entry carries the parsed entry.
//
// Time is the wall-clock instant the merge loop detected the event (the
// poll tick for PTY events; the channel-receive instant for JSONL events).
type Event struct {
    Kind   EventKind
    Source EventSource
    Time   time.Time
    Modal  ModalClass // populated only on EventKindPtyModal*
    Entry  JSONLEntry // populated only on EventKindJsonl*
}

// Events spawns a merge goroutine that emits PTY-state transitions
// (idle / thinking / modal) and per-entry JSONL events on a single
// unified channel in arrival order. Internally tails the JSONL file
// at jsonlPath from startOffset (composes with WaitForSessionJSONL +
// SessionJSONLPath); errors from the synchronous open/seek phase are
// returned directly. The returned channel is buffered (capacity 32);
// it is closed when ctx is cancelled, when the internal JSONL tail
// closes, or when the session terminates.
//
// The merge loop is single-threaded with respect to the output
// channel — at most one Event is emitted per select iteration; the
// consumer sees a fully ordered stream.
func (s *Session) Events(ctx context.Context, jsonlPath string, startOffset int64) (<-chan Event, error)
```

### Behaviour contracts

**`Events` — open/spawn phase (synchronous, caller goroutine)**

1. Call `TailJSONL(ctx, jsonlPath, startOffset)`. If it returns an error, return `(nil, err)` — same wrap shape as `TailJSONL` (the error already carries the path + offset context).
2. Allocate the output channel with capacity `defaultEventBuffer = 32` (package-level unexported constant in `events.go`; matches the `defaultJSONLTailBuffer` precedent).
3. Spawn the merge goroutine running `mergeEvents(ctx, s.Buffer.Snapshot, jsonlCh, out, DefaultPollInterval)`.
4. Return `(out, nil)`.

**`mergeEvents` — private merge loop**

Signature (no body in this spec):

```go
// mergeEvents owns the unified merge loop. Polls snapshot at pollInterval
// for PTY-state transitions (idle / thinking / modal), drains entries
// from jsonlCh, and writes typed Event values to out in arrival order.
// Closes out on every return path.
func mergeEvents(
    ctx context.Context,
    snapshot func() []byte,
    jsonlCh <-chan JSONLEntry,
    out chan<- Event,
    pollInterval time.Duration,
)
```

Invariants the body must satisfy (developer writes the code; this lists the rules):

- `defer close(out)` once at the top.
- One `*time.Ticker` at `pollInterval`, `defer ticker.Stop()`. Do NOT seed an initial classification from a one-shot snapshot before the first tick — the merge loop starts "transition-blind" and only emits when an edge is observed.
- Local state tracks the previously-observed PTY classification:
  - `prevIdle bool`
  - `prevThinking bool`
  - `prevModal ModalClass`
  - Initial values are all zero (`false`, `false`, `ModalClassUnknown`). The first tick's classification is compared against these; an initial idle classification therefore fires `EventKindPtyIdle` on the first tick (this is the documented behaviour — the consumer learns the initial state by observing the rising edge).
- Three-arm `select` per loop iteration:
  - `case <-ctx.Done(): return`
  - `case e, ok := <-jsonlCh: if !ok { return }; emitJsonl(e)`
  - `case <-ticker.C: emitPtyDiff(snap)`
- `emitJsonl(e)`: send `EventKindJsonlEntry` with `Entry: e`, `Source: EventSourceJsonl`. Then if `IsEndTurn(e)`, send a second event `EventKindJsonlEndOfTurn` with `Entry: e`, same source. Both sends use the backpressure-aware shape `select { case out <- ev: case <-ctx.Done(): return }`.
- `emitPtyDiff(snap)`: compute `cur := classify(snap)` (returns `(idle bool, thinking bool, modal ModalClass)`). Compare against `prev*` and emit in this order (so consumers observe Hidden before Shown on a class change):
  1. If `cur.modal != prev.modal`:
     - If `prev.modal != ModalClassUnknown`: send `EventKindPtyModalHidden` with `Modal: prev.modal`.
     - If `cur.modal != ModalClassUnknown`: send `EventKindPtyModalShown` with `Modal: cur.modal`.
  2. If `cur.modal == ModalClassUnknown` (no modal currently active — idle/thinking events are suppressed while a modal is up):
     - If `cur.idle && !prev.idle`: send `EventKindPtyIdle`.
     - If `cur.thinking && !prev.thinking`: send `EventKindPtyThinking`.
  3. Update `prev = cur`.
- All sends use the backpressure-aware `select { case out <- ev: case <-ctx.Done(): return }`. After any short-circuit return on `ctx.Done()`, the deferred `close(out)` fires.

**`classify` — private helper (or inline)**

Pure function over a snapshot:

```go
// classify reduces snap to the three independent PTY state axes the
// merge loop tracks. Calls IsIdle / IsThinking / DetectModalClass.
func classify(snap []byte) (idle bool, thinking bool, modal ModalClass)
```

Calls each predicate once; predicates each strip ANSI internally — fine for this slice (the snap is ≤ DefaultBufferCap = 4 KB, three strips on 4 KB per tick is negligible). If a future profile shows otherwise, the optimisation is to strip once and pass the stripped bytes — out of scope here.

### Why suppress idle/thinking while a modal is active

A modal renders with `❯` present (the input-line marker is drawn beneath the modal) and the spinner absent — so `IsIdle(snap)` returns `true` during e.g. a permission prompt. Emitting `EventKindPtyIdle` while the consumer's UI knows a modal is up would be a false "ready for next prompt" signal: claude is waiting on modal input, not on a fresh prompt. The merge loop therefore treats modal-active as the dominating axis and suppresses idle/thinking transitions until the modal hides.

This matches what every spike binary's select loop does in practice — the modal-handler branches gate the idle/thinking observers. The library just makes that gating explicit at the event boundary.

### Why poll, not subscribe to `Buffer.Append`

`Buffer.Append` is the hot path for the PTY reader goroutine — it runs on every PTY read (every few bytes during streaming). Wiring per-append notification into the merge loop adds:

- Synchronisation overhead on every append (a `chan struct{}` send or condition-variable broadcast in a tight loop).
- Coalescing complexity (a single ANSI rendering arrives in multiple `Append` calls; the loop wants to classify the final state, not every intermediate byte).
- Backpressure semantics on the reader goroutine if the merge loop falls behind.

Polling at 50 ms is good enough — typical TUI redraws are bounded by terminal-refresh rates well above this cadence; spike binaries have used this poll cadence across six iterations without issue. If a real consumer surfaces a real latency problem, instrumenting `Buffer` is a follow-up with its own design tradeoffs.

### Why a method on `*Session`, not a free function

`Events()` reads `s.Buffer.Snapshot()` and is conceptually scoped to the lifecycle of one PTY session. A method:

- Makes the dependency on `s.Buffer` explicit at the call site (`session.Events(...)` vs `tuidriver.Events(session.Buffer, ...)`).
- Matches the existing `*Session` method idiom (`Write`, `WritePrompt`, `Wait`, `Close`).
- Leaves room for future internal optimisations that touch private session state (e.g. coalescing multiple `Events()` callers into one poll goroutine) without breaking the API.

The merge loop itself is exported as a private helper (`mergeEvents`) precisely so the tests can drive it without a real PTY — see § Testing strategy.

### Doc-comment update in `tuidriver.go`

One sentence updates in the in-scope list (line ~5-8):

- Current: `"… ANSI/OSC stripping, vt10x-backed grid rendering, modal-class detection, modal parsers …"`.
- New: `"… ANSI/OSC stripping, vt10x-backed grid rendering, modal-class detection, modal parsers (picker / mcp / agents), unified PTY+JSONL event subscription …"`.

Rationale: `Events()` is the first method that delivers PTY and JSONL signals together. Worth one phrase in the package doc-comment so readers know to look for it.

2-line edit, no refactor.

### Concurrency model

One additional goroutine per `Events()` call, owning:

- The output channel (closed via `defer` on return).
- The ticker (`defer ticker.Stop()`).
- The `prev*` state.

The internal `jsonlCh` is owned by `TailJSONL`'s goroutine (already documented in #59). The merge goroutine is its sole reader; closure of `jsonlCh` is observed via the `ok := <-jsonlCh` branch.

| Goroutine | Owns | Exit |
|---|---|---|
| `Spawn`'s reader (already exists) | `Session.Buffer` writes | PTY EOF |
| `TailJSONL`'s tailer (already exists) | the `jsonlCh` | ctx cancel / unrecoverable read error |
| `mergeEvents` (new) | the output channel + ticker | ctx cancel / `jsonlCh` closure |

No new mutexes. No shared state across the three goroutines beyond `Session.Buffer` (mutex-protected; already shared by every existing `Snapshot` caller).

**Shutdown ordering** (the AC: "merge goroutine returns within one poll tick after ctx is cancelled"):

1. Caller cancels `ctx`.
2. `TailJSONL`'s tailer observes `<-ctx.Done()` → returns → closes `jsonlCh`.
3. `mergeEvents` either:
   - Observes `<-ctx.Done()` directly (in one of its three select arms) → returns immediately.
   - Or observes `<-jsonlCh` closed (because the tailer raced ahead) → returns immediately.
4. Deferred `close(out)` fires; deferred `ticker.Stop()` fires.

Maximum delay between `ctx` cancellation and output-channel close: one `pollInterval` (50 ms) — bounded by the ticker arm in the select. The other two arms are observed instantly.

### Error handling

Three exit paths for the merge goroutine, all surfaced to the caller via channel close (no error return — the function already returned synchronously):

| Trigger                                     | Caller observes                          |
|---------------------------------------------|------------------------------------------|
| `ctx` cancelled / deadline expired          | `ctx.Err() != nil` after channel closes  |
| `jsonlCh` closed by `TailJSONL` (rare non-EOF I/O error mid-tail) | `ctx.Err() == nil` after channel closes  |
| Backpressure-aware send observes `<-ctx.Done()` mid-emit | Same as ctx cancellation               |

Synchronous open/seek errors from `TailJSONL` bubble through `Events`'s return:

| Trigger                                     | Returned error                                       |
|---------------------------------------------|------------------------------------------------------|
| `os.Open(jsonlPath)` fails                  | The `TailJSONL` wrap: `open session jsonl <path>: <err>` |
| `f.Seek` fails                              | The `TailJSONL` wrap: `seek session jsonl <path> to <off>: <err>` |

No additional wrap layer in `Events` — the `TailJSONL` error message already names the path. Adding `Events`-level context would be noise.

### Backpressure

The output channel is buffered (capacity 32), same as `TailJSONL`. Sends use the backpressure-aware `select` shape. If the consumer lags:

- Output buffer fills. Merge goroutine blocks on the next send.
- While blocked, no further `jsonlCh` reads → `jsonlCh` buffer fills (also cap 32) → `TailJSONL`'s tailer blocks → claude's stdout writes back-pressure naturally.
- PTY ticker keeps firing but `ticker.C` is cap-1; surplus ticks drop silently. No event accumulation on the PTY side.
- When `ctx` is cancelled, the `<-ctx.Done()` arm of the blocked send fires; goroutine returns immediately.

This is the same shape as `TailJSONL`'s send arm — established precedent, no novel backpressure design.

### Why retain the ID-less / no-handle shape

The ticket allows a "method (or function on a session)". Method on `*Session` chosen (see above). No `EventsHandle` struct, no `Err()` method on a handle. Two reasons:

- Terminal-error reporting is already handled via `ctx.Err()` after channel close — same idiom as `TailJSONL`. A handle would duplicate API surface for no marginal benefit.
- If a future ticket needs pause/resume control or multi-consumer fan-out, a `NewEventsSubscription`-shaped API can be added alongside `Events()` (additive). Not needed for the AC.

## Testing strategy

`pkg/tuidriver/events_test.go` covers the five AC bullets. The merge loop is the unit under test; the testable seam is the private `mergeEvents` function, which takes a `snapshot func() []byte` (the buffer-injection seam) and a `<-chan JSONLEntry` (the JSONL-injection seam). Tests construct these directly — no real PTY, no real JSONL file.

Test scaffolding shape (the developer writes the code; this lists the helpers):

- A controllable `snapshot` source: e.g. a `*Buffer` constructed with `NewBuffer(0)` and mutated via `Append([]byte(...))` between phases; pass `buf.Snapshot` as the `snapshot func() []byte`. (Alternative: an `atomic.Value`-wrapped `[]byte`. Either is acceptable; the `*Buffer` route exercises real library code and is cheap.)
- An injection source for JSONL entries: a `chan JSONLEntry` the test owns and writes to (pass it as the read-only `jsonlCh` parameter via a `<-chan` conversion).
- A test-local output channel the test reads from. `mustReceive(t, ch, timeout) Event` helper (timeout ~500 ms) bounds runtime under deadlock; mirror the `TestTailJSONL_*` precedent in `jsonl_test.go`.
- `time.Sleep(150ms)` between phases is reliable (3× the 50 ms poll interval); CI flakiness margin matches the existing JSONL tests.

Construct realistic byte payloads using the actual glyphs (`tuidriver.IdleGlyph`, `tuidriver.SpinnerGlyph`) so the predicates exercise real code paths.

### Scenarios

Each scenario is one (or two) discrete `TestMergeEvents_*` function(s). The developer chooses table-driven vs discrete style; both are idiomatic.

#### `TestMergeEvents_PtyIdleAndThinkingTransitions`

Verifies AC bullet "PTY-only events with no JSONL activity" + the idle↔thinking edges.

- Start `mergeEvents` against a `*Buffer` and an empty (never-written-to) JSONL channel.
- Append bytes that make `IsIdle` true (the `❯` glyph, ANSI-irrelevant). Wait one tick.
- Receive: `EventKindPtyIdle`, no `Entry`/`Modal` payload.
- Append bytes that make `IsThinking` true (spinner glyph present). Wait one tick.
- Receive: `EventKindPtyThinking`.
- Append bytes that drop the spinner and re-expose `❯`. Wait one tick.
- Receive: `EventKindPtyIdle` (idle re-entered after a thinking interval).
- Cancel ctx; assert channel closes.

#### `TestMergeEvents_ModalShowAndHide`

Verifies the modal-class show/hide pair + the suppression of idle/thinking while a modal is active.

- Start `mergeEvents` against the same scaffolding.
- Append bytes that trigger `DetectModalClass == ModalClassPermission` (e.g. include `Do you want to proceed`). Wait one tick.
- Receive: `EventKindPtyModalShown` with `Modal == ModalClassPermission`.
- (No `EventKindPtyIdle` even though `❯` is present — the suppression rule.)
- Append bytes that change the class to `ModalClassMCP` (include `ManageMCPservers`). Wait one tick.
- Receive: `EventKindPtyModalHidden` with `Modal == ModalClassPermission` THEN `EventKindPtyModalShown` with `Modal == ModalClassMCP` (Hidden-before-Shown ordering).
- Append bytes that clear the modal anchors and re-expose plain idle. Wait one tick.
- Receive: `EventKindPtyModalHidden` with `Modal == ModalClassMCP` THEN `EventKindPtyIdle` (idle resumes once no modal is active).
- Cancel ctx; assert channel closes.

#### `TestMergeEvents_JsonlEntryAndSyntheticEndOfTurn`

Verifies AC bullet "JSONL-only events with no PTY transitions" + the synthetic end-of-turn.

- Static buffer (no append between phases, so no PTY transitions are observed beyond the initial-tick edges — design tests so the static state matches `prev*` zero values to suppress an initial PtyIdle; e.g. start with non-idle bytes).
- Push a `JSONLEntry{Type: "user", Message: …}` (non-assistant). Receive: one `EventKindJsonlEntry` with the entry; no follow-up.
- Push a `JSONLEntry` satisfying `IsEndTurn` (assistant + stop_reason=end_turn + non-empty text). Receive: `EventKindJsonlEntry` THEN `EventKindJsonlEndOfTurn`, both carrying the same `Entry`.
- Push a `JSONLEntry` with stop_reason=end_turn but only thinking blocks (`IsEndTurn` returns false). Receive: one `EventKindJsonlEntry` with the entry; assert no `EndOfTurn` follows within ~150 ms (`select { case e := <-ch: if e.Kind == EventKindJsonlEndOfTurn { t.Fatal(...) }; case <-time.After(150ms): }`).
- Cancel ctx; assert channel closes.

#### `TestMergeEvents_InterleavedArrivalOrder`

Verifies AC bullet "interleaved arrival ordering preserved".

- Append bytes that trigger an idle classification. Wait one tick. Receive: `EventKindPtyIdle`.
- Push a JSONL entry. Receive: `EventKindJsonlEntry`.
- Append bytes that trigger a thinking classification. Wait one tick. Receive: `EventKindPtyThinking`.
- Push another JSONL entry. Receive: `EventKindJsonlEntry`.
- Assert the received order matches the order events were created (PTY-Idle → JSONL → PTY-Thinking → JSONL).
- Cancel ctx; assert channel closes.

Note: within a single tick, when both `jsonlCh` and `ticker.C` are ready, Go's `select` chooses randomly — that's acceptable per the AC ("arrival order preserved" reads as "if A is detected in tick N and B in tick N+1, A precedes B in the output"; within one tick, either is fine). The test sequences events deliberately one-per-phase to avoid this race.

#### `TestMergeEvents_CleanShutdown`

Verifies AC bullet "clean shutdown closes the channel" + the "within one poll tick after ctx is cancelled" timing.

- Start `mergeEvents` against any scaffolding.
- Drain any startup events (the initial PtyIdle if the buffer is seeded with idle bytes).
- `cancel(sentinel)` where `sentinel := errors.New("test stop")`.
- Assert the channel closes within `2 * DefaultPollInterval` (100 ms — well above one tick, well below CI timeout): `select { case _, ok := <-ch: if ok { t.Fatal("got event instead of close") }; case <-time.After(2*DefaultPollInterval): t.Fatal("channel did not close in time") }`.
- Assert `errors.Is(context.Cause(ctx), sentinel)` (sanity check on the test, not the library).

#### `TestMergeEvents_JsonlChClosureClosesOutput`

Verifies the "jsonlCh closed → output closes" exit path (not in AC explicitly but documented as a behaviour contract).

- Start `mergeEvents` against scaffolding where the test owns the `jsonlCh`.
- `close(jsonlCh)` directly (without cancelling ctx).
- Assert output channel closes within ~100 ms.

#### `TestEvents_TailJSONLErrorBubbles`

Spec-only test for the `Events` method's synchronous error path.

- Call `session.Events(ctx, "/nonexistent/path/x.jsonl", 0)` on a Session (or use the merge-loop seam if Session construction is awkward — see § Testing notes).
- Assert `(ch, err)` — `ch == nil`, `err != nil`, `strings.Contains(err.Error(), "/nonexistent/path/x.jsonl")`, `errors.Is(err, fs.ErrNotExist)`.

### Testing notes

The `Events` method itself (as opposed to the `mergeEvents` private helper) is awkward to unit-test in isolation because `Session` requires a real spawned process. Two acceptable options:

1. **Don't unit-test `Events` directly** — its body is "call `TailJSONL`, allocate channel, spawn `mergeEvents`, return". All three steps are covered: `TailJSONL` has its own tests, the channel allocation is trivial, `mergeEvents` has its own tests. The above `TestEvents_TailJSONLErrorBubbles` is the one Session-level integration the test file does need; build a minimal `Session` for it (a `*Session{Buffer: NewBuffer(0)}` literal compiles, since `Spawn` doesn't return a `Session` value the test can use without a PTY — see `pkg/tuidriver/session.go:40-56` for the unexported fields the test can leave zero-valued).
2. **Skip the synchronous-error test entirely** — `TailJSONL`'s test covers the same error shape; the `Events` wrap adds no new behaviour. If introducing a minimal `*Session` literal feels uncomfortable, drop the test. The developer's call.

### No-regression on existing tests

The new file is purely additive; `TestSessionJSONLPath_*`, `TestWaitForSessionJSONL_*`, `TestTailJSONL_*`, `TestIsEndTurn_*`, `TestAssistantText_*`, `TestIsIdle_*`, `TestIsThinking_*`, `TestDetectModalClass_*`, and all other existing `pkg/tuidriver` tests are untouched. `go test ./pkg/tuidriver/...` must continue to pass.

## Out of scope (reminder, mirrors ticket)

- **Standalone PTY transition channel.** Already covered above — the merge-only API is the surface; PTY-only consumers compose snapshot predicates.
- **`Buffer.Append`-driven notification.** Polling at 50 ms is the contract.
- **msg_id grouping / cross-line assistant-text aggregation.** Stays consumer-side (or a future ticket).
- **Cancellation-marker typed event.** Consumers branch on `e.Entry.Type == "user"` themselves.
- **Migrating the four `cmd/spike-*/main.go` select loops** (#62).
- **Configurable poll interval / channel capacity.** `DefaultPollInterval` and `defaultEventBuffer = 32` are the only knobs; consumers tune timeout via context.
- **`SpawnOpts.JSONLPath`** — JSONL path isn't known at spawn time (the file doesn't exist until claude receives a prompt). Path flows in at `Events()` time.
- **Per-Session uniqueness.** Calling `Events()` twice on one `*Session` spawns two independent merge loops; both work. Not advertised; not prohibited.
- **Output-channel termination metadata.** No `(EventKind, reason)` sentinel before close; consumers check `ctx.Err()` after the channel closes if they need to distinguish cancellation from `jsonlCh` closure.

## Open questions

- **Should `EventKindUnknown` (the zero value) be exported, or kept as an internal sentinel?** Exported so consumers writing `switch ev.Kind` get a compile-error path for unhandled kinds (the empty default arm). Same idiom as `ModalClassUnknown`.
- **Should the merge loop emit an event when classification first transitions from "all-zero" (initial state) to a real classification?** Yes — the first tick emits whichever rising edges hold relative to the zero-valued `prev*`. A buffer that's already idle at subscription time fires `EventKindPtyIdle` on the first tick; a buffer already in a modal fires `EventKindPtyModalShown`. Consumers should subscribe before they expect transitions, or accept the initial-state event as the "current state on subscribe" signal.
- **Should there be a `WithSnapshot` option to pass a custom snapshot function?** No. The current shape passes `s.Buffer.Snapshot` and that's all the AC needs; the `mergeEvents` seam is internal and is sufficient for tests.
- **Should `Event.Time` be set by the merge loop or by the source (PTY reader timestamp / JSONL parser timestamp)?** Merge-loop. The PTY side has no per-byte timestamp; the JSONL side does carry a `timestamp` field per envelope, but the merge-loop wall-clock is a stable, consistent reference across both sources. If a consumer wants the JSONL envelope's own timestamp, it's available via `Entry.Raw["timestamp"]`.
- **Should `EventKindJsonlEndOfTurn` carry an explicit "text-content" snapshot (the `AssistantText(e)` result), or just the entry?** Just the entry. The consumer calls `AssistantText(e.Entry)` themselves if they want it. Bundling it would duplicate state for the consumers that don't need text (e.g. ones that only want the "turn finished" signal).

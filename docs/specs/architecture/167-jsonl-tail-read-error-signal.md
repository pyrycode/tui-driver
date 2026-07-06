# Spec: distinguish a JSONL tail read error from a clean shutdown

**Ticket:** [#167](https://github.com/pyrycode/tui-driver/issues/167)
**Size:** S (production change is small; the honest cost is the contract-flip on one existing test + two new tests + a doc-comment amendment)
**Status:** ready for development
**Security-sensitive:** No (verified on the issue labels — `size:s`, `done:po`, `wip:architect`; no `security-sensitive` label. This is an error-signalling bug, same class as [#160](../../knowledge/codebase/160.md), no attacker-content or classification surface. Security-review pass skipped per label gate.)

## Files to read first

Load these in order; each entry says what to extract.

- `pkg/tuidriver/jsonl.go:185-226` — `tailJSONLLoop`, the goroutine body. **The load-bearing fact:** it has exactly four exit paths, and *three of them are ctx-driven* (top-of-loop `ctx.Err() != nil`, the send-arm `<-ctx.Done()`, the EOF-arm `<-ctx.Done()`). The **only** exit that closes the channel with ctx still live is the `default:` arm (222-223) — a non-EOF `ReadBytes` error. Extract this invariant; the whole fix rests on it.
- `pkg/tuidriver/jsonl.go:171-183` — `TailJSONL`. Opens the `*os.File`, spawns `tailJSONLLoop(ctx, f, ch)`. **Its public signature `(<-chan JSONLEntry, error)` must not change** (8 test call sites + external consumers). The only edit here is the argument type it hands to `tailJSONLLoop`.
- `pkg/tuidriver/events.go:15-128` — the `EventKind` const block and the `Event` struct. Extract the **kind-specific-payload doc pattern** (`Modal` populated only on `EventKindPtyModal*`, `Entry` only on `EventKindJsonl*`) — the new `Err` field mirrors it exactly.
- `pkg/tuidriver/events.go:193-375` — `mergeEvents`. The emission point is the `case e, ok := <-jsonlCh: if !ok { return }` branch (227-230). The `send` helper (214-221) is the backpressure-aware, ctx-aware send you reuse. **`mergeEvents`' signature must not change** (~16 test call sites) — the fix is body-only.
- `pkg/tuidriver/events.go:130-170` — `Events` method + its doc comment. The doc's close-conditions sentence ("closed when ctx is cancelled, when the internal JSONL tail closes, or when the session terminates") gets one amendment describing the terminal error event.
- `pkg/tuidriver/events_test.go:405-453` — the two shutdown tests. `TestMergeEvents_CleanShutdown` (405, ctx-cancel) **stays green unchanged** — it's the clean-path companion the AC asks for. `TestMergeEvents_JsonlChClosureClosesOutput` (433) closes `jsonlCh` with **ctx live** and asserts a clean close — that action *is* the read-error simulation, so its assertion **flips** to expect `EventKindError` first. This is the one existing test whose contract this ticket changes.
- `pkg/tuidriver/events_test.go:14-76` — the reusable test helpers: `testSnap`, `neverQuiet`, `mustReceiveEvent`, `assertEventChClosed`. The new tests build on these; do not reinvent them.
- `pkg/tuidriver/jsonl_test.go:351-379` — `TestTailJSONL_ContextCancellationMidRead`, the closest precedent for driving the tail under ctx and asserting channel-close timing. The new fault-reader test mirrors its shape, swapping the real file for a fake `io.ReadCloser`.
- `docs/specs/architecture/61-unified-events-channel.md:264-272` and `:414` — **the decision this ticket reverses.** #61 deliberately chose "no terminal metadata; consumers check `ctx.Err()` after the channel closes" (line 414) and tabled the read-error case as `ctx.Err() == nil after channel closes` (line 271). #167 promotes that out-of-band discriminator to an in-band stream event. Read it to understand what is being superseded — **do not edit #61's spec** (frozen sibling artifact).
- `docs/knowledge/codebase/160.md` — the sibling error-signalling fix from the same Cross-Repo Code Review. Mirror its framing (a distinct signal for a rare failure) and its posture (a false-negative on a concurrent cancel is the safe report).

## Context

`tailJSONLLoop` (`jsonl.go:187`) collapses every exit into one `defer close(ch)`. Three exits are ctx-driven and clean; the fourth — the `default:` arm at `jsonl.go:222-223`, reached on a non-EOF `ReadBytes` failure *after the goroutine has started* — closes the same channel the same way. The merge loop reads that close as `if !ok { return }` (`events.go:227-230`) and closes its own `Events()` output identically. So a runtime tail-read failure is byte-for-byte indistinguishable, from inside the stream, from Claude finishing cleanly: a consumer blocked on `EventKindJsonlEndOfTurn` just sees the stream end.

Spec #61 knew about this and chose the out-of-band route: "consumers check `ctx.Err()` after the channel closes if they need to distinguish cancellation from `jsonlCh` closure" (#61 line 414). That works — `ctx.Err() == nil` after close *does* imply a read error, because a read error is the sole non-ctx exit — but it is not observable to a consumer that only ranges the stream (`for ev := range events`), and it is exactly the gap the Cross-Repo Code Review 2026-07-03 flagged, same class as #160 (a signal that exists but the consumer can't act on).

This ticket reverses #61's punt: the same `ctx.Err() == nil ⟺ read-error` invariant is promoted from "consumer checks after close" to "the merge loop emits one terminal `EventKindError` on the stream before closing." No new plumbing carries the discriminator — the invariant is already there and already documented; we just make it in-band.

**Scope.** The change is deliberately minimal and leans on an existing invariant rather than adding a channel/holder/relay to carry an error *value*. The AC constrains the observable *signal* (distinguishable failure vs clean end), not the underlying `os` error string — see § Error handling for why the terminal event carries a sentinel, not the wrapped `ReadBytes` cause, and § Open questions for when to revisit.

## Design

### Files touched

```
pkg/tuidriver/jsonl.go        (MODIFIED — tailJSONLLoop takes io.ReadCloser so a fault reader is injectable)
pkg/tuidriver/events.go       (MODIFIED — EventKindError const, Event.Err field, ErrJSONLTailRead sentinel,
                                          one branch in mergeEvents, one doc-comment amendment)
pkg/tuidriver/jsonl_test.go   (MODIFIED — new fault-reader test)
pkg/tuidriver/events_test.go  (MODIFIED — flip one test, add clean-shutdown-no-error assertion)
```

Scope-check: **2 production source files** (`jsonl.go`, `events.go`), well under the 5-file red line. New exported identifiers: `EventKindError`, `Event.Err`, `ErrJSONLTailRead` = **3**, under the 5-type red line. Edit fan-out: `tailJSONLLoop` has **1** caller (`TailJSONL`); `mergeEvents` and `TailJSONL` keep their signatures, so their ~16 and 8 call sites respectively are **untouched**. No red line tripped.

### The invariant the fix rests on

`tailJSONLLoop` closes its channel with `ctx.Err() == nil` **iff** it exited via the `default:` read-error arm. Every other exit is ctx-driven, so `ctx.Err() != nil` at those closes. Therefore, at the merge loop's `!ok` branch:

- `ctx.Err() == nil` → the tail broke on a read error → emit a terminal error.
- `ctx.Err() != nil` → clean ctx-driven shutdown → emit nothing.

This is the same discriminator #61 documented (line 271); we act on it in-band instead of exporting it to the consumer.

### Public surface additions (events.go)

One new `EventKind`, one new field on `Event`, one exported sentinel. Contracts only — the developer writes the doc comments in the house style (mirror the existing kind-specific-payload phrasing at `events.go:101-128`).

```go
// EventKindError is the terminal error signal: the internal JSONL tail
// goroutine hit a non-EOF read error and closed its stream while ctx was
// still live. Emitted at most once, immediately before Events()' channel
// closes. Err carries the reason (ErrJSONLTailRead). Source is
// EventSourceJsonl. A clean shutdown (ctx cancel / EOF-then-cancel) emits
// no EventKindError. Added at the end of the const block (new highest value).
EventKindError

// Event gains one field, populated only on EventKindError (nil otherwise),
// mirroring how Modal/Entry are kind-scoped:
//   Err error // populated only on EventKindError
```

```go
// ErrJSONLTailRead is the Err carried by a terminal EventKindError. A
// consumer matches it with errors.Is(ev.Err, tuidriver.ErrJSONLTailRead).
// It marks "the JSONL tail's underlying read failed"; it does not wrap the
// os-level cause (see the spec's Error-handling section).
var ErrJSONLTailRead = errors.New("tuidriver: jsonl tail read failed")
```

`events.go` currently imports only `context` and `time`; add `errors`.

### mergeEvents — the body-only change

Signature unchanged. Only the `!ok` branch grows (currently `if !ok { return }`):

```go
case e, ok := <-jsonlCh:
    if !ok {
        // The tail closes jsonlCh with ctx still live ONLY on its default:
        // read-error arm (jsonl.go:222-223) — every other tail exit is
        // ctx-driven. So ctx.Err()==nil here means the tail broke; emit one
        // terminal error before the deferred close(out). ctx.Err()!=nil is a
        // clean shutdown: emit nothing. (Reverses spec #61 §Error-handling.)
        if ctx.Err() == nil {
            _ = send(Event{Kind: EventKindError, Source: EventSourceJsonl, Time: time.Now(), Err: ErrJSONLTailRead})
        }
        return
    }
    // ... unchanged ...
```

Use the existing `send` helper (`events.go:214`) so the emission is backpressure- and ctx-aware; its return value is irrelevant here (we `return` either way), hence the discard. The invariant comment is mandatory — it binds this branch to `tailJSONLLoop`'s exit-path structure so a future edit to the tail can't silently break the discriminator. Add a reciprocal one-line note at `tailJSONLLoop`'s `default:` arm ("mergeEvents reads this ctx-live close as a read error — see events.go").

### Events doc-comment amendment (events.go:130-161)

Amend the close-conditions sentence to name the new event, e.g. append: "…or, if the internal JSONL tail hits a runtime read error, a single terminal `EventKindError` (carrying `ErrJSONLTailRead`) is emitted immediately before the channel closes; clean shutdowns emit no such event." Prose only, no behavioural code in the comment.

### tailJSONLLoop — injectable reader (jsonl.go)

The AC (#4) requires *injecting a read error into the running tail loop*. Today `tailJSONLLoop(ctx, f *os.File, ch)` reads a concrete `*os.File`, which cannot be made to return a deterministic non-EOF error without platform-hacky fd tricks. Change the parameter to `io.ReadCloser`:

```go
func tailJSONLLoop(ctx context.Context, r io.ReadCloser, ch chan<- JSONLEntry)
```

- Body change: `bufio.NewReader(r)` and `defer r.Close()` (was `f`). Nothing else in the loop changes — the `default:` arm already returns on a non-EOF error; that behaviour is exactly what we now want to be observable.
- Caller: `TailJSONL` passes its opened `f` (an `*os.File`, which satisfies `io.ReadCloser`) — one call-site edit, no signature change to `TailJSONL`. `io` is already imported in `jsonl.go`.
- Tests gain a fault `io.ReadCloser` seam (see § Testing strategy). This is the only reason the type widens; the production path is unchanged (`*os.File` in, `*os.File` behaviour out).

### Data flow

```
                       (unchanged)                 (unchanged)
  os.File ──► tailJSONLLoop ──► jsonlCh ──► mergeEvents ──► out ──► consumer
                   │                            │
   read error hits default: arm                │  at !ok:
   → return → close(jsonlCh)  ───────────────► │  ctx.Err()==nil? ── yes ─► send EventKindError, then close(out)
   (ctx still live)                            │                  └─ no ──► close(out)  (clean)
```

## Concurrency model

No new goroutines, no new channels, no shared state, no mutexes. The change is:

- `tailJSONLLoop`: unchanged goroutine, same single owner of `jsonlCh` and the reader (now typed `io.ReadCloser`), same close-on-every-exit contract.
- `mergeEvents`: unchanged goroutine, same single owner of `out`, same `defer close(out)`. One extra conditional send inside the already-existing `!ok` branch before the already-existing `return`.

The happens-before edge is the existing one: `close(jsonlCh)` in the tail synchronises-with `mergeEvents`' `!ok` receive; `mergeEvents`' `close(out)` synchronises-with the consumer's range-end. The terminal `EventKindError` is sent on `out` before `close(out)`, so a ranging consumer observes it strictly before the close. This is why no holder/relay is needed — the discriminator (`ctx.Err()`) is read locally in the goroutine that already owns the emission point.

**#61's concurrency table (line 245-249) still holds**: two goroutines per `Events()` call (the tail and the merge), each exiting on ctx-cancel or channel-closure. This ticket adds nothing to it.

## Error handling

| Trigger | Tail exit | `ctx.Err()` at merge `!ok` | Consumer observes on `Events()` stream |
|---|---|---|---|
| Runtime non-EOF read error (`default:` arm) | closes `jsonlCh`, ctx live | `nil` | one `EventKindError{Err: ErrJSONLTailRead}`, then channel close |
| ctx cancel / deadline | ctx-driven exit | non-nil | channel close, no error event |
| EOF-then-shutdown (EOF arm parks, then ctx cancels) | ctx-driven exit | non-nil | channel close, no error event |
| Synchronous open/seek failure (`TailJSONL`) | goroutine never starts | — | `Events` returns `(nil, err)` — **unchanged**, still covered by `TestEvents_TailJSONLErrorBubbles` |

**Why a sentinel, not the wrapped `os` cause.** Carrying the underlying `ReadBytes` error value to `mergeEvents` would require a shared error-holder threaded from the tail goroutine to the merge goroutine — which means either a new `mergeEvents` parameter (≈16 test call-site edits — trips the edit-fan-out red line) or an interposed relay goroutine (permanent per-subscription complexity). Neither is warranted: **the AC requires a distinguishable, retrievable signal, not the os-level cause** (AC #1 explicitly allows "a terminal error event … not as an indistinguishable channel close"; no AC mentions the underlying error string). The read error is rare on an append-only local file; the consumer's response (mark the turn failed, re-observe the file) does not need the cause. This is the Evidence-Based / right-sized call: ship the distinct signal; defer cause-propagation until a consumer demonstrates it needs the `os` error. See § Open questions.

**Concurrent-cancel race (benign).** If a genuine read error closes `jsonlCh` and the consumer cancels ctx in the same instant, `ctx.Err()` may already be non-nil when `mergeEvents` reads `!ok`, suppressing the `EventKindError`. This is acceptable and intended: the consumer is shutting down anyway, and AC #2 requires ctx-cancel to close cleanly. The discriminator biases toward "clean" on any ambiguity — the safe report, mirroring #160's "prefer not-done over false success" posture.

## Testing strategy

All seams are unexported and in-package; drive them directly — no live PTY, no real Claude. Reuse `mustReceiveEvent`, `assertEventChClosed`, `testSnap`, `neverQuiet` (`events_test.go:14-76`) and the `mustReceive` precedent in `jsonl_test.go`.

**New — a fault `io.ReadCloser`.** A tiny test type: `Read` returns any pre-scripted good bytes, then a non-EOF error (e.g. a sentinel `errBoom`); `Close` returns nil. This is the injectable read-error the AC calls for. (Keep it in whichever `_test.go` file its test lives in.)

Scenarios (bullet form — developer writes the assertions in-idiom):

1. **`TestTailJSONLLoop_ReadErrorClosesChannelCtxLive`** (jsonl_test.go, mirrors `TestTailJSONL_ContextCancellationMidRead`'s shape). Wire `go tailJSONLLoop(ctx, faultReader, ch)` with a *live, uncancelled* ctx. Optionally script one good line first (assert it arrives on `ch`), then the fault. Assert `ch` closes within a bounded timeout **and** `ctx.Err() == nil` at close time — proving the tail's read-error exit closes the channel without touching ctx. This is the AC-#4 "inject a read error into the running tail loop" coverage at the tail layer.

2. **Full-stack read-error → `EventKindError`** (events_test.go — this is the flipped `TestMergeEvents_JsonlChClosureClosesOutput`, or a sibling next to it). Two acceptable wirings; pick one:
   - *Merge-seam* (matches the current test's style): `close(jsonlCh)` with ctx live → assert `mustReceiveEvent` yields `EventKindError` with `Source == EventSourceJsonl` and `errors.Is(ev.Err, ErrJSONLTailRead)`, **then** the channel closes (`assertEventChClosed`).
   - *End-to-end* (most faithful to AC #4): `faultReader → tailJSONLLoop → jsonlCh → mergeEvents → out`; assert the same `EventKindError` surfaces on `out`.
   Prefer flipping `TestMergeEvents_JsonlChClosureClosesOutput` in place (its `close(jsonlCh)`-with-ctx-live action already *is* the read-error simulation) and, if you want the end-to-end coverage too, add it as a second test.

3. **Clean shutdown emits no error** (events_test.go — the AC-#4 companion). `TestMergeEvents_CleanShutdown` (405) already cancels ctx and asserts the next receive is a close, not an event — under this design it **stays green** because `ctx.Err() != nil` suppresses the emission. Extend it (or add a focused test) to make the "no `EventKindError`" guarantee explicit for both clean sub-cases: (a) ctx-cancel, (b) jsonlCh closed *after* ctx is already cancelled (EOF-then-shutdown shape) → assert the received value is a channel close, never an `EventKindError`.

**No-regression.** Every other `TestMergeEvents_*`, `TestTailJSONL_*`, and `TestEvents_TailJSONLErrorBubbles` is untouched and must stay green (`go test ./pkg/tuidriver/...`). `TestEvents_TailJSONLErrorBubbles` specifically continues to cover the synchronous open/seek path — that path is **not** what this ticket changes.

## Open questions

- **Should `Event.Err` carry the wrapped `os` cause instead of the `ErrJSONLTailRead` sentinel?** Deferred. Recommendation: sentinel now (§ Error handling). Revisit only when a consumer demonstrates it needs the os-level cause for telemetry — at which point the minimal upgrade is a tail→merge error-holder, and the relay-vs-parameter trade-off documented above gets re-decided against real evidence.
- **Name: `EventKindError` (general) vs `EventKindJsonlError` (precise)?** Recommendation: `EventKindError`, general, with `Source: EventSourceJsonl` naming the origin — consistent with how `Source` already disambiguates PTY vs JSONL for every other kind, and leaving room for a future PTY-side terminal error to reuse the same kind. If the developer finds a strong reason to prefer the JSONL-specific name during implementation, that's an acceptable deviation — the AC constrains behaviour, not the identifier.
- **Should the sentinel be exported at all, or an inline `errors.New` per emission?** Recommendation: export it, so consumers can `errors.Is`. A per-emission inline error would be unmatchable.

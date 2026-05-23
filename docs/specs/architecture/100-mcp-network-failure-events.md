# Spec: `Session.Events` emits MCP-failure and network-failure transitions

**Ticket:** [#100](https://github.com/pyrycode/tui-driver/issues/100)
**Size:** XS
**Status:** ready for development

## Files to read first

Load these in order; each entry says what to extract.

- `pkg/tuidriver/events.go:1-246` — the full unified event stream as it stands today. Read the EventKind block (lines 15-55), the `Event` struct doc (66-88), the `Session.Events` doc + body (90-117), the `mergeEvents` loop (119-237), and `classify` (239-245). The new constants slot into the EventKind table, the new prev-fields slot into the `mergeEvents` locals, and a new emission block slots in after the existing modal/idle/thinking block. `classify` widens (see § Design / classify return shape).
- `pkg/tuidriver/mcp_banner.go:21-33` — `HasMcpFailureBanner(snap []byte) bool` and the package-internal regex. Note the comment "doesn't trigger DetectModalClass" — the banner lives in the lower status area and coexists with idle/thinking/modal state. The merge loop treats it as an independent axis, not a fourth modal class.
- `pkg/tuidriver/network.go:18-38` — `HasNetworkFailure(snap []byte) bool` and the anchor list. Same independent-axis semantics — the failure can coexist with the spinner (in fact, the failure observation that motivated this predicate was "spinner spinning forever + `FailedToOpenSocket` in the stream").
- `pkg/tuidriver/events_test.go:13-69` — `testSnap` (the mutable snapshot harness), `mustReceiveEvent`, `assertEventChClosed`. The new tests reuse all three; do not introduce parallel helpers.
- `pkg/tuidriver/events_test.go:113-171` — `TestMergeEvents_ModalShowAndHide` is the closest precedent for the new test shape (drive a snapshot to make a predicate flip, assert a typed event, flip back, assert the paired hidden event). Mirror its structure.
- `pkg/tuidriver/mcp_banner_test.go:14-41` — synthetic banner fixtures (`"1 MCP server failed · /mcp"`, the high-count plural form, the CSI-wrapped form). The merge-loop tests use the same fixture vocabulary; do not invent new shapes.
- `pkg/tuidriver/network_test.go:14-43` — synthetic `FailedToOpenSocket` fixtures plus the negative-control set (anchors that must NOT match). The merge-loop tests reuse the same anchors.
- `pkg/tuidriver/state.go:9-27` — `IdleGlyph` / `SpinnerGlyph` byte literals. The merge-loop tests reuse these for the predicate-coexistence scenario (banner showing while the buffer is also idle).
- `pkg/tuidriver/wait.go:8-11` — `DefaultPollInterval = 50ms`. Existing tests pace phase transitions by `mustReceiveEvent(..., 500*time.Millisecond)` (10× the poll interval); the new tests follow the same precedent.
- `docs/specs/architecture/61-unified-events-channel.md` — the parent spec for the merge loop. Specifically § "Behaviour contracts / mergeEvents" (lines 144-184) and § "Why suppress idle/thinking while a modal is active" (199-202). The new axes follow the same edge-detection idiom but do NOT inherit modal suppression — see § Design / Why banner events fire independently of modal state.

## Context

`Session.Events` (#61) surfaces idle, thinking, and modal show/hide transitions on the unified PTY-half of the merged stream. Two PTY-derived failure signals are excluded:

- `HasMcpFailureBanner` — the "N MCP server(s) failed · /mcp" status line.
- `HasNetworkFailure` — `FailedToOpenSocket` (and future variants).

Both render in the lower status area, not as a modal — their comments explicitly say so. A consumer wanting continuous monitoring of either has to bolt on a parallel `Buffer.Snapshot` poll loop alongside the merge channel, defeating the point of the unified API.

The motivating consumer is pyrycode's ptyrunner migration to `Session.Events`. The operator picked continuous monitoring over today's one-shot post-idle check; without these event kinds, ptyrunner keeps its own poll loop just for the two banners.

Trust-modal is already covered by `ModalClassTrustFolder` via `EventKindPtyModalShown`, so no new kind is needed for it.

**Scope:** purely additive. Two new boolean axes on the existing PTY-state poll; four new EventKind constants on the existing enum; new prev-fields in the merge loop; a small struct refactor of `classify`'s return shape. No public-API breakage, no behavioural change for existing kinds.

What is explicitly **out of scope** for this slice:

- **`FailedMcpCount` in the event payload.** Consumer calls `FailedMcpCount(snap)` themselves if needed; bundling the count would couple Event lifecycle to predicate-API surface. AC explicitly says no payload fields beyond Source/Time.
- **A unified "banner" EventKind discriminated by payload.** The ticket's Technical Notes considered this and recommends matching the modal pattern (separate Shown/Hidden per condition) for switch-statement parity. Spec follows that recommendation.
- **Additional banner predicates** (DNS failures, TLS errors, rate limits, auth failures). When future anchors land in `network.go` or new banner files, they will extend `HasNetworkFailure` or get their own predicate + event kind in a follow-up. Today: only the two predicates that already exist.
- **Per-axis configurability** (disable MCP banner events, only emit network). Consumers ignore kinds they don't care about; the cost of always-on detection is one extra `bytes.Contains` and one `regexp.Match` per 50 ms tick over a 4 KB snap — negligible.

## Design

### Files touched

```
pkg/tuidriver/events.go        (MODIFIED — 4 new EventKind constants, classify struct, new prev-fields + emission block, doc updates)
pkg/tuidriver/events_test.go   (MODIFIED — new TestMergeEvents_McpFailureBannerTransitions + TestMergeEvents_NetworkFailureTransitions + coexistence assertion)
```

One production source file. Test file is additive (no edits to existing tests). Scope check: 1 production source file (`events.go`), well under the ≥5 red line.

### New EventKind constants

Four new constants on the existing `EventKind` iota block in `events.go`, slotted between the existing `EventKindPtyModalHidden` and the JSONL-side block — keeps the PTY-side grouping contiguous, preserves binary backwards-compat with anyone serialising EventKind by integer value (no one does, but the discipline costs nothing).

```go
// EventKindPtyMcpFailureShown fires when the "N MCP server(s) failed" status
// banner appears in the snapshot (rising edge: HasMcpFailureBanner false →
// true on a poll tick). No payload fields — the kind itself is the signal;
// consumers wanting the failure count call FailedMcpCount(snap) directly.
//
// Independent of modal/idle/thinking state: this event can fire while a
// modal is up or while the spinner is running. The banner persists in the
// status area regardless of the dominant UI axis.
EventKindPtyMcpFailureShown

// EventKindPtyMcpFailureHidden is the paired falling edge —
// HasMcpFailureBanner true → false on a poll tick. Same payload-free shape.
EventKindPtyMcpFailureHidden

// EventKindPtyNetworkFailureShown fires when a network-failure anchor
// (e.g. FailedToOpenSocket) appears in the snapshot. Same rising-edge
// semantics, same payload-free shape, same independence from the
// modal/idle/thinking axes.
EventKindPtyNetworkFailureShown

// EventKindPtyNetworkFailureHidden is the paired falling edge.
EventKindPtyNetworkFailureHidden
```

The existing `Event` struct gets one new entry in the "Field population by Kind" doc block:

- `EventKindPtyMcpFailure*`, `EventKindPtyNetworkFailure*`: no payload fields (Source/Time only).

No new fields on `Event`. No new exported types.

### `classify` return shape

Today: `func classify(snap []byte) (idle bool, thinking bool, modal ModalClass)`.

The 3-tuple cannot extend cleanly to 5; a 5-return function reads poorly in Go. Convert to a private struct in the same file:

```go
// ptyState is one tick's PTY-derived classification across the axes the
// merge loop tracks. Internal to events.go — never exported.
type ptyState struct {
    idle           bool
    thinking       bool
    modal          ModalClass
    mcpFailure     bool
    networkFailure bool
}

func classify(snap []byte) ptyState {
    return ptyState{
        idle:           IsIdle(snap),
        thinking:       IsThinking(snap),
        modal:          DetectModalClass(snap),
        mcpFailure:     HasMcpFailureBanner(snap),
        networkFailure: HasNetworkFailure(snap),
    }
}
```

`classify` is called from one call site (the ticker arm in `mergeEvents`) and is not exported, so the refactor is a 1-callsite update plus the function signature.

The merge loop's locals collapse to one:

```go
var prev ptyState  // initially zero: !idle, !thinking, ModalClassUnknown, !mcpFailure, !networkFailure
```

Replacing the existing `prevIdle / prevThinking / prevModal` triple. Edge comparisons read `prev.idle`, `prev.modal`, etc.

This struct rewrite is small but non-trivial; the existing modal/idle/thinking emission block must be updated to dereference the struct fields. Net diff in `mergeEvents`: ~10 lines changed in existing code + ~25 lines added for the new emission block (see below).

### Emission block for the new axes

Slotted into the ticker arm of `mergeEvents`'s `select`, AFTER the existing modal block and the existing idle/thinking block, BEFORE `prev = cur`:

Algorithm (developer writes the code; this is the contract):

1. **MCP failure axis:**
   - If `cur.mcpFailure && !prev.mcpFailure`: send `EventKindPtyMcpFailureShown`.
   - Else if `!cur.mcpFailure && prev.mcpFailure`: send `EventKindPtyMcpFailureHidden`.
   - (The two branches are mutually exclusive — boolean transitions have only one edge per tick.)

2. **Network failure axis:** symmetric to the MCP block, against `cur.networkFailure` / `prev.networkFailure`, emitting `EventKindPtyNetworkFailureShown` / `EventKindPtyNetworkFailureHidden`.

3. After both blocks: `prev = cur` (unchanged from today).

Each send uses the existing backpressure-aware `send(ev)` helper (defined inline in `mergeEvents` at events.go:147-154). Standard `Source: EventSourcePty`, `Time: now`, no `Modal` or `Entry` payload.

Order within one tick (informational, not contract — consumers must not assume cross-axis ordering):

1. Modal Hidden / Modal Shown (existing — modal axis dominates).
2. PtyIdle / PtyThinking (existing — only if no modal active).
3. McpFailure Hidden-or-Shown (new).
4. NetworkFailure Hidden-or-Shown (new).

The new blocks run on every tick regardless of modal state — see next subsection for the rationale.

### Why banner events fire independently of modal state

The existing rule "idle/thinking suppressed while a modal is active" exists because `IsIdle(snap)` returns `true` during e.g. a permission prompt — the `❯` glyph is still drawn beneath the modal. Emitting `PtyIdle` while the consumer's UI knows a modal is up would be a false "ready for next prompt" signal.

Banner events are different in kind:

- They are **condition events**, not state events. `HasMcpFailureBanner` does not mean "claude is in the MCP-failure state" — it means "this string is in the status area right now." The string is just as true when a modal is also up, or when the spinner is running.
- The banner predicates' own doc comments document the coexistence: `mcp_banner.go:16` ("doesn't trigger DetectModalClass. State-detection predicates can include HasMcpFailureBanner alongside isIdle"); `network.go:18-22` (the failure observation was "spinner spinning forever AND `FailedToOpenSocket` in the stream").
- Consumers want the signal regardless of dominant-axis state. Suppressing the MCP-failure event because a `/mcp` modal happened to be open would defeat the use case (it's exactly when the user is looking at MCP UI that they want to know about MCP failures).

So: no suppression. The new blocks evaluate every tick, fire whenever the boolean flips, and do not consult `cur.modal`.

This is the one design decision worth being explicit about — it diverges from the existing modal-suppression rule, and the divergence is intentional.

### Initial-state behaviour

Same as today (events.go:124-127): the loop starts transition-blind with zero-valued `prev`. If on the first tick `HasMcpFailureBanner(snap)` is already true, `EventKindPtyMcpFailureShown` fires on tick one — same "rising edge from the zero baseline" idiom as today's `EventKindPtyIdle`.

Documented as part of `mergeEvents`'s existing initial-tick comment; no new comment needed beyond a brief mention that the new axes follow the same rule.

### Doc-comment updates in events.go

1. **`EventKind` constant block:** four new constants with doc-comments as drafted above. Keep them grouped under the existing PTY block, before the JSONL block.

2. **`Event` struct doc (events.go:67-88):** one new bullet in the "Field population by Kind" list:

   > - `EventKindPtyMcpFailureShown`, `EventKindPtyMcpFailureHidden`, `EventKindPtyNetworkFailureShown`, `EventKindPtyNetworkFailureHidden`: no payload fields. The kind itself is the signal; the predicate identity is implicit.

3. **`Session.Events` doc (events.go:90-108):** update the parenthetical "(idle / thinking / modal)" in the first sentence to "(idle / thinking / modal / mcp-failure / network-failure)". Update the "PTY-state polling cadence" paragraph's "(the modal axis dominates)" caveat to add a half-sentence note that the banner axes are independent and not suppressed by modal state.

4. **`mergeEvents` doc (events.go:119-127):** add a one-sentence note that the loop also tracks `HasMcpFailureBanner` and `HasNetworkFailure` as independent boolean axes with paired Shown/Hidden emission.

No doc edits to `tuidriver.go` package comment — `Session.Events` is mentioned generically there ("unified PTY+JSONL event subscription"); the new axes don't change the one-line summary.

### Concurrency model

Unchanged. Same one goroutine per `Events()` call. Same channel capacity, same ticker, same shutdown ordering. The new axes are additional reads inside the existing ticker arm; no new goroutines, no new shared state.

### Error handling

Unchanged. The new predicates are pure functions that cannot fail; no new error paths.

### Backpressure

Unchanged in shape. The maximum number of events that can fire per tick increases from "1 Hidden + 1 Shown + 1 PtyIdle/Thinking = 3" to "+ 2 more = 5". The output channel is buffered at 32; the existing backpressure-aware send pattern already handles the case where the consumer lags. No design change.

## Testing strategy

`pkg/tuidriver/events_test.go` gets two new test functions following the structure of `TestMergeEvents_ModalShowAndHide` (the closest precedent). Reuse the existing `testSnap`, `mustReceiveEvent`, `assertEventChClosed` helpers — do not introduce parallel scaffolding.

Fixture vocabulary comes from `mcp_banner_test.go` and `network_test.go` — use the same byte literals (`"1 MCP server failed · /mcp"`, `"FailedToOpenSocket"`, the ANSI-wrapped variants) so the merge-loop tests exercise the same predicate-matching paths the unit tests already validate.

### Scenarios

#### `TestMergeEvents_McpFailureBannerTransitions`

Covers the AC bullets: clean → no event; appears → Shown fires once; clears → Hidden fires; re-appears → Shown fires again (rising-edge semantics).

- Start `mergeEvents` with a fresh `testSnap` (empty), empty `jsonlCh`, fresh output channel.
- Phase 1: `snap.Set(nil)` (or already empty) — no event expected. Use a short `select { case ev := <-out: t.Fatal(...); case <-time.After(2*DefaultPollInterval): }` window to assert quiescence.
- Phase 2: `snap.Set([]byte("...1 MCP server failed · /mcp..."))`. Receive `EventKindPtyMcpFailureShown` with `Source == EventSourcePty`, `Modal == ModalClassUnknown`, `Entry` zero-valued, `Time` non-zero.
- Phase 3: `snap.Set([]byte("plain text"))` (banner gone). Receive `EventKindPtyMcpFailureHidden` with the same field expectations.
- Phase 4: `snap.Set([]byte("2 MCP servers failed"))` (plural, different count — the predicate matches; re-appearance after clear). Receive `EventKindPtyMcpFailureShown` again.
- Phase 5: cancel ctx; `assertEventChClosed`.

#### `TestMergeEvents_NetworkFailureTransitions`

Same shape, against `HasNetworkFailure`:

- Phase 1: empty snap, assert quiescence.
- Phase 2: `snap.Set([]byte("...FailedToOpenSocket..."))`. Receive `EventKindPtyNetworkFailureShown`.
- Phase 3: `snap.Set([]byte("recovered"))`. Receive `EventKindPtyNetworkFailureHidden`.
- Phase 4: `snap.Set([]byte("\x1b[31mFailedToOpenSocket\x1b[0m"))` (ANSI-wrapped re-appearance). Receive `EventKindPtyNetworkFailureShown`.
- Phase 5: cancel ctx; `assertEventChClosed`.

#### `TestMergeEvents_BannerCoexistsWithIdleAndModal`

Verifies the "banner events fire independently of modal state" contract. Without this scenario, the suppression-divergence is not exercised and a regression that silently re-applied modal-suppression to banners would not be caught.

- Phase 1: `snap.Set([]byte("\xe2\x9d\xaf input ... 1 MCP server failed · /mcp"))` — idle glyph AND banner present in one snapshot. Expect TWO events in the same tick (order between them is internal — test should drain both via two `mustReceiveEvent` calls and assert the multiset, not the sequence): `EventKindPtyIdle` AND `EventKindPtyMcpFailureShown`.
- Phase 2: `snap.Set([]byte("Do you want to proceed ... 1 MCP server failed · /mcp"))` — permission modal anchor AND banner. Banner is already up from phase 1 (no new event); modal axis flips. Expect: `EventKindPtyModalShown` with `Modal == ModalClassPermission`. Critically: NO `EventKindPtyMcpFailureHidden` should fire — the banner is still present and the modal does not suppress it.
- Phase 3: `snap.Set([]byte("Do you want to proceed"))` — modal still up, banner gone. Expect: `EventKindPtyMcpFailureHidden`. The modal remains; no modal event fires.
- Phase 4: cancel ctx; `assertEventChClosed`.

Implementation note: phases 1 and 2 produce two events per phase. Test asserts both arrive within ~500 ms each, and (in phase 1) collects both before checking the set because Go `select` randomises arm choice when multiple are ready — but here there's only one channel-out arm, so the merge loop's internal emission order applies; either ordering is acceptable per the spec ("order between axes within one tick is informational"). Use a small helper or two sequential `mustReceiveEvent` calls into a `map[EventKind]bool` to assert both kinds were observed.

### No-regression on existing tests

All six existing `TestMergeEvents_*` plus `TestEvents_TailJSONLErrorBubbles` must continue to pass unchanged. The `classify` struct refactor is internal — the merge loop's observable behaviour for the existing axes is byte-for-byte identical.

`go test ./pkg/tuidriver/...` must continue green.

## Open questions

- **Should the McpFailure event payload carry the failure count?** No (resolved). AC explicitly says no additional payload fields. Consumer calls `FailedMcpCount(snap)` if needed — it's a public function, no information is being hidden.
- **Should the banner predicates also drive a synthetic "banner cleared" event when ctx is cancelled?** No. Same shape as today's idle/thinking/modal — no termination metadata, no goodbye events. Consumer observes channel close and treats it as "no further information."
- **Within one tick, what is the relative ordering of MCP-Shown and Network-Shown if both flip simultaneously?** Internal emission order is MCP first, network second, by the order of the two blocks in code. Not promised in any public doc — consumers must not depend on cross-axis ordering. Documented inline only as a "for the implementer" comment, not in the public Event struct doc.
- **Should `classify`'s struct fields be exported (`Idle`, `Thinking`, etc.) in case a future ticket wants to surface a "current state" snapshot accessor on `*Session`?** No. The struct is internal to `events.go`. If a public state-snapshot accessor lands later, it will get its own exported type with curated naming and documentation. Today's struct stays unexported.

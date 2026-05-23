# Spec: library API — end-of-turn discriminator and assistant-text helper

**Ticket:** [#60](https://github.com/pyrycode/tui-driver/issues/60)
**Size:** XS
**Status:** ready for development

## Files to read first

Load these in order; each entry says what to extract.

- `cmd/spike-long-prompt/main.go:441-463` — reference implementations of `isEndTurn` (stop_reason-only half) and `extractAssistantText` (the full content-block walk). The library functions are "these two, lifted onto the typed `JSONLEntry` shape, with `isEndTurn` tightened to the full Phase-A rule (stop_reason AND non-empty text content)". Read both bodies — they are the canonical shape this spec promotes.
- `pkg/tuidriver/jsonl.go:79-120` — existing `JSONLEntry`, `EntryMessage`, `ContentBlock` types. The two new helpers operate on this shape; no new exported types. Note that `ContentBlock.Raw` holds the per-block fields including `"text"`.
- `pkg/tuidriver/jsonl.go:217-255` — existing private `parseEntry` / `parseMessage` / `parseContentBlock` helpers. Idiom for "zero-value-on-mismatch type assertion" — the same `_, _ = m["x"].(string)` pattern that yields `""` on missing/wrong type is what the new helpers use to read `Raw["text"]`.
- `pkg/tuidriver/tuidriver.go:5-12` — package doc comment, in-scope / out-of-scope split. **One sentence changes** (see § Doc-comment update below). Read the current phrasing first.
- `pkg/tuidriver/jsonl_test.go:1-13` — existing imports and test-file shape. The new tests live in this file and reuse only `testing` (no goroutines, no temp files, no time.Sleep — these are pure-function tests).
- `docs/knowledge/architecture/jsonl-layout.md:55-77` — the **Phase-A discriminator** in context: why `stop_reason == "end_turn"` alone is insufficient (a `thinking`-only line carries `stop_reason=end_turn` with no text). The text-non-empty half of the rule is what disambiguates. **Do not edit** — documentation phase owns this file.
- `docs/specs/architecture/59-jsonl-tail-entry-channel.md:387-396` — the explicit "out of scope (for #59), tracked separately" list. This ticket lands the first two bullets there (end-of-turn discriminator, single-entry text extraction). msg_id grouping stays deferred — see § Out of scope.

## Context

The Phase-A turn-completion rule has been stable across all six spike binaries for ~3 weeks: a claude assistant event marks "the user-visible turn finished" when both halves hold:

1. `message.stop_reason == "end_turn"`, AND
2. The assistant's combined `type: "text"` content (concatenation of all `content[]` blocks whose `type == "text"`) has non-zero character count.

The text-non-empty half matters because a single assistant message is serialised as **one JSONL line per content block**, not one line per message — and every delta line carries `stop_reason` (jsonl-layout.md § 55-77). A line whose only block is `thinking` carries `stop_reason=end_turn` with no text; the spike's `isEndTurn` (stop_reason-only) returns `true` for it, which is wrong in the Phase-A "the model produced a user-visible reply" sense.

Both helpers exist in seven in-tree files today, in the same shape, all operating on `map[string]any`:

```
cmd/probe-first-prompt-hang/main.go isEndTurn
cmd/spike-one-turn/main.go          isEndTurn, extractAssistantText
cmd/spike-multi-turn/main.go        isEndTurn  (uses extractByMsgID instead — out of scope)
cmd/spike-cancel/main.go            isEndTurn
cmd/spike-permission/main.go        isEndTurn
cmd/spike-ask-user/main.go          isEndTurn
cmd/spike-long-prompt/main.go       isEndTurn, extractAssistantText
```

This ticket promotes both helpers into the library, tightens `isEndTurn` to the full Phase-A rule, and lifts them onto the typed `JSONLEntry` shape that `TailJSONL` already emits (#59). The same justification as #58 / #59: the shape is stable, the duplication is real, and a single canonical implementation closes the drift at source.

**Scope delta from the spike helpers.**

1. `isEndTurn` is tightened: the library function returns `false` for a `stop_reason=end_turn` line whose text content is empty (the `thinking`-only or `tool_use`-only case). This is the documented Phase-A rule.
2. Both helpers shift from `map[string]any` to the typed `JSONLEntry` parameter. The walk semantics are identical; only the parameter type changes.

What is explicitly **out of scope** for this slice:

- **msg_id grouping** (`extractByMsgID` shape). A turn whose text is split across multiple JSONL lines (one block per line, all sharing `message.id`) needs grouping — the single-line `AssistantText` returns only that line's text. Per-entry helpers are the correct primitive for "did THIS event mark turn-end" stream processing; consumers that need cross-line aggregation already build it on top (see `cmd/spike-multi-turn/main.go:extractByMsgID`), and a future ticket can promote that separately.
- **Cancellation marker detection** (`user(text="[Request interrupted by user]")`). One-line consumer-side branch; not worth a library helper today.
- **Migrating the seven in-tree spike/probe sites** to consume the new API. Mechanical follow-up, lands separately.

## Design

### Files touched

```
pkg/tuidriver/jsonl.go         (MODIFIED — append IsEndTurn + AssistantText)
pkg/tuidriver/jsonl_test.go    (MODIFIED — append TestIsEndTurn_* + TestAssistantText_*)
pkg/tuidriver/tuidriver.go     (MODIFIED — 1-sentence doc-comment edit)
```

No new files. The two new functions are thematically grouped under the existing `jsonl.go`; types and idioms are already in place.

### Public surface

Two exported functions, no new types.

```go
// IsEndTurn reports whether e marks turn-end under the Phase-A
// discriminator: the entry is an assistant envelope, its
// message.stop_reason == "end_turn", AND its combined "text"
// content (concatenation of content[] blocks whose type == "text")
// has non-zero length.
//
// The text-non-empty half disambiguates a per-block delta line that
// carries only thinking or tool_use blocks from the line that carries
// the user-visible reply. Both can land with stop_reason=end_turn (a
// single assistant message is serialised as one JSONL line per content
// block, and every delta line carries stop_reason); the text content
// is what distinguishes them. See
// docs/knowledge/architecture/jsonl-layout.md § "Turn lifecycle".
//
// Returns false for non-assistant entries, entries with a nil Message,
// entries with stop_reason != "end_turn", and zero-value entries. Safe
// to call on a JSONLEntry{} — never panics.
//
// Per-entry semantics: this function returns true for the single JSONL
// line that carries the (non-empty) text block of a turn. A turn whose
// text is split across multiple lines (one block per line, all sharing
// message.id) needs msg_id grouping on top — out of scope for the
// library; consumers compose if needed.
func IsEndTurn(e JSONLEntry) bool

// AssistantText returns the concatenation of e.Message.Content[] blocks
// whose Type == "text", reading each block's "text" field from its Raw
// map. Returns "" if e is not an assistant entry, has a nil Message,
// or carries no non-empty text blocks. Safe to call on a JSONLEntry{} —
// never panics.
//
// Blocks are joined in JSONL arrival order (the order content[] was
// parsed in). Non-string or missing "text" fields contribute nothing
// (zero-value-on-mismatch type assertion); type="thinking",
// "tool_use", "tool_result", etc. are skipped.
//
// This is the per-entry primitive. Consumers needing the full turn's
// text (split across lines under one message.id) build msg_id grouping
// on top — out of scope for the library.
func AssistantText(e JSONLEntry) string
```

### Behaviour contracts

**`AssistantText(e JSONLEntry) string`** — pure function over the typed shape.

Body invariants (the developer writes the code; this lists the rules):

- If `e.Type != "assistant"` → return `""`.
- If `e.Message == nil` → return `""`.
- Walk `e.Message.Content`:
  - Skip blocks where `Type != "text"`.
  - Read `text, _ := c.Raw["text"].(string)` (zero-value-on-mismatch — same idiom as `parseEntry`).
  - If `text != ""`, append to a `strings.Builder` (or equivalent).
- Return the accumulator.

The empty-block guard (`text != ""`) is defensive against malformed `{"type":"text","text":null}` blocks — they don't exist in observed claude output, but skipping them costs one branch and removes one source of zero-length-but-non-empty contributions.

**`IsEndTurn(e JSONLEntry) bool`** — pure function, composes with `AssistantText`.

Body invariants:

- If `e.Type != "assistant"` → return `false`.
- If `e.Message == nil` → return `false`.
- If `e.Message.StopReason != "end_turn"` → return `false`.
- Return `AssistantText(e) != ""`.

The `e.Type == "assistant"` check is explicit even though non-assistant envelopes don't carry `stop_reason` in practice — see § Type-vs-message rationale below.

The implementation calls `AssistantText` rather than duplicating the walk. Cost is one walk through `content[]` (typically <10 blocks, often 1–2). An early-exit "first non-empty text block ⇒ true" variant would shave a few cycles in the common case but doubles the code surface and gives no perceptible win on entries this small. Single source of truth wins.

### Why two functions, not one `(bool, string)` return

The ticket allows either shape. Two separate functions chosen because:

1. **The AC enumerates two distinct helpers**, each with its own behavioural contract.
2. **`AssistantText` is independently useful** on non-end-turn assistant lines (e.g. logging mid-stream content, observing partial outputs during a long turn, post-cancel reconstruction). Bundling it into the discriminator return would make the only API path for "give me the text" go through "tell me if this is end-turn"; the caller wanting just the text would have to discard the bool.
3. **The spike code already separates them** (line 254-255 of `cmd/spike-long-prompt/main.go`): `if isEndTurn(ev) { assistantText = extractAssistantText(ev) }`. Two-function shape mirrors what consumers already write.
4. The cost of computing the text twice (once inside `IsEndTurn`, once for the caller after) is exactly zero in practice — entries have ≤10 blocks, the consumer caches the result, and the lookup is a few map indexes + string append.

### Why free functions, not methods on `JSONLEntry`

Free functions match the existing `pkg/tuidriver` idiom: every public primitive (`StripANSI`, `SessionJSONLPath`, `WaitForSessionJSONL`, `TailJSONL`, all the modal/banner detectors) is a free function. `JSONLEntry` carries no methods today; promoting one helper to a method would introduce inconsistency. If a future ticket adds three or more `JSONLEntry`-shaped methods, that's the moment to reconsider — not this one.

### Type-vs-message rationale

`IsEndTurn` checks both `e.Type == "assistant"` and `e.Message != nil`. Belt-and-suspenders here is cheap (two if-statements) and defensive against future envelope kinds. Claude's `system` envelope, for instance, *also* carries a `message` field in some shapes; if a future variant of `system` happens to include a `stop_reason` for any reason, the assistant-only guard prevents a false positive. The cost is two compares per call; the safety is non-trivial.

The AC bullets call out both "non-assistant entry types (false)" and "malformed/empty entry (false, no panic)" — the type check is the cleanest way to satisfy the first and the nil check is the cleanest way to satisfy the second.

### Doc-comment update in `tuidriver.go`

One sentence updates in the out-of-scope list (line ~10):

- Current: `"JSONL semantic interpretation (end-of-turn detection, msg_id grouping, content extraction)"`.
- New: `"JSONL semantic interpretation across lines (msg_id grouping, cross-line content aggregation, cancellation-marker detection)"`.

Rationale: per-entry end-of-turn detection and per-entry text extraction are now in-scope (this ticket). The remaining out-of-scope items are all things that operate across multiple JSONL lines or that require domain-specific stream interpretation.

Also update the in-scope sentence (line ~6-7) to mention the new helpers:

- Current: `"session JSONL path resolution, appearance polling, and tail-with-parsed-entries"`.
- New: `"session JSONL path resolution, appearance polling, tail-with-parsed-entries, and per-entry assistant-text / end-of-turn helpers"`.

2-line edit, no refactor.

### Concurrency model

None. Both functions are pure — no goroutines, no I/O, no mutexes, no shared state. Safe to call concurrently with itself and with any other tuidriver primitive. Safe to call on the same `JSONLEntry` from multiple goroutines (the function only reads).

### Error handling

No error returns. Both functions are total over their input type: every `JSONLEntry` value (including the zero value, including ones with nil `Message`, including ones with garbage in `Raw`) produces a defined result (`false` / `""`) without panicking. The "no panic" half of AC 6 is satisfied structurally by the nil-guards and the zero-value type assertions.

## Testing strategy

`pkg/tuidriver/jsonl_test.go` grows by ~6-8 small table-driven (or discrete) test functions. Each test synthesises `JSONLEntry` values directly as struct literals — no file I/O, no JSON parsing, no goroutines. This is the cleanest way to lock in the per-shape contract; routing through `parseEntry` would add coupling to the parse layer that the helpers don't share.

Test scaffolding shape: each scenario is one `Test*` function or one row in a `table-driven` test, builder pattern:

```go
e := JSONLEntry{
    Type: "assistant",
    Message: &EntryMessage{
        StopReason: "end_turn",
        Content: []ContentBlock{
            {Type: "text", Raw: map[string]any{"text": "hello"}},
        },
    },
}
```

The developer chooses table-driven vs discrete `Test*` funcs per their own taste; both are idiomatic in this package (`TestSessionJSONLPath` is table-driven, the `TestTailJSONL_*` set is discrete). Either is fine.

### Scenarios — `IsEndTurn`

Each is a one-row check that `IsEndTurn(e) == want`:

- **assistant + end_turn + one non-empty text block** → `true`. The canonical Phase-A turn-end case.
- **assistant + end_turn + one empty-string text block** → `false`. The text-non-empty half of the rule rejects empty-text deltas.
- **assistant + end_turn + only tool_use blocks** → `false`. (The "tool_use-only" delta line case observed during tool-bearing turns.)
- **assistant + end_turn + only thinking blocks** → `false`. (The "thinking-only" delta line case documented in jsonl-layout.md § 75.)
- **assistant + end_turn + thinking AND text blocks** → `true`. The text block dominates; thinking contributes nothing.
- **assistant + stop_reason="tool_use" + text block** → `false`. Stop reason gate.
- **assistant + stop_reason="" + text block** → `false`. Stop reason gate (defensive).
- **assistant + nil Message** → `false`. Nil-Message guard.
- **type="user" + end_turn + text block** (synthetic — claude doesn't emit user envelopes with stop_reason, but the guard must still hold) → `false`. Type guard.
- **type="system"** → `false`. Type guard.
- **zero-value `JSONLEntry{}`** → `false`. No panic; total function.

### Scenarios — `AssistantText`

Each is a one-row check that `AssistantText(e) == want`:

- **assistant + one text block "hello"** → `"hello"`.
- **assistant + two text blocks "hello", " world"** → `"hello world"`. (Concatenation in arrival order.)
- **assistant + text, thinking, text blocks** → concatenation of the two text blocks only; thinking skipped.
- **assistant + only tool_use block** → `""`.
- **assistant + text block with non-string `"text"` field** (e.g. `{"text": 42}`) → `""`. Zero-value-on-mismatch.
- **assistant + text block with missing `"text"` field** → `""`.
- **assistant + nil Message** → `""`.
- **type="user" + text block** → `""`. Type guard.
- **zero-value `JSONLEntry{}`** → `""`. No panic.

### No-regression on existing tests

The new code is additive; existing `TestSessionJSONLPath_*`, `TestWaitForSessionJSONL_*`, and `TestTailJSONL_*` are untouched. `go test ./pkg/tuidriver/...` must continue to pass. AC 4 ("No regression in existing `pkg/tuidriver` tests") is covered by running the existing suite unchanged.

## Out of scope (reminder)

- **msg_id grouping** (`extractByMsgID` shape). A turn whose text is split across multiple JSONL lines under one `message.id` needs cross-line aggregation. The per-entry `AssistantText` is the correct primitive for "give me this line's text"; aggregation is a separate concern with its own design tradeoffs (memory bounds on long turns, replay semantics on resume, msg_id selection rule documented in jsonl-layout.md § 76). Promote separately when a consumer surfaces it.
- **Cancellation marker detection** (`user(text="[Request interrupted by user]")`). One-line consumer-side branch.
- **Migration of the seven in-tree spike/probe sites** (`cmd/spike-*/main.go` and `cmd/probe-*/main.go`) to consume `tuidriver.IsEndTurn` / `tuidriver.AssistantText`. Mechanical follow-up, lands separately. The new functions are purely additive in this slice.
- **A combined `EndOfTurn(e) (bool, string)` shape** that returns the bool and the extracted text together. Not chosen — see § "Why two functions, not one (bool, string) return" above. Could be added later as a thin wrapper if a real consumer wants it.

## Open questions

- **Should `AssistantText` strip leading/trailing whitespace?** No. Claude's text blocks land verbatim (no observed leading/trailing whitespace from the model itself), and a consumer wanting trimmed text already calls `strings.TrimSpace` (see `cmd/spike-long-prompt/main.go` for an example). The library returns bytes as-is.
- **Should `IsEndTurn` distinguish between `"end_turn"` and `"end-turn"` / `"endTurn"` / future variants?** No. The string literal `"end_turn"` is the documented value (jsonl-layout.md § 55-77). Any future drift surfaces as a behaviour change worth tracking, not as a silent fallback the library normalises away.
- **Should there be a `IsTurnTerminator(e) bool` helper that also recognises the cancellation marker?** Deferred. The cancel marker has a distinct shape (`type == "user"`, `content[0].text == "[Request interrupted by user]"`) and serves a distinct semantic ("the user said stop", not "the model said done"). Bundling them under one predicate would conflate two concepts; consumers that need both already write a 2-line branch (`if IsEndTurn(e) || isCancelMarker(e)`).

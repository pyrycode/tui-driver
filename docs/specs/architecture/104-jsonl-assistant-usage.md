# Spec: Typed `Usage` helper for assistant JSONL entries (#104)

## Files to read first

- `pkg/tuidriver/jsonl.go:108-135` — `JSONLEntry` / `EntryMessage` / `ContentBlock` shapes. `Message.Raw` is the entire `message` object, including `"usage"`. The helper reads from `Message.Raw["usage"]` — no struct-shape changes.
- `pkg/tuidriver/jsonl.go:274-337` — `IsEndTurn` and `AssistantText`. These are the per-entry primitives the new helper sits alongside. Mirror their style: free function on `JSONLEntry`, never panic on zero value, guard `Type != "assistant" || Message == nil` first.
- `pkg/tuidriver/jsonl_test.go:600-693` — `TestAssistantText` table-driven layout, with `textBlock` / `blockWithRaw` helpers nearby. Mirror this shape for the new test (a single table-driven `TestAssistantUsage` next to it).
- Issue body §"Technical Notes" — wire format: `Message.Raw["usage"]` → assert `map[string]any` → for each counter key, assert `float64` and convert to `int`.

## Context

`JSONLEntry.Message.Raw["usage"]` carries per-assistant-entry token counters (Anthropic API shape: `input_tokens`, `output_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`, JSON numbers → `float64` after `encoding/json`). Every consumer aggregating per-turn cost walks the same map by hand. Adding a typed sibling to `AssistantText` / `IsEndTurn` removes that boilerplate and gives one place to land future usage-shape changes (e.g. cache TTL breakdowns).

Pure additive. No change to parse pipeline or struct shapes — just a new exported struct and a new exported helper function reading from existing fields.

## Design

### New exported type

```go
// Usage is the four token counters carried by an assistant entry's
// message.usage block. Counter zero is the wire value; absence is
// signalled by AssistantUsage returning nil — see that function.
type Usage struct {
    InputTokens              int
    OutputTokens             int
    CacheCreationInputTokens int
    CacheReadInputTokens     int
}
```

Four counters only — matches the ticket AC and pyrycode's existing `jsonl.UsageBlock`. No `ServiceTier`, no cache-TTL breakdowns; YAGNI until a consumer needs them.

### New exported function

```go
func AssistantUsage(e JSONLEntry) *Usage
```

Contract:

- Returns `nil` if `e.Type != "assistant"`, `e.Message == nil`, or `e.Message.Raw["usage"]` is absent / not a `map[string]any`.
- Otherwise returns a non-nil `*Usage` populated from the four counter keys (`input_tokens`, `output_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`). Each key is read via `float64` type assertion and converted to `int`; missing or non-numeric keys contribute `0`.
- Distinguishes "field absent" (`nil`) from "field present, all zero" (non-nil `&Usage{}`). This is the AC's required presence-distinction; pointer return is the chosen mechanism (parallels pyrycode's existing `*jsonl.UsageBlock` pattern, slightly cleaner caller ergonomics than `(Usage, bool)`).
- Safe to call on a zero-value `JSONLEntry{}`. Never panics.

Wire-key → field mapping is intentionally hand-written (not reflection / `json.Unmarshal` into the struct). Reasons: the entry is already parsed into `Raw` once; the four-key walk is shorter than a re-marshal/unmarshal round-trip and consistent with how `AssistantText` reads `c.Raw["text"]`.

Place the function and `Usage` struct immediately after `AssistantText` at `jsonl.go:337`, with a doc comment in the same style as `AssistantText`'s.

### What the helper does NOT do

- Does not sum across entries — that's the consumer's aggregation (e.g. pyrycode's `streamjson.Emitter`).
- Does not validate counters are non-negative — wire trust; consumers can sanity-check.
- Does not surface unknown usage keys — `Raw["usage"]` remains reachable for forward-compat.
- Does not parse the `service_tier` string or any non-counter sibling field. Out of scope for this ticket.

## Concurrency model

None. Pure function over a value-typed entry.

## Error handling

No error return. All failure modes (wrong envelope type, nil message, absent usage, malformed usage map, non-numeric counters) collapse to either `nil` (envelope-level absence) or `0` on the specific counter (key-level mismatch). This matches `AssistantText`'s zero-value-on-mismatch posture.

## Testing strategy

Add `TestAssistantUsage` next to `TestAssistantText` in `pkg/tuidriver/jsonl_test.go`. Table-driven, same shape. Scenarios (from AC §4):

- **assistant + usage with all four counters populated** — expect non-nil `*Usage` with the four wire values.
- **assistant + usage map present but counter keys omitted** — expect non-nil `*Usage{}` (all zero ints). Asserts the "present-but-zero ≠ absent" distinction by also checking the returned pointer is non-nil.
- **assistant + usage map present with a non-numeric counter value** (e.g. `"input_tokens": "lots"`) — expect non-nil `*Usage` with that counter zeroed, others populated. Mirrors `AssistantText`'s "non-string text field → empty" case.
- **assistant + no `usage` key in `Message.Raw`** — expect `nil`.
- **assistant + `Raw["usage"]` of the wrong shape** (e.g. a string or a number, not a map) — expect `nil`.
- **assistant + nil `Message`** — expect `nil`.
- **type=user + usage map present** — expect `nil` (non-assistant envelope short-circuits).
- **zero-value `JSONLEntry{}`** — expect `nil`, must not panic.

Use `Message.Raw` directly when constructing test entries (no new helper needed). The existing `textBlock` / `blockWithRaw` helpers are content-block-shaped and not applicable here; the usage block sits on `Message.Raw["usage"]`, not on a `ContentBlock`.

Each case asserts both the return value equality (via a `reflect.DeepEqual` on `*Usage`, or by checking `got == nil` for absent cases and field-by-field for present cases — pick whichever reads cleaner; both are accepted).

## Open questions

None. Pointer-return semantics chosen per architect's call (see Design §"New exported function"). All other AC items are deterministic.

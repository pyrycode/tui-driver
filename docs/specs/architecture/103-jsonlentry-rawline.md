# Spec: `JSONLEntry.RawLine` — verbatim source-line bytes

Ticket: [#103](https://github.com/pyrycode/tui-driver/issues/103). Split from #101. Size: XS.

## Files to read first

- `pkg/tuidriver/jsonl.go:80-99` — current `JSONLEntry` struct and doc comment; the new field slots in here.
- `pkg/tuidriver/jsonl.go:173-212` — `tailJSONLLoop`: the `partial` / `line` / `parseEntry` flow. Critical: `line := bytes.TrimRight(partial, "\r\n")` shares backing memory with `partial`, and the next iteration's `partial = append(partial, chunk...)` may clobber it. The verbatim bytes MUST be copied before being handed to the consumer.
- `pkg/tuidriver/jsonl.go:214-233` — `parseEntry`: where the entry is materialised. Cleanest place to do the defensive copy.
- `pkg/tuidriver/jsonl_test.go:155-262` — existing `mustReceive` / `mustAppend` helpers and append-during-tail patterns; the new round-trip test reuses these.
- `pkg/tuidriver/jsonl_test.go:264-289` — `TestTailJSONL_MalformedLineNonFatal` is the closest existing test in shape (write file, tail, assert one entry); model the new test on it.

## Context

`JSONLEntry.Raw` is `map[string]any`. Re-marshalling that map via `json.Marshal` does **not** preserve key order or whitespace — Go's encoder normalises both. The motivating consumer (pyrycode's `internal/agentrun/streamjson/emitter.go:195`) writes claude's per-session JSONL back onto stdout as stream-json and needs byte-verbatim fidelity to keep its dispatcher-parser contracts intact (pyrycode #506 pins the byte-equivalence test).

Today the consumer cannot get byte-fidelity from `JSONLEntry`. This spec adds a field that surfaces the trimmed source-line bytes exactly as the tail goroutine read them.

## Design

### Public API change

Add one field to `JSONLEntry` in `pkg/tuidriver/jsonl.go`:

```go
type JSONLEntry struct {
    Type    string
    Message *EntryMessage
    Raw     map[string]any
    RawLine []byte // verbatim source-line bytes; see doc below
}
```

**Type choice:** `[]byte`, not `json.RawMessage`. `json.RawMessage` only adds value if the struct itself round-trips through `encoding/json`, which `JSONLEntry` does not. The motivating consumer does `append([]byte(nil), ev.RawLine...)` — a plain byte slice is the direct shape. (The acceptance criteria explicitly allows the architect's call between the two; this is mine.)

**Field name:** `RawLine` (distinct from existing `Raw`). Doc comment must clarify that `Raw` is the parsed map and `RawLine` is the source bytes, and that they are NOT round-trips of each other.

**Field placement:** at the end of the struct, after `Raw`. The order does not affect call sites (no positional struct literals exist for `JSONLEntry` — all uses are keyed), but keeping `Raw` adjacent to its byte counterpart aids readability.

**Contract:**

- `RawLine` is populated for every entry emitted on the `TailJSONL` channel — never nil, never empty (parser already filters empty lines).
- `RawLine` is the bytes the tail goroutine read between line breaks, with the trailing `\r\n` (or `\n`) stripped — matching what `parseEntry` consumed. No leading/trailing whitespace stripping beyond `\r\n`.
- `RawLine` is owned by the entry: the consumer may read it freely; the library will not mutate it after emission.
- For entries constructed by callers directly (test fixtures, synthetic events), `RawLine` may be nil — its presence is the `TailJSONL` contract, not a struct invariant.

### Implementation sketch

Two production-code changes in `pkg/tuidriver/jsonl.go`:

1. **Add `RawLine []byte` to `JSONLEntry`** with a doc comment that distinguishes it from `Raw` and pins the trimming rule.

2. **Make `parseEntry` populate `RawLine` with a defensive copy** of `line`. The copy is mandatory — `line` aliases `partial`'s backing array, which `tailJSONLLoop` reuses across iterations. A naked assignment would race the next read.

   Shape (signature unchanged; one new line inside the function):

   ```go
   entry := JSONLEntry{Raw: raw, RawLine: bytes.Clone(line)}
   ```

   (`bytes.Clone` is stdlib since Go 1.20 — already used in this codebase? if not, `append([]byte(nil), line...)` is the equivalent idiom and matches what the consumer does at the receive side.)

`tailJSONLLoop` itself does NOT change — it still passes `line` to `parseEntry`, and the copy happens inside `parseEntry`. Keeping the copy inside `parseEntry` localises the byte-ownership concern to the parser; the loop continues to own only the rolling buffer.

### Allocation cost

One extra allocation per entry, sized to the line length. The technical-notes section of the ticket already accepts this: "The capture happens once per line and is small relative to the JSON parse, so an extra allocation per entry is acceptable." `defaultJSONLTailBuffer = 32` is unchanged.

## Concurrency model

Unchanged. The bytes captured by `parseEntry` are immutable from the consumer's perspective: the tail goroutine never holds a reference to them after the channel send, and `parseEntry` itself returned them to the entry value before that send. No new synchronisation needed.

## Error handling

Unchanged. `parseEntry` continues to return `(zero, false)` for malformed JSON; in that case no entry is emitted and `RawLine` is irrelevant. Empty / whitespace-only lines are still filtered upstream (`if len(line) == 0 { continue }` at `jsonl.go:190`).

## Testing strategy

Add one new test to `pkg/tuidriver/jsonl_test.go`:

**`TestTailJSONL_RawLineByteFidelity`** — write a temp JSONL file containing four lines that cover the realistic worst cases for re-marshal drift, tail it, and assert each `ev.RawLine` is byte-identical to the corresponding source line (with the trailing `\n` removed). Scenarios:

- **Mixed-order keys** — a line whose top-level keys appear in non-alphabetical order (e.g. `{"z":1,"a":2,"m":3}`). Go's encoder sorts map keys alphabetically on marshal; this is the canonical drift source.
- **Unicode payload** — a line whose string values contain multi-byte UTF-8 (Japanese characters, emoji). Asserts no encoding/decoding pass mutates the bytes.
- **Escaped newlines** — a line whose JSON string value contains `\n` as an escape sequence (NOT a literal byte). Asserts the escape stays escaped on the way through.
- **Embedded base64** — a line carrying a long base64-looking string value (mixed case, `+`/`/`/`=` characters). Stand-in for tool_use input payloads.

Each scenario is one entry on the wire. The assertion is `bytes.Equal(ev.RawLine, expectedLineWithoutTrailingNewline)`.

**Additional micro-assertion in the same test** (or a sibling test if it grows): after receiving entry N, append a new line to the file, receive entry N+1, then re-read `entries[N].RawLine` and assert it is still equal to its original value. This pins the defensive-copy contract — if a future refactor "optimises" the copy away, the rolling-buffer reuse would clobber `entries[N].RawLine` between iterations and this assertion would fail.

Reuse `mustReceive` and (where needed) `mustAppend` from `jsonl_test.go:161` and `jsonl_test.go:175`. No new helpers required.

### What is NOT tested

- Behaviour for `parseEntry` called directly with synthetic input — it's unexported and the public contract is via `TailJSONL`. The end-to-end test above already exercises the copy path.
- Marshal-drift of the consumer's downstream re-emit pipeline — that's pyrycode #506's responsibility, not ours.

## Open questions

None. The acceptance criteria are unambiguous; the only architect-level call (field name + type) is decided above.

## Out of scope

- Exposing `RawLine` on `EntryMessage` or `ContentBlock`. Only the top-level line is captured; sub-object byte-spans would require a streaming JSON parser, which is a different ticket.
- Surfacing byte offsets within the source file. The current `TailJSONL` API does not expose offsets either; if a future consumer needs `(offset, length)` for resumable tailing, that's a separate change.
- Changing `defaultJSONLTailBuffer`. The extra per-entry allocation does not change backpressure characteristics meaningfully.

# Spec #142 — `ParseToolResult` JSONL extractor

**Ticket:** [pyrycode/tui-driver#142](https://github.com/pyrycode/tui-driver/issues/142)
**Size:** S (single new production file, one exported type, one exported function, purely additive — the structural twin of #140).
**Part of:** pyrycode#596 Phase 2 (structured streaming); enables the daemon bridge core (pyrycode#608) to emit `ToolUpdate` events from JSONL rather than screen-scraped PTY signals.

This is the `tool_result` half of the original #140. The `tool_use` half (`ParseToolUse` → `ToolStart`) shipped as #140 and is the direct precedent — read its spec and its code first. The two are independent; neither blocks the other.

## Files to read first

- `pkg/tuidriver/tool_use.go:1-59` — **`ParseToolUse`: the just-shipped sibling. Copy its skeleton verbatim and change three things only: (1) the envelope gate (`"assistant"` → `"user"`), (2) the block type (`"tool_use"` → `"tool_result"`), (3) the projected field set.** Everything else — `parseEntry` reuse, the `for _, c := range entry.Message.Content` walk, first-match-wins, nil-on-no-match, the "match gate is the block type alone, NOT field presence" divergence note in the doc comment — is identical.
- `docs/specs/architecture/140-parse-tool-use.md` — the sibling spec. The Design / Concurrency / Error-handling / Testing sections below mirror it; read it for the full rationale behind the `Parse<Tool>` family conventions (signature choice, no-`error`-return, fixture pair).
- `pkg/tuidriver/jsonl.go:228-272` — `parseEntry` / `parseMessage` / `parseContentBlock`: confirms `parseEntry` populates `Type` and `Message` for **`user`** envelopes too (`raw["message"]` decode at `:244` is envelope-agnostic — it fires for any envelope carrying a `message` object, which is `assistant` AND `user`). This is what makes the `user`-gate work with zero decoder changes. `parseEntry(snap) (JSONLEntry, bool)` returns `ok=false` on non-JSON / non-object — the malformed-JSON → nil gate.
- `pkg/tuidriver/jsonl.go:108-135` — `JSONLEntry` / `EntryMessage` / `ContentBlock` field definitions. Note `ContentBlock` doc at `:127-131` already names `tool_use_id` among the example `Raw` fields — the projection reads from `Raw`, exactly as `ParseToolUse` reads `id`/`name`/`input`.
- `pkg/tuidriver/jsonl.go:321-337` (`AssistantText`) and the `AssistantUsage` zero-value posture — the `text, _ := c.Raw["text"].(string)` / "Safe to call on a `JSONLEntry{}` — never panics" contract wording to echo in `ToolResult`'s doc.
- `pkg/tuidriver/tool_use_test.go:1-167` — the RED→GREEN test pattern to mirror exactly: a nil-cases table (`TestParseToolUseReturnsNilOnNon`), a field-projection table (`TestParseToolUseProjectsFields`), a zero-value-safe test, and a `.bin` real-fixture test with substring guards (`TestParseToolUseRealFixture`). Adapt all four.
- `pkg/tuidriver/testdata/tool-use-snapshot.bin` / `.json` — the fixture pair convention (a full, realistic raw JSONL line `.bin` + a human-readable expected-projection `.json`). The new `tool-result-snapshot.*` pair mirrors this — and the `.bin` is the natural **`user(tool_result)` reply to the existing `tool-use-snapshot.bin`'s Bash call** (same `tool_use_id`), so the two fixtures read as one observed `assistant(tool_use) → user(tool_result)` exchange.
- `docs/knowledge/architecture/jsonl-layout.md:81-96` — § "Tool-use shapes (observed)": the observed `assistant(tool_use) → user(tool_result) → …` sequence (the row table at `:85-89`), the note that the tailer's assistant-only filter drops `user(tool_result)`, and the `Parse<Tool>` family paragraph. **The `user`-envelope source for `tool_result` is documented here — this is the one place the shape diverges from `ParseAskUserQuestion`/`ParseToolUse`, which gate on `assistant`.**
- `docs/knowledge/architecture/jsonl-layout.md:102-110` — § "Cancellation signal": the `user(text "[Request interrupted by user]")` marker is also a `user` envelope. It carries a `text` block, not a `tool_result` block, so the content-type walk skips it → `nil`. No special-casing needed; called out in Error handling below so a reader doesn't expect a false match.

## Context

The daemon bridge (pyrycode#608) drains tool-call data off an interactive `claude` session's JSONL to drive its `ToolStart` / `ToolUpdate` stream. JSONL is the robust source; the PTY spinner signals are dead at `claude` 2.1.158 (see `CLAUDE.md` spinner caveat). This ticket is the `tool_result` half (→ `ToolUpdate`); `ParseToolUse` (→ `ToolStart`) shipped as the independent sibling #140.

`ParseToolResult` is the **third member of the `Parse<Tool>` family** documented in `jsonl-layout.md` (after `ParseAskUserQuestion` and `ParseToolUse`). It shares the family posture exactly — pure projection off `ContentBlock.Raw`, nil-on-no-match, no `error` return, `.bin`+`.json` fixture pair. It diverges from both prior members in **one structural way**: `tool_result` blocks ride **`user`**-role envelopes, not `assistant`. The match gate is therefore `entry.Type == "user"` ∧ a `content[]` block of `type == "tool_result"`.

## Design

One new file: `pkg/tuidriver/tool_result.go`. One exported type, one exported function. No existing file is modified.

### Signature decision: `snap []byte`, not `JSONLEntry`

The ticket delegates the signature choice. **Chosen: `snap []byte`** — identical reasoning to #140 (read its § "Signature decision" for the full argument):

- **Family consistency.** `Parse<Tool>(snap []byte) *<Tool>` is the documented family signature. `AssistantText` / `AssistantUsage` are a *different* (per-entry) family.
- **The AC is `[]byte`-native.** AC#3 lists "malformed JSON" as a no-match input. A `JSONLEntry`-arg function never sees malformed JSON — `parseEntry` dropped it upstream. Only a `[]byte`-arg function that calls `parseEntry` itself can honour "malformed JSON → nil."
- **`parseEntry` reuse keeps decoding single-sourced.**
- **Consumer access is cheap.** The bridge holds `JSONLEntry` from `TailJSONL` and calls `ParseToolResult(e.RawLine)`. `RawLine` is the verbatim line bytes, populated for every `TailJSONL` entry. The re-parse is one `json.Unmarshal` per line at interactive-conversation rate — not a hot path. The bridge loop stays uniform: `ParseToolUse(e.RawLine)` for `assistant` lines, `ParseToolResult(e.RawLine)` for `user` lines.

### Type

```go
type ToolResult struct {
    ToolUseID string `json:"tool_use_id"`
    IsError   bool   `json:"is_error"`
    Content   any    `json:"content"`
}
```

- `ToolUseID` — the id of the `tool_use` block this result answers (e.g. `toolu_…`), read from `c.Raw["tool_use_id"]`. This is the join key the bridge uses to correlate a `ToolUpdate` back to its `ToolStart`.
- `IsError` — the wire `is_error` flag; `true` when the tool run failed. Read from `c.Raw["is_error"]`. Absent → `false` (AC#4).
- `Content` — the result payload. **Typed as `any`, deliberately**, because the wire shape is a union: `content` arrives either as a string (the common `Bash`/`Read` result) **or** as an array of content blocks (`[{"type":"text","text":"…"}]`, the Anthropic structured shape). `any` preserves whichever arrived "exactly as found on the wire" (AC#2) with no normalisation, the same generic-preservation choice `ParseToolUse.Input map[string]any` makes for its per-tool-variable shape. A string projects to `string`; an array projects to `[]any`; an absent key projects to `nil`. Read with a plain map index (`c.Raw["content"]`), **no type assertion** — there is nothing to assert against a union, and a missing key already yields the `nil` zero value without one. (See Open questions for why this is preferred over a typed string field or a `json.RawMessage`.)

Doc-comment the type in the `ToolUse` / `AssistantText` style, including the "Safe to read on a zero value — never panics" guarantee and a one-line note that `Content` is intentionally `any` to span the string|array wire union.

### Function contract

```go
// ParseToolResult projects the first tool_result content block of a
// user-envelope JSONL line (snap) into a *ToolResult. Returns nil on
// no-match. Reuses parseEntry so envelope decoding stays single-sourced.
func ParseToolResult(snap []byte) *ToolResult
```

Behaviour — mirror `ParseToolUse`'s body with the three substitutions:

1. `entry, ok := parseEntry(snap)`. Return `nil` if `!ok` (not valid JSON / not a JSON object) OR **`entry.Type != "user"`** OR `entry.Message == nil`.
2. Walk `entry.Message.Content` in arrival order. Skip blocks where **`c.Type != "tool_result"`**.
3. **First** `tool_result` block wins (later blocks ignored — matches `ParseToolUse`). On match, project and return non-nil:
   - `tr.ToolUseID, _ = c.Raw["tool_use_id"].(string)`
   - `tr.IsError, _ = c.Raw["is_error"].(bool)`
   - `tr.Content = c.Raw["content"]` — no assertion (union; absent key → `nil`)
4. If no `tool_result` block is found after the walk, return `nil`.

**Match gate is `type == "tool_result"` alone.** Same deliberate divergence the doc comment must flag as `ParseToolUse` does: the match is NOT field-gated. A `tool_result` block with a missing/non-string `tool_use_id`, missing/non-bool `is_error`, or missing `content` still returns a **non-nil** `*ToolResult` with those fields at their zero values (`""`, `false`, `nil`). The presence of `type:"tool_result"` alone is the match signal; field absences are zero-value-on-mismatch per AC#4. Copy `ParseToolUse`'s "do not 'fix' this into a gate" warning into the doc comment so a future consistency pass doesn't re-converge it.

### Data flow

```
snap []byte ──parseEntry──▶ JSONLEntry ──content walk──▶ first tool_result block
                  │                                              │
              nil if not                          ToolResult{ToolUseID, IsError, Content}
              user/JSON-object                    from c.Raw[...]  (or nil if no such block)
```

## Concurrency model

None. `ParseToolResult` is a pure function: no I/O, no shared state, no goroutines. Safe for concurrent use, like `AssistantText` / `ParseToolUse` / `ParseAskUserQuestion`.

## Error handling

No `error` return — nil-on-no-match is the entire failure surface, matching the family ("ceremony without information gain"). Failure modes and their results:

| Input | Result |
|-------|--------|
| `nil` / empty / non-JSON / JSON non-object | `nil` (via `parseEntry` ok=false) |
| Non-`user` envelope (`assistant`, `system`, …) | `nil` |
| `user` envelope, `Message == nil` | `nil` |
| `user` envelope, only a `text` block (e.g. the `[Request interrupted by user]` cancel marker) | `nil` — no `tool_result` block present |
| `user` envelope with a `tool_result` block | non-nil `*ToolResult` (fields zero-value-on-mismatch) |
| `tool_result` block, individual field missing / type-mismatched | non-nil; that field zero-valued; never panics |

The cancel-marker row is worth an explicit test case (below): it confirms the content-type walk — not an envelope-only gate — is what rejects a non-`tool_result` `user` line. A naive "gate on `user` and return non-nil" would wrongly match the cancel marker; the `c.Type != "tool_result"` skip is what prevents it.

## Testing strategy

New file `pkg/tuidriver/tool_result_test.go`, mirroring `tool_use_test.go` (read it first — adapt, don't invent). RED→GREEN. Four tests; describe table inputs as inline JSON literals (no fixture needed except the real-fixture test):

**1. Nil / no-match table** (mirror `TestParseToolUseReturnsNilOnNon`) — each case asserts `ParseToolResult(snap) == nil`:
- `nil`; empty `[]byte{}`; garbage (`not json`); JSON non-object (`[1,2,3]`)
- **assistant envelope carrying a `tool_result` block** — proves the `user`-gate: even with a well-formed `tool_result` block, an `assistant` envelope returns `nil` (the envelope-kind divergence from `ParseToolUse`, asserted directly)
- `user` envelope, nil message (`{"type":"user"}`)
- `user` envelope, only a `text` block (the cancel-marker shape: `content:[{"type":"text","text":"[Request interrupted by user]"}]`) → `nil`

**2. Match + zero-value scenarios** (mirror `TestParseToolUseProjectsFields`) — each asserts non-nil + projected fields:
- `user` + `tool_result` with **string** `content` (`tool_use_id`, `is_error:false`, `content:"file1\nfile2\n"`) → non-nil; `ToolUseID==` the id, `IsError==false`, `Content.(string)==` the string
- `user` + `tool_result` with **array** `content` (`content:[{"type":"text","text":"out"}]`) → non-nil; `Content` asserts to `[]any` of length 1 (proves the union's array arm is preserved, not flattened)
- `user` + `tool_result` with `is_error:true` → `IsError==true`
- missing `tool_use_id` → `ToolUseID==""`
- missing `is_error` → `IsError==false`
- missing `content` → `Content==nil`
- non-bool `is_error` (e.g. `"is_error":"yes"`) → `IsError==false` (never panics — the type-mismatch case)
- non-string `tool_use_id` (e.g. `"tool_use_id":42`) → `ToolUseID==""` (never panics)
- `[text block, tool_result block]` (text first) → finds the `tool_result`; and `[tool_result, tool_result]` → **first** wins (assert by `tool_use_id`)

**3. Zero-value-safe test** (mirror `TestParseToolUseZeroValueInputSafe`) — `ParseToolResult(nil) == nil`, no panic.

**4. Real-fixture test** (mirror `TestParseToolUseRealFixture`) — read `testdata/tool-result-snapshot.bin`, substring-guard the wire markers (`"type":"tool_result"`, `"tool_use_id":`, `"is_error":`), then assert `ParseToolResult` returns non-nil with `ToolUseID` non-empty and `Content` non-nil.

### Fixtures

Two new files, mirroring the `tool-use-snapshot.*` pair (which is itself a full, realistic claude line — match that fidelity, not a stripped-down stub):

- `pkg/tuidriver/testdata/tool-result-snapshot.bin` — one raw JSONL **`user`** envelope line carrying a `tool_result` block, shaped as the reply to `tool-use-snapshot.bin`'s Bash call (reuse its `tool_use_id` `toolu_01BashListTmpExampleAAAAAA` so the two fixtures read as one `assistant(tool_use) → user(tool_result)` exchange). The `message` object holds `role:"user"` and `content:[{"type":"tool_result","tool_use_id":"toolu_01BashListTmpExampleAAAAAA","is_error":false,"content":"<some files>\n"}]`; wrap it in the same envelope scaffold the tool-use fixture uses (`parentUuid`, `isSidechain`, `type:"user"`, `uuid`, `timestamp`, `sessionId`, `version:"2.1.158"`, `gitBranch`). Use **string** `content` here (the common Bash result shape); the array-content arm is exercised by the inline table in test #2, so the real fixture need only carry one arm.
- `pkg/tuidriver/testdata/tool-result-snapshot.json` — human-readable expected projection (documentary, as `tool-use-snapshot.json` is): `{"tool_use_id":"toolu_01BashListTmpExampleAAAAAA","is_error":false,"content":"<same string>"}`.

**Sourcing the `.bin`:** same guidance as #140 — prefer capturing a real `user(tool_result)` line from a live session (driving `claude` with a tool-triggering prompt like `list the files in /tmp`; the `tool_result` follows the `tool_use` in the same run). If a live `claude` is impractical, hand-craft from the shape above — the test's structural assertions (substring guards + field presence) don't depend on a real session's exact values, only on the `tool_result` shape being well-formed. Provenance is left to the developer per the environment; both satisfy the AC.

## Open questions

- **Why `Content any` rather than a typed field?** A `string` field would silently drop the array arm of the union (lossy, violates AC#2's "exactly as found on the wire"). A `json.RawMessage` would force the consumer to re-decode bytes `parseEntry` already decoded, and `Raw["content"]` is already a live `any` (string or `[]any`), not bytes — there are no bytes to hand back. `any` is the faithful, zero-cost representation; the bridge consumer type-switches on it (`switch v := tr.Content.(type)`) exactly as it would have on the wire. Deferred unless a consumer surfaces a concrete need for a richer typed `content` (none observed — evidence-based-fix posture).
- **Multiple `tool_result` blocks in one line.** Empirically one JSONL line = one content block (`jsonl-layout.md:69-71`), so first-match-wins is moot in practice but defines the contract for the rare multi-block line. A `ParseToolResults`-plural is a deferred follow-up if a real consumer needs all blocks from one envelope — do not pre-build it (same stance as #140).
- **Fixture provenance** (recorded vs. hand-crafted) is left to the developer per the environment; both satisfy the AC.

## Acceptance criteria mapping

- AC1 (projects `tool_use_id`/`is_error`/`content` from `ContentBlock.Raw`) → `ToolResult` type + step 3 of the contract.
- AC2 (carries the three fields exactly as on the wire) → real-fixture test #4 + the string-content and array-content match scenarios in test #2 (the array case is what proves "exactly as on the wire" for the union).
- AC3 (nil on no `tool_result` / malformed JSON / wrong envelope kind) → error-handling table + nil-cases test #1 (including the `assistant`-envelope-with-`tool_result` case, which asserts the `user`-gate).
- AC4 (missing/type-mismatched fields → zero values, never panic; absent `is_error`→`false`, absent `content`→zero value; safe on zero value) → zero-value scenarios in test #2 + the `nil`-snap case in tests #1 and #3.
- AC5 (RED→GREEN with the established fixture pattern) → `.bin`+`.json` pair + the four-test structure mirroring `tool_use_test.go`.

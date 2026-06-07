# Spec #140 — `ParseToolUse` JSONL extractor

**Ticket:** [pyrycode/tui-driver#140](https://github.com/pyrycode/tui-driver/issues/140)
**Size:** S (single new production file, one exported type, one exported function, purely additive)
**Part of:** pyrycode#596 Phase 2 (structured streaming); enables the daemon bridge core (pyrycode#608) to emit `ToolStart` events from JSONL rather than screen-scraped PTY signals.

## Files to read first

- `pkg/tuidriver/ask_user.go:38-105` — `ParseAskUserQuestion`: the `Parse<Tool>(snap []byte) *<Tool>` precedent this follows exactly — `parseEntry` reuse, the `for c := range entry.Message.Content` walk, the `c.Raw["name"]` / `c.Raw["input"]` zero-value type assertions, first-match-wins, nil-on-no-match. Copy its skeleton.
- `pkg/tuidriver/ask_user.go:1-36` — the type doc-comment style for `AskUserQuestion*`; mirror the tone for `ToolUse`.
- `pkg/tuidriver/jsonl.go:307-337` — `AssistantText`: the zero-value-on-mismatch posture (`text, _ := c.Raw["text"].(string)`) and the "Safe to call on a JSONLEntry{} — never panics" contract wording to echo in `ToolUse`'s doc.
- `pkg/tuidriver/jsonl.go:228-272` — `parseEntry` / `parseMessage` / `parseContentBlock`: confirms `entry.Type`, `entry.Message *EntryMessage`, `ContentBlock{Type, Raw map[string]any}`. `parseEntry(snap []byte) (JSONLEntry, bool)` returns `ok=false` on non-JSON / non-object — this is the malformed-JSON → nil gate.
- `pkg/tuidriver/jsonl.go:80-130` — `JSONLEntry` / `EntryMessage` / `ContentBlock` field definitions, so field access is exact.
- `pkg/tuidriver/ask_user_test.go:1-80` — the RED→GREEN test pattern: a nil-cases table (`TestParseAskUserQuestionReturnsNilOnNon`) + a `.bin` real-fixture test with substring guards (`TestParseAskUserQuestionRealFixture`). Mirror both.
- `pkg/tuidriver/testdata/ask-user-question-snapshot.bin` / `.json` — the fixture pair convention (raw JSONL assistant-envelope line `.bin` + human-readable expected-shape `.json`). The new fixtures mirror this exactly.
- `docs/knowledge/architecture/jsonl-layout.md:81-93` — § "Tool-use shapes (observed)": the wire shape of a `tool_use` block on an `assistant(stop_reason=tool_use)` envelope, and the `Parse<Tool>` convention paragraph. The fixture's wire shape comes from here.

## Context

The daemon bridge (pyrycode#608) drains tool-call data off an interactive `claude` session's JSONL to drive its `ToolStart` / `ToolUpdate` stream. JSONL is the robust source; the PTY spinner signals are dead at `claude` 2.1.158 (see `CLAUDE.md` spinner caveat). This ticket is the `tool_use` half (→ `ToolStart`); `tool_result` (→ `ToolUpdate`) is the independent sibling.

`tool_use` blocks ride `assistant`-role envelopes — structurally identical to where `ParseAskUserQuestion` reads — so this is the **second member of the `Parse<Tool>` family** documented in `jsonl-layout.md`. It differs from `ParseAskUserQuestion` in exactly one way: it is **not name-gated**. `ParseAskUserQuestion` matches only `tool_use` blocks named `AskUserQuestion`; `ParseToolUse` matches *any* `tool_use` block and projects whatever tool it carries.

## Design

One new file: `pkg/tuidriver/tool_use.go`. One exported type, one exported function.

### Signature decision: `snap []byte`, not `JSONLEntry`

The ticket delegates the signature choice (per-entry `JSONLEntry` like `AssistantText`, vs. `snap []byte` like `ParseAskUserQuestion`). **Chosen: `snap []byte`.**

Rationale:
- **Family consistency.** `ParseToolUse` is the second `Parse<Tool>` projection. `jsonl-layout.md` documents the family signature as `Parse<Tool>(snap []byte) *<Tool>`. `AssistantText` / `AssistantUsage` / `IsEndTurn` are a *different* family (per-entry content discriminators), not per-tool projections. Consistency belongs within the `Parse<Tool>` family → `[]byte`.
- **The AC is `[]byte`-native.** AC lists "malformed JSON" as a no-match input case. A `JSONLEntry`-arg function never sees malformed JSON — `parseEntry` already dropped it upstream in the tail loop. Only a `[]byte`-arg function that calls `parseEntry` itself can honour "malformed JSON → nil."
- **`parseEntry` reuse keeps decoding single-sourced** — the same single-sourcing `ParseAskUserQuestion` relies on.
- **Consumer access is supported and cheap.** The bridge holds `JSONLEntry` from `TailJSONL` and calls `ParseToolUse(e.RawLine)`. `RawLine` is the verbatim line bytes, populated for every `TailJSONL` entry precisely for "consumers that need the line bytes" (see `JSONLEntry` doc, `jsonl.go:96-107`). The re-parse is one `json.Unmarshal` of one line at interactive-conversation rate — not a throughput hot path.

The sibling `ParseToolResult` ticket decides its own signature independently; if it also follows the `Parse<Tool>` convention the bridge loop stays uniform (`ParseToolUse(e.RawLine)` / `ParseToolResult(e.RawLine)`).

### Type

```go
type ToolUse struct {
    ID    string         `json:"id"`
    Name  string         `json:"name"`
    Input map[string]any `json:"input"`
}
```

- `ID` — the content block's own `tool_use` id (e.g. `toolu_…`), read from `c.Raw["id"]`.
- `Name` — the tool name (e.g. `Bash`, `Read`), read from `c.Raw["name"]`.
- `Input` — the tool's arbitrary input object, read from `c.Raw["input"]`. `map[string]any` preserves the wire shape generically (input shape varies per tool: `Bash`→`command`, `Read`→`file_path`, …) "exactly as found on the wire." `Raw["input"]` is *already* `map[string]any` from `parseEntry`, so no re-decode — same posture `ParseAskUserQuestion` uses internally for `input`.

Doc-comment the type in the `AskUserQuestion` / `AssistantText` style, including the "Safe to call on a zero value — never panics" guarantee.

### Function contract

```go
// ParseToolUse projects the first tool_use content block of an
// assistant-envelope JSONL line (snap) into a *ToolUse. Returns nil
// on no-match. Reuses parseEntry so envelope decoding stays single-sourced.
func ParseToolUse(snap []byte) *ToolUse
```

Behaviour (mirror `ParseAskUserQuestion`'s body, minus the name gate and the empty-payload gate):

1. `entry, ok := parseEntry(snap)`. Return `nil` if `!ok` (not valid JSON / not a JSON object) OR `entry.Type != "assistant"` OR `entry.Message == nil`.
2. Walk `entry.Message.Content` in arrival order. Skip blocks where `c.Type != "tool_use"`.
3. **First** `tool_use` block wins (later blocks ignored — matches `ParseAskUserQuestion`). On match, project and return non-nil:
   - `ID, _   = c.Raw["id"].(string)`
   - `Name, _ = c.Raw["name"].(string)`
   - `Input, _ = c.Raw["input"].(map[string]any)`
4. If no `tool_use` block is found after the walk, return `nil`.

**Match gate is `type == "tool_use"` alone.** Unlike `ParseAskUserQuestion` (which returns nil when its `input.questions` is absent/empty), `ParseToolUse` does **not** gate on any field being present. A `tool_use` block with a missing/non-string `name`, missing `id`, or missing `input` still returns a **non-nil** `*ToolUse` with those fields at their zero values (`""`, `""`, `nil`). The presence of `type:"tool_use"` is the match signal; field absences are zero-value-on-mismatch per AC #4. This is the one deliberate divergence from `ParseAskUserQuestion` — call it out in the doc comment so a future reader doesn't "fix" it into a gate.

### Data flow

```
snap []byte ──parseEntry──▶ JSONLEntry ──content walk──▶ first tool_use block
                  │                                            │
              nil if not                              ToolUse{ID,Name,Input}
              assistant/JSON                          from c.Raw[...]  (or nil if none)
```

## Concurrency model

None. `ParseToolUse` is a pure function: no I/O, no shared state, no goroutines. Safe for concurrent use, like `AssistantText` / `ParseAskUserQuestion`.

## Error handling

No `error` return — nil-on-no-match is the entire failure surface, matching `ParseAskUserQuestion` ("ceremony without information gain"). Failure modes and their results:

| Input | Result |
|-------|--------|
| `nil` / empty / non-JSON / JSON non-object | `nil` (via `parseEntry` ok=false) |
| Non-assistant envelope (`user`, `system`, …) | `nil` |
| Assistant envelope, `Message == nil` | `nil` |
| Assistant envelope, no `tool_use` block (only `text` / `thinking`) | `nil` |
| Assistant envelope with a `tool_use` block | non-nil `*ToolUse` (fields zero-value-on-mismatch) |
| `tool_use` block, individual field missing / type-mismatched | non-nil; that field zero-valued; never panics |

## Testing strategy

New file `pkg/tuidriver/tool_use_test.go`, mirroring `ask_user_test.go`. RED→GREEN. Two tests:

**1. Nil / no-match table** (mirror `TestParseAskUserQuestionReturnsNilOnNon`) — each case asserts `ParseToolUse(snap) == nil`:
- `nil`; empty `[]byte{}`; garbage (`not json`); JSON non-object (`[1,2,3]`)
- user envelope (`{"type":"user",…}`)
- assistant plain text (content is a single `text` block — no `tool_use`)
- assistant with only a `thinking` block — no `tool_use`

**2. Match + zero-value scenarios** — describe inputs inline as JSON literals (no fixture needed), assert projected fields:
- assistant + a `Bash` `tool_use` block (`id`,`name`,`input.command`) → non-nil; `Name=="Bash"`, `ID==`the id, `Input["command"]==`the command string
- assistant + `tool_use` missing `name` → non-nil; `Name==""`
- assistant + `tool_use` missing `id` → non-nil; `ID==""`
- assistant + `tool_use` missing `input` → non-nil; `Input==nil`
- assistant + `tool_use` with non-string `name` (e.g. `"name":42`) → non-nil; `Name==""` (never panics — the type-mismatch case)
- assistant + `[text block, tool_use block]` (text first) → finds the `tool_use`; and `[tool_use, tool_use]` → **first** wins (assert by id)

**3. Real-fixture test** (mirror `TestParseAskUserQuestionRealFixture`) — read `testdata/tool-use-snapshot.bin`, substring-guard the wire markers (`"type":"tool_use"`, `"name":`, `"id":`), then assert `ParseToolUse` returns non-nil with `Name` non-empty, `ID` non-empty, `Input` non-nil and carrying the tool's expected key (e.g. `Input["command"]` non-empty for a `Bash` fixture).

### Fixtures

Two new files, mirroring the `ask-user-question-snapshot.*` pair:
- `pkg/tuidriver/testdata/tool-use-snapshot.bin` — one raw JSONL **assistant** envelope line carrying a `tool_use` block. Wire shape (from `jsonl-layout.md` § "Tool-use shapes"):
  `{"type":"assistant","message":{"id":"msg_…","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_…","name":"Bash","input":{"command":"ls /tmp"}}]}}`
- `pkg/tuidriver/testdata/tool-use-snapshot.json` — human-readable expected projection (documentary, as `ask-user-question-snapshot.json` is): `{"id":"toolu_…","name":"Bash","input":{"command":"ls /tmp"}}`

**Sourcing the `.bin`:** prefer capturing a real line from a live session — driving `claude` with a prompt that triggers a tool call (`list the files in /tmp` produced a `Bash` `tool_use` in ticket #9 run 7; an existing `cmd/spike-*` binary can record it). If a live `claude` is impractical in the dev environment, hand-craft the line from the documented wire shape above — the test's structural assertions (substring guards + field presence) don't depend on a real session's exact values, only on the `tool_use` shape being well-formed. Use a non-`AskUserQuestion` tool (`Bash`/`Read`) so the fixture proves name-agnostic projection. (Aside: the existing `ask-user-question-snapshot.bin` is itself a `tool_use` block and `ParseToolUse` will project it too — a fine optional secondary assertion of name-agnosticism, but the primary fixture should be a `Bash`/`Read` tool to represent the `ToolStart` use case.)

## Open questions

- **Should `Input` ever be normalised (e.g. drop to `json.RawMessage`)?** No — `map[string]any` is what `parseEntry` already yields and matches the "exactly as found on the wire" AC. Deferred unless a consumer surfaces a concrete need for byte-fidelity of `input` (none observed; this is the evidence-based-fix posture).
- **Multiple `tool_use` blocks in one line.** Empirically one JSONL line = one content block (`jsonl-layout.md:69-71`), so first-match-wins is moot in practice but defines the contract for the rare multi-block line. If a real consumer needs *all* tool_use blocks from a single envelope, that's a separate `ParseToolUses`-plural follow-up, not this ticket — do not pre-build it.
- **Fixture provenance** (recorded vs. hand-crafted) is left to the developer per the environment; both satisfy the AC.

## Acceptance criteria mapping

- AC1 (projects name/id/input from `ContentBlock.Raw`) → `ToolUse` type + step 3 of the contract.
- AC2 (carries name/id/input exactly as on the wire) → real-fixture test #3 + Bash match scenario.
- AC3 (nil on no `tool_use` / malformed JSON / wrong envelope) → error-handling table + nil-cases test #1.
- AC4 (missing/type-mismatched fields → zero values, never panic; safe on zero value) → zero-value scenarios in test #2; `nil`-snap case in test #1.
- AC5 (RED→GREEN with the established fixture pattern) → `.bin`+`.json` pair + the two-test structure mirroring `ask_user_test.go`.

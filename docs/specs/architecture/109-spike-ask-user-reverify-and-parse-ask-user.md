# 109 — spike-ask-user: re-verify `--strict-mcp-config`; extract `ParseAskUserQuestion` if green

## Files to read first

- `cmd/spike-ask-user/main.go:295-312` — `extractAskUserQuestion` helper, the literal code being promoted to library API. Note JSONL shape (`message.content[]` → `tool_use` block with `name == "AskUserQuestion"` and `input.questions[]`).
- `cmd/spike-ask-user/main.go:51-68` — spike timeouts (`sessionFileWait = 10s`, `askUserQuestionLimit = 60s`). The original 2026-05-18 failure surfaced as `session JSONL did not appear at <path> within 60s`; the current code uses `sessionFileWait` (10s) for that, so reproduction will surface as `open session jsonl: ... did not appear: context deadline exceeded` after ~10s rather than 60s — same root cause (`--strict-mcp-config` blocks JSONL creation), different message. Treat either timeout fingerprint as a reproduction. Document the actual fingerprint in the PR.
- `pkg/tuidriver/picker.go:159-237` — `ParsePicker` shape: doc comment first, public function, returns nil when no match. Mirror this header style and "returns nil if no match" semantic.
- `pkg/tuidriver/picker_test.go:31-113` — `TestParsePickerReturnsNilOnNonPicker` + `TestParsePickerRealFixture`. Mirror the two-test shape: nil-on-non-input, then real-fixture assertions on key parsed fields.
- `pkg/tuidriver/modal.go:21-31` — `ModalClass` constants. `ModalClassAskUserQuestion` already exists; the AC explicitly says **do not add a new ModalClass constant**.
- `pkg/tuidriver/jsonl.go:108-272` — existing `JSONLEntry` / `EntryMessage` / `ContentBlock` types and `parseEntry` / `parseMessage` / `parseContentBlock` helpers. The new parser reuses `parseEntry` so JSON-deserialization stays in one place.
- `pkg/tuidriver/testdata/picker-snapshot.json` (entire file) — example of the parsed-shape sidecar fixture format (committed as human-readable expectation alongside the `.bin`).
- `pkg/tuidriver/agents.go:11-22` — type-doc style for an exported library type with nested struct + `json` tags on fields. Mirror this for `AskUserQuestion` and its nested types.
- `pkg/tuidriver/cwd.go` (skim) — `EncodeCwd` resolution if the developer needs to locate a captured session JSONL on disk during AC1.

## Context

Two work items on one ticket, gated by a re-verification:

1. **AC1 (always, first):** confirm whether the 2026-05-18 `--strict-mcp-config` JSONL-never-appears block still reproduces against claude 2.1.150. Three runs from a fresh cwd, mirroring the original repro conditions.
2. **AC2 (only if AC1 is 3/3 green):** extract the JSONL `AskUserQuestion` tool_use parser currently sitting locally in `cmd/spike-ask-user/main.go` into the public `pkg/tuidriver/` API. The `ModalClassAskUserQuestion` PTY-side detector already exists — this work is the JSONL-side counterpart.
3. **AC3 (only if AC1 fails):** open a focused investigation ticket capturing the current reproduction and close this one as superseded. No mechanism investigation here.

The re-verification matters because PR #77 ("byte-by-byte prompt submit per #71") changed prompt submission shape and may have incidentally resolved the original block; alternatively, claude version drift (2.1.144 → 2.1.150) may have masked or shifted the failure. Either way, three runs from a fresh cwd settles it.

## Design

### AC1 — re-verification (no code)

From a fresh cwd **outside this repo** (e.g. `mktemp -d`), run three times:

```
cd $(mktemp -d)
TUIDRIVER_STRICT_MCP_CONFIG=1 go run github.com/pyrycode/tui-driver/cmd/spike-ask-user -trust-folder=accept
```

Record for each run: success (saw `OBSERVED:` stdout line) or failure (specifically the JSONL-never-appears fingerprint — at `cmd/spike-ask-user/main.go:188-192` `session jsonl ... did not appear: ...`, or any timeout further down the flow). Also capture `claude --version`. PR description gates the next phase:

- **3/3 green** → execute AC2 in this same branch.
- **≥1/3 red** with the JSONL-never-appears fingerprint → execute AC3.

Acceptance criterion is exactly three runs (not "until stable"). An intermittent failure is a failure for routing purposes.

### AC2 (green path only) — library API

#### New file: `pkg/tuidriver/ask_user.go`

Public types — mirror `agents.go:11-22` style (doc-comment-then-struct, `json` tags on every field, nested types defined immediately below the parent):

```go
type AskUserQuestion struct {
    Questions []AskUserQuestionItem `json:"questions"`
}

type AskUserQuestionItem struct {
    Question    string                  `json:"question"`
    Header      string                  `json:"header,omitempty"`
    MultiSelect bool                    `json:"multi_select"`
    Options     []AskUserQuestionOption `json:"options"`
}

type AskUserQuestionOption struct {
    Label       string `json:"label"`
    Description string `json:"description,omitempty"`
}
```

Naming rationale: top-level type matches the tool name (`AskUserQuestion`) per the existing `ModalClassAskUserQuestion` precedent. The per-question element is named `AskUserQuestionItem` (not `Question` — too generic at package scope) and the per-option element is `AskUserQuestionOption`. `MultiSelect` is `bool` (the JSONL emits a JSON boolean; absent → `false`, matching the type's zero value).

Public function — mirror `picker.go:159-237` shape:

```go
// ParseAskUserQuestion extracts the AskUserQuestion tool_use shape from a
// single JSONL assistant-envelope line. ... [full doc comment per spec
// guidance below]
func ParseAskUserQuestion(snap []byte) *AskUserQuestion
```

**Behaviour contract** (documented in the doc comment; asserted by tests):

- `snap` is the bytes of a single JSONL assistant-envelope line (one JSON object).
- The function delegates JSON deserialization to the existing `parseEntry` (or its internals) so envelope/message/content parsing stays single-sourced.
- Walks `message.content[]` for the **first** block where `type == "tool_use"` AND `name == "AskUserQuestion"`.
- Reads that block's `input.questions[]` and projects each element into `AskUserQuestionItem` / `AskUserQuestionOption` via field-by-field type assertions (zero values on missing/mismatched fields, matching the library's `AssistantUsage` / `parseContentBlock` posture).
- Returns `nil` on any of: not valid JSON, not an assistant envelope, no matching `tool_use` block, `input.questions` absent or empty, malformed `questions` element type.
- Returns a non-nil `*AskUserQuestion` with `len(Questions) >= 1` on success.

**Internal helper reuse.** The existing `parseEntry(line []byte) (JSONLEntry, bool)` already does the envelope decode, the `message` projection, and the `content[]` walk producing `ContentBlock` values whose `Raw` map is preserved. `ParseAskUserQuestion` can build on it: call `parseEntry`, check `Type == "assistant"`, scan `Message.Content` for `Type == "tool_use"` with `Raw["name"] == "AskUserQuestion"`, then dig into `Raw["input"].(map[string]any)["questions"].([]any)` for the projection. Do not re-implement envelope parsing.

**Why bytes, not `JSONLEntry`.** The AC pins `(snap []byte)`. The natural consumer (the spike, and `pyry acp` later) already has bytes in hand from `JSONLEntry.RawLine` or from a captured fixture. Taking bytes keeps the API symmetrical with `ParsePicker(snap []byte)` and makes the snapshot-fixture pattern (`.bin` + `.json`) work uniformly. An entry-flavored seam (`ParseAskUserQuestionEntry(JSONLEntry)`) is an XS follow-up if a consumer surfaces the need — out of scope here.

#### New file: `pkg/tuidriver/ask_user_test.go`

Two tests, mirroring `picker_test.go:31-113`:

- **`TestParseAskUserQuestionReturnsNilOnNon`** — table-driven nil-input cases:
  - nil snap → nil
  - empty snap → nil
  - non-JSON garbage bytes → nil
  - a valid JSONL line that is NOT an assistant envelope (e.g. `{"type":"user","message":{...}}`) → nil
  - an assistant envelope with no `tool_use` blocks (e.g. plain text reply) → nil
  - an assistant envelope with a `tool_use` block whose `name` is something other than `AskUserQuestion` → nil

- **`TestParseAskUserQuestionRealFixture`** — reads `testdata/ask-user-question-snapshot.bin`, calls `ParseAskUserQuestion`, asserts:
  - return value is non-nil
  - `len(Questions) >= 1`
  - `Questions[0].Question` is non-empty and matches the captured prompt's question text (e.g. contains `"language"` if the fixture comes from the `defaultPrompt` in `spike-ask-user/main.go:63`)
  - `len(Questions[0].Options) >= 2` (the spike prompt asks for three: Rust, Zig, Go)
  - `Questions[0].Options[i].Label` is non-empty for every option
  - `Questions[0].MultiSelect` matches the captured value (the default prompt produces a single-select; assert explicitly so a fixture re-record with a different prompt forces the assertion to be re-examined)

Assertions follow `picker_test.go`'s structural style (assert key properties, not full DeepEqual against the JSON sidecar). The JSON sidecar (see below) is the human-readable reference; tests assert specific invariants that prove parser correctness without coupling to incidental fixture-content drift.

#### New fixture files

- `pkg/tuidriver/testdata/ask-user-question-snapshot.bin` — the raw bytes of one JSONL line: the assistant envelope that carried the `AskUserQuestion` tool_use during AC1. Capture procedure:
  1. Pick one of the three (now-passing) AC1 runs. Run it with the JSONL path logged at startup (`session-id-resolved id=... jsonl=...`).
  2. After the run completes, grep the captured JSONL for the tool_use line:
     ```
     grep '"name":"AskUserQuestion"' "$JSONL_PATH"
     ```
     Expect exactly one matching line per run.
  3. Write that single line (no trailing newline) to `pkg/tuidriver/testdata/ask-user-question-snapshot.bin`. The `.bin` extension follows the existing `picker-snapshot.bin` / `agents-snapshot.bin` convention; the contents are textual JSON, but `.bin` keeps the testdata convention uniform.

- `pkg/tuidriver/testdata/ask-user-question-snapshot.json` — the **parsed shape** of the fixture for human reading. Following the `picker-snapshot.json` model (parsed-shape sidecar, not the raw input). Pretty-printed JSON matching the shape of the `AskUserQuestion` struct with its `json` tags. Example:

  ```json
  {
    "questions": [
      {
        "question": "Which programming language should I learn next?",
        "header": "Language",
        "multi_select": false,
        "options": [
          { "label": "Rust", "description": "..." },
          { "label": "Zig",  "description": "..." },
          { "label": "Go",   "description": "..." }
        ]
      }
    ]
  }
  ```

  Generate the file by running the parser on the `.bin` and `json.MarshalIndent`-ing the result, then committing the output. No test currently reads this file; it is documentation. (Picker's sidecar is used the same way — committed, not asserted against — per PR #81's parsed-shape posture: tests assert structure, the JSON sidecar is human-readable reference.)

### AC3 (red path only) — supersede + close

If any of the three AC1 runs reproduces the JSONL-never-appears timeout fingerprint:

1. Skip AC2 entirely. Do not add `pkg/tuidriver/ask_user.go`, the test, or the fixtures.
2. Open a new tui-driver issue. Title suggestion: `spike-ask-user: --strict-mcp-config still blocks JSONL creation (<X>/3 reproduced against claude 2.1.150)`. Body must include:
   - `claude --version` output
   - Fresh-cwd repro steps (the exact commands from AC1)
   - Observed stderr fingerprint(s) and which spike phase they tripped at
   - Failure count (`X/3 failed`)
   - Note: PR #77's prompt-submit change did NOT resolve the original block (link to PR #77, link to the original 2026-05-18 finding in the vault open-questions doc).
3. Comment on this ticket linking the new issue.
4. Close this ticket via PR or GitHub-CLI as superseded by the new issue. Do not mark `done:architect` until AC3 + AC4 are both done.

Mechanism investigation (why `--strict-mcp-config` blocks JSONL creation) is explicitly out of scope for this ticket; it lives in the new investigation issue.

### AC4 (both paths) — closing comment

Leave one comment on #109 per the AC's templates:

- Green path: `spike re-verified clean (3/3); extraction landed in PR #<N>`
- Red path: `spike still broken (<X>/3 failed); investigation continues in #<N>`

The human syncs the vault open-questions doc from this comment; no doc edits in this ticket.

## Concurrency model

None — the parser is pure (`[]byte` → `*AskUserQuestion`). The spike binary's existing goroutine fan-out (watchdog, JSONL tail) is untouched.

## Error handling

The parser does not return `error`. Failure modes — malformed JSON, wrong envelope type, no matching `tool_use`, malformed `questions` shape — collapse to `nil`. This matches `ParsePicker`'s posture: parsers are best-effort projections that return `nil` on no-parse, and callers test `if got := ParseAskUserQuestion(snap); got != nil` rather than handling typed errors.

Justification: the only consumer pattern is "given some bytes I think contain an AskUserQuestion, hand me the structure if present." There is no recovery path that distinguishes "bytes were corrupt" from "bytes were a different tool_use" — both collapse to "this isn't an AskUserQuestion event." A typed error API would be ceremony without information gain.

## Testing strategy

Covered above in the test-file sketch. Two test functions, structural assertions, real fixture captured during AC1. No e2e test is added for the parser (the spike binary already exercises the JSONL path end-to-end as a side effect of AC1).

## Open questions

- **Fixture re-recording cadence.** If a future claude version changes the `AskUserQuestion` JSONL shape (e.g. renames `multiSelect` → `multi_select`, adds a new option field), `TestParseAskUserQuestionRealFixture` should fail loudly. No automatic re-record machinery; treat the failure the same way picker-fixture drift is treated (manual re-record + version-lock bump). No action this ticket.
- **`AskUserQuestionItem` naming.** Picked because `Question` at package scope is too generic. If the consumer-facing name reads awkwardly when used (`tuidriver.AskUserQuestionItem`), a future refactor can rename — exported names are not load-bearing on disk shape (no `json` tag references the Go name). No action this ticket.

## Split proposal

Not needed. Production-source files: 1 (`pkg/tuidriver/ask_user.go`). LOC estimate: ~50 production + ~50 test. Exported types: 3 (`AskUserQuestion`, `AskUserQuestionItem`, `AskUserQuestionOption`). No new exported function beyond `ParseAskUserQuestion`. No consumer cascade — `cmd/spike-ask-user/main.go`'s `extractAskUserQuestion` is local-only and stays as-is (the ticket scope is library extraction, not spike migration; spike cleanup belongs in a separate ticket if desired). Red lines all clear by wide margins.

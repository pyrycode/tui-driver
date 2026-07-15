# JSONL layout (claude session logs)

Where `claude` writes its session JSONL and what the records look like, as observed against locally-installed `claude` 2.1.x on macOS during ticket [#1](../codebase/1.md).

> These facts contradict the original ticket body and parts of the vault design doc. The empirical version (this file + [#1 spec](../../specs/architecture/1-spike-one-turn.md) § *Empirical findings already captured*) is the authority. Re-verify against new `claude` versions before relying on it.

## Directory layout

```
~/.claude/projects/<encoded-cwd>/<session-id>.jsonl
```

- **Flat.** No `sessions/` subdirectory. (Vault design doc and ticket body both implied a `sessions/` subdir — wrong.)
- **`<encoded-cwd>` maps every character outside `[a-zA-Z0-9]` to a hyphen**, not a reversible encoding. `claude` applies JS `.replace(/[^a-zA-Z0-9]/g,'-')` semantics — **one hyphen per UTF-16 code unit**: a BMP character (`/`, `.`, `_`, space, `ö`) → one hyphen; an astral character (`😀`, a surrogate pair) → two hyphens. Adjacent specials therefore produce adjacent hyphens (`/.` → `--`). The encoding is **not reversible** — never try to recover the cwd from the directory name.
  - The canonical implementation is `tuidriver.EncodeCwd` (`pkg/tuidriver/cwd.go`) — it resolves the path to its on-disk canonical form (symlinks resolved, case canonicalised) first, then applies the per-UTF-16-unit map. Route through it, or through `SessionJSONLPath`, which wraps it; never re-implement the transform. The per-UTF-16 rule was **observed against real `claude` 2.1.199** ([#206](../codebase/206.md)) and shipped in [#207](../codebase/207.md), replacing an earlier per-*byte* transform that emitted 2 hyphens for a 2-byte char like `ö` — a projects-dir `claude` never writes. The #1-era "only `/` and `.` map" note below was a narrower observation of an ASCII-only cwd; the rule is every non-alnum character.
  - Compute at runtime from the resolved cwd; never hardcode.

Examples (the ASCII cwd is unaffected by the per-byte → per-UTF-16 change; the non-ASCII cwd is the case [#207](../codebase/207.md) fixed):
- `/Users/jhi/Workspace/Projects/.pyrycode-worktrees/architect-1` → `-Users-jhi-Workspace-Projects--pyrycode-worktrees-architect-1`
- cwd leaf `Työ😀` → `Ty---` (`ö` = 1 UTF-16 unit → `-`; `😀` = 2 units → `--`); the old per-byte transform produced the wrong `Ty------`.

## Discovering the session file

The robust mechanism is to **dictate the session ID up-front** via `claude --session-id <uuid>` (verified ticket [#7](../codebase/7.md)). The JSONL filename is then the deterministic `<uuid>.jsonl` in the encoded-cwd directory — no directory scan, no mtime heuristic, no `fsnotify`. The discovery strategy is:

1. **Before spawning `claude`**, resolve the session ID (generate a fresh UUIDv4 by default, or accept an operator-supplied one via a flag). Compute `jsonlPath = ~/.claude/projects/<encoded-cwd>/<uuid>.jsonl`.
2. Spawn `claude --session-id <uuid>`. Log the resolved id + path *before* `pty.Start` so an external operator can `tail -f <path>` or `claude --resume <uuid>` from another shell.
3. **After writing the prompt** (see *Empirical surprise* below), poll `os.Stat(jsonlPath)` every 100 ms with a ≥10 s timeout. On success the file is brand new; tail from offset 0.
4. The tailer's filter (`obj["message"]` is a map AND `obj["type"] == "assistant"`) silently skips claude's startup envelopes — same parser rule that handles non-`--session-id` runs.

Polling is sufficient — do NOT pull in `fsnotify` for this. Implemented in `cmd/spike-one-turn/main.go` (`resolveSession`, `openSessionJSONL`, `tailJSONL`).

> **Canonical library API (post-[#58](../codebase/58.md), extended in [#59](../codebase/59.md), augmented in [#103](../codebase/103.md), baseline-offset ownership inverted in [#290](../codebase/290.md), mid-tail rotation/truncation recovery added in [#291](../codebase/291.md)).** Steps 1 and 3 above are now `tuidriver.SessionJSONLPath(home, cwd, sessionID)` (pure path composition, wraps `EncodeCwd` so no consumer can re-implement the byte transform) and `tuidriver.WaitForSessionJSONL(ctx, path)` (`os.Stat` poll at `DefaultPollInterval`, returns `nil` on appearance, wraps `context.Cause(ctx)` with the path in the message on cancellation/deadline). Step 4 — the tail loop itself — is `tuidriver.TailJSONL(ctx, path, startOffset) (<-chan JSONLEntry, error)`: open at offset, `bufio.Reader.ReadBytes('\n')`, accumulate partial bytes across short reads, JSON-decode each completed line into a typed `JSONLEntry` (with nested `EntryMessage` and `ContentBlock` carrying `Type` + `Raw` for fields outside the typed view, **plus a `RawLine []byte` field since #103 carrying the verbatim source-line bytes the goroutine read with the trailing `\r\n` stripped — for consumers that re-emit JSONL byte-for-byte (`json.Marshal(Raw)` is NOT a round-trip: Go's encoder sorts map keys alphabetically and normalises whitespace; `RawLine` preserves both, defensively `bytes.Clone`'d at the parse site because `line` aliases the rolling `partial` buffer)**), emit on a buffered channel (cap 32), EOF-sleep one tick then retry, silently drop malformed lines, close the channel on ctx cancellation. **Since [#290](../codebase/290.md), `startOffset` is resolved against the tail's OWN open fd, not a caller-precomputed offset measured against a foreign `os.Stat`** — `TailJSONL` opens the file, stats that same fd, and derives the actual seek target: `0` starts from the beginning; the new exported sentinel `tuidriver.TailFromEnd` (`-1`) starts at the current end of the tail's own fd ("skip existing content" without the caller measuring size itself); a positive offset is **clamped** to the own-fd size (an oversized/stale offset lands at the end, never past content — a raw `Seek` past EOF does not error on a regular file, which is what made the old caller-measured offset silently land the reader nowhere); any other negative offset is a synchronous error. This closes the cross-fd TOCTOU that was the root of the pyrycode #929/#930 tail-offset class — the stat↔seek window is same-fd and therefore not a TOCTOU itself (an append-only file can only grow under a held fd). The signature is unchanged (`int64`), so `Events` and all pre-#290 tests kept compiling with zero edits. **Since [#291](../codebase/291.md), the tail loop also recovers from two mid-tail failure modes**, checked at each EOF poll: *rotation* (the path is replaced by a new inode — a log rotation or a `/clear`-style session-file swap; detected via device/inode identity, since a same-byte-length replacement is invisible to a size check) reopens the path fresh and reads the new generation from offset 0; *truncation* (the file is rewritten shorter under the same inode) reseeks the existing fd to offset 0 and re-reads. Both recovery paths re-sync to offset 0, so a consumer may re-observe entries from the top of the new/rewritten generation — genuinely new bytes, not duplicates of already-consumed content. Recovery is best-effort (a stat/open/seek error or rotation-window race parks and retries the next poll tick, never closing the channel with an error) and inode identity is Unix-only, so rotation detection is a documented no-op on Windows (truncation detection, size-only, still works there). This closes the mid-tail half of the pyrycode #929/#930 tail-offset class that #290 left as the sibling's scope. The library version **emits every envelope type** — the assistant-only filter the spike used (`type == "assistant"` AND `message` is a map) does NOT apply at the library boundary because cancellation markers are `user`-role and attachment markers are `attachment`-type; consumers filter per-use. The 7 in-tree spike/probe binaries and pyrycode's downstream consumers still inline these shapes; migration is tracked separately ([pyrycode/pyrycode#501](https://github.com/pyrycode/pyrycode/issues/501) for the cross-repo consumer). New consumers should use the library functions; do not hand-roll the path composition or the tail loop.

### Why the deterministic path

"Newest `*.jsonl` by mtime" is fragile in two distinct ways:

- Stale `.jsonl` files left in the cwd by prior runs make `max(ModTime)` return *the wrong existing file* when called before claude has written to the new one — the spike then tails an inert byte range and the watchdog eventually trips. Empirically observed in ticket #7 Run 4 with ≥5 stale files accumulated.
- Even on a quiet cwd, the heuristic depends on claude touching its log "right now" — a thin race window that the #3 1-second retry papered over but did not eliminate.

The hierarchy for "find the active file" is therefore: (1) dictate the identifier when the tool exposes a flag for it (preferred); (2) snapshot-before / diff-after (no flag available); (3) newest-by-mtime + retry (only safe when no stale data is plausible).

### Empirical surprise: `--session-id` defers JSONL creation until first input

Plain `claude` writes `permission-mode` and `file-history-snapshot` into the JSONL during its boot sequence (observed in ticket [#3](../codebase/3.md)). `claude --session-id <uuid>` does **not** — `os.Stat(jsonlPath)` fails with `IsNotExist` at the moment `❯` (idle) first renders. The file appears only after the user's first input is received and claude begins processing it. Confirmed in ticket #7 Runs 5 and 6 by the ~200 ms gap between `prompt-written` and `session-jsonl-opened`.

Implementation consequence: the stat poll runs **after `prompt-written`**, not after `idle-detected`. Tailing starts at offset 0 because the file is brand new. Whether the deferral is intentional in v2.1.143 or an accident of the `--session-id` path is unknown; either way, consumers driving claude via this mechanism must not assume the file exists at idle.

> **Historical note.** Ticket #1's spike snapshotted the directory and waited for a *new* file to appear post-prompt; ticket #3 replaced that with newest-by-mtime + offset-tail; ticket #7 replaced *that* with the deterministic-path model above. Each predecessor is described in its own per-ticket notes for context.

## Turn lifecycle in JSONL

Each line is one JSON object. The **interactive** `claude` mode does NOT emit a `type:"result"` line (despite what the ticket body said — that's batch-mode behaviour). The turn-terminator in interactive mode is `type=="assistant"` with `message.stop_reason=="end_turn"`. Example:

```json
{
  "type": "assistant",
  "sessionId": "...",
  "message": {
    "id": "msg_01W3f…",
    "stop_reason": "end_turn",
    "content": [{"type": "text", "text": "..."}]
  }
}
```

### One Anthropic message ⇒ N JSONL lines (msg_id grouping)

A single assistant message is serialised as **one JSONL line per content block**, not one line per message. All lines share the same `message.id` and the same `stop_reason`. Verified in ticket [#9](../codebase/9.md) against three probe prompts: a slow-thinking response splits into two `assistant` lines with the same `msg_id`, both carrying `stop_reason=end_turn` — line 1's `content[]` holds the `thinking` block, line 2's holds the `text` block.

Implications for content extraction:

- **"First `end_turn` line carries the full answer" is wrong.** A line whose only content block is `thinking` will appear with `stop_reason=end_turn` and an empty `text` slot. The spike-one-turn `extractAssistantText` (single-record concatenate) silently returned `""` for these.
- **The correct rule is msg_id grouping.** Identify the `msg_id` of the **latest** assistant line with `stop_reason=end_turn`; collect every assistant event whose `.message.id == that_id`; walk their `content[]` in JSONL arrival order; concatenate blocks with `type == "text"`. Skip `thinking` and `tool_use` blocks. Implemented in `cmd/spike-multi-turn/main.go` as `extractByMsgID`.
- **`stop_reason` lives on every delta line of a message**, not only the last one. "First line with `stop_reason=end_turn`" is therefore not a reliable turn-complete signal even in the single-block case — it's reliable iff there is exactly one block. Use msg_id grouping unconditionally.

> **Canonical library API (post-[#60](../codebase/60.md), extended in [#104](../codebase/104.md)).** The per-entry half of the discriminator is `tuidriver.IsEndTurn(e JSONLEntry) bool` (Phase-A rule: assistant envelope AND `stop_reason == "end_turn"` AND combined `type:"text"` content non-empty) and `tuidriver.AssistantText(e JSONLEntry) string` (concatenation of `content[]` blocks where `Type == "text"`, in JSONL arrival order, reading each block's `Raw["text"]` zero-value-on-mismatch). Per-entry token-usage is `tuidriver.AssistantUsage(e JSONLEntry) *Usage` since #104 — returns a `*Usage` carrying the four Anthropic-API counters (`InputTokens`, `OutputTokens`, `CacheCreationInputTokens`, `CacheReadInputTokens`) read from `e.Message.Raw["usage"]`, or `nil` when the usage block is absent (distinguishing absence from "present, all zero" via the pointer); same zero-value-on-mismatch posture as `AssistantText` for malformed counter values. All three are total over `JSONLEntry`, including the zero value and `Message == nil`; all three are pure (no I/O, no state) and safe for concurrent use. `IsEndTurn` returns `true` for the **single JSONL line** that carries the (non-empty) text block of a turn — the cross-line aggregation that walks every assistant event sharing a `msg_id` and concatenates their text blocks across lines (`extractByMsgID` in `cmd/spike-multi-turn/main.go`) remains a consumer-side concern; the per-entry primitives compose, the cross-line shape has design tradeoffs (memory bounds on long turns, replay semantics on resume, msg_id selection rule) that vary per consumer and will be promoted separately when a second consumer surfaces the same shape. The text-non-empty half of `IsEndTurn` is what rejects the `thinking`-only or `tool_use`-only per-block delta lines that also carry `stop_reason=end_turn` (see § 76 above) — the spike binaries' `isEndTurn` check stop_reason alone, which is the bug this slice fixes at source. New consumers should call `tuidriver.IsEndTurn` / `tuidriver.AssistantText`; the seven in-tree spike/probe `isEndTurn` / `extractAssistantText` open-codes are tracked for migration separately.

### Tool-use shapes (observed)

Tool-use turns produce **multiple `assistant` messages with different `msg_id`s**, interleaved with `user(tool_result)` events that the assistant-only parser filter drops automatically. Empirically observed for the prompt `list the files in /tmp` (ticket #9, run 7):

| Idx | type | stop_reason | msg_id | content |
|-----|------|-------------|--------|---------|
| 13 | assistant | `tool_use` | `msg_011JC…` | `tool_use` block (Bash call) |
| 14 | user | — | — | `tool_result` (filtered out by tailer) |
| 18 | assistant | `end_turn` | `msg_01YEr…` | `text` block (final answer) |

The msg_id grouping rule handles this correctly because it keys on the **latest** `end_turn` msg_id (line 18); the earlier `tool_use`-tagged msg_id is inert noise. Heavier tasks may produce many tool_use lines under a single msg_id with interleaved tool_results — the grouping rule still applies; only the `end_turn` msg_id's content is extracted.

> **Per-tool extractors (the `Parse<Tool>` family).** Where a specific tool-call shape has a stable wire contract worth a typed projection, the library exposes a `Parse<Tool>(snap []byte) *<Tool>` pure-projection function alongside the per-entry primitives. All members take the bytes of one JSONL envelope, reuse the per-entry `parseEntry` decoder so envelope/message/content parsing stays single-sourced, walk `message.content[]` for a matching content block, and follow the nil-on-no-match / no-`error`-return / `.bin`+`.json`-fixture-pair posture (post-[#81](../codebase/81.md) parsed-shape model). Three members (the first two gate on the **assistant** envelope and a `tool_use` block; the third gates on the **user** envelope and a `tool_result` block — see its bullet):
>
> - **`tuidriver.ParseAskUserQuestion`** since [#109](../codebase/109.md), in `pkg/tuidriver/ask_user.go` — walks for the first `tool_use` block **named `AskUserQuestion`**, projects `input.questions[]` into the typed `*AskUserQuestion` shape (three exported types: `AskUserQuestion`, `AskUserQuestionItem`, `AskUserQuestionOption`; `MultiSelect` is `bool` tagged `json:"multiSelect"` per the empirical wire spelling — NOT snake_case). Returns `nil` on any non-parse (malformed JSON, non-assistant envelope, missing/empty `input.questions`, every-element malformed); returns non-nil with `len(Questions) >= 1` on success. Mirrors `ParsePicker`'s posture ("ceremony without information gain" — the only consumer pattern is "given some bytes I think contain X, hand me the structure if present"). The complementary PTY-side `ModalClassAskUserQuestion` detector (`pkg/tuidriver/modal.go`, [#13](../codebase/13.md)) is the rolling-buffer half of the same modal — JSONL-side projection and PTY-side classification are independent helpers consumers compose as needed.
> - **`tuidriver.ParseToolUse`** since [#140](../codebase/140.md), in `pkg/tuidriver/tool_use.go` — the name-agnostic projection. Walks for the **first `tool_use` block of any name** and projects its `id`/`name`/`input` off `ContentBlock.Raw` into `*ToolUse{ID, Name string; Input map[string]any}` (`Input` is the tool's arbitrary input object verbatim — `Bash`→`command`, `Read`→`file_path` — kept as a generic map, not a typed-per-tool union). It is the JSONL-robust source for a downstream `ToolStart` signal (pyrycode#608's daemon bridge), replacing the dead PTY spinner. **One deliberate divergence from `ParseAskUserQuestion`: the match gate is `type == "tool_use"` alone — not name-gated, and not payload-gated.** A `tool_use` block with a missing/non-string `name`, missing `id`, or missing `input` still returns a **non-nil** `*ToolUse` with those fields at their zero values (`""`/`""`/`nil`) — the presence of `type:"tool_use"` alone is the match signal; field absences are zero-value-on-mismatch (the `AssistantText`/`AssistantUsage` posture), never a no-match and never a panic. Returns `nil` only on non-JSON / non-object / non-assistant envelope / nil message / no `tool_use` block. The doc comment flags the missing gate explicitly so a future "consistency" pass doesn't re-converge it onto the name-gated sibling.
> - **`tuidriver.ParseToolResult`** since [#142](../codebase/142.md), in `pkg/tuidriver/tool_result.go` — the `tool_result` projection, the reply half of `ParseToolUse`. Walks for the **first `tool_result` block** and projects its `tool_use_id`/`is_error`/`content` off `ContentBlock.Raw` into `*ToolResult{ToolUseID string; IsError bool; Content any}`. It is the JSONL-robust source for a downstream `ToolUpdate` signal (pyrycode#608's daemon bridge), `ToolUseID` joining a `ToolUpdate` back to its `ToolStart`. **The one structural divergence from the two siblings above: the envelope gate is `type == "user"`, NOT `"assistant"`** — `tool_result` blocks ride **user**-role envelopes (the observed `assistant(tool_use) → user(tool_result)` sequence; the assistant-only tailer filter drops them). No decoder change was needed — `parseEntry` already populates `Type`/`Message` for `user` envelopes (its `message` decode is envelope-agnostic), so the gate string is the only edit. Shares `ParseToolUse`'s non-field-gated posture (the match signal is `type:"tool_result"` alone; absent/type-mismatched fields fall to `""`/`false`/`nil`, never a no-match, never a panic). **`Content` is typed `any` deliberately**, because the wire shape is a union — a string for the common Bash/Read result, or an array of content blocks (`[{"type":"text","text":"…"}]`, the Anthropic structured shape) — read with a plain map index (no type assertion: nothing to assert against a union, absent key already yields `nil`) so it is preserved exactly as on the wire (string→`string`, array→`[]any`, absent→`nil`). Returns `nil` only on non-JSON / non-object / non-`user` envelope / nil message / no `tool_result` block — the last case is what rejects the `user(text)` cancellation marker (§ Cancellation signal below), which is also a `user` envelope but carries a `text` block, not a `tool_result` one: the content-type walk, not the envelope gate alone, is what makes it a no-match.
>
> A `ParseToolUses` / `ParseToolResults`-plural for multiple blocks in one envelope is a deferred follow-up for either member (empirically one line = one content block, so first-match-wins is moot in practice).

### Turn-complete = JSONL `end_turn` ∧ PTY idle

JSONL `end_turn` says "model done speaking"; the PTY's `❯` glyph says "TUI ready to accept input." Consumers driving multi-turn sessions must wait for both before writing the next prompt — `cmd/spike-multi-turn/main.go` does this with a 250 ms `idleStableWindow` debounce on the `isIdle` half to absorb transient redraw observations.

### Cancellation signal: `user(text "[Request interrupted by user]")`

When the operator interrupts claude mid-response (ESC keystroke; see [system overview § Key signals](system-overview.md)), the JSONL records cancellation as a **`user`-role event** with a single `text` content block carrying the literal string `"[Request interrupted by user]"`. Verified in ticket [#11](../codebase/11.md). Important properties:

- The **cancelled assistant message keeps its pre-cancel `stop_reason`** (e.g. `tool_use` for a cancel-during-tool-use). No `stop_reason=canceled` / `null` / `max_tokens` ever surfaces — the cancel is not a new value of the existing field; it's a new event downstream of the cancelled message.
- For a cancel-during-tool-use, the marker lands **after** any `tool_result` events for in-flight tools. Tool subprocesses are NOT killed by ESC — they run to completion and emit their `tool_result` before the cancellation marker arrives. The sequence on disk is `assistant(stop_reason=tool_use) → user(tool_result) → user(text "[Request interrupted by user]")`. No follow-up assistant line resolves the cancelled `msg_id`.
- The marker acts as a **hard turn boundary**. The next user prompt is treated as a clean new turn by claude — fresh `msg_id`, normal `stop_reason=end_turn`, no continuation of the cancelled work. The same `--session-id` is fully recoverable.

> **The assistant-only tailer filter (`type=="assistant"`) drops this marker.** The marker has `type=="user"`. Consumers needing JSONL-side cancel acknowledgment must either widen the filter to also pass `user(text)` events upstream, or rely on PTY-side detection (the `❯-reappeared` predicate; see [system overview § Key signals](system-overview.md) for the PTY-quiescence form spike #11 uses). Both are reasonable — the post-spike library should likely surface the JSONL marker as the precise signal and PTY quiescence as a backstop.

### Permission modals (zero JSONL footprint)

When claude wants to invoke a tool that requires permission, it raises a permission-prompt modal on the PTY side (`Bash command … Do you want to proceed?` / `Read file … Do you want to proceed?` with a numbered option list). The modal has **no JSONL footprint at all**. Verified in ticket [#13](../codebase/13.md). Specifically, across observed Probe 1 runs with a 5 s observation window starting at `modal-detected`:

- No new envelope `type` value appears.
- No new `stop_reason` value appears on any existing envelope.
- No `assistant` envelope is written while the modal is up — `assistant(stop_reason=tool_use)` is **deferred until after the modal is approved or cancelled**.
- The only JSONL traffic during the modal-open window is claude's own boot envelopes (`permission-mode`, `file-history-snapshot`, the spike's prompt write as `user`, and `attachment` metadata).

Post-approve, the JSONL stream resumes with the same shape as a non-modal tool-use turn: `assistant(stop_reason=tool_use, thinking)` → `assistant(stop_reason=tool_use, tool_use)` → `user(tool_result)` → `assistant(stop_reason=end_turn, text)`. No new envelope types, no new `stop_reason` values, no acknowledgement of the modal itself. The modal is purely a PTY-side affordance.

Architectural consequence: **modal detection MUST be PTY-side, not JSONL-side.** Consumers cannot subscribe to a JSONL signal to learn that a permission modal is up; they need the rolling-buffer pattern-match predicate (see [system overview § Key signals](system-overview.md) — "Permission modal present" entry). This is structurally analogous to the cancellation signal above (lives in a `user(text)` event the assistant-only filter drops) — both modals and cancellation live on signal paths the existing JSONL tailer filter does not surface. The post-spike `pkg/tuidriver/` API should expose PTY-side state events as a first-class channel separate from the JSONL content stream.

## Observed top-level `type` values

Captured from a single 11-event session JSONL during ticket [#3](../codebase/3.md) and corroborated against a 27-line multi-turn run in ticket [#9](../codebase/9.md):

- `permission-mode` (claude startup; also fires once after each turn in multi-turn runs)
- `file-history-snapshot` (claude startup; also fires post-turn)
- `user` (the prompt the spike wrote, or tool_result events from claude-managed tool runs)
- `attachment` (claude metadata)
- `ai-title` (claude metadata)
- `assistant` (the response; carries `stop_reason` and `message.id`)
- `system` (claude metadata; fires once after every turn's `end_turn`)
- `last-prompt` (claude metadata; in multi-turn runs fires after turns 2+ specifically, not turn 1 — appears to correlate with multi-turn continuation rather than per-turn)

Of these, only `user` and `assistant` carry a `message` object; everything else is a claude-internal envelope with no `message` field. The set is likely to grow with new `claude` versions. (`queue-operation` was predicted by a pre-spike-#9 probe but did not appear in #9's runs; possibly suppressed by the `--permission-mode bypassPermissions` flag that #9 ran with.)

**Parser rule for consumers.** When using `tuidriver.TailJSONL` (the canonical library API), every envelope above flows through the channel — `e.Type` carries the envelope kind, `e.Message` is non-nil only for `assistant` / `user` envelopes (the two kinds that carry a `message` object), and `e.Raw` holds the unparsed map for fields outside the typed view. The classic "I only want the assistant's final answer" filter is one line on the consumer side: `if e.Type != "assistant" || e.Message == nil { continue }`. The tradeoff this resolves is that cancellation markers (`user`-role with `text "[Request interrupted by user]"`), `user(tool_result)` interleavings, and `attachment` metadata are also visible on disk; consumers needing those signals widen their filter rather than re-implementing the tail loop. The pre-#59 spikes inlined a tighter filter at the tailer (`obj["message"] is map[…] AND obj["type"] == "assistant"`) — that decision was right for spike-one-turn but wrong at the library boundary, since it dropped the cancel marker and the attachment marker the cancel/permission/probe spikes needed. See [#59 codebase notes](../codebase/59.md) for the full reasoning.

## Caveats

- **Path casing on case-insensitive filesystems (default macOS APFS, default Windows).** The same directory can be reached as `WorkSpace` or `Workspace` depending on how you got there, and the on-disk canonical casing is what `claude` writes into `~/.claude/projects/`. Consumers that route through `tuidriver.EncodeCwd` get this for free — since [#57](../codebase/57.md), `EncodeCwd` resolves to the on-disk canonical form (symlinks resolved, case canonicalised) before encoding, so the output matches the directory `claude` writes to. Consumers that build the encoded-cwd themselves must canonicalise the input first (open the path and `fcntl(F_GETPATH)` on darwin, or `filepath.EvalSymlinks` on case-sensitive Linux defaults where a differently-cased lookup naturally returns `ENOENT`).
- The shape above is the snapshot from one `claude` version (2.1.x). Future versions may rename fields or add new envelope types. Per [ADR-0001](../decisions/0001-hybrid-jsonl-tui.md), JSONL parsing is the **consumer's** responsibility, not tui-driver's — this file exists to inform consumers and to anchor the state-detection side that does need to know when a turn is done.

## Related

- [ADR-0001 — Hybrid JSONL + TUI](../decisions/0001-hybrid-jsonl-tui.md)
- [System overview](system-overview.md)
- Code: `pkg/tuidriver/jsonl.go` — `SessionJSONLPath` (canonical path composition; wraps `EncodeCwd`) + `WaitForSessionJSONL` (canonical `os.Stat`-poll appearance waiter) + `TailJSONL` (canonical tail loop returning a buffered channel of typed `JSONLEntry`; emits every envelope type — consumers filter; `JSONLEntry` carries `Type` / `Message` / `Raw` / `RawLine` — the last for byte-verbatim re-emit; `startOffset` resolved against the tail's own fd since #290, with the exported `TailFromEnd` sentinel) + `IsEndTurn` / `AssistantText` / `AssistantUsage` (canonical per-entry Phase-A discriminator, content walk, and typed token-usage accessor, all pure over `JSONLEntry`). Use these from new consumers — the spike helpers below are pre-extraction inlines that have not yet been migrated. See [#58](../codebase/58.md) for the path + appearance functions, [#59](../codebase/59.md) for the tail, [#60](../codebase/60.md) for the per-entry helpers, [#103](../codebase/103.md) for the `RawLine` byte-fidelity field, [#104](../codebase/104.md) for the typed `Usage` / `AssistantUsage` accessor, and [#290](../codebase/290.md) for the own-fd baseline-offset inversion.
- Code: `cmd/spike-one-turn/main.go` — `projectsDir`, `encodeCwd`, `resolveSession`, `openSessionJSONL`, `tailJSONL`, `isEndTurn`, `extractAssistantText` (single-record content extractor; superseded by msg_id grouping)
- Code: `cmd/spike-multi-turn/main.go` — `extractByMsgID` (the msg_id-grouped content extractor described above)
- Code: `cmd/spike-cancel/main.go` — `logCancelEvent` (handles `<nil>` / `<missing>` / string `stop_reason` representations), `runCancel` (the cancel-probe driver; the assistant-only filter still drops the `user(text)` cancel marker, so detection is PTY-side via `❯-reappeared`)

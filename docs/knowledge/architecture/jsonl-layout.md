# JSONL layout (claude session logs)

Where `claude` writes its session JSONL and what the records look like, as observed against locally-installed `claude` 2.1.x on macOS during ticket [#1](../codebase/1.md).

> These facts contradict the original ticket body and parts of the vault design doc. The empirical version (this file + [#1 spec](../../specs/architecture/1-spike-one-turn.md) § *Empirical findings already captured*) is the authority. Re-verify against new `claude` versions before relying on it.

## Directory layout

```
~/.claude/projects/<encoded-cwd>/<session-id>.jsonl
```

- **Flat.** No `sessions/` subdirectory. (Vault design doc and ticket body both implied a `sessions/` subdir — wrong.)
- **`<encoded-cwd>` is byte-by-byte substitution**, not a reversible encoding. For each byte of the absolute `os.Getwd()` result:
  - `/` → `-`
  - `.` → `-`
  - everything else passes through
- Adjacent `/.` therefore produces `--`. The encoding is **not reversible** — never try to recover the cwd from the directory name.
- Compute at runtime via `os.Getwd()`; never hardcode.

Example: cwd `/Users/jhi/Workspace/Projects/.pyrycode-worktrees/architect-1` → `-Users-jhi-Workspace-Projects--pyrycode-worktrees-architect-1`.

## Discovering the session file

The robust mechanism is to **dictate the session ID up-front** via `claude --session-id <uuid>` (verified ticket [#7](../codebase/7.md)). The JSONL filename is then the deterministic `<uuid>.jsonl` in the encoded-cwd directory — no directory scan, no mtime heuristic, no `fsnotify`. The discovery strategy is:

1. **Before spawning `claude`**, resolve the session ID (generate a fresh UUIDv4 by default, or accept an operator-supplied one via a flag). Compute `jsonlPath = ~/.claude/projects/<encoded-cwd>/<uuid>.jsonl`.
2. Spawn `claude --session-id <uuid>`. Log the resolved id + path *before* `pty.Start` so an external operator can `tail -f <path>` or `claude --resume <uuid>` from another shell.
3. **After writing the prompt** (see *Empirical surprise* below), poll `os.Stat(jsonlPath)` every 100 ms with a ≥10 s timeout. On success the file is brand new; tail from offset 0.
4. The tailer's filter (`obj["message"]` is a map AND `obj["type"] == "assistant"`) silently skips claude's startup envelopes — same parser rule that handles non-`--session-id` runs.

Polling is sufficient — do NOT pull in `fsnotify` for this. Implemented in `cmd/spike-one-turn/main.go` (`resolveSession`, `openSessionJSONL`, `tailJSONL`).

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

### Tool-use shapes (observed)

Tool-use turns produce **multiple `assistant` messages with different `msg_id`s**, interleaved with `user(tool_result)` events that the assistant-only parser filter drops automatically. Empirically observed for the prompt `list the files in /tmp` (ticket #9, run 7):

| Idx | type | stop_reason | msg_id | content |
|-----|------|-------------|--------|---------|
| 13 | assistant | `tool_use` | `msg_011JC…` | `tool_use` block (Bash call) |
| 14 | user | — | — | `tool_result` (filtered out by tailer) |
| 18 | assistant | `end_turn` | `msg_01YEr…` | `text` block (final answer) |

The msg_id grouping rule handles this correctly because it keys on the **latest** `end_turn` msg_id (line 18); the earlier `tool_use`-tagged msg_id is inert noise. Heavier tasks may produce many tool_use lines under a single msg_id with interleaved tool_results — the grouping rule still applies; only the `end_turn` msg_id's content is extracted.

### Turn-complete = JSONL `end_turn` ∧ PTY idle

JSONL `end_turn` says "model done speaking"; the PTY's `❯` glyph says "TUI ready to accept input." Consumers driving multi-turn sessions must wait for both before writing the next prompt — `cmd/spike-multi-turn/main.go` does this with a 250 ms `idleStableWindow` debounce on the `isIdle` half to absorb transient redraw observations.

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

**Parser rule for consumers.** Filter to `obj["message"] is map[…] AND obj["type"] == "assistant"`. This is narrower than "skip every unrecognised type" but tolerates every envelope above (and every future one) with a single positive condition — both spikes use exactly this filter. `user(tool_result)` events are visible on disk but the filter drops them; that's intentional, since the assistant's final-answer `text` block is what consumers want, not the raw tool I/O.

## Caveats

- **`os.Getwd()` casing on macOS** (HFS+/APFS case-insensitive): the same directory can be reached as `WorkSpace` or `Workspace` depending on how you got there. If `os.Getwd()`'s casing disagrees with the path `claude` saw, the computed `<encoded-cwd>` won't match the directory `claude` writes to. The spike documents this; the eventual library may need to resolve the path canonically.
- The shape above is the snapshot from one `claude` version (2.1.x). Future versions may rename fields or add new envelope types. Per [ADR-0001](../decisions/0001-hybrid-jsonl-tui.md), JSONL parsing is the **consumer's** responsibility, not tui-driver's — this file exists to inform consumers and to anchor the state-detection side that does need to know when a turn is done.

## Related

- [ADR-0001 — Hybrid JSONL + TUI](../decisions/0001-hybrid-jsonl-tui.md)
- [System overview](system-overview.md)
- Code: `cmd/spike-one-turn/main.go` — `projectsDir`, `encodeCwd`, `resolveSession`, `openSessionJSONL`, `tailJSONL`, `isEndTurn`, `extractAssistantText` (single-record content extractor; superseded by msg_id grouping)
- Code: `cmd/spike-multi-turn/main.go` — `extractByMsgID` (the msg_id-grouped content extractor described above)

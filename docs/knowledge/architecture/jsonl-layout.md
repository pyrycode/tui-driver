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

`claude` creates the session file **during its startup sequence**, before the first user prompt — it writes `permission-mode` and `file-history-snapshot` envelopes into the JSONL before any user input has been read (verified in ticket [#3](../codebase/3.md)). The discovery strategy is therefore:

1. **After `idle-detected`, before writing the prompt**, pick the newest `*.jsonl` entry in the directory (`max(ModTime)`). claude just spawned and is touching the active log, so the freshly-mtimed file is unambiguous in practice.
2. Stat the file to read its current byte `Size()`. This is the offset to seek to before tailing — everything written before that offset is claude's own startup chatter, which the spike has no use for.
3. After writing the prompt, open the file, `Seek(offset, SeekStart)`, and tail with a 50 ms EOF backoff. Lines appended in response to the prompt arrive here.
4. 1 s deadline on the discovery retry — covers the fresh-cwd race where `~/.claude/projects/<encoded-cwd>/` is created concurrently with the spike's idle detection, without masking a genuine absence.

Polling is sufficient — do NOT pull in `fsnotify` for this. Implemented in `cmd/spike-one-turn/main.go` (`openSessionJSONL` + `tailJSONL`).

> **Historical note.** Spike #1 originally snapshotted the directory and waited for a *new* file to appear post-prompt; that approach was based on the pre-empirical assumption that the JSONL is created lazily at first turn. It isn't. The "wait for a new file" path was deleted in ticket #3.

## Turn lifecycle in JSONL

Each line is one JSON object. The **interactive** `claude` mode does NOT emit a `type:"result"` line (despite what the ticket body said — that's batch-mode behaviour). The turn-terminator in interactive mode is:

```json
{
  "type": "assistant",
  "sessionId": "...",
  "message": {
    "stop_reason": "end_turn",
    "content": [{"type": "text", "text": "..."}]
  }
}
```

- The final-turn record can contain multiple `text` blocks under `message.content[]`. **Concatenate all of them** to get the assistant's full answer. (Note: `content[]` may also contain non-`text` blocks like tool calls — filter on `content[i].type == "text"`.)
- Intermediate `assistant` records (before the final one) carry `stop_reason: "tool_use"` for tool calls, or `content[].type == "thinking"` for chain-of-thought. They are NOT the answer; the spike ignores them and waits for `end_turn`.

## Observed top-level `type` values

Captured from a single 11-event session JSONL during ticket [#3](../codebase/3.md) (orientation, not enumeration):

- `permission-mode` (claude startup)
- `file-history-snapshot` (claude startup)
- `user` (the prompt the spike wrote)
- `attachment` (claude metadata)
- `ai-title` (claude metadata)
- `assistant` (the response; carries `stop_reason`)
- `system` (claude metadata)
- `last-prompt` (claude metadata)

Of these, only `user` and `assistant` carry a `message` object; everything else is a claude-internal envelope with no `message` field. The set is likely to grow with new `claude` versions.

**Parser rule for consumers.** Filter to `obj["message"] is map[…] AND obj["type"] == "assistant"`. This is narrower than "skip every unrecognised type" but tolerates every envelope above (and every future one) with a single positive condition — the spike uses exactly this filter.

## Caveats

- **`os.Getwd()` casing on macOS** (HFS+/APFS case-insensitive): the same directory can be reached as `WorkSpace` or `Workspace` depending on how you got there. If `os.Getwd()`'s casing disagrees with the path `claude` saw, the computed `<encoded-cwd>` won't match the directory `claude` writes to. The spike documents this; the eventual library may need to resolve the path canonically.
- The shape above is the snapshot from one `claude` version (2.1.x). Future versions may rename fields or add new envelope types. Per [ADR-0001](../decisions/0001-hybrid-jsonl-tui.md), JSONL parsing is the **consumer's** responsibility, not tui-driver's — this file exists to inform consumers and to anchor the state-detection side that does need to know when a turn is done.

## Related

- [ADR-0001 — Hybrid JSONL + TUI](../decisions/0001-hybrid-jsonl-tui.md)
- [System overview](system-overview.md)
- Code: `cmd/spike-one-turn/main.go` — `projectsDir`, `encodeCwd`, `openSessionJSONL`, `newestJSONL`, `tailJSONL`, `isEndTurn`, `extractAssistantText`

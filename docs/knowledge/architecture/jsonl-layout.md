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

## Discovering the new file

`claude` creates the session file only after the first prompt is received. Strategy:

1. Snapshot the directory's `*.jsonl` filenames **after idle, before writing the prompt**.
2. After writing the prompt, poll the directory at 100 ms intervals.
3. The new file is the one NOT in the snapshot.
4. 5 s deadline before treating absence as an error.

Polling is sufficient — do NOT pull in `fsnotify` for this. Implemented in `cmd/spike-one-turn/main.go` (`snapshotJSONL` + `waitForNewJSONL`).

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

In a single real session, the spike author saw these top-level `type` values (orientation, not enumeration):

- `ai-title`
- `queue-operation`
- `user`
- `attachment`
- `assistant`
- `last-prompt`

The library currently cares about `assistant` only. The set is likely to grow with new `claude` versions.

## Caveats

- **`os.Getwd()` casing on macOS** (HFS+/APFS case-insensitive): the same directory can be reached as `WorkSpace` or `Workspace` depending on how you got there. If `os.Getwd()`'s casing disagrees with the path `claude` saw, the computed `<encoded-cwd>` won't match the directory `claude` writes to. The spike documents this; the eventual library may need to resolve the path canonically.
- The shape above is the snapshot from one `claude` version (2.1.x). Future versions may rename fields or add new envelope types. Per [ADR-0001](../decisions/0001-hybrid-jsonl-tui.md), JSONL parsing is the **consumer's** responsibility, not tui-driver's — this file exists to inform consumers and to anchor the state-detection side that does need to know when a turn is done.

## Related

- [ADR-0001 — Hybrid JSONL + TUI](../decisions/0001-hybrid-jsonl-tui.md)
- [System overview](system-overview.md)
- Code: `cmd/spike-one-turn/main.go` — `projectsDir`, `encodeCwd`, `tailJSONL`, `isEndTurn`, `extractAssistantText`

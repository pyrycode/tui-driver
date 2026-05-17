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
- Code: `cmd/spike-one-turn/main.go` — `projectsDir`, `encodeCwd`, `resolveSession`, `openSessionJSONL`, `tailJSONL`, `isEndTurn`, `extractAssistantText`

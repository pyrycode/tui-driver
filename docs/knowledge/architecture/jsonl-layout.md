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

> **Canonical library API (post-[#58](../codebase/58.md)).** Steps 1 and 3 above are now `tuidriver.SessionJSONLPath(home, cwd, sessionID)` (pure path composition, wraps `EncodeCwd` so no consumer can re-implement the byte transform) and `tuidriver.WaitForSessionJSONL(ctx, path)` (`os.Stat` poll at `DefaultPollInterval`, returns `nil` on appearance, wraps `context.Cause(ctx)` with the path in the message on cancellation/deadline). The 7 in-tree spike binaries and pyrycode's `agentrun.EncodeProjectDir` still inline these shapes; migration to the library functions is tracked separately ([pyrycode/pyrycode#501](https://github.com/pyrycode/pyrycode/issues/501) for the cross-repo consumer). New consumers should use the library functions; do not hand-roll the path composition.

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

**Parser rule for consumers.** Filter to `obj["message"] is map[…] AND obj["type"] == "assistant"`. This is narrower than "skip every unrecognised type" but tolerates every envelope above (and every future one) with a single positive condition — both spikes use exactly this filter. `user(tool_result)` events are visible on disk but the filter drops them; that's intentional, since the assistant's final-answer `text` block is what consumers want, not the raw tool I/O.

## Caveats

- **Path casing on case-insensitive filesystems (default macOS APFS, default Windows).** The same directory can be reached as `WorkSpace` or `Workspace` depending on how you got there, and the on-disk canonical casing is what `claude` writes into `~/.claude/projects/`. Consumers that route through `tuidriver.EncodeCwd` get this for free — since [#57](../codebase/57.md), `EncodeCwd` resolves to the on-disk canonical form (symlinks resolved, case canonicalised) before encoding, so the output matches the directory `claude` writes to. Consumers that build the encoded-cwd themselves must canonicalise the input first (open the path and `fcntl(F_GETPATH)` on darwin, or `filepath.EvalSymlinks` on case-sensitive Linux defaults where a differently-cased lookup naturally returns `ENOENT`).
- The shape above is the snapshot from one `claude` version (2.1.x). Future versions may rename fields or add new envelope types. Per [ADR-0001](../decisions/0001-hybrid-jsonl-tui.md), JSONL parsing is the **consumer's** responsibility, not tui-driver's — this file exists to inform consumers and to anchor the state-detection side that does need to know when a turn is done.

## Related

- [ADR-0001 — Hybrid JSONL + TUI](../decisions/0001-hybrid-jsonl-tui.md)
- [System overview](system-overview.md)
- Code: `pkg/tuidriver/jsonl.go` — `SessionJSONLPath` (canonical path composition; wraps `EncodeCwd`) + `WaitForSessionJSONL` (canonical `os.Stat`-poll appearance waiter). Use these from new consumers — the spike helpers below are pre-extraction inlines that have not yet been migrated. See [#58](../codebase/58.md).
- Code: `cmd/spike-one-turn/main.go` — `projectsDir`, `encodeCwd`, `resolveSession`, `openSessionJSONL`, `tailJSONL`, `isEndTurn`, `extractAssistantText` (single-record content extractor; superseded by msg_id grouping)
- Code: `cmd/spike-multi-turn/main.go` — `extractByMsgID` (the msg_id-grouped content extractor described above)
- Code: `cmd/spike-cancel/main.go` — `logCancelEvent` (handles `<nil>` / `<missing>` / string `stop_reason` representations), `runCancel` (the cancel-probe driver; the assistant-only filter still drops the `user(text)` cancel marker, so detection is PTY-side via `❯-reappeared`)

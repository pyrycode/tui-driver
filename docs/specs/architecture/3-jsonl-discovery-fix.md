# Spec: fix(spike) — JSONL discovery + end-turn detection aligned with observed schema

**Ticket:** [#3](https://github.com/pyrycode/tui-driver/issues/3)
**Size:** S
**Status:** ready for development

## Files to read first

Load these in order; each entry says what to extract.

- `cmd/spike-one-turn/main.go:41-69` — current constants + regexes; note `jsonlFirstFileWait` and `jsonlPollInterval` which are about to lose their meaning
- `cmd/spike-one-turn/main.go:182-208` — current `snapshotJSONL` → `writePrompt` → `tailJSONL` sequence in `run()`; this is the exact region the fix rewires
- `cmd/spike-one-turn/main.go:226-254` — the `for !(spinnerGone && gotEndTurn)` block; only the log-line token changes here, the logic is already correct
- `cmd/spike-one-turn/main.go:427-533` — `snapshotJSONL`, `tailJSONL`, `waitForNewJSONL`: the discovery + tailing functions being replaced
- `cmd/spike-one-turn/main.go:537-557` — `isEndTurn` and `extractAssistantText`: keep as-is, no behavior change
- `cmd/spike-one-turn/README.md:55-65` — *Required state log lines* table; the `result-event-received` line is the one to rename
- `cmd/spike-one-turn/README.md:108-152` — *Surprises / findings* sections 1, 3, 4; the empirical record that drives this ticket
- `docs/specs/architecture/1-spike-one-turn.md:74-86, 125, 144-156` — the parent spec's State machine, Watchdog list, and State log lines table; all three reference `result-event-received` and must be updated
- `docs/knowledge/architecture/jsonl-layout.md:24-32` — *Discovering the new file* section. **Do not edit** (documentation phase owns this); read it only so the developer recognizes that this knowledge file currently describes the old behavior and will be reconciled later
- `docs/knowledge/decisions/0001-hybrid-jsonl-tui.md` — ADR confirming the JSONL side belongs in the spike's responsibility set even though longer-term parsing is the consumer's job

## Context

Spike #1 shipped a binary whose JSONL handling was built against three wrong assumptions about claude's behavior; both manual runs failed before reaching `SUCCESS` ([README findings #1, #3, #4](../../../cmd/spike-one-turn/README.md)). The runs proved the *architecture* sound (claude received the prompt via PTY and produced a correct response in JSONL with `stop_reason=end_turn`); the bugs are confined to discovery and naming.

This ticket realigns the spike with observed behavior so subsequent runs can characterize TUI state-transition timing. It does **not** address the orthogonal "thinking spinner never fires for trivial prompts" problem — that's ticket #4.

## Design

### Overview of the change

All edits are inside `cmd/spike-one-turn/main.go` plus two doc files. The reader goroutine, watchdog, shutdown sequence, state machine shape, and end-turn check logic are unchanged. Three things move:

1. **JSONL discovery** stops waiting for a *new* file to appear and instead identifies the session file that already exists at idle, records its byte offset, then tails forward from there.
2. **JSONL parser** ignores anything that is not an `assistant` event with a `message` field — silently, including known-noise types (`permission-mode`, `file-history-snapshot`, `attachment`, `ai-title`, `system`, `last-prompt`).
3. **Log-line token** `result-event-received` is renamed to `end-turn-detected` at the single emission site, the README's *Required state log lines*, and the parent spec.

### JSONL discovery (replaces `snapshotJSONL` + `waitForNewJSONL`)

New helper, replacing both functions:

```
func openSessionJSONL(dir string) (path string, offset int64, err error)
```

Behavior contract:

- List `*.jsonl` entries in `dir` (use `os.ReadDir` filtering on `.jsonl` suffix, mirroring the existing pattern).
- Pick the entry with the **largest `ModTime()`** — this is the file claude is currently writing into. The spike just spawned claude, so the freshly-touched file is unambiguous in practice.
- Stat the file to read `Size()`. Return `(absolutePath, size, nil)`.
- If the directory does not exist or contains no `*.jsonl` entries, retry every 100 ms for up to 1 s, then return a named error: `no session JSONL appeared in <dir> within 1s after idle`. (The empirical finding is that the file is present at idle, but a brief retry absorbs any small claude-startup-vs-our-idle-detection race without masking a genuine absence.)
- The 5 s `jsonlFirstFileWait` constant goes away. Replace with a `sessionFileWait = 1 * time.Second` constant.

Call-site change in `run()` (around current lines 183–208):

- Delete the `preSnapshot, err := snapshotJSONL(projDir)` block.
- After `idle-detected` and before `ptmx.Write(promptText)`, call `openSessionJSONL(projDir)` and capture `(sessionPath, startOffset)`.
- Pass `(sessionPath, startOffset)` to the tailer goroutine.
- Log `session-jsonl-opened path=<sessionPath> offset=<startOffset>` (replaces today's `jsonl-file-discovered path=…` log line, which happens inside `tailJSONL` — see note below).

### JSONL tailing (rewrites `tailJSONL`)

New signature:

```
func tailJSONL(ctx context.Context, logger *log.Logger, path string, startOffset int64, out chan<- map[string]any) error
```

Behavior contract:

- Open `path`. `Seek(startOffset, io.SeekStart)`. From there, the existing `bufio.Reader` + `ReadBytes('\n')` + partial-line handling pattern (current lines 463–501) is reused verbatim — the only logic difference is the seek and the new filter.
- For each parsed JSON line: see § *JSONL parser tolerance* below for the filter rules.
- The `jsonl-file-discovered` log inside the old `tailJSONL` is removed; discovery now happens before the goroutine starts, and is logged at the call site (`session-jsonl-opened …`).
- The `waitForNewJSONL` helper is deleted in full.

### JSONL parser tolerance

Inside the tail loop, after `json.Unmarshal(line, &obj)`:

- If `obj["message"]` is not a `map[string]any` → `continue` silently. This covers `permission-mode`, `file-history-snapshot`, `attachment`, `ai-title`, `system`, `last-prompt`, and any other future no-message envelope.
- If `obj["type"]` is not the string `"assistant"` → `continue` silently. This drops `user`-typed events (which have a `message` field but aren't relevant to end-of-turn detection).
- Otherwise, push `obj` on the channel.

No warning logs for either skip path. Parse errors from `json.Unmarshal` itself **keep** the existing `jsonl-parse-warning err=…` log line — a malformed JSONL line is a genuine surprise worth seeing in stderr, distinct from a known-noise envelope.

This is intentionally narrower than the AC's literal "continue on any unrecognized type" — by filtering to `assistant`-with-`message` we tolerate every type the AC lists (and unknown future ones) without enumerating them.

### Log-line rename

Single emission site at `cmd/spike-one-turn/main.go:241-242`:

- `tr.recordTransition("result-event-received")` → `tr.recordTransition("end-turn-detected")`
- `logger.Printf("result-event-received")` → `logger.Printf("end-turn-detected")`

Doc updates:

- `cmd/spike-one-turn/README.md` § *Required state log lines*: replace the `result-event-received` row and its trailing comment.
- `docs/specs/architecture/1-spike-one-turn.md` lines 83, 125, 153 (three references in *State machine*, *Watchdog*, *State log lines* sections respectively): replace the token and update the inline comment that justifies the old name.

### Constants to remove / rename

```
jsonlFirstFileWait = 5 * time.Second   → REMOVE
jsonlPollInterval  = 100 * time.Millisecond → REMOVE (no more directory polling)
jsonlTailInterval  = 50 * time.Millisecond  → KEEP (still used for EOF backoff)
                                         + ADD sessionFileWait = 1 * time.Second
                                         + ADD sessionFilePoll  = 100 * time.Millisecond
```

### Concurrency model

Unchanged. Same three goroutines, same shutdown sequence, same context propagation. Only the JSONL tailer's startup precondition changes (file path + offset are known before it starts, instead of being discovered by the tailer itself).

### Error handling

- `openSessionJSONL` returning an error → propagate as `fmt.Errorf("open session jsonl: %w", err)` from `run()` and fail the spike. This now fires synchronously **before** `prompt-written`, which is preferable to today's behavior (the failure currently fires inside the tailer goroutine via `cancelCause`, several state transitions later).
- All other failure modes (PTY error, watchdog trip, parse error) are unchanged.

## Testing strategy

No automated tests (consistent with spike #1's policy — see `1-spike-one-turn.md` § *Testing strategy*). Verification is by execution:

1. `go build ./cmd/spike-one-turn`
2. Run the binary at least once from the worktree root.
3. Confirm stderr no longer contains `jsonl-tailer-error: no new JSONL file appeared` or `watchdog: no new JSONL file appeared within 5s`.
4. Confirm stderr contains `session-jsonl-opened path=… offset=…` between `idle-detected` and `prompt-written`.
5. Confirm the parser does not crash, panic, or emit `jsonl-parse-warning` lines for the known-noise types listed in the README findings.
6. If the spike still fails to reach `SUCCESS` because of the orthogonal "spinner never fires" problem (likely — that's ticket #4), record the new observed failure shape in the README's *Surprises / findings* section. **Do not** expand this ticket's scope to fix that.
7. If the spike does reach `SUCCESS`, that's a bonus — record the timing row in the README's *Observed timings* table.

The AC's verification clause is satisfied by step 3 (no spurious tailer error) and step 5 (parser tolerates unknowns), regardless of whether step 7 succeeds.

## Open questions

- **Edge case: claude has never run from this cwd before.** In that case `~/.claude/projects/<encoded-cwd>/` is created by claude *during* its startup, possibly racing the spike's `idle-detected`. The 1 s retry inside `openSessionJSONL` should cover it, but if a real run from a fresh cwd consistently trips the 1 s deadline, bump the constant or add a single log line `session-jsonl-waiting` once per poll-cycle to make the wait visible. Document the observation in the README either way.
- **Multiple JSONL files in the same directory.** If the user has run claude from this cwd before, older `*.jsonl` files exist. Using `max(ModTime)` should always pick the freshly-opened one because claude touches it during startup; if a future run shows otherwise (e.g., an old session file with a stale-but-newer mtime due to clock skew), the README should record it and we'd switch to "newest mtime AND mtime > spike-start-time" as a defensive filter.

## Out of scope (reminder, mirrors ticket)

- Making `thinking-detected` optional (ticket #4 owns this — do not touch the `waitUntil` block that gates on the spinner).
- Multi-turn / tool-use behavior.
- Extracting reusable primitives into `pkg/tuidriver/`.
- Updating `docs/knowledge/architecture/jsonl-layout.md` — documentation phase owns this and will reconcile it from the new spike behavior post-merge.

# Spec: spike — end-to-end PTY drive of interactive `claude` (one turn)

**Ticket:** [#1](https://github.com/pyrycode/tui-driver/issues/1)
**Size:** S
**Status:** ready for development

## Files to read first

The developer should start by loading these into context. The repo has no production Go code yet, so most pointers are vault notes and the empirical findings already captured in this spec — read this spec end-to-end before writing any code.

- `README.md` (repo root) — purpose + hybrid JSONL/TUI architecture (one paragraph)
- `CLAUDE.md` (repo root) — scope discipline (especially: do NOT design `pkg/tuidriver/` during the spike)
- Vault `📋 Projects/2026-04-10 - Pyrycode/TUI Driver.md` — full design doc; pay particular attention to:
  - § *Hybrid JSONL + TUI* (the two-channel rationale)
  - § *State Machine* (the linear sequence the spike implements)
  - § *Pattern Matching, Not Full Emulation (Initially)* (regex-over-rolling-buffer approach)
  - § *Spike Plan (~4 hours)* (the procedure this spec elaborates)
- `creack/pty` package docs at <https://pkg.go.dev/github.com/creack/pty> — canonical recipe is `cmd := exec.Command("claude"); ptmx, err := pty.Start(cmd)`; `ptmx` is the master side, both read and write
- This spec § *Empirical findings already captured (do NOT rediscover)* below — the two non-obvious facts about where claude writes JSONL and what its turn-terminator looks like in interactive mode

## Context

The hybrid architecture (PTY pattern-matching for state, JSONL log tailing for content) was decided 2026-05-16 and is unproven. This spike is a single-file Go program in `cmd/spike-one-turn/` that runs a real `claude` session through one happy-path question (`"What is 2+2?\r"`) end-to-end, exercising every primitive that the eventual `pkg/tuidriver/` library will own: PTY allocation, rolling-buffer state detection, keystroke injection, JSONL discovery + tailing, watchdog, clean shutdown.

The deliverable is *empirical knowledge*, not reusable code. Single-file is intentional. Resist designing the public API.

## Empirical findings already captured (do NOT rediscover)

These were verified against the operator's local `~/.claude/projects/` at spec-writing time. They contradict the ticket body and vault doc in two places — believe this spec over those.

1. **JSONL layout is FLAT.** Files live at `~/.claude/projects/<encoded-cwd>/<session-id>.jsonl` — not `<encoded-cwd>/sessions/<session-id>.jsonl`. There is no `sessions/` subdirectory.
2. **`encoded-cwd` is a byte-by-byte substitution.** For each rune of the absolute cwd: `/` → `-`, `.` → `-`, everything else passes through. The encoding is NOT reversible — adjacent `/` and `.` produce `--`. Example: cwd `/Users/jhi/Workspace/Projects/.pyrycode-worktrees/architect-1` encodes to `-Users-jhi-Workspace-Projects--pyrycode-worktrees-architect-1` (the double-dash reflects `/` then `.`). The spike must compute its own encoded-cwd from `os.Getwd()` at runtime — do not hardcode a path.
3. **Interactive `claude` does NOT emit a `type:"result"` JSONL event.** The ticket body says to wait for one; it never arrives. The empirical turn-terminator is a JSONL line with `type=="assistant"` AND `message.stop_reason=="end_turn"`. Intermediate `assistant` events carry `stop_reason: "tool_use"` (tool calls) or `content[].type == "thinking"` (chain-of-thought) and are NOT the final answer. Treat `stop_reason: "end_turn"` as the JSONL completion signal and concatenate every `content[].text` (`type == "text"`) on that one line to get the assistant's response.
4. **Observed `type` values in a real session JSONL** (for orientation, not enumeration): `ai-title`, `queue-operation`, `user`, `attachment`, `assistant`, `last-prompt`. The spike only cares about `assistant`.
5. **Assistant text envelope.** Final-turn record shape (top-level fields shown abbreviated):
   ```json
   {"type":"assistant","sessionId":"...","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"..."}]}}
   ```
   `message.content` is an array; the final record can contain multiple `text` blocks — concatenate all of them.

The README the developer writes is the canonical place to record any further surprises (verb variants, ANSI quirks, timing observations, schema variants across runs). This spec captures only what's needed to avoid wasted developer turns.

## Design

### Layout

```
cmd/spike-one-turn/
  main.go      # everything — PTY, reader, state machine, JSONL tailer, watchdog, shutdown
  README.md    # how to run, observed timings, observed verbs, surprises
go.mod         # add github.com/creack/pty
go.sum         # generated
```

No `pkg/tuidriver/` content in this spike. The spike's role is to surface friction before any extraction is justified.

### Concurrency model

Three goroutines, all coordinated by a single `context.Context` cancelled on shutdown:

| Goroutine | Responsibility | Exit condition |
|---|---|---|
| `main` (orchestrator) | Runs the linear state machine: wait-idle → write-prompt → wait-thinking → wait-terminator → extract → success. Runs the watchdog tick (1 Hz). | State machine completes OR watchdog trips OR error. Always runs the shutdown path on exit. |
| `ptyReader` | Reads from PTY master in a loop into a rolling buffer (last ~4 KB). Mirrors output to stderr so a human can see what claude is doing. Maintains an ANSI-stripped view of the buffer for regex matching. | PTY master returns EOF or error (which happens after the orchestrator closes it). |
| `jsonlTailer` | Started immediately after `idle-detected`. Polls `~/.claude/projects/<encoded-cwd>/` for a new file vs. the startup snapshot. Once found, tails it line-by-line; for each line decodes JSON and emits the parsed object on a channel. | Context cancellation. |

Synchronization:

- The rolling buffer is owned by `ptyReader` and read under a `sync.Mutex` by the orchestrator's regex probes.
- JSONL events flow on a buffered channel (`chan map[string]any`, size 32).
- Use `sync.WaitGroup` to ensure both goroutines exit before `main` returns.

### State machine (linear; no actual machine)

The spike is sequential. Each step is a "block until condition OR watchdog timeout" call. Implement as a series of helper functions returning `error`; the watchdog supplies the deadline:

1. `waitIdle(ctx) error` — poll the rolling buffer for the `❯` glyph (UTF-8 `\xe2\x9d\xaf`) appearing at-or-near-the-end-of-the-buffer with no active spinner. Log `idle-detected`.
2. `writePrompt(ptmx, "What is 2+2?\r") error` — single `Write`. Log `prompt-written`.
3. Start the JSONL tailer goroutine here (the new file will only be created once claude has received the prompt — starting earlier risks racing the directory snapshot).
4. `waitThinking(ctx) error` — poll the rolling buffer for the spinner regex; on first match, log `thinking-detected verb=<captured>`. Start tracking the spinner's time-tail integer for the freeze watchdog.
5. `waitTerminationBoth(ctx, jsonlCh) (assistantText, error)` — wait until BOTH:
   - the spinner regex no longer matches the rolling buffer (log `spinner-gone`), AND
   - a JSONL event arrives with `type=="assistant"` and `message.stop_reason=="end_turn"` (log `result-event-received` — keep the historical log token even though it's an `end_turn` event; the README explains the divergence)

   The two events can arrive in either order. Once both have fired, extract the assistant text from the `end_turn` event's `message.content[].text` (concatenated) and log `assistant-text-extracted len=<n>`.
6. Print `SUCCESS: <assistant text>` to stdout.
7. Run the shutdown sequence (always, including on error — `defer`).

### Pattern matching

Two compiled regexes; both run against the ANSI-stripped rolling buffer.

| Name | Regex | Capture |
|---|---|---|
| Spinner | `✻\s+(\S+(?:\s+\S+)?)\s+for\s+(?:(\d+)m\s+)?(\d+)s` | g1 = verb (1–2 words); g2 = minutes (optional); g3 = seconds |
| Idle | literal `❯` (UTF-8 `\xe2\x9d\xaf`) — `bytes.Contains` is sufficient | n/a |

Notes for the dev:

- The verb is variable per-prompt ("Baked", "Whipped up", "Cooking", …). Capture and log it on every `thinking-detected` transition — these logs are the empirical record we want.
- "Idle" is `❯ present` AND `spinner regex does NOT match` simultaneously. While thinking, `❯` may still appear in the buffer because the TUI redraws the input line below the spinner — don't treat lone `❯` as idle.
- ANSI stripping: a single CSI regex like `\x1b\[[0-9;?]*[a-zA-Z]` suffices for the spike — the spinner and `❯` are wrapped in color codes in real output. Maintain two parallel buffers in the reader (raw for stderr mirror; stripped for regex matching) or strip on-the-fly inside the matcher; either is fine.
- Buffer policy: cap at 4 KB by re-slicing on each append (`if len(buf) > 4096 { buf = buf[len(buf)-4096:] }`). The state signals are always in the last few hundred bytes; older context is irrelevant.

### JSONL discovery and tailing

```
1. resolveProjectsDir() = $HOME/.claude/projects/<encode(getwd())>/
   where encode replaces '/' and '.' bytes with '-' character-by-character.
   (See § Empirical findings — encoding is byte-by-byte, NOT a normalization.)
2. snapshot := set of existing *.jsonl filenames in that directory at startup (after waitIdle, before writePrompt — so the new file is unambiguous)
3. After writePrompt: poll directory every 100 ms for a *.jsonl file whose name is NOT in snapshot. First hit is our session file.
   Watchdog: if no new file appears within 5 s of writePrompt, abort with a named error.
4. Open the file, read to EOF, then sleep 50 ms and re-read in a loop — classic tail.
   Each line: json.Unmarshal into map[string]any. On parse error, log a warning and skip the line — one bad line shouldn't kill the spike.
5. Filter for type == "assistant" — push those onto the channel. (Other types ignored; documented in README if anything notable shows up.)
```

Do NOT use `fsnotify`. Polling at 100 ms is sufficient, deterministic, and avoids a dependency for a spike.

### Watchdog

Two deadlines, both enforced from the main goroutine on a 1 Hz ticker:

- **Per-state inactivity (60 s):** maintain `lastTransitionAt time.Time`. Bump on every logged state event (`idle-detected`, `prompt-written`, `thinking-detected`, `spinner-gone`, `result-event-received`, `assistant-text-extracted`). If `time.Since(lastTransitionAt) > 60s`, abort with `watchdog: stuck in state <currentState> for 60s`.
- **Spinner counter freeze (30 s):** only active while the spinner regex is matching. Maintain `lastSpinnerProgressAt time.Time`. Recompute the total spinner seconds (`m*60 + s`) from the regex capture every tick; bump when strictly greater than the previous reading. If `time.Since(lastSpinnerProgressAt) > 30s` while the spinner is still visible, abort with `watchdog: spinner counter frozen at <prev>s for 30s`.

Watchdog errors must be distinguishable in stderr — prefix the log line with `watchdog:` so README post-mortems can grep cleanly.

### Shutdown

Single `defer` in `main` that:

1. Logs `shutdown-signalled`.
2. Sends SIGTERM via `cmd.Process.Signal(syscall.SIGTERM)`.
3. Races a 3-second `time.After` against `<-cmdExited` (where `cmdExited` is a channel closed when `cmd.Wait()` returns from a goroutine launched just after `pty.Start`).
4. If the 3-second timer wins, sends SIGKILL.
5. Closes the PTY master fd. This forces the `ptyReader` goroutine out of its `Read` call.
6. Cancels the parent context. The `jsonlTailer` exits.
7. `wg.Wait()` — both goroutines must have returned before `main` does.

The shutdown path must run on every exit (success, watchdog timeout, PTY error, panic recovery if any). The success path then exits with code 0; failure paths exit with code 1 and the named error printed to stderr.

### State log lines (stderr, structured-ish)

Required event names (with the watchdog log gated by `watchdog:` prefix as called out above):

```
idle-detected
prompt-written
thinking-detected verb=<captured-verb>
spinner-gone
result-event-received           # the end_turn assistant event; name kept per AC
assistant-text-extracted len=<n>
shutdown-signalled
```

One line per event. Free-form key=value tail is fine — these logs are for the README's timing table, not for machine parsing.

## Open questions for the dev

These are deliberately left for the spike to answer empirically. The README captures the answer; this spec does not pre-decide.

- **How long does the typical step take?** Capture wall-clock times for each transition and put them in the README.
- **Which verbs appear?** Log each one. The README accumulates the observed set over several runs.
- **Does anything else interesting land in JSONL between `assistant`-with-`tool_use` and `assistant`-with-`end_turn` for a simple math question?** Probably no tool use at all — but if the spike observes one, that's a finding worth documenting (means simple questions aren't a guaranteed-tool-free baseline).
- **Does `❯` ever appear in the buffer concurrently with the spinner (i.e., does the input-line redraw bleed into the spinner area)?** The spec assumes yes and uses "idle = `❯ present AND no spinner`". If empirically `❯` is suppressed during thinking, the AND is conservative and harmless; document either way.
- **Does the spinner ever pause mid-prompt (e.g., between thinking and tool-use)?** If so the 30 s freeze watchdog might false-positive. Document if observed; a follow-up ticket would refine the policy.

## Testing strategy

This spike is verified by execution against real `claude`, not by automated tests. The developer:

1. Builds: `go build ./cmd/spike-one-turn`
2. Runs from a directory where `claude` is on `$PATH` and the user has a valid Claude subscription session
3. Confirms stdout shows `SUCCESS: 2 + 2 = 4` (or equivalent)
4. Confirms stderr shows every required state log line in order
5. Runs it 3+ times, records observed verbs and timings in the README
6. Confirms a clean process exit (no orphaned `claude` process — check `ps`)

No unit tests for the spike itself. Future tests for `pkg/tuidriver/` (post-spike) would mock the PTY with a fake binary; that's out of scope here.

Negative-path validation that the developer should do at least once:

- Kill the spawned `claude` externally mid-run (`kill -TERM <pid>` from another terminal) and confirm the spike exits within a few seconds with a clear error, NOT hanging.

## Out of scope (reminder)

Per ticket § *Out of Scope* — none of the following belongs in this spike:

- Modal handling (`/doctor`, multiselects, y/n prompts)
- Consumer API design under `pkg/tuidriver/`
- Multi-turn sessions
- Process-mode (standalone binary speaking JSON-RPC)
- Cost telemetry extraction
- Terminal emulation (xterm-headless, tcell, etc.)
- Building a verb dictionary as a discrete artifact — the README records what's seen; that's enough until/unless verb classification ever becomes load-bearing

If the dev encounters something that feels like it warrants any of the above, the right move is to note it in the README's *Surprises* section so the post-spike triage can decide whether to file a ticket.

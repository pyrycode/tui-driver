# spike-one-turn

End-to-end PTY drive of one interactive `claude` turn. Spike validation for the
hybrid JSONL + TUI architecture decided 2026-05-16. See
[ticket #1](https://github.com/pyrycode/tui-driver/issues/1) and
`docs/specs/architecture/1-spike-one-turn.md`.

## What it does

1. Allocates a 120×40 PTY via `github.com/creack/pty`.
2. Spawns `claude` (interactive, no flags) attached to the slave side.
3. Reads the PTY master into a 4 KB rolling buffer, mirroring raw bytes to
   stderr so a human watching the spike sees claude's UI live.
4. Waits for the idle prompt (`❯` glyph present and no `✻` spinner).
5. Snapshots `~/.claude/projects/<encoded-cwd>/`.
6. Writes `What is 2+2?\r` to the PTY master.
7. Starts the JSONL tailer: polls the snapshotted directory at 100 ms for a new
   `*.jsonl` file (up to a 5 s deadline), then tails it line-by-line.
8. Waits for the thinking spinner regex (`✻ <verb> for <Ns>|<Nm Ns>`).
9. Waits for BOTH: the spinner regex stops matching, AND a JSONL line arrives
   with `type=="assistant"` and `message.stop_reason=="end_turn"`.
10. Concatenates every `content[].text` block on that record.
11. Prints `SUCCESS: <assistant text>` to stdout.
12. SIGTERMs `claude`, races a 3 s timer against `cmd.Wait()`, SIGKILLs if
    timer wins, closes the PTY, drains both goroutines.

A watchdog ticks at 1 Hz throughout, enforcing two deadlines:

- **60 s** since the last state transition (bumped on every named state log
  line). Trips with `watchdog: stuck in state <X> for <N>s`.
- **30 s** since the spinner counter last incremented, *while the spinner
  regex is still matching*. Trips with
  `watchdog: spinner counter frozen at <N>s for <M>s`.

## How to run

Requirements:

- `claude` 2.1.x on `$PATH`, authenticated against a Claude subscription.
- Go 1.26+ (`go.mod` pins `go 1.26.2`).

```sh
go build -o /tmp/spike-one-turn ./cmd/spike-one-turn
/tmp/spike-one-turn
```

On success, stdout is one line:

```
SUCCESS: 2 + 2 = 4
```

Stderr contains the raw claude UI bytes interleaved with the state log lines.

### Required state log lines (in order)

```
idle-detected
prompt-written
thinking-detected verb="<captured>"
spinner-gone
result-event-received          # the end_turn assistant event; name kept per AC
assistant-text-extracted len=<n>
shutdown-signalled
```

Watchdog trips are prefixed `watchdog:` so `grep '^.*watchdog:' stderr.log`
finds them cleanly during post-mortems.

### Verifying clean exit

After the spike returns, no orphaned `claude` should remain:

```sh
pgrep -lf 'claude$' || echo 'no orphans — good'
```

### Negative-path check

The spec asks for at least one externally-killed run. From another terminal:

```sh
pkill -TERM -f 'claude$'
```

The spike should exit within a few seconds — the PTY reader sees EOF and the
state-machine waiter exits via the watchdog inactivity deadline.

## Observed timings

| Run | idle → prompt | prompt → thinking | thinking → spinner-gone | spinner-gone → result | total | outcome |
|-----|---------------|-------------------|--------------------------|------------------------|-------|---------|
|  1  | 0.408 ms      | never fired       | n/a                      | n/a                    | 5.0 s | FAIL — `jsonl-tailer-error: no new JSONL file appeared within 5s` |
|  2  | 0.158 ms      | never fired       | n/a                      | n/a                    | 60.6 s | FAIL — `watchdog: stuck in state prompt-written for 1m1s` |

Both runs failed before any thinking-state observation. Run 1 hit the JSONL-tailer's 5-second deadline; run 2 hit the main state-machine watchdog's 60-second inactivity deadline. The architecture's failure-handling worked correctly in both cases.

**The architecture is sound — implementation has bugs that the empirical run exposed.** Detail in `Surprises / findings` below.

## Observed thinking verbs

| Verb (captured) | Run(s) | Notes |
|-----------------|--------|-------|
| (none)          | —      | Spinner never observed on the PTY across either run — see surprise #2 in findings. |

Known-good verbs from the operator's prior observation (NOT used as a regex whitelist): "Baked", "Whipped up", "Cooking". Empirically observed in the spike: zero, because the trivial prompt completed faster than the spinner could be drawn.

## Surprises / findings

### 1. The session JSONL file is created during claude *startup*, not at first user turn

The spike's logic ("snapshot the projects/ dir, then poll for a *new* `.jsonl` file post-prompt") was based on the assumption that the session JSONL doesn't exist until the user's first message is processed. Empirical observation: claude writes at least two events (`permission-mode` and `file-history-snapshot`) into the session JSONL *during its startup sequence*, before the user has typed anything. By the time the spike snapshots the directory, the file already exists.

Inspecting the actual JSONL file from run 2 (session `ced8f502-009a-4d15-8a69-cf5b8f19e724`), the event sequence was:

| Idx | type | role | stop_reason | notes |
|-----|------|------|-------------|-------|
| 0 | `permission-mode` | — | — | written at claude startup |
| 1 | `file-history-snapshot` | — | — | written at claude startup |
| 2 | `user` | `user` | — | content: `"What is 2+2?"` — the prompt DID get through |
| 3 | `attachment` | — | — | claude metadata |
| 4 | `attachment` | — | — | claude metadata |
| 5 | `ai-title` | — | — | claude metadata |
| 6 | `assistant` | `assistant` | `end_turn` | content: `"4"` — claude responded correctly |
| 7 | `system` | — | — | claude metadata |
| 8 | `last-prompt` | — | — | claude metadata |
| 9 | `ai-title` | — | — | claude metadata |
| 10 | `permission-mode` | — | — | claude metadata |

**Fix shape for the spike:** instead of "look for a new file post-prompt," the JSONL-discovery logic should either (a) read the file's mtime/size pre-prompt, then poll for *appended bytes* post-prompt, or (b) discover the file via the spike's own controlled session-id (pass `claude --session-id <known-uuid>` if claude supports it, or read claude's stdout for the session-id banner).

### 2. The thinking spinner may never appear for trivial prompts

Both runs failed to observe the `✻ <verb> for <Ns>` spinner anywhere in the PTY output. The spike's state machine waits for `thinking-detected` before falling through to JSONL polling, so this gap blocks happy-path progression.

Likely cause: claude responded with a single token (`"4"`) in well under 100 ms — faster than the spinner UI can fade in. The spinner is presumably a TUI affordance for *slow* completions, not a guaranteed visual element of every turn.

**Fix shape for the spike:** make `thinking-detected` optional, not a required gate. The state machine should accept either:
- Spinner appears → spinner disappears → JSONL `end_turn` (the slow path the spike was designed for)
- JSONL `end_turn` arrives before any spinner is observed (the fast path that just happened for both runs)

The current state-machine requirement that `thinking-detected` fires before JSONL is checked is the actual blocker, not the JSONL discovery alone.

### 3. There is no separate `result` envelope event in the JSONL

The spec's AC mentions waiting for "a JSONL `result` event." There is none. The end-of-turn marker is `stop_reason=end_turn` embedded in the `assistant` message itself (event 6 above). The spike's named log line `result-event-received` is named for an event that doesn't exist; should be renamed to `end-turn-detected` or similar.

### 4. JSONL schema has many more event types than the spec predicted

The spec described the shape as `{type, message:{stop_reason, content:[…]}}`. Actual schema (from run 2) has these `type` values: `permission-mode`, `file-history-snapshot`, `user`, `attachment`, `ai-title`, `assistant`, `system`, `last-prompt`. Of these, only `user` and `assistant` carry a `message` object; the others are claude-internal metadata.

**Fix shape:** the JSONL parser should `continue` on any unrecognized `type`, and key only on `type=="assistant"` + `message.stop_reason=="end_turn"` for end-of-turn. Treat all other event types as noise for spike purposes.

### 5. Idle-detection was actually correct (corrects my earlier hypothesis)

The 0.158–0.408 ms gap between `idle-detected` and `prompt-written` initially looked like a race — `❯` matching the placeholder hint (`❯ Try "fix typecheck errors"`) before claude was actually ready. The JSONL evidence contradicts this: claude DID receive the prompt and DID respond, which means it was genuinely ready when `idle-detected` fired.

So `❯` is in fact a usable idle indicator even though it also appears in the welcome-banner placeholder. The placeholder is dimmed and gets cleared on first keystroke; claude accepts input from the moment `❯` is rendered.

This was a wrong hypothesis on my part — I'd guessed at the bug from the timing alone before reading the JSONL. **Useful lesson: when the JSONL evidence is available, read it before theorizing about PTY-level races.** Same shape as the [[vault: instruction-design#Claudian's Inferred-Signal-Over-Authoritative-State Tendency (2026-05-16)]] pattern.

### 6. Watchdog architecture works correctly

The two-tier watchdog (60s inactivity, 30s spinner-freeze) caught the wedge cleanly. Run 1's JSONL-tailer used its own 5s deadline (which fired correctly). Run 2 made it past the JSONL-tailer (file existed) but stalled in the state machine; the 60s inactivity watchdog fired exactly when expected.

### 7. ANSI / encoded-cwd / claude-version notes

- **claude version at runtime:** v2.1.143 (`Claude Code v2.1.143` in the welcome banner; matches the spike's environment)
- **encoded-cwd:** `/Users/juhanailmoniemi/.claude/projects/-Users-juhanailmoniemi-WorkSpace-Projects-tui-driver/` — both `/` and `.` in the cwd became `-`, casing preserved (`WorkSpace` stayed `WorkSpace`)
- **ANSI quirks:** no apparent strip failures; the `\x1b\[[0-9;?]*[a-zA-Z]` strip handled the welcome-banner sequences cleanly. The raw stderr-mirroring during the spike was readable when decoded as a terminal would render it (boxed banner appeared correctly via `cat` of the captured bytes — for the curious, `cat /tmp/spike-run1.err`).
- **`❯` during thinking:** not observed (no thinking state visible)
- **Spinner pauses mid-prompt:** not observed (no spinner at all)

## Follow-up tickets the spike's findings justify

Concrete tickets to file (none filed yet — operator decides priority):

1. **Fix JSONL discovery: tail existing file for appended lines** (the architectural problem behind findings #1 + #3 + #4). Likely `size:s`.
2. **Make `thinking-detected` optional in the state machine** (finding #2). Either accept fast-path JSONL arrival without spinner, OR add an early "fast-response detected" branch. `size:xs` once the JSONL fix lands.
3. **Reframe the state-log lines + AC** — `result-event-received` → `end-turn-detected`; drop the architectural assumption that a separate `result` envelope exists. Documentation-shaped; could fold into ticket 1 above.
4. **(Maybe) Add a "first successful end-to-end turn" verification ticket** — re-run the spike after fixes 1+2 land, fill in this README's empty timings and verb rows for a real success path.

## Why no automated tests

Per `docs/specs/architecture/1-spike-one-turn.md` § *Testing strategy*: this
spike's deliverable is empirical knowledge, not reusable code. Verification is
by execution against real `claude`. Future `pkg/tuidriver/` tests (post-spike,
not this ticket) will mock the PTY with a fake binary; that's out of scope
here, and adding unit tests now risks freezing the wrong abstractions.

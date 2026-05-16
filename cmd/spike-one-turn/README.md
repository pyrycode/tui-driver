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
5. Opens the session JSONL claude already created during startup (newest
   `*.jsonl` in `~/.claude/projects/<encoded-cwd>/`) and records its current
   byte size — a 1 s retry covers a fresh-cwd race.
6. Writes `What is 2+2?\r` to the PTY master.
7. Starts the JSONL tailer: seeks the recorded offset and tails appended lines.
8. Waits for the thinking spinner regex (`✻ <verb> for <Ns>|<Nm Ns>`).
9. Waits for BOTH: the spinner regex stops matching, AND a JSONL line arrives
   with `type=="assistant"` and `message.stop_reason=="end_turn"`. The parser
   silently skips any event lacking a `message` map or whose `type` is not
   `assistant` (covers `permission-mode`, `file-history-snapshot`, `user`,
   `attachment`, `ai-title`, `system`, `last-prompt`, and unknown future
   envelopes).
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
session-jsonl-opened path=<path> offset=<bytes>
prompt-written
thinking-detected verb="<captured>"   # slow path only — iff spinner observed
spinner-gone                          # slow path only — iff spinner observed
end-turn-detected              # assistant event with stop_reason=="end_turn"
assistant-text-extracted len=<n>
shutdown-signalled
```

On the fast path (trivial prompts where the spinner never renders, or where the
spinner regex never matches it — see finding #8), `thinking-detected` and
`spinner-gone` are skipped; `end-turn-detected` fires directly after
`prompt-written`.

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
|  3  | 0.857 ms      | never fired       | n/a                      | n/a                    | 60.6 s | FAIL — `watchdog: stuck in state prompt-written for 1m1s` (post-#3 fix; JSONL side now clean — see finding #8 for the new shape) |

Run 1 hit the JSONL-tailer's 5-second deadline; runs 2 and 3 hit the main state-machine watchdog's 60-second inactivity deadline. The architecture's failure-handling worked correctly in all cases.

**The architecture is sound — implementation has bugs that the empirical run exposed.** Detail in `Surprises / findings` below.

## Observed thinking verbs

| Verb (captured) | Run(s) | Notes |
|-----------------|--------|-------|
| (none)          | 1, 2   | Spinner not observed in the raw PTY bytes in the earliest runs — see surprise #2. |
| `Skedaddling`   | 3      | Appears as `✻ Skedaddling…` (ellipsis form, no `for Ns` counter — regex doesn't match). |
| `Baked`         | 3      | Appears as `✻ Baked for 1s` but with a CSI cursor-forward between glyph and verb that the ANSI strip removes — see surprise #8. |

Known-good verbs from the operator's prior observation (NOT used as a regex whitelist): "Baked", "Whipped up", "Cooking". The spike's regex still captures zero verbs because of the strip/whitespace and ellipsis-form issues above.

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

### 8. Spinner DOES render — the regex misses it after ANSI strip (post-#3 observation)

Originally finding #2 hypothesised that `claude` never draws a spinner for trivial prompts. Run 3 (post-ticket-#3 fix) refines that: the spinner glyph **is** in the PTY output. The raw bytes around it look like:

```
\x1b[38;2;153;153;153m✻\x1b[1CBaked for 1s\x1b[39m
```

That `\x1b[1C` is a CSI cursor-forward-1 — claude positions the cursor with a control sequence instead of writing a literal space. The single-pass ANSI strip (`\x1b\[[0-9;?]*[a-zA-Z]`) treats cursor moves the same as colour codes and removes them. The stripped buffer reads `✻Baked for 1s` with no whitespace between glyph and verb, so `✻\s+(\S+(?:\s+\S+)?)\s+for\s+(?:(\d+)m\s+)?(\d+)s` does not match. `thinking-detected` is never logged, the state machine sits at `prompt-written`, and the 60 s inactivity watchdog fires.

Note the JSONL side is fully healthy in this run — the `assistant` event with `stop_reason=end_turn` and `content[].text == "4"` arrives within ~1 s of `prompt-written` and is queued in the event channel — it just never gets dequeued because the state machine is blocked upstream.

**Fix shape for the spike:** belongs in ticket #4 (`thinking-detected` should be optional, not a required gate). A separate small change to the spinner matcher (either treat `✻` followed by the verb with zero-or-more whitespace, or normalise CSI cursor moves into spaces before stripping) would *also* let this be detected — but that's not required if #4 makes the gate optional. Out of scope for #3.

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

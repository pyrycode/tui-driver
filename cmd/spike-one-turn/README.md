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

To be filled in after first-runs. One row per run; record wall-clock deltas
between adjacent transitions.

| Run | idle → prompt | prompt → thinking | thinking → spinner-gone | spinner-gone → result | total |
|-----|---------------|-------------------|--------------------------|------------------------|-------|
|  1  |               |                   |                          |                        |       |
|  2  |               |                   |                          |                        |       |
|  3  |               |                   |                          |                        |       |

The log lines emitted with `log.LstdFlags|log.Lmicroseconds` give the
timestamps directly — compute deltas from the stderr capture.

## Observed thinking verbs

The verb after `✻` is per-prompt randomized. Record every value seen here so
post-spike work can decide if a verb dictionary is ever worth maintaining
(spec § *Out of Scope* — not yet).

| Verb (captured) | Run(s) | Notes |
|-----------------|--------|-------|
|                 |        |       |

Known-good verbs from the operator's prior observation (NOT used as a regex
whitelist): "Baked", "Whipped up", "Cooking".

## Surprises / findings

Open log. Fill in after each run with anything that didn't match the spec's
expectations. Suggested categories:

- **JSONL schema variants** — fields that appeared/disappeared across the
  spec-captured shape `{type, message:{stop_reason, content:[…]}}`.
- **Intermediate assistant events** — anything between
  `assistant`+`tool_use` and `assistant`+`end_turn` for a simple math question
  (the spec predicts none for `What is 2+2?` — note if a tool call shows up).
- **ANSI quirks** — places where the single-pass `\x1b\[[0-9;?]*[a-zA-Z]`
  strip wasn't enough.
- **`❯` during thinking** — the spec assumes `❯` may appear concurrently with
  the spinner due to input-line redraw; document either way.
- **Spinner pauses mid-prompt** — would false-positive the 30 s freeze
  watchdog; note any sighting.
- **`os.Getwd()` casing on macOS** — case-insensitive HFS+/APFS sometimes
  resolves `WorkSpace` vs `Workspace` differently depending on how the path
  was reached. The spike's encoding uses whatever `os.Getwd()` returns and
  should agree with claude's; document if they diverge.
- **claude version** at run time: `claude --version`.

(empty)

## Why no automated tests

Per `docs/specs/architecture/1-spike-one-turn.md` § *Testing strategy*: this
spike's deliverable is empirical knowledge, not reusable code. Verification is
by execution against real `claude`. Future `pkg/tuidriver/` tests (post-spike,
not this ticket) will mock the PTY with a fake binary; that's out of scope
here, and adding unit tests now risks freezing the wrong abstractions.

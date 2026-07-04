# spike-one-turn

End-to-end PTY drive of one interactive `claude` turn. Spike validation for the
hybrid JSONL + TUI architecture decided 2026-05-16. See
[ticket #1](https://github.com/pyrycode/tui-driver/issues/1) and
`docs/specs/architecture/1-spike-one-turn.md`.

## Status

**Spike complete (2026-05-17).** Hybrid JSONL + TUI architecture empirically
validated end-to-end across Runs 5 + 6 — generated-UUID and
operator-supplied-UUID paths both produce `SUCCESS: 4`. All three fix tickets
the spike findings justified
([#3](https://github.com/pyrycode/tui-driver/issues/3),
[#4](https://github.com/pyrycode/tui-driver/issues/4),
[#7](https://github.com/pyrycode/tui-driver/issues/7)) merged. Empirical
knowledge captured in the *Observed timings* / *Observed thinking verbs* /
*Surprises* sections below; the next tui-driver work item is library
extraction into `pkg/tuidriver/` consumed by `pyry acp`.

## What it does

1. Resolves a session ID — generates a fresh UUIDv4 by default, or accepts one
   via `-session-id <uuid>` (validated; bad UUID exits 2 before claude spawns).
   Computes the deterministic JSONL path
   `~/.claude/projects/<encoded-cwd>/<uuid>.jsonl` and logs
   `session-id-resolved id=<uuid> jsonl=<path>` *before* spawn so an operator
   can `tail -f` or `claude --resume` from another terminal.
2. Allocates a 120×40 PTY via `github.com/creack/pty`.
3. Spawns `claude --session-id <uuid>` attached to the slave side.
4. Reads the PTY master into a 4 KB rolling buffer, mirroring raw bytes to
   stderr so a human watching the spike sees claude's UI live.
5. Waits for the idle prompt (`❯` glyph present and no `✻` spinner).
6. Writes `What is 2+2?\r` to the PTY master.
7. Polls `os.Stat(jsonlPath)` every 100 ms with a 10 s timeout. Under
   `--session-id` the file is *not* created during claude startup — it appears
   only after the prompt lands (see finding #9). On first stat success, tails
   the file from offset 0.
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

### Optional flags

- `-session-id <uuid>` — pin claude's session ID (and therefore the JSONL filename). Default: an empty value triggers a fresh UUIDv4 at startup. A non-empty value must parse as a valid UUID; otherwise the spike exits non-zero with a usage error. Whether the ID was generated or supplied, it is emitted in the `session-id-resolved` log line **before** `claude` spawns — so an operator can `tail -f <jsonl-path>` or run `claude --resume <id>` from another terminal while the spike runs.

  Use cases: debugging (tail the same JSONL from another shell), parallel post-mortem, deterministic reproduction across runs.

### Required state log lines (in order)

```
session-id-resolved id=<uuid> jsonl=<path>   # fires before pty.Start
idle-detected
prompt-written
session-jsonl-opened path=<path> offset=0    # fires AFTER prompt-written — see finding #9
thinking-detected verb="<captured>"   # slow path only — iff spinner observed
spinner-gone                          # slow path only — iff spinner observed
end-turn-detected              # assistant event with stop_reason=="end_turn"
assistant-text-extracted len=<n>
shutdown-signalled
```

On the fast path (trivial prompts where the spinner never renders, or where the
spinner regex never matches it — see finding #8), `thinking-detected` and
`spinner-gone` are skipped; `end-turn-detected` fires directly after
`session-jsonl-opened`.

Note the ordering change vs the pre-#7 spike: `session-jsonl-opened` now fires
**after** `prompt-written`, not after `idle-detected`. The deterministic JSONL
path (pinned via `--session-id`) does not exist on disk until claude starts
processing the first input — see finding #9.

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

| Run | spawn → idle | idle → prompt | prompt → jsonl-opened | jsonl-opened → end-turn | total | outcome |
|-----|--------------|---------------|------------------------|--------------------------|-------|---------|
|  1  | n/a          | 0.408 ms      | n/a (pre-#7 ordering)  | n/a                      | 5.0 s | FAIL — `jsonl-tailer-error: no new JSONL file appeared within 5s` |
|  2  | n/a          | 0.158 ms      | n/a (pre-#7 ordering)  | n/a                      | 60.6 s | FAIL — `watchdog: stuck in state prompt-written for 1m1s` |
|  3  | n/a          | 0.857 ms      | n/a (pre-#7 ordering)  | n/a                      | 60.6 s | FAIL — `watchdog: stuck in state prompt-written for 1m1s` (post-#3; JSONL side clean — see finding #8) |
|  4  | n/a          | 0.470 ms      | n/a (pre-#7 ordering)  | n/a                      | 60.7 s | FAIL — `watchdog: stuck in state prompt-written for 1m1s` (post-#3 + #4; opened STALE JSONL — see finding #9) |
|  5  | 303 ms       | 0.079 ms      | 202 ms                 | 2.737 s                  | 3.24 s | **SUCCESS** — `SUCCESS: 4` (post-#7; deterministic --session-id; spinner "Brewing…" rendered briefly but ellipsis-form so #8 still applies) |
|  6  | 453 ms       | 0.068 ms      | 304 ms                 | 1.565 s                  | 2.32 s | **SUCCESS** — `SUCCESS: 4` (post-#7; operator-supplied `-session-id 9b375373-…`; AC #4 dual-path verification) |
|  7  | n/a          | n/a           | > 10 s (exceeded bound) | n/a                     | 11.1 s | FAIL then **SUCCESS** — under `make e2e` concurrent-suite load the `prompt → jsonl-opened` wait exceeded the old 10s `sessionFileWait` and the run died with `open session jsonl: … context deadline exceeded` (PR #184 `make e2e`, 11090ms); standalone re-runs were 3/3 pass. Bumping `sessionFileWait` to 30s (matching probe-first-prompt-hang) makes the same wait load-tolerant — see finding #10 (#185) |

Runs 1-4 all failed in distinct shapes — each surfaced a real bug. Runs 5 and 6 are the first end-to-end successes — Run 5 with a generated UUID, Run 6 with an operator-supplied UUID via `-session-id`. The architecture's failure-handling worked correctly in every failing run; nothing wedged silently.

The column shape changed between Run 4 and Run 5 because #7 reordered the state-log line sequence (`session-id-resolved` now fires before spawn, `session-jsonl-opened` now fires after `prompt-written`). The pre-#7 columns are kept as `n/a` for the earlier runs rather than retroactively rewriting them — the chronological honesty matters more than column uniformity.

**The architecture is sound — implementation has bugs that the empirical run exposed.** Detail in `Surprises / findings` below.

## Observed thinking verbs

| Verb (captured) | Run(s) | Notes |
|-----------------|--------|-------|
| (none)          | 1, 2   | Spinner not observed in the raw PTY bytes in the earliest runs — see surprise #2. |
| `Skedaddling`   | 3      | Appears as `✻ Skedaddling…` (ellipsis form, no `for Ns` counter — regex doesn't match). |
| `Baked`         | 3      | Appears as `✻ Baked for 1s` but with a CSI cursor-forward between glyph and verb that the ANSI strip removes — see surprise #8. |
| `Brewing`       | 5      | Appears as `✳ Brewing…` then transitions through `✶ Brewing…` → `✻ Brewing…` → `✽ Brewing…` (animation glyph cycles). Ellipsis form, no `for Ns` counter — regex doesn't match. The success path now exists without the spinner regex ever matching — finding #2/#8 lessons hold. |

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

**Resolved by [#3](https://github.com/pyrycode/tui-driver/issues/3)** (parser tolerates non-`assistant` event types — superseded later by [#7](https://github.com/pyrycode/tui-driver/issues/7), which adopted option (b) via `--session-id <uuid>`).

### 2. The thinking spinner may never appear for trivial prompts

Both runs failed to observe the `✻ <verb> for <Ns>` spinner anywhere in the PTY output. The spike's state machine waits for `thinking-detected` before falling through to JSONL polling, so this gap blocks happy-path progression.

Likely cause: claude responded with a single token (`"4"`) in well under 100 ms — faster than the spinner UI can fade in. The spinner is presumably a TUI affordance for *slow* completions, not a guaranteed visual element of every turn.

**Fix shape for the spike:** make `thinking-detected` optional, not a required gate. The state machine should accept either:
- Spinner appears → spinner disappears → JSONL `end_turn` (the slow path the spike was designed for)
- JSONL `end_turn` arrives before any spinner is observed (the fast path that just happened for both runs)

The current state-machine requirement that `thinking-detected` fires before JSONL is checked is the actual blocker, not the JSONL discovery alone.

**Resolved by [#4](https://github.com/pyrycode/tui-driver/issues/4)** — `thinking-detected` is now optional; state machine accepts JSONL `end_turn` arrival without prior spinner observation. Runs 5 + 6 successfully traverse the fast path without the spinner regex ever matching.

### 3. There is no separate `result` envelope event in the JSONL

The spec's AC mentions waiting for "a JSONL `result` event." There is none. The end-of-turn marker is `stop_reason=end_turn` embedded in the `assistant` message itself (event 6 above). The spike's named log line `result-event-received` is named for an event that doesn't exist; should be renamed to `end-turn-detected` or similar.

**Resolved by [#3](https://github.com/pyrycode/tui-driver/issues/3)** — log line renamed to `end-turn-detected`; state machine keys on `type=="assistant" && message.stop_reason=="end_turn"`.

### 4. JSONL schema has many more event types than the spec predicted

The spec described the shape as `{type, message:{stop_reason, content:[…]}}`. Actual schema (from run 2) has these `type` values: `permission-mode`, `file-history-snapshot`, `user`, `attachment`, `ai-title`, `assistant`, `system`, `last-prompt`. Of these, only `user` and `assistant` carry a `message` object; the others are claude-internal metadata.

**Fix shape:** the JSONL parser should `continue` on any unrecognized `type`, and key only on `type=="assistant"` + `message.stop_reason=="end_turn"` for end-of-turn. Treat all other event types as noise for spike purposes.

**Resolved by [#3](https://github.com/pyrycode/tui-driver/issues/3)** — parser silently skips events lacking a `message` map or whose `type` is not `assistant`.

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

### 9. JSONL discovery: mtime heuristic opened a STALE session file; fixed by pinning `--session-id`

Run 4 (post-#3 + #4) failed in a new shape. State log:

```
2026/05/17 11:09:01.694709 idle-detected
2026/05/17 11:09:01.695096 session-jsonl-opened path=.../dddf559a-…jsonl offset=63216
2026/05/17 11:09:01.695179 prompt-written
2026/05/17 11:10:02.394169 watchdog: stuck in state prompt-written for 1m1s
```

The spike opened `dddf559a-….jsonl`. Claude's actual session JSONL for this run was `9707673d-….jsonl` (revealed by the `Resume this session with: claude --resume 9707673d-…` banner). Different files. At the moment the spike called `openSessionJSONL` (immediately after `idle-detected`), `9707673d` did NOT yet exist on disk; `newestJSONL` returned whichever existing `.jsonl` was newest at that instant, and prior spike runs had accumulated 4+ stale files in the projects-cwd. Claude's `9707673d` JSONL recorded the correct exchange (`"What is 2+2?"` → `"4"` with `stop_reason=end_turn`) within ~1 s of `prompt-written` — the architecture was fine; the spike just tailed the wrong file.

**Generalizable lesson:** "newest file by mtime" is a common pattern for "find the active file." It is fragile when (a) files of the same shape persist across runs (stale data in the cwd) and (b) the thing being looked for hasn't been created yet at the moment of polling. The robust patterns, in order of preference:

1. **Dictate the identifier** if the tool provides a way to (`--session-id`, `--output-file`, etc.). No race, no discovery logic at all.
2. **Snapshot baseline + diff** — record state before triggering work; look for what's new after.
3. **Newest-by-mtime + retry** — fragile, only safe if no stale data is plausible.

Always check the tool's flags for option (1) before implementing (2) or (3).

**Fix shape (this ticket):** claude exposes `--session-id <uuid>` (verified via `claude --help` v2.1.143). The spike now:

- Generates a UUIDv4 at startup (or accepts an operator-supplied one via `-session-id`), spawns `claude --session-id <uuid>`, and computes the deterministic JSONL path `~/.claude/projects/<encoded-cwd>/<uuid>.jsonl`.
- Logs `session-id-resolved id=<uuid> jsonl=<path>` **before** spawning claude, so the operator can `tail -f` or `claude --resume` from another terminal.
- Polls `os.Stat(jsonlPath)` for up to 10 s instead of scanning the directory. Stale files in the cwd are now structurally invisible.

#### Empirical sub-finding: interactive `claude --session-id` defers JSONL creation until first input

Discovered during this fix's developer iteration: when claude is spawned with `--session-id <uuid>`, the deterministic JSONL path does NOT exist at the moment `❯` (idle) first renders. The file appears only after claude receives the first user input. Confirmed by the spike's stat-poll timing across the developer's iterations.

This contrasts with finding #1 (where plain `claude` writes startup envelopes — `permission-mode`, `file-history-snapshot` — into the JSONL during its boot sequence, *before* any user input). Whether the `--session-id` deferral is intentional or an implementation accident in v2.1.143 is unknown; either way, the spike must accommodate it.

**Implementation consequence:** `openSessionJSONL` is invoked **after** `prompt-written`, not after `idle-detected`. The JSONL tailer therefore starts seeking at offset 0 (the file is brand new) — the parser filter (`continue` on non-assistant types) handles whatever startup envelopes claude writes when it processes the first input.

**Verification status — confirmed by Run 5** (2026-05-17, post-#7 fix):

- `session-id-resolved` fires at T+0 (before claude spawns).
- `idle-detected` fires at T+303 ms (claude finished startup; `❯` rendered).
- `prompt-written` fires at T+304 ms (immediately after idle).
- `session-jsonl-opened path=… offset=0` fires at T+506 ms.

The 202 ms gap between `prompt-written` and `session-jsonl-opened` is the deferral: claude with `--session-id` did not write the JSONL during its startup; the file appeared only after the prompt landed. If the deferral hypothesis were wrong, `os.Stat` would have succeeded immediately after the directory was scanned at startup time, and the gap would have been ≈0. The 202 ms is small but consistent across the spike run (the polling interval is 100 ms — so the true gap is in the [102, 202] ms range, but the sign and the ordering are unambiguous).

The architect's original spec assumed the opposite ordering (poll after idle, before prompt). That ordering would have wedged in `openSessionJSONL` for 10 s and timed out — claude would not start processing input until the spike wrote the prompt, so the JSONL never would have appeared.

### 10. The deferred-JSONL-creation window is load-sensitive — the 10 s wait raced it under concurrent-suite load (#185)

Finding #9 established that interactive `claude --session-id` defers JSONL creation until it processes the first input. That first-input work — session init + file open — is **local-process-bound**, so it slows under CPU/IO contention. Standalone the file opens in ~200 ms (Runs 5/6: 202 ms, 304 ms), but under `make e2e` concurrent-suite load (`PYRY_MAX_CONCURRENT=2` dispatch, and/or QA's baseline + PR `make e2e` overlapping — each a live `claude` competing for cores) that work exceeded the old `sessionFileWait = 10 s` bound. The deadline won the race, `WaitForSessionJSONL` returned `context deadline exceeded`, and the run reddened with `open session jsonl: session jsonl … did not appear: context deadline exceeded` at ~11 s (PR #184's `make e2e`, 11090 ms — Run 7 above). The 3/3-standalone-pass-after-a-single-make-e2e-fail signature is textbook load-sensitive timing, not an assertion failure — the operation *succeeds* given enough wall-clock.

**Fix:** bump `sessionFileWait` to 30 s, matching `cmd/probe-first-prompt-hang/main.go:41`, which already reasoned to 30 s for the same deferred-JSONL-creation reason. 30 s is 3× the observed ~10 s failure threshold and stays well inside the e2e-runner's 60 s per-check budget, so a genuinely hung claude still surfaces via the 60 s `ptyQuietLimit` watchdog / runner timeout — only the legitimate-but-slow case is newly tolerated. `WaitForSessionJSONL` itself (`pkg/tuidriver/jsonl.go`) was not touched; the library is correct, the caller's deadline was too tight.

**Repro recipe.**

1. *Mechanism isolation (deterministic).* Set `sessionFileWait` to a value below the ~200 ms standalone JSONL-open latency (Runs 5/6) — e.g. `1 * time.Millisecond` — rebuild, run once → the deadline fires before the file can appear and it fails immediately after `prompt-written` with `open session jsonl: session jsonl … did not appear: context deadline exceeded`, matching the PR #184 error string and shape. Verified on claude 2.1.199. (A 1 s value is *not* reliably reproducing, since standalone the file opens in ~200 ms.) Revert.
2. *Load realism (best-effort).* With the deadline at the old 10 s, launch N concurrent copies (fresh UUIDs → distinct JSONL paths, no collision; `-trust-folder=accept` clears the modal) plus CPU pressure until at least one copy hits the 10 s deadline and prints the same error:

   ```sh
   for i in $(seq 8); do ./bin/spike-one-turn -trust-folder=accept & done; wait
   # stack `yes >/dev/null &` per core, or raise N, on a fast machine
   ```

## Follow-up tickets the spike's findings justified

All filed and merged. Captured here as a record of the empirical-iteration
loop the spike drove:

| Ticket | Finding origin | Outcome |
|---|---|---|
| [#3](https://github.com/pyrycode/tui-driver/issues/3) | Findings 1, 3, 4 | JSONL discovery + end-turn detection aligned with observed schema; parser tolerates unrecognized event types. Merged via PR #5 (2026-05-16). |
| [#4](https://github.com/pyrycode/tui-driver/issues/4) | Finding 2 (refined by 8) | `thinking-detected` made optional; fast path accepted. Merged via PR #6 (2026-05-16). |
| [#7](https://github.com/pyrycode/tui-driver/issues/7) | Finding 9 | Deterministic `--session-id <uuid>` pins JSONL path; mtime heuristic dropped. Merged via PR #8 (2026-05-17). Run 5 produced the first end-to-end SUCCESS. |

The "first successful end-to-end turn" verification that finding-list-item 4
gestured at was Run 5 itself — no separate verification ticket was needed
because Run 5's data populated the *Observed timings* / *Observed thinking
verbs* tables above directly. Run 6 (operator-supplied `-session-id`) verified
#7's dual-path AC.

## Why no automated tests

Per `docs/specs/architecture/1-spike-one-turn.md` § *Testing strategy*: this
spike's deliverable is empirical knowledge, not reusable code. Verification is
by execution against real `claude`. Future `pkg/tuidriver/` tests (post-spike,
not this ticket) will mock the PTY with a fake binary; that's out of scope
here, and adding unit tests now risks freezing the wrong abstractions.

# spike-cancel

End-to-end PTY drive of three cancellation probes against a single
interactive `claude` session: cancel during thinking, cancel during tool
execution, and a recovery turn that verifies the same `--session-id` is
still usable. Validates ESC as the cancel keystroke, documents the
JSONL shape claude uses to signal "request interrupted by user", and
captures cancel-to-recovery latency.

See [ticket #11](https://github.com/pyrycode/tui-driver/issues/11) and
`docs/specs/architecture/11-spike-cancel.md`.

## Status

**Spike complete (2026-05-17).** Five end-to-end runs (3 with ESC,
1 with double-ESC, 1 with Ctrl-C) all green: `SUCCESS: Hello.` /
`Hello!` from Probe 3 in ~13 s total wall time. Headline findings:

- **ESC (`0x1b`) is the cancel keystroke.** Single byte, no `\r`.
  Double-ESC and Ctrl-C also work, but ESC is the simplest and the
  one the on-screen `esc to interrupt` hint advertises.
- **The session is fully recoverable.** Probe 3 lands a fresh `msg_id`,
  produces `SUCCESS:` in ~2-2.7 s — same shape as a non-recovery
  turn 1 baseline.
- **Tool subprocesses are NOT killed by ESC.** Probe 2's `ls /tmp`
  Bash subprocess ran to completion and emitted its `tool_result`
  BEFORE the cancellation marker landed. Cancel-during-tool-use lets
  the in-flight tool finish, then claude declines to continue.
- **Cancellation is signaled in the JSONL via a `user(text)` event
  carrying `[Request interrupted by user]`, not via a new
  `stop_reason` value.** The cancelled assistant message keeps its
  pre-cancel `stop_reason` (e.g. `tool_use`). The current
  assistant-only tailer filter drops the marker — consumers needing
  JSONL-side cancel detection must widen the filter or rely on
  PTY-side `❯-reappeared`.

Three implementation deviations from the spec, all from open questions
the spec explicitly flagged for the dev (see *Surprises / findings*):

1. The spec's `hasSpinnerGlyph false AND isIdle true` post-cancel
   predicate is unsound — post-cancel claude emits ~1.4 KB of redraw +
   title-bar updates, which doesn't roll the spinner glyph out of the
   4096-byte rolling buffer. Replaced with a PTY-quiescence predicate
   (`❯ idle AND no new PTY bytes for 1.5 s`).
2. Post-cancel, claude restores the previously-submitted prompt as
   the drafted input. Without an explicit `Ctrl-U` (kill-to-beginning-
   of-line) before `typePrompt`, Probe 2's prompt got concatenated
   with Probe 1's residue (`"...monadsrecursively list all files..."`)
   and the wait condition timed out. The fix is a single `0x15`
   keystroke before every probe's prompt — idempotent on an empty
   input box.
3. Probes 1+2 both consume the wait-condition event for their kind;
   for `kindToolUse`, the SECOND JSONL line for the same `msg_id`
   (different content block) lands AFTER `cancel-sent` and shows up
   as a `jsonl-cancel-event` log line — same one-message-⇒-N-lines
   pattern spike #9 established.

## What it does

1. Resolves a UUID + deterministic JSONL path; logs `session-id-resolved`.
2. Spawns `claude --session-id <uuid> --permission-mode bypassPermissions`
   on a 120×40 PTY. The bypass is necessary for Probe 2's `ls /tmp`
   tool call (same reason spike #9 used it).
3. Waits for idle (`❯` glyph + spinner not visible), logs `idle-detected`.
4. Drives three sequential probes:
   - Probe 1 (`kindThinking`): submits the monad-essay prompt; waits
     for the `✻` spinner glyph in the rolling buffer; sends the
     cancel keystroke; waits for `❯-reappeared` (PTY-quiescence).
   - Probe 2 (`kindToolUse`): submits the `ls /tmp` prompt; waits for
     an assistant event with `stop_reason=tool_use`; sleeps 200 ms
     for the tool to start; sends the cancel keystroke; waits for
     `❯-reappeared`. Any assistant events arriving between
     `cancel-sent` and `❯-reappeared` are logged as
     `jsonl-cancel-event` lines.
   - Probe 3 (`kindRecovery`): submits `say hello`; runs the full
     msg_id-grouped extraction from spike #9 to produce `SUCCESS:`.
5. Watchdog: 60 s session-level inactivity + 30 s spinner-freeze;
   per-probe 30 s post-cancel limit.

| Probe | Prompt | Kind | What it exercises |
|------:|--------|------|-------------------|
| 1 | `think carefully and write a 1000-word essay on the philosophy of monads` | thinking | Cancel before any assistant tokens are emitted; no JSONL assistant line at all for the cancelled request |
| 2 | `recursively list all files under /tmp` | tool-use | Cancel after `stop_reason=tool_use`; tool subprocess completes; `[Request interrupted by user]` marker in JSONL |
| 3 | `say hello` | recovery | Verifies same `--session-id` is recoverable post-cancel — fresh msg_id, normal `end_turn`, `SUCCESS:` extraction |

## How to run

Requirements: `claude` 2.1.x on `$PATH`, authenticated; Go 1.26+.

```sh
go build -o /tmp/spike-cancel ./cmd/spike-cancel
/tmp/spike-cancel
```

Optional flags:

- `-session-id <uuid>` — pin the session (same semantics as the other spikes).
- `-cancel-keystroke {esc|double-esc|ctrl-c}` — empirical iteration mechanism.
  Default `esc`. See *Cancel keystroke that worked* below.

On success, stdout is one line:

```
SUCCESS: Hello.
```

Stderr is the raw claude UI stream interleaved with the state log:

```
session-id-resolved id=<uuid> jsonl=<path>
idle-detected
probe=1 probe-start kind="thinking" prompt="..."
probe=1 prompt-written
session-jsonl-opened path=<path> offset=0          # AFTER probe=1 prompt-written
probe=1 spinner-or-tool-visible kind=spinner-glyph
probe=1 cancel-sent keystroke=1b
probe=1 ❯-reappeared
probe=1 elapsed-after-cancel=<dur>
probe=2 probe-start kind="tool-use" prompt="..."
probe=2 prompt-written
probe=2 spinner-or-tool-visible kind=tool-use-stop-reason msg_id=<id>
probe=2 cancel-sent keystroke=1b
probe=2 jsonl-cancel-event type=assistant stop_reason=tool_use msg_id=<id>
probe=2 ❯-reappeared
probe=2 elapsed-after-cancel=<dur>
probe=3 probe-start kind="recovery" prompt="say hello"
probe=3 prompt-written
probe=3 end-turn-detected msg_id=<id>
probe=3 ❯-reappeared
probe=3 assistant-text-extracted len=<n>
probe=3 recovery-turn-success len=<n>
complete elapsed=<dur>
shutdown-signalled
```

The optional `probe=3 ❯-disappeared` line is emitted only when `❯`
is observed missing within ~500 ms of `probe=3 prompt-written`. Same
empirical absence as spike #9 — does not fire in any captured run
(see *What `❯` actually does between turns* in spike #9's README).

### Verifying clean exit

```sh
pgrep -lf 'claude --session-id' || echo 'no claude orphans'
pgrep -lf '^ls '                 || echo 'no ls orphans'
```

Both checked across all green runs — no orphans.

## Cancel keystroke that worked

**ESC (`0x1b`), single byte, no trailing `\r`.** Confirmed across runs 5,
6, and 7 (default `-cancel-keystroke esc`). Double-ESC and Ctrl-C also
work; ESC is preferred because (a) it matches the `esc to interrupt`
hint claude shows in the status bar, (b) it's a single byte with no
ambiguity, and (c) it's the simplest representation in any consumer
API.

The keystroke is written as a single `pty.Write` — no inter-byte
delay, no trailing `\r`. The inter-byte-delay reasoning that drives
`typePrompt` (claude's input handler can swallow `\r` arriving in
the same buffered write as the prompt body after a tool-use wind-down)
does not apply here — there is no `\r`.

| Attempt | Keystroke | Hex | Run | Outcome |
|---|---|---|---|---|
| First (with spec's spinner-glyph predicate) | ESC | `1b` | run 1 | **Tripped post-cancel 30 s watchdog.** False positive — investigation showed claude *had* cancelled (no assistant JSONL line for the user prompt) but the spec's predicate was unsound (see surprise 1). |
| Same predicate, double-ESC | double-ESC | `1b 1b` | run 2 | Same false-positive timeout — predicate bug, not keystroke bug. |
| Same predicate, Ctrl-C | Ctrl-C | `03` | run 3 | Same false-positive timeout. |
| Predicate fixed (PTY-quiescence), no clear-line | ESC | `1b` | run 4 | Probe 1 worked; Probe 2 timed out because the input area still held Probe 1's text — `typePrompt` appended Probe 2's prompt to it, and the JSONL `user` record showed `"think carefully...of monadsrecursively list all files under /tmp"` (the concatenation). See surprise 2. |
| Predicate + Ctrl-U clear-line | ESC | `1b` | run 5 | **All three probes green.** `SUCCESS: Hello.` in 12.747 s. |
| Same | ESC | `1b` | run 6 | All green. 12.96 s. |
| Same | ESC | `1b` | run 7 | All green. 13.212 s. |
| Same, alternative keystroke | double-ESC | `1b 1b` | one run | All green; Probe 1 ~1 s slower than ESC alone (2.601 s vs 1.601 s) — possibly because claude treats the second `1b` as a separate keystroke and emits another redraw, extending PTY activity. |
| Same | Ctrl-C | `03` | one run | All green. 12.56 s. Behavior indistinguishable from ESC. |

Implication for the library: ESC is the recommended cancel keystroke;
the API should expose it as a default and document that double-ESC and
Ctrl-C work equivalently for the cancel side but may cause extra
post-cancel redraw activity that the recovery predicate has to tolerate.

## JSONL events recorded during/after each cancel

### Probe 1 — cancel during thinking

Across runs 5, 6, 7: zero `probe=1 jsonl-cancel-event` log lines fired.
Claude's request was cancelled before any assistant tokens were
generated.

Post-hoc on-disk view (run 5's JSONL, lines 1-7 — equivalent across
the other runs):

| idx | type | role | stop_reason | msg_id | content | notes |
|----:|------|------|------|--------|---------|-------|
| 1 | permission-mode | — | — | — | — | session boot |
| 2 | file-history-snapshot | — | — | — | — | session boot |
| 3 | **user** | user | — | — | string: `"think carefully and write a 1000-word essay on the philosophy of monads"` | Probe 1's prompt |
| 4 | attachment | — | — | — | — | — |
| 5 | attachment | — | — | — | — | — |
| 6 | attachment | — | — | — | — | — |
| 7 | ai-title | — | — | — | — | claude's auto-title (`Essay on the philosophy of monads`) |

No assistant line at all for Probe 1's msg_id — the cancellation
happened before token generation started. The session-state markers
(`ai-title`, `last-prompt`) fire as if the turn had run to completion.

### Probe 2 — cancel during tool-use

Across runs 5, 6, 7: exactly one `probe=2 jsonl-cancel-event` log line
per run, carrying `stop_reason=tool_use` and a `msg_id` that matches
the wait-condition event's msg_id. Example log lines:

```
probe=2 spinner-or-tool-visible kind=tool-use-stop-reason msg_id=msg_01NPzX9oeDaqjJ5GmBT453Pa
probe=2 cancel-sent keystroke=1b
probe=2 jsonl-cancel-event type=assistant stop_reason=tool_use msg_id=msg_01NPzX9oeDaqjJ5GmBT453Pa
```

The two assistant lines under that msg_id arrive in this order:
the first carries a `thinking` content block (consumed by
`waitForKickoff`), the second carries the `tool_use` content block
(arrives ~1 ms after `cancel-sent` and logs as `jsonl-cancel-event`).
Same one-message-⇒-N-lines pattern spike #9 established.

Post-hoc on-disk view (run 5, lines covering Probe 2):

| idx | type | role | stop_reason | msg_id | content[].type | notes |
|----:|------|------|------|--------|----------|-------|
| 8 | file-history-snapshot | — | — | — | — | inter-probe |
| 9 | **user** | user | — | — | string: `"recursively list all files under /tmp"` | Probe 2's prompt — clean (Ctrl-U fix; see surprise 2) |
| 10 | attachment | — | — | — | — | — |
| 11 | attachment | — | — | — | — | — |
| 12 | **assistant** | assistant | **tool_use** | msg_01NPzX9oeDaqjJ5GmBT453Pa | `thinking` | Consumed by waitForKickoff |
| 13 | **assistant** | assistant | **tool_use** | msg_01NPzX9oeDaqjJ5GmBT453Pa | `tool_use` | Logged as `jsonl-cancel-event` (arrives just after `cancel-sent`) |
| 14 | **user** | user | — | — | `tool_result` (`"/tmp"`) | Tool subprocess ran to completion |
| 15 | **user** | user | — | — | `text` (`"[Request interrupted by user]"`) | **Cancellation marker** — see findings 3 + 4 |
| 16 | file-history-snapshot | — | — | — | — | post-probe |

Across the three runs, the `tool_result.content` varied:

- Run 5: `"/tmp"` (the model called `ls /tmp` — single line)
- Run 6: `"lrwxr-xr-x@ 1 root  wheel  11 Feb 25 05:41 /tmp -> private/tmp"` (the model called something like `ls -la /tmp` — symlink form)
- Run 7: `"/tmp"`

In every case the tool subprocess completed BEFORE the cancellation
marker fired. See *Probe 2: tool subprocess behavior* below.

### Probe 3 — recovery (no cancel)

Same shape as spike #9's turn 1. One assistant line, stop_reason=end_turn,
single `text` content block. Fresh msg_id — not derived from any of
the cancelled probes' msg_ids.

| Run | recovery msg_id | text length | text |
|---|---|---:|---|
| 5 | msg_013vz4ZCLgreuA3wMh2BuZcw | 6 | `"Hello."` |
| 6 | msg_01GNfaGqfTtfvdVR77mwSo9d | 6 | `"Hello!"` |
| 7 | msg_01Xdat2YefXVL8vW8dUdjikY | 6 | `"Hello!"` |

## Per-probe observed timing

`cancel-sent → ❯-reappeared` is the load-bearing measurement. It is
dominated by the 1500 ms PTY-quiescence window (see surprise 1) —
subtract that to get the actual cancel-propagation latency: ~100 ms
for thinking-cancels (no tool to drain), ~600-1200 ms for tool-use
cancels (Bash subprocess + tool_result emission + interrupt marker).

`prompt-written → spinner-or-tool-visible` reflects claude's pre-token
latency: ~1 s for the thinking probe (claude starts processing
immediately), ~3 s for the tool-use probe (claude needs to plan,
emit a `thinking` block, then plan the tool call). `signal → cancel`
is ~0 ms for kindThinking (cancel fires the same tick as the wait
condition observation) and 200 ms for kindToolUse (the `toolUseStartGrace`
sleep — see spec § Wait conditions).

| Run | Probe | prompt→signal (ms) | signal→cancel (ms) | cancel→❯ (ms) | total (ms) | notes |
|----:|------|---:|---:|---:|---:|-------|
| 5 | 1 | 1002 | 0 | 1602 | 2604 | thinking — actual cancel-propagation ≈ 100 ms |
| 5 | 2 | 3740 | 201 | 2101 | 6042 | tool-use — actual cancel-propagation ≈ 600 ms |
| 5 | 3 | 2102 | n/a | n/a | 2102 | recovery — same shape as spike #9 turn 1 baseline |
| 6 | 1 | 952 | 0 | 1601 | 2553 | |
| 6 | 2 | 3308 | 201 | 2501 | 6010 | actual cancel-propagation ≈ 1 s (heavier tool output) |
| 6 | 3 | 2451 | n/a | n/a | 2451 | |
| 7 | 1 | 951 | 0 | 1601 | 2552 | |
| 7 | 2 | 3113 | 201 | 2701 | 6015 | actual cancel-propagation ≈ 1.2 s |
| 7 | 3 | 2701 | n/a | n/a | 2701 | |

Total wall time per run: ~12.7-13.2 s — almost entirely dominated by
the three probes' end-to-end times; per-probe variance is mostly in
Probe 2's pre-cancel latency.

## Probe 2: tool subprocess behavior

**The Bash subprocess executed and completed BEFORE the cancellation
marker fired.** Across all three runs, the on-disk JSONL shows a
`tool_result` line carrying real output (variously `"/tmp"` for `ls /tmp`
and `"lrwxr-xr-x@ ... /tmp -> private/tmp"` for `ls -la /tmp`) BEFORE
the `[Request interrupted by user]` marker. The sequence is:

```
assistant(thinking,   stop_reason=tool_use, msg_X)
assistant(tool_use,   stop_reason=tool_use, msg_X)
user(tool_result)                                       # tool ran to completion
user(text "[Request interrupted by user]")              # then the cancel
```

No `assistant` follow-up line for msg_X exists — the tool_use was
never resolved into a final text response.

**No orphan subprocesses.** `pgrep -lf '^ls '` after spike exit returns
nothing across all runs. The tool subprocess terminates cleanly on its
own (because `ls /tmp` is a fast invocation that completes within the
200 ms toolUseStartGrace + the keystroke RTT), not because cancel killed
it. With a slow tool (`sleep 60` or similar) the behavior may differ —
the cancel may or may not propagate to the tool subprocess. Out of
scope for this spike; tracked as a follow-up.

Implication for the eventual `pkg/tuidriver/` API and `pyry acp`: an
ACP `session/cancel` issued during tool execution will NOT abort the
running tool. The consumer must surface this honestly to its user
("cancel acknowledged; in-flight tools will run to completion before
the response stops").

## Probe 3: recovery outcome

**The session is fully recoverable.** Probe 3's `say hello` prompt
landed in the same `--session-id`, produced a fresh `msg_id` (not
reused from Probe 1 or Probe 2), `stop_reason=end_turn`, and a
coherent response (`"Hello."` or `"Hello!"`). The recovery turn's
wall time (~2.1-2.7 s) is comparable to spike #9's turn 1 baseline
(~2.2-2.8 s for `say hello` on a fresh session).

Claude's response was on-topic for `say hello` — not a partial
continuation of the cancelled monad essay or the cancelled `ls /tmp`
listing. The cancellation marker (`[Request interrupted by user]`)
in the JSONL appears to act as a hard turn boundary: claude treats
the next user prompt as a clean new turn, not a continuation.

Probe 3 was driven by the same `runTurn`-equivalent code spike #9
established — msg_id-grouped text extraction, conjunction predicate
(`gotEndTurn ∧ isIdle stable`), 250 ms `idleStableWindow` debounce.
The Probe 3 code path is structurally identical; the spike's actual
contribution is verifying that the predicate works against a
post-cancel session, not a fresh one.

## New event types / stop_reason values observed beyond spikes #1 and #9

**No new envelope `type` values.** The 8 envelope types this spike's
runs produced (`permission-mode`, `file-history-snapshot`, `user`,
`attachment`, `assistant`, `ai-title`, `last-prompt`, `system`) are
exactly spike #9's catalog.

**No new `stop_reason` values.** The cancelled Probe 2 assistant
message keeps its pre-cancel `stop_reason=tool_use`; the on-disk view
shows NO `stop_reason=canceled` / `interrupted` / `max_tokens` /
`null` ever surfaces. Probe 1 has no assistant line at all (cancelled
before token generation), so its msg_id never gets a `stop_reason`.

**One new content-block shape inside the existing `user(text)`
envelope:** `[{type: "text", text: "[Request interrupted by user]"}]`.
This is how claude signals cancellation in the JSONL. It is:

- A `user`-role event (not `assistant`).
- Carries a single `text` content block with the literal string
  `"[Request interrupted by user]"`.
- Always lands AFTER any `tool_result` events for in-flight tools.
- **Filtered out by the current assistant-only tailer** (`obj["type"] != "assistant"`).

Implication for the library: if a consumer needs JSONL-side cancel
acknowledgment (e.g. to satisfy ACP `session/cancel` "I have cancelled"
semantics), the tailer's filter must be widened to also pass
`user(text)` events upstream, OR the consumer relies on PTY-side
`❯-reappeared` for the same signal. PTY-side detection is what this
spike implements and is sufficient for the cancel-followed-by-recovery
flow.

## Surprises / findings

### 1. The spec's post-cancel predicate (`hasSpinnerGlyph false AND isIdle true`) is unsound

The spec specified the post-cancel `❯-reappeared` predicate as
"hasSpinnerGlyph becomes false AND isIdle true AND both hold
continuously for `idleStableWindow`" (with a fallback to plain
`isIdle stable` if the spinner glyph wasn't observed pre-cancel).
The spec's open question #4 explicitly flagged this might not work:
*"if the spinner glyph stays painted in the rolling buffer after
cancel because the rolling buffer hasn't churned past it yet, the
predicate could fire late or never."*

Empirically: it never fires. Run 1 (ESC, this predicate) tripped the
30 s post-cancel watchdog. Investigation:

- Between `cancel-sent` and the watchdog trip, claude emitted ~1.4 KB
  of bytes to the PTY (title-bar updates + input-box redraw +
  permission-mode footer). The `✻` spinner glyph was NOT in those new
  bytes.
- BUT the rolling buffer is capped at 4096 bytes. The spinner glyph
  written during processing sits in the buffer from BEFORE `cancel-sent`,
  and 1.4 KB of subsequent activity doesn't roll past it. The glyph
  stays painted indefinitely.
- The JSONL confirmed claude HAD cancelled — no assistant line for
  Probe 1's user prompt — so the keystroke worked; only the predicate
  was wrong.

**Fix taken: PTY-quiescence predicate.** Track `lastAppendAt` on the
rolling buffer (incremented on every `append`). The recovery
predicate becomes `isIdle(rb.snapshot()) AND rb.quietFor() >=
ptyQuietWindow` with `ptyQuietWindow = 1500 ms`. Detects "claude has
finished settling" directly instead of relying on the rolling buffer
to roll past a stale glyph.

This is structurally cleaner: the recovery signal we actually care
about is "claude is no longer writing to the PTY", not "the spinner
glyph has been forgotten." The previous predicate was an indirect
proxy for the same thing, with a known failure mode the buffer-size
math makes inevitable.

**Implication for the library:** `pkg/tuidriver/` will need either
(a) a PTY-quiescence predicate similar to this spike's, OR (b) a
substantially larger rolling buffer guaranteed to overflow post-cancel,
OR (c) a tighter spinner-region observer (only check the most recent
K bytes). Option (a) is the lightest and is what this spike implements.

### 2. Post-cancel, claude restores the previously-submitted prompt as the drafted input

After ESC successfully cancels Probe 1, claude's input box visibly
shows the previously-submitted prompt redrawn as if it were a draft.
On its own, this is cosmetic — the prompt was already submitted, the
JSONL recorded it as a `user` event before cancellation. But for the
next `typePrompt`, it's catastrophic: bytes are appended after the
existing text, and the final `\r` submits the concatenation.

Observed in run 4 (predicate fixed, no clear-line yet): Probe 2's
`user` JSONL record was:

```json
{
  "type": "user",
  "message": {
    "role": "user",
    "content": "think carefully and write a 1000-word essay on the philosophy of monadsrecursively list all files under /tmp"
  }
}
```

— Probe 1's prompt concatenated with Probe 2's. Probe 2's wait
condition (`stop_reason=tool_use`) then never fired because claude
was producing a response to the mangled prompt, not the intended one.

**Fix taken: send `Ctrl-U` (`0x15`, kill-to-beginning-of-line) before
`typePrompt` on every probe.** Idempotent on an empty input box
(Probe 1's case — input is already empty after session boot). The
keystroke is followed by a 50 ms settle window before the prompt
typing begins.

**Implication for the library:** the eventual cancel API must clear
the input area as part of its semantics, OR document the post-cancel
input-dirty state and require consumers to clear it explicitly. The
"submit the next prompt" path cannot blindly type — there's a hidden
state machine in claude's input handler that ACP `session/prompt`
calls must account for.

### 3. Cancellation is signaled in the JSONL via `user(text "[Request interrupted by user]")`, not via `stop_reason`

The pre-spike open-question candidates for the JSONL cancel signal
were "partial assistant message with `stop_reason=null`," "new
`stop_reason=canceled` value," or "a brand-new envelope type." None
of those happen.

What actually happens: the cancelled assistant message keeps its
pre-cancel `stop_reason` (here `tool_use`), no `stop_reason=canceled`
is ever emitted, and the cancellation is signaled via a separate
`user`-role event with content `[{type: "text", text: "[Request
interrupted by user]"}]`. The marker lands AFTER any `tool_result`
events for in-flight tools (see finding 4).

The current assistant-only tailer filter (`type=="assistant"` AND
`message` is a map) drops this marker because it has `type=="user"`.
**This is a real gap for the eventual `pyry acp` consumer.** ACP's
`session/cancel` semantics expect an acknowledgment ("yes, I have
cancelled, no more tokens are coming"); the marker is the canonical
JSONL-side signal of that acknowledgment.

**Implication for the library:** the post-spike API should expose
the cancel marker to consumers. Two options:

- **Widen the tailer filter.** Pass `user`-role events with `text`
  content blocks upstream as a separate channel or as part of the
  same stream. Cheap; adds one filter case.
- **PTY-side detection.** The `❯-reappeared` predicate this spike
  uses (PTY quiescence) is structurally equivalent for the
  "cancel finished settling" semantics. Already implemented.

Both are reasonable; the library should probably do both, with the
JSONL marker as the precise signal and PTY quiescence as a backstop.

### 4. Tool subprocesses are NOT killed by ESC — they complete first

Probe 2's `ls /tmp` Bash subprocess executed fully and returned its
output (recorded as `tool_result` in the JSONL) BEFORE the
`[Request interrupted by user]` marker landed. Across all three
green runs, the sequence on disk is:

```
assistant(thinking, stop_reason=tool_use)
assistant(tool_use, stop_reason=tool_use)
user(tool_result content=<actual ls output>)            # tool DID run
user(text "[Request interrupted by user]")              # then cancel
```

The 200 ms `toolUseStartGrace` between observing `stop_reason=tool_use`
and sending the cancel keystroke was enough time for `ls /tmp` to
start AND finish; the subprocess emitted its result before the cancel
propagated. `pgrep -lf '^ls '` after spike exit returns nothing — no
orphan subprocess.

With a slower tool (a hypothetical `sleep 60` or a real long-running
Bash invocation), the behavior could be different — cancel might
propagate while the subprocess is still running, and the subprocess
might be killed by the parent claude process. This spike did not
probe that case; tracked as a follow-up.

**Implication for `pyry acp`:** consumers must surface this honestly.
"Cancel acknowledged, but the tool the model was running may finish
first." For fast tools this is invisible; for slow tools it could
matter to the human watching.

### 5. ESC, double-ESC, and Ctrl-C all work; ESC is preferred for its single-byte simplicity

Once the predicate was fixed (surprise 1) and the input was being
cleared (surprise 2), all three `-cancel-keystroke` values produced
green runs. Behavior is indistinguishable across keystrokes for the
recovery side; double-ESC takes ~1 s longer for Probe 1 (2.601 s vs
1.601 s) because claude likely treats the second `1b` as a separate
keystroke event and emits another redraw burst that delays the
PTY-quiescence predicate from firing.

**Library default: ESC.** Single byte, matches the `esc to interrupt`
on-screen hint, no ambiguity. Document the alternatives so consumers
debugging cancel-not-working can iterate without rebuilding.

### 6. JSONL line count varies across runs (22, 25, 22 — unlike spike #9's identical-27)

Spike #9 observed exactly 27 JSONL lines per run, identical envelope
sequence. This spike sees 22, 25, 22 — variance is in the number of
`attachment` envelopes claude emits between prompts (3-5 each) and
the presence/absence of mid-stream `last-prompt` / `ai-title`
envelopes after cancelled probes. Sample size 3; the variance is
real but small.

The headline empirical truth — what assistant lines appear, what
their `stop_reason` is, what content blocks they carry — is
identical across runs.

### 7. `❯-disappeared` still never fires

Same finding as spike #9. The `❯` idle glyph stays visible in the
rolling buffer through every cancel and recovery; spike #1 finding
#8 (spinner regex misses CSI-cursor-forward-prefixed verbs) is still
open, so `isIdle()` remains a heartbeat rather than a tight gate.
The PTY-quiescence predicate this spike adds (surprise 1) is what
actually carries the recovery signal; `isIdle()` is the necessary-
but-not-sufficient half (without `❯` visible, the input box isn't
ready for the next prompt; quiescence is what tells us claude has
stopped settling).

### 8. `claude 2.1.148` post-cancel byte volume shrank below the buffer-rotation threshold — `IsIdle` wedge surfaced in `waitReappeared` (ticket #69)

**Symptom.** Against `claude 2.1.148` (`claude-version.lock` pins
`2.1.144`, advisory only after #64), Probe 1 (`kindThinking`, the
1000-word monad essay) wedges in `waitReappeared` with
`watchdog: stuck after cancel for 30s`. Total wall time ~32.9 s
(Probe 1 setup + 30 s `cancelRecoveryLimit`). Reproduced in two
deterministic runs during ticket #69's Step 1 diagnosis.

**Mechanism — same family as finding 1, different consumer.** Finding
1 above documents the original predicate failure: post-cancel claude
emits ~1.4 KB of redraw, well under the 4 KB rolling-buffer cap, so
any spinner glyph painted before the cancel stays painted afterwards.
The original fix added the PTY-quiescence half of the predicate but
kept the `tuidriver.IsIdle` wrapper (`❯` present AND `✻` absent) — the
spinner-absent half was assumed to flip once the cancel completed and
no more spinner paints arrived. Across five `spike-cancel` runs in
issue #11's README that assumption held empirically on `claude
2.1.144`. On `claude 2.1.148` it doesn't — the empirical byte budget
shrank just enough (1577 bytes of churn since the last `✻` paint at
watchdog-trip time, well under 4096) that the pre-cancel `✻` paint
isn't rolled out. `IsIdle`'s spinner-absent half stays false forever,
the predicate wedges, the 30 s `cancelRecoveryLimit` trips.

The same wedge mechanism applies to `runRecovery` (Probes 3, 4, 5),
which carries the verbatim pre-#74 `spike-multi-turn` predicate
(`gotEndTurn ∧ tuidriver.IsIdle(rb) stable for idleStableWindow =
250ms`). Probes 3/4/5 didn't reach in Step 1's runs — Probe 1's 30 s
fire is earlier in the linear flow — but the predicate shape is the
same one PR #74 (ticket #73) already proved wedges, and the byte-count
substrate (post-`end_turn` claude emits well under 4 KB) is the same
as `waitReappeared`'s. Bundled in the same fix to avoid the
unmasking-on-next-`make e2e` trap.

**Fix.** Both predicates drop the `tuidriver.IsIdle` wrapper and use a
direct `bytes.Contains(tuidriver.StripANSI(snap), tuidriver.IdleGlyph)`
check for the `❯` half. The `rb.QuietFor() >= ptyQuietWindow` half
is unchanged in `waitReappeared` (it already had it) and is adopted
in `runRecovery` in place of `idleStableWindow` (which is removed —
no other consumers). The new predicate is weaker on the spinner-glyph
axis (the glyph is no longer required to be absent — that's exactly
the wedging clause being dropped) but strictly stronger on the
rendering-activity axis (1500 ms of zero PTY bytes vs. 250 ms of
glyph-absence — quiescence cannot be faked by a transient buffer
state).

Three consumers now share the PTY-quiescence pattern: `runTurn` in
`spike-multi-turn` (ticket #73 / PR #74), and `waitReappeared` +
`runRecovery` in this binary (ticket #69 / PR for #69). If the
duplication becomes a maintenance burden the library extraction at
#58–#62 is the right home.

**Why not bigger buffer / `spinnerGone` heuristic / no-op keystroke**:
spelled out in `docs/specs/architecture/73-spike-multi-turn-pty-quiescence.md`
§ *Why fix shapes 1, 2, 4 are not chosen*. Briefly: bigger buffer
changes the substrate for every consumer; the `spinnerGone` heuristic
regresses safety in multi-turn / recovery contexts by predicate-shape
luck; "send a no-op keystroke" couples the consumer contract to
claude's renderer internals.

**Post-fix verification (2026-05-22, `claude 2.1.148`, dispatcher
host).** `bin/spike-cancel -trust-folder=accept` exits 0 with three
`SUCCESS:` lines (Probes 3, 4, 5 are `kindRecovery`); total wall
time ~63 s, dominated by Probe 4's 1000-word monad essay re-run
(actual model time, not a wedge). No probe hits its watchdog.

## Why no automated tests

Per the spec § *Testing strategy*: verification is by execution
against real `claude`. Future `pkg/tuidriver/` tests (post-spike)
will mock the PTY; not this ticket.

## Reused helpers from spike-multi-turn

The following are copy-pasted unchanged from
`cmd/spike-multi-turn/main.go`, each with a `// copied from
cmd/spike-multi-turn/main.go — keep in sync until library extraction`
attribution comment in the source:

- Constants: `rollingBufferCap`, `statePollInterval`, `jsonlTailInterval`,
  `sessionFileWait`, `sessionFilePoll`, `watchdogTick`,
  `inactivityLimit`, `spinnerFreezeLimit`, `shutdownGrace`,
  `disappearedWindow`, `idleStableWindow`, `ptyRows`, `ptyCols`
- Regex/glyph: `spinnerRe`, `ansiRe`, `idleGlyph`
- Types/funcs: `rollingBuffer` + methods (plus the new `lastAppendAt`
  field and `quietFor` method — see surprise 1), `matchSpinner`,
  `isIdle`, `tracker` + all methods, `waitUntil`, `projectsDir`,
  `encodeCwd`, `resolveSession`, `openSessionJSONL`, `tailJSONL`,
  `isEndTurn`, `msgIDOf`, `extractByMsgID`, `typePrompt`

New in this spike:

- `spinnerGlyph` byte constant — `✻` UTF-8 bytes.
- `hasSpinnerGlyph` — bare-glyph detector (the full `spinnerRe`
  regex never matches; see spike #9 surprise 7 / spike #1 finding 8).
- `isToolUse` — same shape as `isEndTurn`, returns true on
  `stop_reason == "tool_use"`.
- `ProbeKind` enum + `probes []probeSpec` table — three kinds:
  `kindThinking`, `kindToolUse`, `kindRecovery`.
- `runProbe`, `runCancel`, `runRecovery`, `waitForKickoff`,
  `waitReappeared`, `logCancelEvent` — the cancel-probe driver.
- `sendCancel` — single bulk-write of the cancel keystroke.
- `clearInputLine` — Ctrl-U keystroke (see surprise 2).
- `parseCancelKeystroke` + `-cancel-keystroke` flag.
- `rollingBuffer.quietFor()` + `lastAppendAt` field — PTY-quiescence
  primitive (see surprise 1).
- Constants: `cancelRecoveryLimit`, `waitConditionLimit`,
  `toolUseStartGrace`, `ptyQuietWindow`, `clearLineSettle`.

All three spike binaries delete when `pkg/tuidriver/` is extracted;
the attribution comments are the index for which symbols the library
should pick up.

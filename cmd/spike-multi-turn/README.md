# spike-multi-turn

End-to-end PTY drive of three sequential interactive `claude` turns: simple
text, tool-use (Bash via `ls /tmp`), and slow thinking (Gauss sum 1..100).
Validates the multi-turn loop, the msg_id-grouped content extractor, and
the turn-boundary detection rule (JSONL `end_turn` ∧ ❯ idle ∧ stable).
See [ticket #9](https://github.com/pyrycode/tui-driver/issues/9) and
`docs/specs/architecture/9-spike-multi-turn.md`.

## Status

**Spike complete (2026-05-17).** Three runs (5, 6, 7) produced `SUCCESS:`
× 3 each, end-to-end in ~16 s wall time. The msg_id-grouping extractor
correctly reassembled turn 3's response from two assistant JSONL lines
(thinking + text blocks split, same msg_id, both tagged
`stop_reason=end_turn`) — empirical validation of ticket findings 1 (one
message ⇒ N JSONL lines) and 3 (`stop_reason` on every delta). Turn 2's
Bash tool-use turn produced two assistant lines with *different* msg_ids
(`tool_use` then final `text`) interleaved with one `user(tool_result)`
event — simpler than the pre-spike probe predicted but exercising the
same grouping principle. Two implementation surprises drove deviations
from the spec (see *Surprises / findings* below): (a) the default
`--permission-mode` blocks tool use on a modal — fixed by spawning with
`--permission-mode bypassPermissions`; (b) bulk-writing
`prompt+"\r"` to the PTY between turns leaves the input rendered but
unsubmitted after a tool-use turn — fixed by typing the prompt one byte
at a time with a 10 ms inter-byte delay.

## What it does

1. Resolves a UUID + deterministic JSONL path (same as spike-one-turn),
   logs `session-id-resolved` before spawn.
2. Spawns `claude --session-id <uuid> --permission-mode bypassPermissions`
   on a 120×40 PTY. The bypass is necessary for turn 2's `ls /tmp` to
   run without a permission modal that the spike does not interpret.
3. Waits for idle (`❯` glyph + spinner not visible), logs `idle-detected`.
4. Drives three sequential turns via `runTurn`:
   - Turn 1: `say hello` — baseline; one assistant JSONL line.
   - Turn 2: `list the files in /tmp` — Bash tool; two assistant lines
     (different msg_ids) + one user(tool_result).
   - Turn 3: `think carefully and compute 1+2+3+...+100, showing your
     reasoning` — extended thinking; two assistant lines with same
     msg_id, both `end_turn` (thinking + text split).
5. Per turn: types the prompt char-by-char into the PTY (~10 ms/byte),
   accumulates assistant JSONL events, detects turn-complete on the
   conjunction `gotEndTurn ∧ isIdle(rb) stable for ≥250 ms`, then groups
   accumulated events by `msg.id == latestEndTurnMsgID` and concatenates
   all `text`-type content blocks in JSONL order.
6. Watchdog (60 s inactivity, 30 s spinner-freeze) ticks throughout; on
   trip, SIGTERM → 3 s grace → SIGKILL → close PTY → drain goroutines.

| Turn | Prompt | Expected shape | What it exercises |
|------|--------|----------------|-------------------|
| 1 | `say hello` | single `text` block, one JSONL line | Baseline — msg_id grouping reduces to spike #1's single-record case |
| 2 | `list the files in /tmp` | `tool_use` block (msg_id A) → user(tool_result) → final `text` block (msg_id B, `end_turn`) | Tool-use streaming (ticket finding 2); extractor correctly picks up only the final `end_turn` msg_id's text |
| 3 | `think carefully and compute 1+2+3+...+100, showing your reasoning` | `thinking` block + `text` block under same msg_id, BOTH tagged `end_turn` | Multi-block message (ticket finding 1) + `stop_reason` on every delta (finding 3); msg_id grouping skips `thinking`, keeps `text` |

## How to run

Requirements: `claude` 2.1.x on `$PATH`, authenticated; Go 1.26+.

```sh
go build -o /tmp/spike-multi-turn ./cmd/spike-multi-turn
/tmp/spike-multi-turn
```

Optional flag: `-session-id <uuid>` to pin the session (same semantics
as spike-one-turn).

On success, stdout is three lines:

```
SUCCESS: Hello!
SUCCESS: Listed /tmp — ...
SUCCESS: ... 5050 ...
```

Stderr is the raw claude UI stream interleaved with the state log:

```
session-id-resolved id=<uuid> jsonl=<path>            # before spawn
idle-detected
turn=1 turn-start prompt="say hello"
turn=1 prompt-written
session-jsonl-opened path=<path> offset=0             # AFTER turn=1 prompt-written
turn=1 end-turn-detected msg_id=<id>
turn=1 ❯-reappeared
turn=1 assistant-text-extracted len=<n>
turn=2 turn-start prompt="list the files in /tmp"
turn=2 prompt-written
turn=2 end-turn-detected msg_id=<id>
turn=2 ❯-reappeared
turn=2 assistant-text-extracted len=<n>
turn=3 turn-start prompt="think carefully ..."
turn=3 prompt-written
turn=3 end-turn-detected msg_id=<id>
turn=3 ❯-reappeared
turn=3 assistant-text-extracted len=<n>
complete elapsed=<dur>
shutdown-signalled
```

The optional `turn=N ❯-disappeared` line is emitted only when `❯` is
observed missing from the rolling buffer within ~500 ms of
`prompt-written`. In every run captured so far it has NOT fired — see
*What `❯` actually does between turns* below.

`thinking-detected` / `spinner-gone` (spike #1's slow-path lines) never
fired here either: the spinner regex still misses every observed verb
form (`Crunched for 5s`, `Cooked for 6s`, etc.) because of the CSI
cursor-forward issue documented in spike #1 finding #8. The fast path
absorbs this without wedging.

### Verifying clean exit

```sh
pgrep -lf 'claude --session-id' || echo 'no orphans — good'
```

### Negative-path check

From another terminal, while the spike is mid-turn:

```sh
pkill -TERM -f 'claude --session-id'
```

The spike exits within a few seconds. Tested 2026-05-17: pkill landed
mid-turn-1; watchdog tripped via `spinner counter frozen at 1s for 30s`
(claude was rendering the spinner when killed, so `tracker.observeSpinner`
saw `visible=true` with no counter increment), `shutdown-signalled`
fired, no orphans remained.

## Per-turn observed timing

`turn-start → end-turn-detected` includes the `idleStableWindow` of
250 ms; the actual end_turn JSONL event arrives ~250 ms earlier than
the log line. `end-turn-detected → ❯-reappeared` is effectively zero
because the predicate's `isIdle` half is already satisfied when the
loop exits.

| Run | Turn | start→end (ms) | end→❯-reappeared (ms) | total (ms) | notes |
|----:|------|---------------:|----------------------:|-----------:|-------|
| 5 | 1 | 5862 | 0 | 5862 | warm-start session (first run after session-id resolution) |
| 5 | 2 | 8405 | 0 | 8405 | Bash tool ran on `/tmp` |
| 5 | 3 | 5905 | 0 | 5905 | slow thinking — 224-char response |
| 6 | 1 | 2227 | 0 | 2227 | |
| 6 | 2 | 6760 | 0 | 6760 | 12-char `text` response after Bash ("Files listed." in another run) |
| 6 | 3 | 6803 | 0 | 6803 | |
| 7 | 1 | 2820 | 0 | 2820 | |
| 7 | 2 | 7061 | 0 | 7061 | |
| 7 | 3 | 5408 | 0 | 5408 | |

Total wall time per run: ~16 s (run 6, run 7) up to ~21 s (run 5). Most
of the variance is in turn 2's tool-use response length and turn 1's
warm-up.

## What `❯` actually does between turns

`❯` (the idle glyph) is **continuously visible** in the rolling buffer
across the spinner. The `turn=N ❯-disappeared` log line never fired
across 3 successful runs + 1 negative-path run, even though each turn
visibly rendered a spinner phrase (`✻ Crunched for 5s`, `✻ Cooked for
6s`, `✻ Baked for 5s`, `✻ Sautéed for 5s`).

Mechanism (from raw PTY inspection): claude does NOT clear the input
prompt area when processing. The input line keeps showing `❯` with a
faint hint area; the spinner is rendered on a separate line (or in the
status block above). The `isIdle()` predicate — `❯ glyph present AND
spinner regex does NOT match` — is therefore satisfied by the `❯` half
the entire time. The spinner regex half is what would tell us "claude
is thinking," but per spike #1 finding #8 the regex still misses
real-world spinner renderings (CSI cursor-forward between glyph and
verb, ellipsis-form `Brewing…` instead of `Brewing for Ns`, etc.). The
result is that for the spike's purposes, `isIdle()` returns true even
while claude is processing.

This is why the turn-boundary detection rule is *conjunction*: JSONL
`end_turn` is what truly says "model done speaking"; `❯` is a
necessary but-not-sufficient TUI-side signal. If the spinner regex
gets fixed (a separate ticket — finding #8 from spike #1), `isIdle()`
becomes meaningfully tighter and the conjunction's `isIdle` half
becomes a real gate rather than a heartbeat. Until then,
the predicate is effectively "wait for JSONL `end_turn` and let the
PTY catch up briefly" — the 250 ms `idleStableWindow` exists for the
catch-up half (see *Surprise 2* below for why it stayed despite being
weakened by the regex miss).

## msg_id grouping behavior

JSONL line structure was identical across all three successful runs
(27 lines total per run, same envelope sequence). Below is run 7's
event sequence:

| Idx | type | role | stop | msg_id | content blocks | notes |
|----:|------|------|------|--------|----------------|-------|
|  1 | permission-mode | — | — | — | — | claude startup |
|  2 | file-history-snapshot | — | — | — | — | claude startup |
|  3 | user | user | — | — | string | turn 1's prompt: `"say hello"` |
|  4 | attachment | — | — | — | — | claude metadata |
|  5 | attachment | — | — | — | — | claude metadata |
|  6 | **assistant** | assistant | **end_turn** | msg_01LYL… | **text** | turn 1's response |
|  7 | system | — | — | — | — | claude metadata |
|  8 | file-history-snapshot | — | — | — | — | post-turn snapshot |
|  9 | user | user | — | — | string | turn 2's prompt: `"list the files in /tmp"` |
| 10 | attachment | — | — | — | — | claude metadata |
| 11 | attachment | — | — | — | — | claude metadata |
| 12 | ai-title | — | — | — | — | claude metadata |
| 13 | **assistant** | assistant | **tool_use** | msg_011JC… | **tool_use** | turn 2 tool call (msg A) |
| 14 | user | user | — | — | tool_result | Bash output (filtered out by tailer; visible on disk only) |
| 15 | last-prompt | — | — | — | — | claude metadata |
| 16 | ai-title | — | — | — | — | claude metadata |
| 17 | permission-mode | — | — | — | — | claude metadata |
| 18 | **assistant** | assistant | **end_turn** | msg_01YEr… | **text** | turn 2 final answer (msg B — different msg_id from line 13!) |
| 19 | system | — | — | — | — | claude metadata |
| 20 | file-history-snapshot | — | — | — | — | post-turn snapshot |
| 21 | user | user | — | — | string | turn 3's prompt |
| 22 | **assistant** | assistant | **end_turn** | msg_01W3f… | **thinking** | turn 3 thinking block |
| 23 | **assistant** | assistant | **end_turn** | msg_01W3f… | **text** | turn 3 text — same msg_id as line 22, both `end_turn`! |
| 24 | system | — | — | — | — | claude metadata |
| 25 | last-prompt | — | — | — | — | claude metadata |
| 26 | ai-title | — | — | — | — | claude metadata |
| 27 | permission-mode | — | — | — | — | claude metadata |

The headline observations:

- **Turn 1 was the trivial case.** One assistant line, one msg_id, one
  `text` block. msg_id grouping reduces to "find the single
  end_turn-tagged line and read its content[0].text" — the same shape
  spike #1's extractor handled.

- **Turn 2's tool-use and final-text are TWO different messages.** The
  tool_use envelope (line 13) has `msg_id=msg_011JC…` and
  `stop_reason=tool_use`; the final text (line 18) has
  `msg_id=msg_01YEr…` and `stop_reason=end_turn`. The msg_id grouping
  rule ("latest end_turn line's msg_id is the response message")
  correctly picks msg_01YEr… and skips the tool_use message entirely.
  This is simpler than the operator's pre-spike probe predicted: that
  probe showed 5 lines under ONE msg_id (1 thinking + 4 tool_use) with
  4 interleaved user(tool_result) events for a heavier task. Our
  observed "list `/tmp`" produced a single tool call — claude returned
  the entire listing in one Bash invocation. The grouping rule handles
  both shapes correctly because the rule keys on the `end_turn`
  msg_id; tool_use msg_ids are inert noise to it.

- **Turn 3 is the load-bearing case for msg_id grouping.** Lines 22
  and 23 share `msg_id=msg_01W3f…`, both tagged `stop_reason=end_turn`,
  but carry different content block types (`thinking` vs `text`).
  Spike #1's single-record extractor would have read line 22 first and
  returned an empty string (no `text` block). The msg_id grouping
  extractor walks both records, collects their `text` blocks (only
  line 23 has one), and produces the correct answer. **This is the
  primary thing the spike validated empirically — one message can split
  into multiple JSONL records by content-block type, all sharing the
  same msg_id and stop_reason.**

The grouping rule's "use the LATEST `end_turn` line's msg_id" robustness
was not really tested here — both runs had only one msg_id per turn's
end_turn-tagged set. A pathological case (e.g., model emits a new
end_turn-tagged message after its first one within the same turn) was
not observed across 3 runs. If it ever happens, the rule still works:
the latest msg_id wins, the extractor picks up that msg_id's blocks.

## New event types observed beyond spike #1's catalog

None. The 8 envelope types observed (`permission-mode`,
`file-history-snapshot`, `user`, `attachment`, `ai-title`,
`assistant`, `system`, `last-prompt`) are exactly spike #1's catalog.
Notably absent: `queue-operation` was mentioned in the
operator's pre-spike probe as a candidate; it did not appear in any of
the 3 spike-#2 runs. May correlate with permission-mode default
prompts that the `bypassPermissions` flag suppresses, but that's
speculation — not investigated. `last-prompt` fired 2× per run
(after turn 2 and turn 3 specifically — turn 1 has none),
suggesting it correlates with multi-turn continuation rather than
firing per-turn unconditionally. Out of scope to dig further.

## Spinner verbs captured

| Verb (raw, post-ANSI-strip) | Run(s) | Form | Regex matched | Notes |
|-----------------------------|--------|------|---------------|-------|
| `Cooked for 5s` | 5, run_neg2 | `for Ns` | N (CSI between glyph and verb) | Same finding #8 cause as spike #1 — CSI cursor-forward eaten by strip |
| `Churned for ` (counter missing) | 5, run_neg3 | counter-less | N | Spinner rendered before counter was painted |
| `Baked for 11s` / `Baked for 5s` / `Baked for 3s` | 5, 7 | `for Ns` | N | Most common verb |
| `Sautéed for 5s` | 6 | `for Ns` | N | New verb (unicode in verb) |
| `Crunched for 4s` / `Crunched for ` | 7, run_neg4 | `for Ns` | N | New verb |
| `Sprouting…` | (earlier debugging) | ellipsis form | N | Ellipsis form, no counter |
| `Ideating…` | (earlier debugging) | ellipsis form | N | Ellipsis form |

Across all 5+ runs captured during this spike, the spinner regex
`✻\s+(\S+(?:\s+\S+)?)\s+for\s+(?:(\d+)m\s+)?(\d+)s` matched **zero
times**. Same root causes as spike #1 finding #8: (a) the CSI
cursor-forward between `✻` and the verb (`\x1b[1C`) is removed by the
ANSI strip without becoming whitespace, leaving `✻Verb for Ns` with no
gap; (b) ellipsis-form verbs (`Sprouting…`) have no `for Ns` counter.
Spike #2 confirms the finding-8 issue is unchanged; out of scope to
fix here. Useful side data for whoever picks up the spinner-regex
ticket: verb set is open-ended and includes unicode (`Sautéed`).

## Surprises / findings

### 1. Default `--permission-mode` blocks tool use on a modal

The architect's spec assumed `claude` in interactive mode would invoke
the Bash tool unattended for turn 2's `list the files in /tmp` ("under
the hood, via the interactive permission-mode default"). Empirically
the default mode renders a modal:

```
Bash command ls /tmp
Listfilesin/tmp
❯ 1. Yes
  2. Yes, allow reading from tmp/ from this project
```

The spike doesn't interpret modals (deferred per ticket). With the
default permission mode, turn 2 wedges at `prompt-written` until the
60 s inactivity watchdog trips.

**Fix taken for this spike:** spawn with
`--permission-mode bypassPermissions`. Recorded in `cmd.Args` as a
two-flag pair (`--permission-mode`, `bypassPermissions`) and called
out in the source comment. This keeps the spike's scope tight
(modal-handling stays out) while letting turn 2 exercise the
JSONL-side tool_use behavior that is the actual subject of the turn.

**Follow-up implication for the library API:** consumers (`pyry acp`)
will need to make permission-mode configurable. The library cannot
default to `bypassPermissions` for arbitrary callers — that's a
deliberate consumer choice tied to trust model. The library should
expose permission-mode as a per-session option and let the consumer
pick.

### 2. Bulk-writing `prompt+"\r"` between turns leaves the input rendered but unsubmitted after tool-use turns

The first three runs (1, 2, 3) all succeeded on turn 1 (simple text)
and turn 2 (tool use) but wedged on turn 3 (slow thinking) with the
60 s inactivity watchdog tripping. Raw PTY bytes showed turn 3's
prompt fully rendered in claude's input area:

```
thinkcarefullyandcompute1+2+3+...+100,showingyourreasoning\r
```

…followed by the input-box redraw, the "bypass permissions on" footer,
and silence — no spinner, no JSONL events, never a response.

Hypothesis (not confirmed by source-diving claude, just by behavior):
the input handler does NOT recognise `\r` as submit when it arrives in
the same buffered write as the prompt body, *after a tool-use turn's
wind-down redraw is still in flight*. Turn 1 succeeded because there
was no prior wind-down to race with (just session boot). Turn 2's
prompt fired after turn 1's simple end_turn, where there is also
relatively little redraw work. Turn 3's prompt fires after turn 2's
tool-use wind-down — which includes the spinner collapse, the
"bypass permissions" footer redraw, MCP-status-line redraw, and
ai-title / last-prompt envelope writes — and that window is enough to
swallow a bulk `\r`.

**Fix taken for this spike:** `typePrompt` writes the prompt one byte
at a time with a 10 ms inter-byte delay, then a 50 ms pause, then a
final `\r`. With this change, all three turns submit reliably across
3 consecutive runs. The 10 ms/byte cadence is empirical (chosen by
the first attempt to fix; not tuned); the 50 ms tail pause is
belt-and-suspenders.

**Why this matters for the library:** the post-spike `pkg/tuidriver/`
API will have to make some choice here. Options visible from where we
are now:

- **Always char-by-char with an inter-byte delay.** Most robust;
  trivially predictable; ~700 ms latency overhead for a 64-char
  prompt. For a library driving an interactive AI assistant, the
  latency is negligible (responses take seconds anyway).
- **Bulk-write with a stability gate.** Wait for some "TUI quiescence"
  signal (no PTY bytes for ≥N ms) before bulk-writing. Implementation
  complexity goes up; depends on a stability heuristic that this
  spike didn't measure precisely.
- **Hybrid.** Try bulk first; if no JSONL `user` event appears within
  some deadline, fall back to char-by-char. Surfaces complexity
  unnecessarily and doubles the failure-mode surface.

The library should probably default to char-by-char and expose the
inter-byte delay as a per-session option for tuning. The 250 ms
`idleStableWindow` gate is also load-bearing — see surprise 3.

### 3. The 250 ms `idleStableWindow` is mostly redundant once typePrompt exists, but it's cheap and the conjunction is the right shape

The architect's spec specified the turn-complete predicate as
`gotEndTurn ∧ isIdle(rb)`. The first implementation interpreted that
literally: predicate fires on the first tick where both halves hold.
Empirically, that worked but was fragile: a transient idle
observation while the TUI was mid-redraw could let the predicate fire
before the input area had settled. After noticing the turn-3 wedge,
the implementation gained an `idleStableWindow = 250 ms` requirement
(isIdle must be continuously true for ≥250 ms). Turning that up to
1500 ms didn't fix the wedge — `typePrompt` did.

In hindsight, the wedge was the bulk-write race (surprise 2), not a
predicate-timing race. With `typePrompt` shipping the prompt over
~700 ms, the natural inter-turn pause is dominated by the typing, and
`idleStableWindow` could be 0 ms with no observable change in
success rate. It's kept at 250 ms because the cost is negligible
(turns take seconds anyway) and the predicate's `isIdle` half is the
correct structural shape — JSONL `end_turn` says "model done
speaking," `isIdle` says "TUI redrawn back to input." If finding #8
ever gets fixed and `isIdle` becomes a tight signal, the
`idleStableWindow` becomes a useful debounce; until then, it's
defensive padding.

### 4. JSONL line count is remarkably consistent across runs

All 3 successful runs produced exactly 27 JSONL lines with the same
envelope sequence around the assistant messages. The variance was
purely in `content[].text` length within the assistant lines (turn 2's
listing description ranged from 12 chars to 259 chars; turn 3's
arithmetic prose ranged from 224 chars to 341 chars). Sample size 3 —
not a guarantee, but suggestive that the per-turn envelope shape is
deterministic given the same probe prompts.

The `system` event fires once after every turn's `end_turn` (3 system
events per run); `permission-mode` fires at startup + after each turn
(3 per run, in this configuration); `last-prompt` fires twice — once
after turn 2's tool-use sequence and once after turn 3's. Spike-one-turn
saw `last-prompt` fire once per turn (different rate); this spike's
multi-turn data suggests `last-prompt` correlates with multi-turn
continuation, not per-turn unconditionally. Single data point — needs
more runs to be sure.

### 5. Turn 3's "two end_turn lines for the same msg_id" is the empirical centerpiece

Spike #1's "one assistant record carries the full answer" assumption
was a degenerate case. Spike #2's turn 3 produces:

```
{type:"assistant", message:{id:"msg_01W3f…", stop_reason:"end_turn",
  content:[{type:"thinking", ...}]}}
{type:"assistant", message:{id:"msg_01W3f…", stop_reason:"end_turn",
  content:[{type:"text", text:"Using Gauss's formula..."}]}}
```

Two lines, same msg_id, both tagged `end_turn`, different content
block types. The "first end_turn line" rule would have returned the
thinking block (which has no `text` field, so empty string). The
"latest end_turn line" rule would have returned the text block — but
only by luck of arrival order, not by anything structural. The
correct rule is **msg_id grouping**: collect every record matching the
target msg_id, walk their content blocks in JSONL order, filter to
`type == "text"`. That's what the spike implements and what the
post-spike library must implement. This is the most important thing
to internalise from this ticket.

## Why no automated tests

Per `docs/specs/architecture/9-spike-multi-turn.md` § *Testing
strategy*: verification is by execution against real `claude`. Future
`pkg/tuidriver/` tests (post-spike) will mock the PTY; not this
ticket.

## Reused helpers from spike-one-turn

The following are copy-pasted unchanged or near-unchanged from
`cmd/spike-one-turn/main.go`, each with a `// copied from
cmd/spike-one-turn/main.go — keep in sync until library extraction`
attribution comment in the source:

- Constants: `rollingBufferCap`, `statePollInterval`, `jsonlTailInterval`,
  `sessionFileWait`, `sessionFilePoll`, `watchdogTick`,
  `inactivityLimit`, `spinnerFreezeLimit`, `shutdownGrace`, `ptyRows`,
  `ptyCols`
- Regex/glyph: `spinnerRe`, `ansiRe`, `idleGlyph`
- Types/funcs: `rollingBuffer` + methods, `matchSpinner`, `isIdle`,
  `tracker` + all methods, `waitUntil`, `projectsDir`, `encodeCwd`,
  `resolveSession`, `openSessionJSONL`, `tailJSONL`, `isEndTurn`

New in this spike:

- `prompts []string` — the three probe prompts.
- `runTurn` — single-turn driver. Called 3× by the orchestrator. Turn 1
  uses a `postPromptHook` to open the JSONL and start the tailer
  goroutine (because the JSONL doesn't exist until the first input
  lands).
- `extractByMsgID(events, targetID)` — msg_id-grouped text extractor.
  Walks all events whose `.message.id == targetID`, collects their
  `text`-typed content blocks in arrival order. Replaces spike #1's
  single-record `extractAssistantText` (no longer used here).
- `typePrompt(ptmx, prompt)` — char-by-char writer with 10 ms
  inter-byte delay, 50 ms tail pause, then `\r`. See surprise 2.
- `idleStableWindow` constant + `idleSince` tracking inside `runTurn`'s
  predicate loop. See surprise 3.

Both spikes are scheduled for deletion when `pkg/tuidriver/` is
extracted; until then, the duplication is intentional and the
attribution comments are the index for which symbols the library
should pick up.

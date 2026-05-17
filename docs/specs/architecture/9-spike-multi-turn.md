# Spec: spike — multi-turn interactive `claude` (tool-use, slow-thinking, msg_id-grouped extraction)

**Ticket:** [#9](https://github.com/pyrycode/tui-driver/issues/9)
**Size:** S
**Status:** ready for development

## Files to read first

These are the developer's turn-1 reading list. Load them before touching code; almost everything the spike needs is already present in spike #1's binary and its README.

- `cmd/spike-one-turn/main.go` (entire file, ~575 lines) — the source of every helper this spike copy-pastes. The reusable surface in this file (in the order it appears) is `rollingBuffer`, `matchSpinner`, `ansiRe`/`spinnerRe`/`idleGlyph`, `isIdle`, `tracker` + `newTracker` + `recordTransition` + `observeSpinner` + `checkWatchdog`, `waitUntil`, `projectsDir` + `encodeCwd`, `resolveSession`, `openSessionJSONL`, `tailJSONL`, `isEndTurn`, `extractAssistantText`, and the shutdown defer in `run()`. All of these come over unchanged or near-unchanged — see § *Reuse policy* below.
- `cmd/spike-one-turn/README.md` § *Surprises / findings* (esp. #2, #8, #9) — the empirical facts the spike #2 design relies on: the spinner regex never matches in practice (fast path always wins), and `--session-id` defers JSONL creation until first input.
- `docs/knowledge/architecture/jsonl-layout.md` — encoded-cwd rule (`/` AND `.` → `-`), the deterministic-path discovery flow, the assistant-only parser filter, and the `last-prompt` / `attachment` / `system` envelope types the tailer must keep skipping.
- `docs/specs/architecture/1-spike-one-turn.md` § *Concurrency model* + § *State log lines* — the three-goroutine shape and the named-event ordering this spike extends (adds `turn=N` prefix, adds `❯-disappeared` / `❯-reappeared`, adds `msg_id=<id>` to `end-turn-detected`).
- `docs/specs/architecture/7-deterministic-session-id.md` — the `--session-id` + post-prompt stat-poll pattern this spike inherits without modification.
- `CLAUDE.md` (repo root) — scope discipline. The hard rule: this is still a spike, no `pkg/tuidriver/` extraction.

Vault context (read iff a finding's *why* gets fuzzy mid-implementation, not by default):

- `📋 Projects/2026-05-16 - tui-driver/Findings.md` § *Pre-spike-#2 multi-turn JSONL observations (2026-05-17)* — the three behaviors this spike validates (multi-block messages, tool-use interleaving, `stop_reason` on every delta), with the example msg ID and counts from the 190-event JSONL the operator probed.

## Context

Spike #1 validated the one-turn happy path on `"What is 2+2?"` — a prompt with no thinking block, no tool use, and a single `content[].text` block on a single JSONL line. Three behaviors that real multi-turn + tool-use sessions exhibit were therefore untouched:

1. **One Anthropic message ⇒ N JSONL lines, one per content block.** All share `msg_id` and `stop_reason`. Spike #1's "concatenate text from one record" silently truncates content on any message that has more than one block (thinking + text, thinking + text + tool_use, etc.).
2. **Tool-use streams blocks interleaved with their `user(tool_result)` events.** A single user turn can produce 10+ JSONL events under one `msg_id` with `stop_reason=tool_use`.
3. **`stop_reason` is on every delta of a message, not only the last line.** "First line with `stop_reason=end_turn`" ≠ "turn complete."

Spike #2 runs three sequential turns end-to-end against a real interactive `claude` to validate (1) and (3) empirically (a tool-using turn exercises (2) for the README, but extraction does not interpret tool blocks) and to capture the per-probe timing/verb/event-type data the README documents. Cancellation, modal handling, and library extraction stay out of scope per the ticket body.

## Empirical facts already in hand (do NOT rediscover)

These come from spike #1 and the operator's pre-spike-#2 JSONL probe (2026-05-17). Believe this spec over the ticket body where they disagree.

1. **`--session-id` JSONL deferral.** Interactive `claude --session-id <uuid>` does not write the JSONL until the first user input arrives. `os.Stat(jsonlPath)` fails with `IsNotExist` at `idle-detected`. Poll **after** `prompt-written`, tail from offset 0 — same pattern as spike #1 post-#7.
2. **Assistant-only tailer filter is enough.** `obj["message"] is a map AND obj["type"] == "assistant"` skips every claude-internal envelope (`permission-mode`, `file-history-snapshot`, `user`, `attachment`, `ai-title`, `system`, `last-prompt`, `queue-operation`, plus any future ones). Keep the filter unchanged. `user(tool_result)` events are visible in the JSONL on disk for the README's structural-counts pass; they do not need to flow through the tailer channel.
3. **Spinner regex misses every observed verb so far.** Either the verb is in ellipsis form (`✻ Brewing…`, `✻ Skedaddling…`) with no `for Ns` counter, or a CSI cursor-forward sits between glyph and verb and the ANSI strip removes it. The fast path (no `thinking-detected`, no `spinner-gone`) is therefore the expected path for turns 1 and 2 of this spike. Turn 3 (slow-thinking prompt) is the deliberate observation surface for the spinner — if it still doesn't render or match, that's a finding worth recording in the README; the spike still passes via JSONL alone (per spike #1 finding #4).
4. **`❯` is a reliable "TUI ready for input" indicator.** Spike #1's `isIdle()` predicate (`❯` present in the ANSI-stripped buffer AND spinner regex does NOT match) is the canonical reuse for turn-boundary detection's PTY-side condition. The ticket's "❯ visible at the start of a recent line" framing is satisfied by `isIdle()` as long as the buffer is sized to hold the TUI's input region — the existing 4 KB cap suffices.

## Design

### Layout

```
cmd/spike-multi-turn/
  main.go      # everything — copy-pasted helpers + 3-turn loop + msg_id extractor
  README.md    # how to run, per-turn timings, ❯ behavior, msg_id observations, new envelope types, verbs
```

No `pkg/tuidriver/` content. No shared helpers extracted from `cmd/spike-one-turn/`. Copy-paste with attribution comments — both spikes will be deleted when the library lands.

### Reuse policy

Bring these from `cmd/spike-one-turn/main.go` unchanged or near-unchanged. Each gets a 1-line attribution comment naming its source. The intent is for the reused surface to be greppable later when the library extraction picks a subset of these to promote.

Unchanged (lift verbatim, prepend `// copied from cmd/spike-one-turn/main.go — keep in sync until library extraction`):

- Constants: `rollingBufferCap`, `statePollInterval`, `jsonlTailInterval`, `sessionFileWait`, `sessionFilePoll`, `watchdogTick`, `inactivityLimit`, `spinnerFreezeLimit`, `shutdownGrace`, `ptyRows`, `ptyCols`
- Regex/glyph: `spinnerRe`, `ansiRe`, `idleGlyph`
- Types/funcs: `rollingBuffer` + methods, `matchSpinner`, `isIdle`, `tracker` + all methods, `waitUntil`, `projectsDir`, `encodeCwd`, `resolveSession`, `openSessionJSONL`, `tailJSONL`, `isEndTurn`, `extractAssistantText`

Near-unchanged (lift, then modify as called out):

- `promptText` constant → delete. Replace with a `prompts []string` slice of three entries (see § *Turn-loop driver*).
- The single-shot termination loop in `run()` (lines 230-265 of spike #1) → delete. Replace with the per-turn driver in § *Turn-loop driver*.
- State log lines after `session-jsonl-opened` → all gain `turn=N` prefix (the lines that fire once per turn). Session-level lines (`session-id-resolved`, `idle-detected`, `session-jsonl-opened`, `shutdown-signalled`) keep their existing prefix-less shape.

### New code (the spike's actual contribution)

Three additions on top of the reused surface, all in `main.go`:

1. **Turn-loop driver** — a `runTurn` helper called three times in sequence.
2. **msg_id grouping extractor** — a function that, given a list of accumulated assistant events and the msg_id of the latest `end_turn`-tagged line, returns the concatenated `text`-block content.
3. **Per-turn `❯-disappeared` observer** — a ~500 ms window after `prompt-written` that logs the optional line iff `❯` is observed missing from the buffer at least once.

#### Turn-loop driver

Signature (in pseudocode — exact Go is the developer's call; this is contract, not implementation):

```go
// runTurn drives one user→assistant exchange. Assumes the orchestrator
// has already reached the idle state for THIS turn (turn 1 = post-`idle-detected`;
// turns 2+ = post-prior-turn's `❯-reappeared`).
//
// Returns the extracted assistant text and the msg_id used for extraction.
// Errors via context cancellation (watchdog) propagate as for spike #1.
func runTurn(
    ctx context.Context,
    logger *log.Logger,
    turn int,
    prompt string,
    ptmx *os.File,
    rb *rollingBuffer,
    eventCh <-chan map[string]any,
    tr *tracker,
) (assistantText string, msgID string, err error)
```

Behavior contract (the developer implements; the order of the named log lines is the contract):

1. Log `turn=N turn-start prompt="<prompt>"` (quote the prompt; the test prompts are short and stable, no need to escape).
2. Write `prompt + "\r"` to `ptmx`. Log `turn=N prompt-written`. Bump `tr` via `recordTransition("turn=N prompt-written")`.
3. Spawn a short-lived `❯-disappeared` observer goroutine (or inline-poll loop): for up to 500 ms, poll `isIdle(rb.snapshot())` at the existing `statePollInterval` (50 ms). If any poll returns `false`, log `turn=N ❯-disappeared` once and exit the observer. If all polls return `true`, exit silently. This line is optional per the AC; do NOT log it if `❯` was never observed missing.
4. Accumulate assistant events arriving on `eventCh` into a local `[]map[string]any` slice. For each event, also test `isEndTurn(ev)` — when it returns true, remember the event's `msg_id` (overwrite on each new `end_turn`; the LATEST end_turn-tagged line's msg_id is the response message per the ticket's grouping rule).
5. Termination predicate (checked after each new event or every 50 ms tick): at least one `end_turn`-tagged event has been seen for this turn AND `isIdle(rb.snapshot())` returns true. The `isIdle` half is what guards against extracting before the TUI has finished redrawing — the JSONL-side `end_turn` can land while the PTY is still painting the input line.
6. When the predicate holds: log `turn=N end-turn-detected msg_id=<id>`. Bump `tr`.
7. Wait one more time for `isIdle` to be observed (it already is, from step 5 — this is the "❯ reappeared after the response finished rendering" signal). Log `turn=N ❯-reappeared`. Bump `tr`. The two log lines (`end-turn-detected` and `❯-reappeared`) may be effectively simultaneous because of step 5's conjunction — that's fine, the README's timing table can show 0 ms deltas where applicable.
8. Call the msg_id extractor (next section) with the accumulated events and the remembered msg_id. Log `turn=N assistant-text-extracted len=<n>`. Bump `tr`.
9. `fmt.Printf("SUCCESS: %s\n", assistantText)`.
10. Return `(assistantText, msgID, nil)`.

The orchestrator drives:

```go
prompts := []string{
    "say hello",
    "list the files in /tmp",
    "think carefully and compute 1+2+3+...+100, showing your reasoning",
}
for i, p := range prompts {
    if _, _, err := runTurn(ctx, logger, i+1, p, ptmx, rb, eventCh, tr); err != nil {
        return fmt.Errorf("turn %d: %w", i+1, err)
    }
}
```

Between turns, the orchestrator does NOT re-run `waitUntil(... isIdle ...)` — step 7's `❯-reappeared` already established idle. Turn N+1's step 2 (`prompt-written`) lands directly on the idle TUI.

#### msg_id grouping extractor

```go
// extractByMsgID collects every assistant event in `events` whose
// `.message.id == targetID`, concatenates their `text`-typed content
// blocks in JSONL arrival order, and returns the result. Skips
// `thinking` and `tool_use` blocks. Returns "" if no matching events
// exist (which would indicate a logic error upstream — extractor is
// only called after at least one end_turn-tagged event for this msg_id
// has been observed).
func extractByMsgID(events []map[string]any, targetID string) string
```

Implementation notes:

- The msg_id lives at `obj["message"]["id"]`. Per the JSONL probe, all delta lines of the same message share it. Use `string` equality.
- For each matching event, walk `obj["message"]["content"]` (slice of `map[string]any`); for any block with `type == "text"`, append `block["text"]` to a `strings.Builder`. This is the same per-block walk as `extractAssistantText` in spike #1 — the difference is that the input is now a slice of events for one msg_id, not a single event.
- The output is the user-visible answer. It may legitimately be empty if a message had only `thinking` blocks under `end_turn` — record that as a finding in the README if it happens (it shouldn't, for the three probe prompts).

#### Watchdog

Reuse `tracker` unchanged. Each named-state log line in the turn driver calls `tr.recordTransition("turn=N <name>")` so the 60 s inactivity deadline resets on every per-turn transition. The deadline carries across turns; a turn that takes >60 s without a single named transition (e.g., a stuck tool call mid-stream) trips the watchdog cleanly.

The 30 s spinner-freeze deadline keeps spike #1's semantics — only active while the spinner regex matches. Turn 3's slow-thinking prompt may exercise this for the first time; if `tracker.observeSpinner` returns a frozen counter for 30 s while the spinner is continuously visible, watchdog trips. If the spinner regex still never matches (per finding #3 in § *Empirical facts*), this stays dormant.

### Concurrency model

Same three-goroutine shape as spike #1. The turn loop runs entirely on the `main` orchestrator goroutine; the PTY reader and JSONL tailer span the whole session.

| Goroutine | Owns | Exit |
|---|---|---|
| `main` orchestrator | linear state machine across 3 turns, watchdog tick, msg_id extraction | last turn returns OR watchdog trips OR error |
| PTY reader | rolling buffer (mutex-protected); mirrors raw bytes to stderr | EOF from PTY master (when shutdown closes it) |
| JSONL tailer | events channel (buffered, size 32); filters to assistant-with-message | context cancellation |

The events channel is shared across all three turns — there's only one tailer for the whole session. The turn driver reads from it during step 4; events are NOT scoped to a turn at the channel level. The "which turn does this event belong to" question is answered by accumulation order + msg_id, which is unique per message.

### Shutdown

Identical to spike #1 — single `defer` running the SIGTERM → 3 s grace → SIGKILL → close PTY → cancel context → `wg.Wait()` sequence under `sync.Once`. Fires on every exit path (last-turn success, watchdog trip, PTY error, turn-loop error). The `shutdown-signalled` log line fires once at the start of the shutdown body.

### State log lines (the full sequence the spike emits)

Session-level (fire once each):

```
session-id-resolved id=<uuid> jsonl=<path>
idle-detected
session-jsonl-opened path=<path> offset=0
shutdown-signalled
```

Per-turn (fire once per turn N ∈ {1,2,3}; same names as the AC's required set):

```
turn=N turn-start prompt="..."
turn=N prompt-written
turn=N ❯-disappeared            # optional; only if observed within ~500 ms
turn=N end-turn-detected msg_id=<id>
turn=N ❯-reappeared
turn=N assistant-text-extracted len=<n>
```

Ordering invariant: `idle-detected` fires once before any `turn=1 turn-start`. `session-jsonl-opened` fires after `turn=1 prompt-written` (the deferral; see empirical fact #1). The watchdog log line (if it trips) is prefixed `watchdog:` per spike #1's convention so `grep '^.*watchdog:' stderr.log` still finds it.

### Three probe prompts (verbatim — the test set)

These are the AC's exact strings; the spike must use them unmodified so README observations are reproducible.

| Turn | Prompt | Expected shape | What it exercises |
|---|---|---|---|
| 1 | `say hello` | greeting text via a single `text` block (likely; finding-dependent) | Baseline: one user turn, one assistant `end_turn` message, msg_id grouping reduces to spike #1's case |
| 2 | `list the files in /tmp` | Bash tool calls interleaved with user(tool_result) events under one or more `tool_use`-tagged assistant messages, then a final `end_turn` message describing the listing | Tool-use streaming (ticket finding 2); confirms msg_id grouping correctly skips tool_use blocks and only picks up the final `text` content; README captures the interleaving pattern |
| 3 | `think carefully and compute 1+2+3+...+100, showing your reasoning` | Extended thinking block + text block(s) under one `end_turn` message | Slow thinking (>1 s spinner visibility intended); README captures verb observation(s) and any new spinner shape; msg_id grouping must concatenate text blocks while skipping thinking blocks |

Append `\r` when writing to the PTY (same as spike #1). The prompts themselves carry no newlines.

## README requirements

The README is the actual deliverable of this spike — the new empirical facts get captured here, not in code comments. Mirror `cmd/spike-one-turn/README.md`'s shape. Required sections, in this order:

1. **Status** — one paragraph: dates, what shipped, link to ticket #9 and to this spec.
2. **What it does** — the 1-paragraph end-to-end summary plus the per-turn prompt table from § *Three probe prompts* above.
3. **How to run** — build command, run command, expected output shape (`SUCCESS:` lines × 3), required state log line sequence (the full list from § *State log lines*).
4. **Per-turn observed timing** — a table per run (one row per turn), columns `turn`, `turn-start → end-turn-detected`, `end-turn-detected → ❯-reappeared`, `turn-start → ❯-reappeared` (total). At least 2 runs recorded; ideally 3+ so the README shows variance.
5. **What `❯` actually does between turns** — narrative answer to the open question. Does `❯` disappear after enter, stay visible with the spinner painted elsewhere, get redrawn under the spinner, or some combination? Cite the `turn=N ❯-disappeared` log lines (or their absence) as evidence. This is one of the spike's primary deliverables.
6. **msg_id grouping behavior on the tool-use turn (turn 2)** — table of JSONL lines for turn 2's session: line index, `type`, role (when applicable), `stop_reason`, `msg_id`, content block types in order. Note any surprises: msg_id stability across blocks, exact line counts per msg, interleaving pattern with user(tool_result) events. The operator's pre-spike probe predicted a 5-line example (1 thinking + 4 tool_use, with 4 interleaved user(tool_result) events). Confirm or refute against what this spike observed.
7. **New event types observed beyond spike #1's catalog** — spike #1 saw `permission-mode`, `file-history-snapshot`, `user`, `attachment`, `ai-title`, `assistant`, `system`, `last-prompt`. The pre-spike probe added `queue-operation`. Document anything else this spike's three turns surfaced (likely candidates: tool-use-specific envelopes, anything around extended thinking). One line per new type with a one-sentence purpose hypothesis.
8. **Spinner verbs captured** — table extending spike #1's: verb (as captured by the regex if it matched; raw bytes if it didn't), turn it appeared in, regex-matched (Y/N), notes. Turn 3 is the deliberate observation surface.
9. **Surprises / findings** — numbered list, same shape as spike #1's. Anything the spike validated that contradicts the ticket body's predictions, anything that surprised, anything that's a candidate follow-up ticket. The list seeds the README's index for whoever reads it next.

Post-hoc data the README needs (and how to get it without changing the tailer filter): once the spike completes, `cat ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl | wc -l` for the total event count; `jq -c '.type'` to bucket types; `jq 'select(.type=="assistant") | .message.id'` to walk msg_ids in order. The README's section 6 table comes from this post-hoc inspection — the spike binary itself does not need to count or render the structural data.

## Open questions for the dev

Same shape as spike #1's open questions — the README answers these; this spec does not pre-decide.

- **Between turns, does the events channel hold a residual end_turn-tagged event from the prior turn at the start of step 4?** The turn driver assumes "any end_turn arriving after this turn's prompt-written belongs to this turn." If the prior turn left events on the channel that were not yet drained, the predicate would fire spuriously. Counter-argument: step 5's conjunction (`isIdle` AND end_turn) is already past idle at the start of a new turn (because the prior turn's step 7 logged `❯-reappeared`), and `prompt-written` triggers claude to start a new message — the channel goes quiet between turns. If the dev observes a spurious fire, the simple fix is to track "events seen before turn started" and skip them; document if needed.
- **Does the tool-use turn (turn 2) emit `text` blocks BEFORE the final `end_turn` message?** Some Anthropic responses interleave commentary text with tool calls under `stop_reason=tool_use`. The msg_id extractor handles this correctly by design (it only collects events whose msg_id matches the LATEST end_turn-tagged line's msg_id), but the README should record what was observed — the answer informs how robust the "latest end_turn msg_id" rule actually is.
- **Does `❯` ever disappear during turn 3's slow thinking?** Spike #1 finding #7 said "not observed (no thinking state visible)." Turn 3 is the first deliberate test. If `❯` is suppressed during the spinner, document; if it's redrawn below the spinner, document; either way it's a fact the library API will need to know.
- **`queue-operation` and `last-prompt` semantics** — out of scope per the ticket, but if anything obvious shows up in the run (e.g., a clear correlation between `queue-operation` count and tool-use turn shape), one sentence in the README is fine. Don't go deep.

## Testing strategy

Same as spike #1: verified by execution against real `claude`, not by automated tests. The developer:

1. Builds: `go build -o /tmp/spike-multi-turn ./cmd/spike-multi-turn`
2. Runs from a directory where `claude` is on `$PATH` and the user has a valid Claude subscription session
3. Confirms stdout shows three `SUCCESS:` lines, one per turn, each carrying the expected response shape (greeting / listing / arithmetic with reasoning)
4. Confirms stderr shows the full state log line sequence in order, including session-level lines once and per-turn lines once per N
5. Runs it 2-3 times to populate the README's timing table and to catch run-to-run variance
6. Runs `pgrep -lf 'claude$'` after exit — no orphans

Negative-path validation (do once):

- Kill the spawned `claude` mid-turn 2 (`pkill -TERM -f 'claude$'`) and confirm the spike exits within a few seconds with a watchdog or PTY-error message, not hanging.

No unit tests. Future `pkg/tuidriver/` tests (post-spike) will mock the PTY; out of scope.

## Out of scope (reminder)

Same exclusions as spike #1, plus the ones the ticket added:

- Cancellation mid-response (reserved for spike #3)
- Modal detection (`/doctor`, permission prompts, multiselects)
- Multi-line input / bracketed paste
- `pkg/tuidriver/` library API extraction
- Multi-session parallelism
- Deep `last-prompt` / `queue-operation` investigation
- Spinner-regex / ANSI-strip refinement (finding #8 is open; not this ticket)
- Building a verb dictionary as a discrete artifact — the README records what's seen

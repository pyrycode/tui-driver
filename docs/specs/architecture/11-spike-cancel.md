# Spec: spike — cancellation mid-response (ESC keystroke, JSONL signaling, recovery)

**Ticket:** [#11](https://github.com/pyrycode/tui-driver/issues/11)
**Size:** S (with caveat — see *Size note*)
**Status:** ready for development

## Size note

The repo only has `size:xs` and `size:s` labels (fleet-wide labels-gap — same one spike #9 hit). Net-new code in this spike is ~150–250 LOC of probe-specific logic, but the binary's total size will be ~800–1000 LOC because ~600 LOC of helpers are copy-pasted verbatim from `cmd/spike-multi-turn/main.go` (same `// copied from … keep in sync until library extraction` pattern spike #9 used). The work is structurally one cohesive deliverable — splitting "cancel during thinking" / "cancel during tool" / "cancel + recovery" into three tickets would force re-copying the helpers three times for zero scope benefit. Architect proceeds at `size:s` per the established fleet convention; this is a labels-surface mismatch, not a real estimation error.

## Files to read first

These are the developer's turn-1 reading list. Load them before touching code; almost everything this spike needs already exists in `cmd/spike-multi-turn/main.go` and its README.

- `cmd/spike-multi-turn/main.go` (entire file, ~742 lines) — the source of every reusable helper this spike copy-pastes. The reusable surface (in the order it appears): `rollingBufferCap`, `statePollInterval`, `jsonlTailInterval`, `sessionFileWait`, `sessionFilePoll`, `watchdogTick`, `inactivityLimit`, `spinnerFreezeLimit`, `shutdownGrace`, `disappearedWindow`, `idleStableWindow`, `ptyRows`, `ptyCols`, `spinnerRe`, `ansiRe`, `idleGlyph`, `rollingBuffer` + methods, `matchSpinner`, `isIdle`, `tracker` + methods, `waitUntil`, `projectsDir`, `encodeCwd`, `resolveSession`, `openSessionJSONL`, `tailJSONL`, `isEndTurn`, `msgIDOf`, `extractByMsgID`, `typePrompt`, and the `run()` shutdown defer.
- `cmd/spike-multi-turn/main.go:127-138` — the `--permission-mode bypassPermissions` spawn comment. Same flag is required here for Probe 2 (`ls /tmp` invokes Bash and would otherwise wedge on a permission modal).
- `cmd/spike-multi-turn/main.go:251-264` — the `for i, p := range prompts` loop the orchestrator uses. This spike's orchestrator has the same shape but calls a polymorphic `runProbe` (kind ∈ {thinking, tool-use, recovery}) instead of `runTurn`.
- `cmd/spike-multi-turn/main.go:266-412` — `runTurn`. Probe 3 (recovery) is essentially a `runTurn` call; Probes 1+2 are a shorter variant that injects the cancel keystroke at the right moment and waits for `❯-reappeared` without ever extracting assistant text.
- `cmd/spike-multi-turn/main.go:445-471` — `typePrompt`. Reuse verbatim. The 10 ms-per-byte / 50 ms tail-pause / `\r` cadence is load-bearing for inter-turn prompts; the cancel keystroke does NOT go through `typePrompt` (single byte, no `\r` — see § *Cancel keystroke writer*).
- `cmd/spike-multi-turn/README.md` § *What `❯` actually does between turns* — `❯` is continuously visible because the spinner regex never matches in practice. Implication: `isIdle()` returns true even while claude is processing. The post-cancel `❯-reappeared` detector therefore needs **a separate trigger to know "claude actually started something to cancel"** — see § *Wait conditions* below.
- `cmd/spike-multi-turn/README.md` § *Surprises / findings* (findings 1 + 2) — `bypassPermissions` is required for Bash tool use; `typePrompt` char-by-char is required for inter-turn prompts. Both apply unchanged here.
- `docs/knowledge/architecture/jsonl-layout.md` § *One Anthropic message ⇒ N JSONL lines* — Probe 3's recovery turn uses msg_id-grouped extraction; that rule plus the assistant-only tailer filter applies unchanged. The cancellation JSONL shape is an empirical unknown — the parser must NOT assume `stop_reason` values; it accepts whatever arrives.
- `docs/knowledge/architecture/system-overview.md` § *Concurrency model* + § *Key signals* — three-goroutine shape this spike reuses; the conjunction "JSONL `end_turn` ∧ PTY idle stable" applies only to Probe 3 (recovery turn). For cancel probes the predicate is different (see § *Cancel probe driver*).
- `docs/specs/architecture/9-spike-multi-turn.md` — predecessor spec. Its empirical-facts section (§ *Empirical facts already in hand*) applies here without modification.
- `CLAUDE.md` (repo root) — scope discipline. The hard rule: still a spike, no `pkg/tuidriver/` extraction.

Vault context (read iff a finding's *why* gets fuzzy mid-implementation, not by default):

- `📋 Projects/2026-05-16 - tui-driver/session-logs/2026-05-17.md` § *Filed pyrycode/tui-driver#11 — spike #3 (cancellation)* — what was known about cancellation before this spike was filed: `esc to interrupt` visible in the prior spike's PTY stderr at "Crunched for 5s", confirming ESC is at least the *advertised* cancel key; everything else is empirical-unknown until this spike runs.

## Context

Spikes #1 and #2 validated the happy paths (one-turn and multi-turn including tool use and slow thinking). Cancellation is the most invasive path tui-driver needs to handle and the one with the most empirical unknowns. ACP `session/cancel` arrives asynchronously; the consumer (`pyry acp`) must (a) inject the right keystroke at the right time, (b) detect that cancellation completed, and (c) return to a state where the next prompt can submit *in the same session*. The TUI status bar shows `esc to interrupt` during processing — an empirical hint that ESC (`0x1b`) is the cancel key. Spike #2's stderr captured this string during a tool-execution window, so we know ESC is at least the **advertised** cancel key. Whether it actually works, what JSONL records, how long recovery takes, what happens to a running tool subprocess, and whether the session is recoverable post-cancel are all open.

Five open questions this spike resolves (one fact per probe is enough; the README does the synthesis):

1. **Exact cancel keystroke** — ESC (`\x1b`) alone, double-ESC, Ctrl-C (`0x03`), or something else / different per state.
2. **JSONL cancel signal shape** — nothing, partial assistant with `stop_reason=null`/`canceled`/`interrupted`/`max_tokens`, or a brand-new envelope type.
3. **Recovery latency** — `cancel-sent → ❯-reappeared` wall clock per probe.
4. **Tool-subprocess kill semantics** (Probe 2 only) — does the Bash subprocess get killed cleanly? Does a `tool_result` user event ever land for the cancelled call?
5. **Session recoverability** (Probe 3) — does a follow-up prompt land in the same `--session-id` and produce a `SUCCESS:`, or is the session poisoned and needs restart?

## Empirical facts already in hand (do NOT rediscover)

These come from spikes #1 and #2. Believe this spec over the ticket body where they disagree.

1. **`isIdle()` is satisfied continuously even during processing** because the spinner regex never matches in practice (spike #9 README § *What `❯` actually does between turns*; spike #1 finding #8). Implication: you cannot use `isIdle()` alone to mean "claude is busy" — you'd see immediate-true and skip the wait condition. Probe 1 (cancel during thinking) needs a positive "spinner glyph visible" signal; Probe 2 needs the JSONL `stop_reason=tool_use` signal. See § *Wait conditions*.
2. **`--session-id` defers JSONL creation until first input.** Same deferral spike #2 handles via `postPromptHook` in Probe 1. Reuse that pattern.
3. **`--permission-mode bypassPermissions` is required for Bash tool use.** Otherwise Probe 2's `ls /tmp` prompt wedges on a permission modal. Same flag spike #2 used.
4. **`typePrompt` char-by-char (10 ms/byte, 50 ms tail-pause, then `\r`) is required for inter-probe prompts.** Bulk-writing `prompt+"\r"` after a prior probe's wind-down loses the `\r`. Reuse verbatim. **NOTE:** the cancel keystroke itself does NOT go through `typePrompt` — it's a single byte with no `\r` (see § *Cancel keystroke writer*).
5. **The assistant-only tailer filter is enough.** `obj["message"] is a map AND obj["type"] == "assistant"`. Keep it unchanged. Cancellation may add new envelope types or new `stop_reason` values that this filter already tolerates (envelope types are silently dropped; the assistant parser accepts any `stop_reason` value because it never branches on it for the cancel probes — see § *Cancel probe driver*).

## Design

### Layout

```
cmd/spike-cancel/
  main.go      # everything — copy-pasted helpers + 3-probe driver
  README.md    # how to run, keystroke that worked, JSONL events seen, timings, recovery outcome, surprises
```

No `pkg/tuidriver/` content. No shared helpers extracted from `cmd/spike-multi-turn/`. Copy-paste with attribution (`// copied from cmd/spike-multi-turn/main.go — keep in sync until library extraction`). All three spike binaries delete when `pkg/tuidriver/` lands.

Also: add `/spike-cancel` to `.gitignore` alongside the existing `/spike-one-turn` entry. Prevents the same accidental-binary-commit incident #7's cleanup caught for spike-one-turn.

### Reuse policy

Lift verbatim from `cmd/spike-multi-turn/main.go` with the same attribution comment:

- Constants: `rollingBufferCap`, `statePollInterval`, `jsonlTailInterval`, `sessionFileWait`, `sessionFilePoll`, `watchdogTick`, `inactivityLimit`, `spinnerFreezeLimit`, `shutdownGrace`, `disappearedWindow`, `idleStableWindow`, `ptyRows`, `ptyCols`
- Regex/glyph: `spinnerRe`, `ansiRe`, `idleGlyph`
- Types/funcs: `rollingBuffer` + methods, `matchSpinner`, `isIdle`, `tracker` + methods, `waitUntil`, `projectsDir`, `encodeCwd`, `resolveSession`, `openSessionJSONL`, `tailJSONL`, `isEndTurn`, `msgIDOf`, `extractByMsgID`, `typePrompt`

The `run()` orchestrator shape (PTY allocation → reader goroutine → watchdog goroutine → wait for idle → tailer-opening hook on first input → loop over probes → shutdown defer) carries over with one structural change: the loop body calls a polymorphic `runProbe(ctx, …, kind)` instead of `runTurn`. The shutdown defer is identical.

### New code (the spike's actual contribution)

Five additions:

1. **`spinnerGlyph` byte constant** — the literal `✻` UTF-8 bytes (`\xe2\x9c\xbb`). Used as the universal "claude has started processing" signal because the regex doesn't match.
2. **`hasSpinnerGlyph(snap []byte) bool`** — `bytes.Contains(ansiRe.ReplaceAll(snap, nil), spinnerGlyph)`. Strips ANSI first to match the same byte shape `isIdle` operates on.
3. **`isToolUse(ev map[string]any) bool`** — same shape as `isEndTurn`, returns true when `message.stop_reason == "tool_use"`.
4. **`sendCancel(ptmx *os.File, keystroke []byte) error`** — single bulk write of the keystroke bytes, no inter-byte delay, no trailing `\r`. The cancel sequence is at most 2 bytes (`\x1b` or `\x1b\x1b` or `\x03`); the inter-byte-delay reasoning that drove `typePrompt` does not apply (no `\r` involved).
5. **`runProbe(ctx, …, kind ProbeKind)`** — the per-probe driver. Three kinds: `kindThinking`, `kindToolUse`, `kindRecovery`. See § *Cancel probe driver* and § *Recovery probe driver*.

Two new constants:

- `cancelRecoveryLimit = 30 * time.Second` — the aggressive post-cancel watchdog window (per AC). Cancellation should be near-instant; if `❯-reappeared` doesn't fire within this window the spike fails with `watchdog: stuck after cancel for <Ns>`.
- `waitConditionLimit = 30 * time.Second` — how long Probe 1 waits for the spinner glyph and Probe 2 waits for `stop_reason=tool_use` before giving up with a watchdog-shaped error. Long enough for any reasonable claude startup + first-token delay; short enough that a wedged probe doesn't sit forever.

One new flag:

- `-cancel-keystroke {esc|double-esc|ctrl-c}` (default `esc`) — lets the developer empirically iterate without rebuilding. The flag controls only what bytes `sendCancel` writes. Probes 1 and 2 both use the same flag value; Probe 3 doesn't cancel.

### Cancel keystroke writer

`sendCancel` writes exactly the bytes the flag selects:

| Flag value | Bytes | Hex |
|---|---|---|
| `esc` (default) | `0x1b` | `1b` |
| `double-esc` | `0x1b 0x1b` | `1b 1b` |
| `ctrl-c` | `0x03` | `03` |

The log line `probe=N cancel-sent keystroke=<hex>` shows the actual bytes (space-separated hex per byte for the multi-byte cases) so the README can record which value worked.

Hypothesis (ESC alone) is supported by the `esc to interrupt` PTY hint observed in spike #2. The flag's existence is the empirical-iteration mechanism: if Probe 1 trips the 30 s post-cancel watchdog with `-cancel-keystroke esc`, the developer reruns with `-cancel-keystroke double-esc`, then `-cancel-keystroke ctrl-c`, and records the outcomes in the README. Document at most three runs of escalation; if all three trip the watchdog, that itself is the finding — the README captures the dead-ends.

### Wait conditions

The probe driver must know "claude has started something to cancel" before injecting the keystroke. `isIdle()` is useless for this (see § *Empirical facts* #1). Per-probe wait conditions:

- **Probe 1 (kindThinking)** — poll `hasSpinnerGlyph(rb.snapshot())` on the `statePollInterval` tick until true OR `waitConditionLimit` elapses. The `✻` glyph is the universal denominator across every observed verb / aphorism / spinner form (vault Findings: *the only stable parts of the spinner are the `✻` glyph and the ` for <Ns>` time-tail*). One log line on success: `probe=1 spinner-or-tool-visible kind=spinner-glyph`.
- **Probe 2 (kindToolUse)** — accumulate assistant events from `eventCh` until one returns `isToolUse(ev) == true`, OR `waitConditionLimit` elapses. After the JSONL signal arrives, sleep 200 ms (the "tool started emitting" grace — sidesteps "what does tool execution look like in the PTY" by not requiring a specific PTY pattern; the spec's AC said "tool-execution indicator visible," this is the practical proxy). One log line: `probe=2 spinner-or-tool-visible kind=tool-use-stop-reason msg_id=<id>`.
- **Probe 3 (kindRecovery)** — no wait condition / no cancel. Straight `runTurn`-equivalent (see § *Recovery probe driver*).

On wait-condition timeout: return `fmt.Errorf("probe %d: wait condition not observed within %s", N, waitConditionLimit)`. The session-level shutdown defer fires as usual.

### Cancel probe driver (Probes 1+2)

The contract — the developer implements the body; the order and content of log lines is the contract.

```go
// runProbe drives one probe end-to-end. For kindThinking/kindToolUse it
// submits the prompt, waits for the kind-specific wait condition, sends
// the cancel keystroke, waits for ❯-reappeared (with 30 s post-cancel
// deadline), logs any JSONL events that arrived between cancel-sent and
// ❯-reappeared, and returns.
//
// For kindRecovery it submits the prompt and drives a full turn to
// assistant-text-extracted + recovery-turn-success, no cancellation.
//
// On turn 1 only, postPromptHook opens the deterministic JSONL and starts
// the tailer (same shape as spike-multi-turn's runTurn — the JSONL only
// appears after first input under --session-id).
func runProbe(
    ctx context.Context,
    logger *log.Logger,
    probe int,
    kind ProbeKind,
    prompt string,
    cancelKey []byte,
    ptmx *os.File,
    rb *rollingBuffer,
    eventCh <-chan map[string]any,
    tr *tracker,
    postPromptHook func() error,
) error
```

Behavior contract for cancel probes (kindThinking or kindToolUse):

1. Log `probe=N probe-start kind="<thinking|tool-use>"`. Bump `tr`.
2. `typePrompt(ptmx, prompt)`. Log `probe=N prompt-written`. Bump `tr`.
3. Run `postPromptHook` if non-nil (Probe 1 only — opens JSONL, starts tailer).
4. Wait for the kind-specific wait condition (see § *Wait conditions*) with `waitConditionLimit` deadline. On observation, log `probe=N spinner-or-tool-visible kind=<spinner-glyph|tool-use-stop-reason>` (with `msg_id=<id>` appended for the tool-use kind). Bump `tr`.
5. `sendCancel(ptmx, cancelKey)`. Log `probe=N cancel-sent keystroke=<hex>`. Bump `tr`. Record `cancelSentAt := time.Now()`.
6. Wait for `❯-reappeared`: until `isIdle(rb.snapshot())` returns true continuously for `idleStableWindow` (250 ms), OR `cancelRecoveryLimit` (30 s) elapses. **CRITICAL:** because `isIdle` is satisfied even during processing (empirical fact #1), the 250 ms stability gate alone is not enough — claude needs to actually transition. Tighten the predicate to: `hasSpinnerGlyph` becomes false AND `isIdle` true AND both hold continuously for `idleStableWindow`. If the spinner glyph was never observed pre-cancel (e.g., on `kindToolUse` where the wait condition was JSONL-side), fall back to the plain `isIdle stable` predicate — the spinner glyph absence is only a meaningful tightener when the pre-cancel state had it.

   On timeout: log `watchdog: stuck after cancel for <Ns>` and return error. The session-level shutdown handles the rest.
7. While waiting in step 6, drain any assistant events arriving on `eventCh` into a local `[]map[string]any`. For each event, log `probe=N jsonl-cancel-event type=assistant stop_reason=<value> msg_id=<id>` (use `<nil>` if `stop_reason` is JSON null; use `<missing>` if the key isn't present). This is the per-event log line the AC's "if observed; optional" wildcard covers — it fires 0+ times per probe.
8. When step 6 succeeds: log `probe=N ❯-reappeared`. Bump `tr`. Log `probe=N elapsed-after-cancel=<duration>` where duration is `time.Since(cancelSentAt).Round(time.Millisecond)`. Bump `tr`.
9. Return `nil`. Drained events are discarded — the next probe's `runProbe` drains the channel at start (same residual-event drain spike #2's `runTurn` uses).

### Recovery probe driver (Probe 3)

Probe 3 is structurally `runTurn` from spike #2: type prompt, accumulate events, wait for `gotEndTurn && isIdle` stable, extract via `extractByMsgID`, print SUCCESS, emit `recovery-turn-success`.

Behavior contract for kindRecovery:

1. Log `probe=3 probe-start kind="recovery"`. Bump `tr`.
2. Residual-event drain (same non-blocking drain as spike #2 — discards any straggler events from Probe 1 or 2's post-cancel period).
3. `typePrompt(ptmx, prompt)`. Log `probe=3 prompt-written`. Bump `tr`.
4. Optional `❯-disappeared` observer (same 500 ms windowed observer as spike #2, optional log line).
5. Accumulate events, detect `gotEndTurn && isIdle` stable per spike #2's `runTurn`. Log `probe=3 end-turn-detected msg_id=<id>`. Bump `tr`. Log `probe=3 ❯-reappeared`. Bump `tr`.
6. `extractByMsgID(events, latestEndTurnMsgID)`. Log `probe=3 assistant-text-extracted len=<n>`. Bump `tr`. `fmt.Printf("SUCCESS: %s\n", text)`.
7. **If `len(text) > 0`:** log `probe=3 recovery-turn-success len=<n>`. Bump `tr`. (Per AC: emitted only if recovery produced a SUCCESS extraction. An empty extraction is a finding — see § *Open questions for the dev*.) Return `nil`.

The implementer can refactor steps 1–6 by extracting a small helper shared between `runProbe` and the would-be `runTurn` equivalent, or by inlining. Either is fine; the binary is ephemeral.

### Probe ordering

The AC says "immediately after Probe 1 or Probe 2 completes (operator's choice — recommend Probe 1)." Default to Probe 1 → Probe 2 → Probe 3 (Probe 3 lands on Probe 2's post-state because Probe 2 is the more recent precedent). If during empirical iteration the Probe 2 → Probe 3 transition wedges in a way Probe 1 → Probe 3 doesn't (e.g., tool subprocess kill leaves the session messier), document the finding and switch to Probe 1 → Probe 3 → Probe 2 as the canonical order. The README records the ordering actually shipped.

The three probe prompts (verbatim from AC):

| Probe | Prompt | Kind |
|---|---|---|
| 1 | `think carefully and write a 1000-word essay on the philosophy of monads` | thinking |
| 2 | `recursively list all files under /tmp` | tool-use |
| 3 | `say hello` | recovery |

The Probe 1 prompt is intentionally heavy-thinking so the spinner glyph appears reliably before any tool dispatch. If empirically claude finishes Probe 1 in <2 s anyway and the spinner glyph never appears in the rolling buffer, swap to a different long-form prompt (the README documents the swap).

### Watchdog

Reuse `tracker` unchanged at the session level (60 s inactivity, 30 s spinner-freeze). Each named log line in a probe calls `tr.recordTransition("probe=N <name>")` so the session-level inactivity deadline resets on every probe transition.

The **post-cancel watchdog is probe-local**, not session-level: enforced by step 6 of the cancel probe driver via a `time.Now().After(cancelSentAt.Add(cancelRecoveryLimit))` check inside the wait loop. On trip, return an error from `runProbe` (orchestrator unwraps, shutdown defer fires). The error message is `watchdog: stuck after cancel for <duration>` to match the AC's prescribed log shape.

### State log lines (the full sequence the spike emits)

Session-level (fire once each):

```
session-id-resolved id=<uuid> jsonl=<path>
idle-detected
session-jsonl-opened path=<path> offset=0     # after probe=1 prompt-written
shutdown-signalled
```

Per-probe lines, for Probes 1 + 2 (cancel kinds):

```
probe=N probe-start kind="<thinking|tool-use>"
probe=N prompt-written
probe=N spinner-or-tool-visible kind=<spinner-glyph|tool-use-stop-reason> [msg_id=<id>]
probe=N cancel-sent keystroke=<hex>
probe=N jsonl-cancel-event type=assistant stop_reason=<value> msg_id=<id>   # 0+ times
probe=N ❯-reappeared
probe=N elapsed-after-cancel=<duration>
```

Per-probe lines, for Probe 3 (recovery):

```
probe=3 probe-start kind="recovery"
probe=3 prompt-written
probe=3 ❯-disappeared                        # optional — same shape as spike #2
probe=3 end-turn-detected msg_id=<id>
probe=3 ❯-reappeared
probe=3 assistant-text-extracted len=<n>
probe=3 recovery-turn-success len=<n>        # only if len > 0
```

Watchdog (if it trips) keeps the `watchdog:` prefix per spike #2 convention.

Ordering invariant: `idle-detected` fires once before any `probe=1 probe-start`. `session-jsonl-opened` fires after `probe=1 prompt-written` (the `--session-id` deferral). Probe 1's `❯-reappeared` precedes Probe 2's `probe-start`; same for Probe 2 → Probe 3.

### Concurrency model

Same three-goroutine shape as spike #2:

| Goroutine | Owns | Exit |
|---|---|---|
| `main` orchestrator | linear state machine across 3 probes, watchdog tick, msg_id extraction (Probe 3 only), per-probe post-cancel deadline (Probes 1+2) | last probe returns OR watchdog trips OR error |
| PTY reader | rolling buffer (mutex-protected); mirrors raw bytes to stderr | EOF from PTY master (when shutdown closes it) |
| JSONL tailer | events channel (buffered, size 32); filters to assistant-with-message | context cancellation |

The events channel is session-scoped; one tailer goroutine services all three probes. Probe 2's wait condition reads from it (looking for `tool_use`), Probes 1+2's cancel-event observer reads from it (logging whatever arrives between cancel-sent and ❯-reappeared), and Probe 3's `runTurn`-equivalent reads from it (accumulating for `end_turn`). Each probe's runProbe drains residual events at start to avoid cross-probe contamination.

### Shutdown

Identical to spike #2 — single `defer` running the SIGTERM → 3 s grace → SIGKILL → close PTY → cancel context → `wg.Wait()` sequence under `sync.Once`. Fires on every exit path (last probe success, watchdog trip, PTY error, probe error). The `shutdown-signalled` log line fires once at the start of the shutdown body.

### Error handling

Failure modes and recovery:

- **Wait condition timeout (Probes 1+2 step 4):** `fmt.Errorf("probe %d: wait condition not observed within %s", N, waitConditionLimit)`. Shutdown handles cleanup. Likely cause: claude finished the response faster than the wait window (Probe 1) or never called a tool (Probe 2). The README documents.
- **Post-cancel watchdog (Probes 1+2 step 6):** `fmt.Errorf("watchdog: stuck after cancel for %s", elapsed)`. Likely cause: wrong cancel keystroke for this claude version, OR claude was in a state that doesn't accept the keystroke (e.g., mid-modal). The README documents the keystroke that was tried; operator reruns with `-cancel-keystroke <alternative>`.
- **Recovery probe timeout (Probe 3):** the session-level 60 s inactivity watchdog catches a recovery prompt that never produces `end_turn`. Likely cause: session was actually poisoned by a prior cancel. The README documents — this is itself one of the five empirical questions the spike resolves.
- **PTY EOF:** the PTY reader returns; the watchdog detects the inactivity stall and trips. Shutdown handles cleanup.

No special-case error paths beyond these. The cancel keystroke flag's three values + the README's run-by-run notes are the empirical-iteration mechanism; the spike itself does not auto-retry with different keystrokes.

## Testing strategy

Same as spikes #1 and #2: verified by execution against real `claude`, not by automated tests. The developer:

1. Builds: `go build -o /tmp/spike-cancel ./cmd/spike-cancel`
2. Runs from a directory where `claude` is on `$PATH` and the user has a valid Claude subscription session.
3. Default run: `/tmp/spike-cancel`. Expected outcome on green: Probes 1 and 2 each log `cancel-sent` / `jsonl-cancel-event` (0+ times) / `❯-reappeared` / `elapsed-after-cancel`, Probe 3 logs `recovery-turn-success len=<n>` and stdout shows one `SUCCESS:` line for the `say hello` reply.
4. If `ESC` doesn't work: rerun with `-cancel-keystroke double-esc`, then `-cancel-keystroke ctrl-c`. Record each attempt's outcome in the README's *Keystrokes tried* section (see § *README requirements*).
5. Runs the green configuration 2–3 times to populate the README's timing tables and catch run-to-run variance.
6. Runs `pgrep -lf 'claude --session-id'` after exit — no orphans. **Also** runs `pgrep -lf '^ls'` (or whatever Probe 2's tool subprocess is) — no orphans means the tool subprocess was killed cleanly, which is itself a finding for the README.
7. Post-hoc JSONL inspection: `cat ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl | jq -c '.type'` to bucket envelope types; `jq 'select(.type=="assistant") | .message.stop_reason'` to enumerate stop_reason values observed across cancels + recovery; the README's *JSONL events recorded during/after cancel* section comes from this post-hoc inspection plus the live `jsonl-cancel-event` log lines.

Negative-path validation (do once):

- While Probe 1 is between `prompt-written` and `cancel-sent`, kill the spawned `claude` from another terminal (`pkill -TERM -f 'claude --session-id'`). Confirm the spike exits within a few seconds with a watchdog or PTY-error message — same shape as spike #2's negative-path check.

No unit tests. Future `pkg/tuidriver/` tests (post-spike) will mock the PTY; out of scope.

## README requirements

The README is the actual deliverable of this spike — the new empirical facts get captured here, not in code comments. Mirror `cmd/spike-multi-turn/README.md`'s shape. Required sections, in this order:

1. **Status** — one paragraph: dates, what shipped, link to ticket #11 and to this spec, headline finding (which keystroke worked, was the session recoverable, did the tool subprocess get killed cleanly).
2. **What it does** — the 1-paragraph end-to-end summary plus the per-probe prompt table from § *Probe ordering*.
3. **How to run** — build command, run command, the `-cancel-keystroke` flag, expected output shape (`SUCCESS:` × 1 from Probe 3), required state log line sequence (the full list from § *State log lines*).
4. **Cancel keystroke that worked** — one paragraph naming the keystroke (`ESC`, `double-ESC`, or `Ctrl-C`) plus the hex bytes. If multiple keystrokes were tried, a table: `attempt | keystroke | hex | outcome (worked / timed-out-after-30s / claude-emitted-X)`.
5. **JSONL events recorded during/after each cancel** — one table per probe (1 and 2): row per event, columns `idx`, `type`, `stop_reason` (raw value as `<nil>` / `<missing>` / `end_turn` / `canceled` / etc.), `msg_id`, `content block types in order`, `arrived-relative-to (cancel-sent / ❯-reappeared)`. This is the cancellation-schema documentation the rest of the codebase depends on; be exhaustive. If nothing arrives between cancel-sent and ❯-reappeared, say so explicitly — that's also a finding.
6. **Per-probe observed timing** — table: `probe`, `prompt-written → spinner-or-tool-visible (ms)`, `spinner-or-tool-visible → cancel-sent (ms)` (should be ~0 — step 5 fires immediately after step 4), `cancel-sent → ❯-reappeared (ms)` (the key cancellation-latency number), `total (ms)`. At least 2 runs recorded.
7. **Probe 2: tool subprocess behavior** — narrative answer: did the Bash subprocess get killed? Cleanly (no zombie processes)? Did any `user(tool_result)` event land in JSONL post-cancel (visible via the post-hoc `jq` inspection, not via the assistant-filter tailer)? If the tool got mid-output when cancelled, what happened to the partial output? Cite the `jsonl-cancel-event` log lines as evidence plus the post-hoc JSONL inspection.
8. **Probe 3: recovery outcome** — narrative: did the recovery prompt produce `SUCCESS:`? Same wall-time as a non-recovery turn 1 (spike #2 baseline ~2–3 s), or notably slower? Did the JSONL show the prior cancel's msg_id reused, or a fresh msg_id for the recovery? Was the assistant's response itself coherent (it knows it's `say hello`, not still trying to write the monad essay)?
9. **New event types / stop_reason values observed beyond spikes #1 and #2's catalog** — spike #2 saw 8 envelope types and `stop_reason` ∈ {`end_turn`, `tool_use`}. List any new envelope `type` value or any new `stop_reason` value cancellation surfaced. One line per new value with a one-sentence purpose hypothesis.
10. **Surprises / findings** — numbered list, same shape as spike #2's. Anything that contradicts the ticket body's predictions, anything that surprised, anything that's a candidate follow-up ticket (e.g., "modal handling — `/doctor` or permission prompts — was untouched; still deferred").

Post-hoc data the README needs (and how to get it without changing the tailer filter): once the spike completes, `cat ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl | wc -l` for the total event count; `jq -c '.type'` to bucket types; `jq 'select(.type=="assistant") | {stop_reason: .message.stop_reason, msg_id: .message.id}'` to walk stop_reason values in order. The structural-data tables in sections 5, 7, 9 come from this inspection plus the live `jsonl-cancel-event` log lines.

## Open questions for the dev

The README answers these; this spec does not pre-decide.

- **Does claude emit anything to the assistant-only JSONL channel after `cancel-sent`?** If nothing arrives (no `jsonl-cancel-event` log lines fire), the cancellation is invisible at the assistant-stream level and `pyry acp` will have to rely on PTY-side detection (`❯-reappeared`) alone. If something arrives — partial assistant message with `stop_reason=null`, or `stop_reason=canceled`, or anything else — the consumer can rely on the JSONL signal. The README's section 5 table answers this concretely.
- **Does Probe 2's `tool_use` assistant message get an `end_turn` line later in the JSONL (e.g., after the tool subprocess is killed)?** Post-hoc `jq` inspection per probe. If yes, document the shape. If no, the cancelled tool-use is structurally a "message that never resolves" in the JSONL — also a useful finding.
- **For Probe 3, does `say hello` produce a fresh msg_id or reuse anything from the cancelled probes?** Should be fresh; if not, that's a surprising finding worth recording.
- **Does the `❯-reappeared` predicate from step 6 of the cancel probe driver fire reliably on Probe 2?** Probe 2's pre-cancel state has the spinner glyph (claude was thinking when it issued the tool_use) AND tool output rendering in the PTY; the post-cancel state has both gone. The spinner-glyph-absence tightener should work, but if it doesn't (e.g., the spinner glyph stays painted in the rolling buffer after cancel because the rolling buffer hasn't churned past it yet), the predicate could fire late or never. Document if the tightener needs revision.
- **`waitConditionLimit = 30 s`** is a guess. If Probe 1's monad-essay prompt produces a fast response (<2 s, no spinner glyph ever visible), swap the prompt for something heavier and document. If 30 s is too short for Probe 2's tool-use trigger, bump it (any value up to 60 s is fine — the session-level inactivity watchdog is 60 s).
- **`cancelRecoveryLimit = 30 s`** matches the AC. If empirically every successful cancel returns to `❯-reappeared` in <2 s, the README can note that the 30 s budget is mostly headroom — useful information for the eventual library API design.

## Out of scope (reminder)

Same exclusions as spikes #1 and #2, plus the ones the ticket adds:

- `pkg/tuidriver/` library API extraction
- Modal detection (`/doctor`, permission prompts, multiselects)
- Multi-line input / bracketed paste
- Long-running / edge-case cancellations (claude crashing mid-cancel, double-cancel race, cancel-during-cancel-recovery)
- Parallel-tool-use interleaving stress test
- Display passthrough of spinner content (verbs / aphorisms)
- Spinner-regex / ANSI-strip refinement (finding #8 still open since spike #1)
- Deep `last-prompt` / `queue-operation` investigation

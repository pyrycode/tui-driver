# Spec: spike — permission-prompt modal (trigger, detect, auto-respond, escalation contract)

**Ticket:** [#13](https://github.com/pyrycode/tui-driver/issues/13)
**Size:** S (with caveat — see *Size note*)
**Status:** ready for development

## Size note

Same labels-surface mismatch the three predecessor spikes hit: the repo only carries `size:xs` and `size:s`. The binary's net-new logic is ~200–300 LOC (modal predicate, three probe drivers, approve-keystroke writer, two-session orchestration), but its total size will be ~900–1100 LOC because ~700 LOC of helpers copy-paste verbatim from `cmd/spike-cancel/main.go` under the established `// copied from cmd/spike-cancel/main.go — keep in sync until library extraction` attribution. Splitting "observe / auto-respond / escalate" into three tickets would force three rounds of the same helper duplication for zero scope benefit — the probes share a session model (Session B carries Probe 2 → Probe 3) and a modal-detection primitive that has to be validated once across all three. Architect ships at `size:s` per fleet convention.

## Files to read first

Developer's turn-1 reading list. Almost every primitive this spike needs already lives in `cmd/spike-cancel/main.go` and its README; load that file first, then the architecture knowledge docs, then this spec's *Design* section. Vault material is on-demand only.

- `cmd/spike-cancel/main.go` (entire file, ~1060 lines) — the source of every reusable helper. The reusable surface (in declaration order): every `const` in the package block except `cancelRecoveryLimit` / `waitConditionLimit` / `toolUseStartGrace` / `ptyQuietWindow` / `clearLineSettle` (those are cancel-specific and we redefine equivalents); `spinnerRe`, `ansiRe`, `idleGlyph`, `spinnerGlyph`; `rollingBuffer` + `append` + `snapshot` + `quietFor`; `tracker` + methods; `waitUntil`; `projectsDir`, `encodeCwd`, `resolveSession`, `openSessionJSONL`, `tailJSONL`; `isIdle`, `matchSpinner`, `hasSpinnerGlyph`, `isEndTurn`, `isToolUse`, `msgIDOf`, `extractByMsgID`; `typePrompt`, `clearInputLine`, `sendCancel` (renamed in this spike — see § *Single-byte keystroke writer*).
- `cmd/spike-cancel/main.go:54-102` — the const block. Note specifically `ptyQuietWindow = 1500ms` and `clearLineSettle = 50ms` — both reused unchanged; the spec's new constants build on top.
- `cmd/spike-cancel/main.go:154-249` — `main` + `run` shape: flag parsing, projects-dir resolution, `--session-id` resolution, PTY allocation, reader goroutine + watchdog goroutine spawn, `shutdownOnce`-guarded shutdown defer. This spike's `run` calls a new `runSession` helper twice (Session A for Probe 1, Session B for Probes 2+3) — see § *Two-session orchestration*.
- `cmd/spike-cancel/main.go:339-451` — `runProbe` + `runCancel`. Probe 2's modal-detect-then-respond shape resembles `runCancel`'s "wait for kickoff → send keystroke → wait for stable" structure. Probe 1 / Probe 3 are simpler (no end-of-turn predicate; just observe / extract + log; teardown via shutdown defer).
- `cmd/spike-cancel/main.go:540-577` — `waitReappeared` (the PTY-quiescence post-cancel predicate). The same `isIdle ∧ rb.quietFor() >= ptyQuietWindow` predicate is what Probe 2 waits on after sending the approve keystroke to detect "modal-cleared + turn-complete-readiness."
- `cmd/spike-cancel/main.go:602-697` — `runRecovery`. Probe 2's post-keystroke half (wait for `end_turn` + `isIdle` stable, then `extractByMsgID`, then `SUCCESS:`) is structurally identical to this function.
- `cmd/spike-cancel/main.go:704-718` — `sendCancel` + `clearInputLine`. The single-byte bulk-write pattern + Ctrl-U input-clear are both reused unchanged; the approve keystroke uses the same bulk-write semantics.
- `cmd/spike-cancel/README.md` (entire file) — empirical log for the predecessor spike. The sections that matter here: § *Status* (what shipped), § *Cancel keystroke that worked* (mirrors what this spike's README needs for the approve keystroke), § *Surprises / findings* (especially #1 = PTY-quiescence rationale, #2 = post-cancel input residue → Ctrl-U fix).
- `cmd/spike-multi-turn/README.md` § *What `❯` actually does between turns* — `isIdle` is satisfied continuously even during processing; modal detection cannot rely on `!isIdle`. The modal predicate must be a positive signal (box-drawing chars or literal text), not "not idle."
- `cmd/spike-multi-turn/README.md` § *Surprises / findings* (findings 1 + 2) — `bypassPermissions` is required when you want to *skip* modals (it's how spikes #9 and #11 worked); for this spike we DROP the flag so the modal actually fires (see § *Spawning claude (no bypassPermissions)*).
- `docs/knowledge/architecture/system-overview.md` § *Key signals* — full catalog of state predicates the spike inherits (`❯` idle glyph, `✻` spinner glyph, JSONL `end_turn`, PTY quiescence, post-cancel input restoration). All apply; modal detection is the new signal this spike adds.
- `docs/knowledge/architecture/jsonl-layout.md` § *Turn lifecycle in JSONL* and § *Tool-use shapes* — context for what Probe 2's JSONL stream looks like once the modal is approved (it should match spike #11 Probe 2's tool-use shape: `assistant(stop_reason=tool_use)` → `user(tool_result)` → `assistant(stop_reason=end_turn)`).
- `docs/knowledge/codebase/11.md` § *Patterns established* — every pattern listed there carries forward (PTY quiescence as readiness, single-byte single-write keystrokes, Ctrl-U before every prompt, multi-action session model). The new pattern this spike establishes is **modal detection**.
- `CLAUDE.md` (repo root) — scope discipline. The hard rule: still a spike. No `pkg/tuidriver/` extraction. Library API design happens in the post-spike extraction ticket.

Vault context (read iff a finding's *why* gets fuzzy mid-implementation, not by default):

- `📋 Projects/2026-04-10 - Pyrycode/TUI Driver.md` § *Architecture* — the three-layer modal-handling design (prevent at source → detect known + handle → detect unknown + bail). This spike implements the empirical groundwork for layer 2; the README's *Escalation contract sketch* feeds layer 2's eventual API design.
- `📋 Projects/2026-04-10 - Pyrycode/TUI Driver.md` § *2026-05-14 `/doctor` poisoning incident* (if present) — the canonical motivating example for layer-2 modal handling. Read once for context on why the consumer's auto-respond logic has to be conservative and escalation-friendly.

## Context

Spikes #1, #9, and #11 all spawned `claude --permission-mode bypassPermissions` so permission modals couldn't fire. That's the right call for happy-path validation but it leaves the entire **modal-handling layer** of the architecture (Layer 2: detect known modals + auto-respond or escalate) unvalidated. ACP `session/prompt` from the eventual `pyry acp` consumer will arrive at a `claude` instance with permission modals enabled — the library must either auto-respond to known shapes or surface them to the host UI. We have zero empirical data on what those modals actually look like in the PTY, what (if anything) they emit to the JSONL, what keystroke approves them, and how the post-response state machine resumes the turn.

Five open questions this spike resolves (one per probe-driven fact; the README does the synthesis):

1. **Modal's PTY shape.** Box-drawing characters (`╭`, `╰`, `│`)? Plain text? Cursor positioning that breaks the rolling buffer? Does the modal redraw on each tick or is it static once painted? Are there ANSI color codes that influence detection?
2. **Modal's JSONL signaling.** Any new envelope `type`? A new `stop_reason` value during the wait? Embedded in an existing `assistant` message? Nothing at all (purely a TUI affordance with no JSONL footprint)? The hypothesis is "nothing extra" — claude has already emitted `assistant(stop_reason=tool_use)` and is now waiting for the operator on the PTY side — but the spike validates.
3. **Exact approve keystroke.** Candidates: `1\r` (numbered "Yes, once"), `y\r` (Y/n shortcut), `\r` alone (if default is "yes"), arrow + `\r` (if default is "no" and operator must select). Same empirical-iteration mechanism as spike #11's `-cancel-keystroke` flag.
4. **Post-response state machine.** After approve, does claude (a) immediately execute the tool and continue the assistant message, (b) emit a brand-new `assistant` message, (c) emit a JSONL event acknowledging the approval, or (d) some combination? How long is `keystroke-sent → end_turn` end-to-end?
5. **Escalation extractability.** For Probe 3's simulated "ACP forwards to host UI" path: how much information about the modal can the spike extract from the rolling buffer alone (modal text, tool name, options offered) without writing back? Is text extraction reliable across multiple invocations, or does cursor-positioning render it brittle?

Closing the layer-2 gap unblocks the post-spike `pkg/tuidriver/` extraction (the library API needs a callback or channel shape the consumer can subscribe to) and removes the `bypassPermissions` crutch the prior spikes leaned on.

## Empirical facts already in hand (do NOT rediscover)

These come from spikes #1, #9, #11. Believe this spec over the ticket body where they disagree.

1. **`isIdle()` is satisfied continuously even during processing** because the spinner regex never matches in practice. The modal predicate cannot rely on `!isIdle`; it needs a positive signal (box-drawing chars or literal text).
2. **`--session-id` defers JSONL creation until first input.** Same `postPromptHook` pattern spike #11 uses — open the JSONL + start the tailer AFTER `prompt-written`, not after `idle-detected`.
3. **`typePrompt` char-by-char (10 ms/byte, 50 ms tail-pause, then `\r`) is required for every prompt write.** Bulk-writing `prompt+"\r"` loses the `\r` after the prior turn's wind-down.
4. **`Ctrl-U` (0x15) before every `typePrompt`.** Defensive against post-cancel input residue AND post-approve input residue (we don't yet know if approve leaves residue — assume yes, prove no). Idempotent on empty input.
5. **The assistant-only tailer filter (`type=="assistant"` AND `message` is a map) is enough for happy-path content extraction.** It silently drops `user(text "[Request interrupted by user]")` cancel markers and would silently drop any hypothetical `user(modal)` envelope too. If a modal-related event shows up under the `assistant` channel, the filter catches it; if it shows up under any other envelope, post-hoc `jq` inspection is the diagnosis path (same as spike #11's cancel-marker investigation).
6. **PTY-quiescence (`rb.quietFor() >= ptyQuietWindow`, 1500 ms) is the right "claude has finished settling" predicate** post any keystroke that changes claude's state. Same predicate used post-cancel by spike #11; reused unchanged for post-approve here.
7. **Single-byte keystrokes write as a single bulk `pty.Write`, no inter-byte delay, no `\r` unless the keystroke explicitly includes one.** Spike #11's `sendCancel` is the canonical shape. The approve keystroke uses the same writer (renamed to a generic name in this spike — see § *Single-byte keystroke writer*).

## Design

### Layout

```
cmd/spike-permission/
  main.go      # everything — copy-pasted helpers + 2-session 3-probe driver
  README.md    # how to run, modal PTY shape, JSONL events seen, approve keystroke that worked,
               # timings, escalation contract sketch, surprises
```

No `pkg/tuidriver/` content. No shared helpers extracted from `cmd/spike-cancel/`. Copy-paste with the established attribution comment. All spike binaries delete when `pkg/tuidriver/` lands.

Also: add `/spike-permission` to `.gitignore` alongside the existing `/spike-cancel` entry.

### Spawning claude (no bypassPermissions)

The spawn line drops `--permission-mode bypassPermissions` so modals fire. This is the only intentional spawn-arg difference from spike #11:

```
exec.Command("claude", "--session-id", sessionID)
```

No `--permission-mode default` either — let claude pick its own default (whatever the locally-installed `claude` 2.1.x does without the flag is what the eventual library's consumer would see). If the dev finds that explicit `--permission-mode default` produces different modal shape from no flag at all, document the divergence in the README; either is acceptable to ship with as long as it's noted.

### Reuse policy

Lift verbatim from `cmd/spike-cancel/main.go` under the attribution comment `// copied from cmd/spike-cancel/main.go — keep in sync until library extraction`:

- Constants: `rollingBufferCap`, `statePollInterval`, `jsonlTailInterval`, `sessionFileWait`, `sessionFilePoll`, `watchdogTick`, `inactivityLimit`, `spinnerFreezeLimit`, `shutdownGrace`, `disappearedWindow`, `idleStableWindow`, `ptyRows`, `ptyCols`, `ptyQuietWindow`, `clearLineSettle`
- Regex/glyph: `spinnerRe`, `ansiRe`, `idleGlyph`, `spinnerGlyph`
- Types/funcs: `rollingBuffer` (with `quietFor`), `matchSpinner`, `isIdle`, `hasSpinnerGlyph`, `tracker` + methods, `waitUntil`, `projectsDir`, `encodeCwd`, `resolveSession`, `openSessionJSONL`, `tailJSONL`, `isEndTurn`, `isToolUse`, `msgIDOf`, `extractByMsgID`, `typePrompt`, `clearInputLine`

The `run` shape adapts: instead of one session running three probes, `run` now calls `runSession` twice (see § *Two-session orchestration*). The PTY allocation / reader goroutine / watchdog goroutine / tailer-open hook / shutdown defer all carry over from spike #11's `run` body — they just live inside `runSession` instead of `run`.

### New code (the spike's actual contribution)

1. **`modalRe` / `modalGlyph` predicate primitives** — the modal-detection signals. Choose between (a) a cheap "any box-drawing char in stripped buffer" check (`bytes.ContainsAny(stripped, "╭╰│╮╯┌└├")`) and (b) a more specific literal-text check (e.g. `bytes.Contains(stripped, []byte("Do you want to allow"))`). See § *Modal detection*.
2. **`hasModal(snap []byte) bool`** — the predicate that wraps whichever detection strategy ships. Returns true when the modal is currently visible in the rolling buffer (after ANSI strip).
3. **`extractModalText(snap []byte) string`** — best-effort extraction. ANSI-strip, find the modal region (between top/bottom box borders OR the last contiguous block of lines containing `│` glyphs), strip the border characters, return cleaned text. If extraction can't find a coherent region, return the entire stripped buffer (the README documents the failure mode if so).
4. **`sendKeystroke(ptmx *os.File, keystroke []byte) error`** — the generic single-byte-bulk-write helper. Same body as spike #11's `sendCancel`; renamed because this spike uses it for the approve keystroke too. (Document the rename in the attribution comment.)
5. **`ProbeKind` + per-kind drivers `runObserve`, `runAutoRespond`, `runEscalate`.** See § *Per-probe drivers*.
6. **`runSession(ctx, logger, sessionID, probes []probeSpec, openTailerOnFirstPrompt bool) error`** — the per-session driver that owns PTY allocation, reader goroutine, watchdog, tailer-open hook, shutdown defer. Called twice from `run`. See § *Two-session orchestration*.

New constants:

- `modalDetectLimit = 30 * time.Second` — how long each probe waits for the modal to appear after `prompt-written`. Long enough for claude's startup + first-token + permission-prompt rendering; short enough that a wedged probe doesn't sit forever. Equivalent purpose to spike #11's `waitConditionLimit`.
- `observationWindow = 5 * time.Second` — Probe 1's window for JSONL-event observation after modal detection (per AC).
- `escalationWindow = 3 * time.Second` — Probe 3's window for confirming the modal stays open (per AC).
- `modalClearedLimit = 30 * time.Second` — Probe 2's window for "approve keystroke sent → modal-cleared." Aggressive vs the session-level 60 s — modal clearing should be near-instant (per AC's "watchdog: 30s no state transition → abort").
- `endTurnAfterApproveLimit = 30 * time.Second` — Probe 2's window for "modal-cleared → `end_turn`-detected." Same aggression rationale.

New flag:

- `-approve-keystroke {1-enter|y-enter|enter|down-enter}` (default `1-enter`) — empirical-iteration mechanism analogous to spike #11's `-cancel-keystroke`. See § *Single-byte keystroke writer*.

### Single-byte keystroke writer

Renames `sendCancel` to `sendKeystroke` because this spike uses the same writer for the approve keystroke. The body is identical: one bulk `pty.Write` of the provided byte slice, no inter-byte delay.

```go
// sendKeystroke writes the keystroke bytes as a single bulk write. The
// keystroke is at most a few bytes (approve: "1\r" / "y\r" / "\r" /
// "\x1b[B\r"). The inter-byte-delay reasoning that drives typePrompt
// (claude's input handler can swallow `\r` arriving in the same
// buffered write as the prompt body after a tool-use wind-down) does
// not apply here — the keystroke is short enough that ordering issues
// don't surface, and any `\r` is INTENDED (vs typePrompt where the
// `\r` is the submission boundary).
func sendKeystroke(ptmx *os.File, keystroke []byte) error
```

The `-approve-keystroke` flag → bytes mapping:

| Flag value | Bytes | Hex |
|---|---|---|
| `1-enter` (default) | `1` `\r` | `31 0d` |
| `y-enter` | `y` `\r` | `79 0d` |
| `enter` | `\r` | `0d` |
| `down-enter` | `\x1b[B` `\r` | `1b 5b 42 0d` |

`parseApproveKeystroke(name string) (bytes []byte, hexStr string, err error)` mirrors spike #11's `parseCancelKeystroke`. The flag-default rationale is that the AC suggests `1\r` ("Yes, once" on a numbered prompt) as the most likely shape; if it doesn't fire, the dev iterates through the alternatives.

The log line `probe=N response-keystroke-sent bytes=<hex>` shows the actual hex written so the README can record the value that worked.

### Modal detection

Two-tier strategy. The spike implements both and the README documents which proved more reliable:

**Tier 1 — Cheap box-drawing predicate.**

```go
var boxDrawingBytes = []byte("\xe2\x95\xad\xe2\x95\xb0\xe2\x94\x82")  // ╭ ╰ │
func hasModal(snap []byte) bool {
    stripped := ansiRe.ReplaceAll(snap, nil)
    return bytes.ContainsAny(stripped, string(boxDrawingBytes))
}
```

Concern: claude's normal TUI may use box-drawing for non-modal elements (status bar borders, the input box itself). The spike's Probe 1 phase establishes whether `bytes.ContainsAny(buffer, boxDrawingBytes)` is true at idle (false positive baseline). If yes, the predicate needs tightening.

**Tier 2 — Literal-text predicate (fallback).**

If Tier 1's false-positive rate is unacceptable, swap to a literal-text check. Candidates (the dev empirically picks what's actually in the modal text):

- `bytes.Contains(stripped, []byte("Do you want to allow"))`
- `bytes.Contains(stripped, []byte("Allow"))`
- `bytes.Contains(stripped, []byte("Yes, once"))`

The spec does NOT pre-pick which literal text to use; that's the spike's empirical job. The README documents the chosen predicate's exact regex/bytes plus the false-positive baseline at idle.

If neither tier works (e.g. the modal uses cursor positioning that paints into a buffer region the rolling buffer can't observe), document the surprise — that itself is empirically valuable for the layer-2 design.

### Modal text extraction

Best-effort. The README documents the exact extraction strategy that shipped plus what it captured per probe.

Default strategy (the dev can revise based on empirical data):

1. `stripped := ansiRe.ReplaceAll(snap, nil)`
2. Split on newlines.
3. Find the contiguous block of lines containing `│` (vertical bar) — that's the modal region.
4. For each line in the region: trim leading/trailing `│ ` and whitespace.
5. Concatenate with `\n`. That's the modal text.

If the modal uses non-bordered layout (e.g. plain text with positional cursor), the dev falls back to "the last N lines of the stripped buffer before the input prompt." Either approach is acceptable; the README documents.

The log line `probe=N modal-text-extracted text=%q` truncates the text to 200 bytes (with `...` suffix if longer) so the live log stays readable; the full extraction is captured for the README via post-hoc inspection (the dev `tee`s stderr to a file).

### Tool-name extraction (best-effort)

Probe 3's escalation log includes a `tool=<name>` field if extractable. Heuristic:

- Look for lines starting with `Bash`, `Read`, `Write`, `Edit`, `WebFetch`, etc.
- If found, use the first match.
- Otherwise log `tool=<unknown>`.

This is empirical-quality, not load-bearing — the README's *Escalation contract sketch* documents what the consumer callback would actually need (and how confidently the spike could extract each field). Failure to extract tool name is itself a finding worth recording.

### Two-session orchestration

Probes 1 and 2 each need a fresh `claude` session because Probe 1's "SIGTERM after observation" leaves the modal open. Probe 3 reuses Probe 2's session per AC ("in the same session").

`run` becomes:

```go
func run(sessionIDFlag string, approveKey []byte, approveHex string) error {
    // Resolve projects-dir once.
    // Session A: Probe 1 (observe + shutdown SIGTERMs)
    sessionA := newSessionID(sessionIDFlag, "a")
    if err := runSession(rootCtx, logger, sessionA, projDir, approveKey, approveHex,
        []probeSpec{{kind: kindObserve, prompt: probe1Prompt}}); err != nil {
        return fmt.Errorf("session A: %w", err)
    }
    // Session B: Probe 2 (auto-respond + complete turn) → Probe 3 (escalate + shutdown SIGTERMs)
    sessionB := newSessionID("", "b")  // always fresh, even if -session-id provided
    if err := runSession(rootCtx, logger, sessionB, projDir, approveKey, approveHex,
        []probeSpec{
            {kind: kindAutoRespond, prompt: probe2Prompt},
            {kind: kindEscalate,    prompt: probe3Prompt},
        }); err != nil {
        return fmt.Errorf("session B: %w", err)
    }
    return nil
}
```

`runSession` owns the per-session lifecycle: PTY allocation, reader goroutine, watchdog goroutine, tailer-open hook (fired on first probe's `prompt-written`), shutdown defer (SIGTERM → 3 s grace → SIGKILL → close PTY → cancel context → `wg.Wait`). Its body is the body of spike #11's `run` from `cmd := exec.Command(...)` through `wg.Wait()`, with the probe-loop body calling `runObserve` / `runAutoRespond` / `runEscalate` based on `kind`.

`newSessionID(flagValue, suffix string) string` generates a fresh UUIDv4 when `flagValue == ""`, else parses the flag. The `suffix` is purely for log clarity (`session-id-resolved id=<uuid> jsonl=<path> tag=a|b`); it does NOT influence the UUID.

The `-session-id` flag (if set) is consumed by Session A only. Session B always generates its own fresh UUID; you can't pin both sessions to the same UUID because they're distinct `claude` invocations producing distinct JSONL files. The README documents this asymmetry.

`probeSpec` matches spike #11's shape:

```go
type ProbeKind int
const (
    kindObserve ProbeKind = iota
    kindAutoRespond
    kindEscalate
)
type probeSpec struct {
    kind   ProbeKind
    prompt string
}
```

Probe prompts (default; the dev can swap if a chosen prompt fails to trigger a modal reliably — document in README):

| Probe | Default prompt | Tool invoked | Notes |
|---|---|---|---|
| 1 | `list the files in /tmp` | Bash | AC's first example; smaller than spike #11's recursive variant so the modal fires before claude does much pre-work. |
| 2 | `list the files in /tmp` | Bash | Same prompt as Probe 1 — Probe 2 validates that a freshly-spawned session, given the same trigger, reaches `SUCCESS` once approved. Useful symmetry with Probe 1's observation. |
| 3 | `read /etc/hostname` | Read | DIFFERENT tool from Probe 2 to force a fresh permission modal in Session B (claude may remember a "yes, once" for Bash from Probe 2 — using Read guarantees a new prompt). |

If Probe 1's `list the files in /tmp` doesn't fire a modal (e.g. claude has internalised a project-level allowlist), the dev swaps to `recursively list all files under /tmp` (the prompt spike #11 used) or `read /etc/hostname`. README documents the actual prompts.

### Per-probe drivers

Each driver is a thin wrapper around shared helpers. The contract — the developer implements the body; the order and content of log lines is the contract.

#### `runObserve` (Probe 1, Session A's only probe)

```go
// runObserve: trigger modal, capture raw bytes + extracted text, log every
// JSONL event for observationWindow, return. Shutdown defer SIGTERMs.
func runObserve(
    ctx context.Context,
    logger *log.Logger,
    probeN int,
    prompt string,
    ptmx *os.File,
    rb *rollingBuffer,
    eventCh <-chan map[string]any,
    tr *tracker,
    postPromptHook func() error,
) error
```

Behavior contract:

1. Log `probe=N probe-start kind="observe" prompt=%q`. Bump `tr`.
2. `clearInputLine(ptmx)` (idempotent — Session A's input box is clean from boot). `typePrompt(ptmx, prompt)`. Log `probe=N prompt-written`. Bump `tr`.
3. `postPromptHook()` (opens deterministic JSONL, starts tailer).
4. Wait for `hasModal(rb.snapshot())` true OR `modalDetectLimit` elapses. On timeout: `fmt.Errorf("probe %d: modal not detected within %s", probeN, modalDetectLimit)`. On detection: log `probe=N modal-detected pattern="<box-drawing|literal-text>"`. Bump `tr`. (`pattern` is the predicate variant that actually fired — the dev wires this through.)
5. Snapshot the rolling buffer immediately. Log `probe=N modal-bytes-snapshot len=%d` (the length is the snapshot's byte length). Persist the snapshot to a tempfile (`/tmp/spike-permission-probe1-bytes-<timestamp>.bin`) — created with `os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)` (NOT `os.WriteFile`, whose default mode is 0644). The 0600 mode is defensive against multi-user systems; the modal text for this spike's benign prompts isn't credential-sensitive but the `security-sensitive` label says be explicit. Log `probe=N modal-bytes-snapshot-path=<path>`. Bump `tr`.
6. Run `extractModalText(snap)`. Log `probe=N modal-text-extracted text=%q` (truncated to 200 bytes for the live log; the tempfile from step 5 has the full bytes for post-hoc inspection). Bump `tr`.
7. Drain `eventCh` for `observationWindow` (5 s), logging each event as `probe=N observation-event type=assistant stop_reason=<value> msg_id=<id>` (use `<nil>` / `<missing>` per spike #11's `logCancelEvent` shape). Bump `tr` once at the start of the window and once when it ends.
8. Return `nil`. The session-level shutdown defer fires SIGTERM. (Probe 1 EXPECTS to leave the modal open — that's the spike's whole point.)

The dev verifies post-exit that no orphan `claude` processes survive (`pgrep -lf 'claude --session-id'`) — same check as spike #11.

#### `runAutoRespond` (Probe 2, Session B's first probe)

```go
// runAutoRespond: trigger modal, send approve keystroke, wait for the
// modal to clear AND the turn to complete (end_turn + isIdle stable),
// extract assistant text by msg_id, print SUCCESS.
func runAutoRespond(
    ctx context.Context,
    logger *log.Logger,
    probeN int,
    prompt string,
    approveKey []byte,
    approveHex string,
    ptmx *os.File,
    rb *rollingBuffer,
    eventCh <-chan map[string]any,
    tr *tracker,
    postPromptHook func() error,
) error
```

Behavior contract:

1. Log `probe=N probe-start kind="auto-respond" prompt=%q`. Bump `tr`.
2. `clearInputLine(ptmx)` + `typePrompt(ptmx, prompt)`. Log `probe=N prompt-written`. Bump `tr`.
3. `postPromptHook()` (Session B's first probe — opens deterministic JSONL, starts tailer).
4. Wait for `hasModal(rb.snapshot())` true OR `modalDetectLimit` elapses. On detection: log `probe=N modal-detected pattern="<...>"`. Bump `tr`.
5. (Optional but useful: capture + log `probe=N modal-text-extracted text=%q` so the README has Probe 2's modal text for comparison with Probes 1 and 3.)
6. `sendKeystroke(ptmx, approveKey)`. Record `keystrokeSentAt := time.Now()`. Log `probe=N response-keystroke-sent bytes=%s`. Bump `tr`.
7. Wait for `modal-cleared`: predicate is `!hasModal(rb.snapshot()) AND isIdle(rb.snapshot()) AND rb.quietFor() >= ptyQuietWindow`, all three for one tick. Deadline `modalClearedLimit` (30 s) from `keystrokeSentAt`. On timeout: `fmt.Errorf("watchdog: modal not cleared after approve within %s", modalClearedLimit)`. On success: log `probe=N modal-cleared`. Bump `tr`.

   **NOTE:** `isIdle` is satisfied continuously (empirical fact #1), so the `isIdle` clause is structurally redundant — but include it anyway for symmetry with the `❯-reappeared` predicate from spike #11 and to defend against a future claude version that paints the modal in a way that leaves `❯` missing during the modal window. If empirically this clause is the slow link, the dev can drop it and document.
8. Wait for `end-turn-detected`: accumulate assistant events from `eventCh`, predicate is `gotEndTurn AND isIdle(rb) stable for idleStableWindow`. Deadline `endTurnAfterApproveLimit` (30 s) from `keystrokeSentAt`. Same shape as spike #11's `runRecovery`. On success: log `probe=N end-turn-detected msg_id=%s`. Bump `tr`.
9. `text := extractByMsgID(events, latestEndTurnMsgID)`. Log `probe=N assistant-text-extracted len=%d`. Bump `tr`. `fmt.Printf("SUCCESS: %s\n", text)`.
10. Return `nil`.

Steps 7 + 8 may overlap in practice — the modal-cleared event and the `end_turn` event can fire close together. The dev can fuse them into one composite predicate if it's cleaner, but log lines must still fire in the order: `response-keystroke-sent` → `modal-cleared` → `end-turn-detected`.

#### `runEscalate` (Probe 3, Session B's second probe)

```go
// runEscalate: trigger ANOTHER modal in Session B, capture extraction
// data, log what a consumer callback would receive, sleep escalationWindow
// to verify the modal stays open (no auto-response), return.
func runEscalate(
    ctx context.Context,
    logger *log.Logger,
    probeN int,
    prompt string,
    ptmx *os.File,
    rb *rollingBuffer,
    eventCh <-chan map[string]any,
    tr *tracker,
) error
```

Behavior contract:

1. Log `probe=N probe-start kind="escalate" prompt=%q`. Bump `tr`.
2. Residual-event drain (same non-blocking drain as spike #11) so Probe 2's straggler events don't pollute Probe 3's wait.
3. `clearInputLine(ptmx)` + `typePrompt(ptmx, prompt)`. Log `probe=N prompt-written`. Bump `tr`.
4. Wait for `hasModal(rb.snapshot())` true OR `modalDetectLimit` elapses. On detection: log `probe=N modal-detected pattern="<...>"`. Bump `tr`.
5. Snapshot rolling buffer → tempfile (same `os.OpenFile(..., 0600)` shape as `runObserve` step 5) + log `probe=N modal-bytes-snapshot len=%d path=%s`. Bump `tr`.
6. Extract text + tool name: `text := extractModalText(snap)`; `tool := extractToolName(snap)`. Log `probe=N modal-text-extracted text=%q` and `probe=N modal-escalation-callback-would-receive text=%q tool=%s` (the AC's prescribed line). Bump `tr`. Log `probe=N escalation-simulated`. Bump `tr`.
7. Sleep `escalationWindow` (3 s). After the sleep, verify the modal is still present (`hasModal(rb.snapshot())`); log `probe=N modal-still-open=true|false`. If false, that's a surprise worth recording — the modal cleared without a keystroke, which suggests either auto-timeout (unlikely) or some claude-side state we don't understand. Either way, log and continue. Bump `tr`.
8. Return `nil`. Session-level shutdown defer SIGTERMs Session B.

### State log lines (the full sequence the spike emits)

Per-session (fire once each per session — twice total because there are two sessions):

```
session-id-resolved id=<uuid> jsonl=<path> tag=<a|b>
idle-detected tag=<a|b>
session-jsonl-opened path=<path> offset=0 tag=<a|b>     # after the first probe's prompt-written
shutdown-signalled tag=<a|b>
```

Per-probe lines, Probe 1 (kindObserve):

```
probe=1 probe-start kind="observe" prompt=%q
probe=1 prompt-written
probe=1 modal-detected pattern="<box-drawing|literal-text>"
probe=1 modal-bytes-snapshot len=<N>
probe=1 modal-bytes-snapshot-path=<path>
probe=1 modal-text-extracted text=%q                    # truncated to 200 bytes
probe=1 observation-window-start window=<duration>
probe=1 observation-event type=assistant stop_reason=<value> msg_id=<id>   # 0+ times
probe=1 observation-window-end events=<count>
```

Per-probe lines, Probe 2 (kindAutoRespond):

```
probe=2 probe-start kind="auto-respond" prompt=%q
probe=2 prompt-written
probe=2 modal-detected pattern="<...>"
probe=2 modal-text-extracted text=%q                    # optional; useful for README comparison
probe=2 response-keystroke-sent bytes=<hex>
probe=2 modal-cleared
probe=2 end-turn-detected msg_id=<id>
probe=2 assistant-text-extracted len=<n>
```

Per-probe lines, Probe 3 (kindEscalate):

```
probe=3 probe-start kind="escalate" prompt=%q
probe=3 prompt-written
probe=3 modal-detected pattern="<...>"
probe=3 modal-bytes-snapshot len=<N>
probe=3 modal-bytes-snapshot-path=<path>
probe=3 modal-text-extracted text=%q
probe=3 modal-escalation-callback-would-receive text=%q tool=<name>
probe=3 escalation-simulated
probe=3 modal-still-open=true|false
```

Watchdog (if it trips) keeps the `watchdog:` prefix per the established convention.

Ordering invariants:

- Session A: `session-id-resolved tag=a` → `idle-detected tag=a` → `probe=1 probe-start` → `probe=1 prompt-written` → `session-jsonl-opened tag=a` → ... → `shutdown-signalled tag=a`.
- Session B starts after Session A's shutdown completes (sequential, not parallel — `runSession` blocks). Same shape with `tag=b` and Probes 2 + 3 inside.

### Concurrency model

Same three-goroutine shape as spike #11, but per-session (twice over):

| Goroutine | Owns | Exit |
|---|---|---|
| `main` orchestrator (within `runSession`) | linear state machine across the session's probes, watchdog tick coordination, msg_id extraction (Probe 2 only), per-probe deadlines | last probe returns OR watchdog trips OR error |
| PTY reader (per session) | rolling buffer (mutex-protected); mirrors raw bytes to stderr | EOF from PTY master (when shutdown closes it) |
| JSONL tailer (per session) | events channel (buffered, size 32); filters to assistant-with-message | context cancellation |

The events channel is session-scoped (one channel per `runSession` call). Probes 2 + 3 (Session B) share the channel; Probe 2 accumulates events for `end_turn`, Probe 3 drains residuals at start.

Session A and Session B do NOT share state — separate rolling buffers, trackers, channels, PTYs. The `tracker`'s session-level inactivity watchdog (60 s) restarts when Session B's `runSession` starts.

### Shutdown

Identical to spike #11, per session — single `defer` running SIGTERM → 3 s grace → SIGKILL → close PTY → cancel context → `wg.Wait()` under `sync.Once`. Fires on every exit path. The `shutdown-signalled tag=<a|b>` log line fires once at the start of the shutdown body.

Because Session A's shutdown leaves a permission modal open mid-claude-process, the SIGTERM → SIGKILL chain is the primary cleanup mechanism. Post-exit, the dev verifies `pgrep -lf 'claude --session-id <session-A-uuid>'` returns nothing (no orphans). Same check for Session B's UUID after the full binary exits.

A more defensive shutdown would kill the spawned process's *group* (via `syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)` after setting `SysProcAttr.Setpgid = true` at spawn time) so any subprocess `claude` forked could not escape. The predecessor spikes (#9, #11) don't do this and have never observed orphans in practice; staying consistent with their pattern here. The `pgrep` verification step is the safety net. Deferred to the `pkg/tuidriver/` library extraction, where it belongs as a single library-wide policy decision.

### Error handling

Failure modes and recovery:

- **`modalDetectLimit` timeout (any probe step 4):** `fmt.Errorf("probe %d: modal not detected within %s", probeN, modalDetectLimit)`. Likely cause: claude has internalised a project-allowlist for the chosen tool (`list the files in /tmp` doesn't fire a modal here), OR the prompt didn't invoke the expected tool, OR `bypassPermissions` is still set in environment / config somewhere. README documents the failure and the dev swaps prompts or removes config.
- **`modalClearedLimit` timeout (Probe 2 step 7):** `fmt.Errorf("watchdog: modal not cleared after approve within %s", modalClearedLimit)`. Likely cause: wrong approve keystroke. README documents; dev reruns with `-approve-keystroke <alternative>`.
- **`endTurnAfterApproveLimit` timeout (Probe 2 step 8):** `fmt.Errorf("watchdog: end_turn not detected after approve within %s", endTurnAfterApproveLimit)`. Likely cause: modal was approved but claude wedged in tool execution (or got into another modal we don't know about). README documents and post-hoc JSONL inspection diagnoses.
- **Session A `claude` orphan after exit:** `pgrep -lf 'claude --session-id <uuid>'` returns matches. Likely cause: SIGKILL didn't take. Manual `pkill -9 -f 'claude --session-id <uuid>'`. README documents if this ever happens.
- **PTY EOF:** PTY reader returns; session-level watchdog (60 s inactivity) trips. Shutdown handles cleanup.

No special-case error paths beyond these. The `-approve-keystroke` flag's four values + the README's run-by-run notes are the empirical-iteration mechanism; the spike itself does not auto-retry with different keystrokes.

## Testing strategy

Same as the predecessor spikes: verified by execution against real `claude`, not by automated tests. The developer:

1. Builds: `go build -o /tmp/spike-permission ./cmd/spike-permission`
2. Runs from a directory where `claude` is on `$PATH` and the user has a valid Claude subscription session.
3. Default run: `/tmp/spike-permission`. Expected outcome on green:
   - Session A: Probe 1 logs `modal-detected` → `modal-bytes-snapshot` → `modal-text-extracted` → `observation-window-end` → Session A shutdown.
   - Session B: Probe 2 logs `modal-detected` → `response-keystroke-sent` → `modal-cleared` → `end-turn-detected` → `assistant-text-extracted`; stdout shows one `SUCCESS:` line (with claude's response to "list the files in /tmp"). Probe 3 logs `modal-detected` → `modal-escalation-callback-would-receive` → `escalation-simulated` → `modal-still-open=true` → Session B shutdown.
4. If the default approve keystroke doesn't work (Probe 2's `modalClearedLimit` trips): rerun with `-approve-keystroke y-enter`, then `enter`, then `down-enter`. Record each attempt's outcome in the README's *Approve keystroke that worked* section.
5. Runs the green configuration 2–3 times to populate the README's timing tables and catch run-to-run variance.
6. Verifies post-exit: `pgrep -lf 'claude --session-id'` returns nothing (no orphan claude processes from either session); `pgrep -lf '^ls'` returns nothing (no orphan tool subprocesses from Probe 2's approved Bash invocation).
7. Post-hoc JSONL inspection (two JSONL files, one per session): `cat ~/.claude/projects/<encoded-cwd>/<session-A-uuid>.jsonl | jq -c '.type'` to bucket envelope types during Probe 1's observation window — does anything modal-related land in the JSONL or is the modal purely a PTY affordance? Same inspection on Session B's JSONL — does the approve action emit anything? Same inspection on Probe 3's modal-open period — same question.
8. The dev `tee`s stderr to a file (`./spike-permission.log`) so the README's *Modal PTY shape* section can quote the raw bytes around `modal-detected` for each probe — same data the `modal-bytes-snapshot-path=` tempfile holds, but easier to navigate alongside the live log.

Negative-path validation (do once):

- While Probe 1 is between `modal-detected` and the end of `observationWindow`, kill the spawned `claude` from another terminal (`pkill -TERM -f 'claude --session-id'`). Confirm the spike either continues Probe 1's window to completion (because the PTY may stay alive briefly) or exits within a few seconds with a watchdog or PTY-error message. Same negative-path shape as spike #11.

No unit tests. Future `pkg/tuidriver/` tests (post-spike) will mock the PTY; out of scope.

## README requirements

The README is the actual deliverable of this spike — the new empirical facts get captured here, not in code comments. Mirror `cmd/spike-cancel/README.md`'s shape. Required sections, in this order:

1. **Status** — one paragraph: dates, what shipped, link to ticket #13 and to this spec, headline findings (which approve keystroke worked, what shape the modal has, whether the modal emits anything to the JSONL).
2. **What it does** — the 1-paragraph end-to-end summary plus the per-probe prompt table from § *Two-session orchestration*.
3. **How to run** — build command, run command, the `-approve-keystroke` flag, expected output shape (`SUCCESS:` × 1 from Probe 2), required state log line sequence (the full list from § *State log lines*), the `pgrep` verification commands.
4. **Approve keystroke that worked** — one paragraph naming the keystroke (`1\r`, `y\r`, `\r`, or `\x1b[B\r`) plus the hex bytes. If multiple keystrokes were tried, a table: `attempt | keystroke | hex | outcome (worked / timed-out / claude-emitted-X)`. Same shape as spike #11's "Cancel keystroke that worked" table.
5. **Modal PTY shape** — for each probe (1, 2, 3), document:
   - **Raw byte excerpt** — paste ~500 bytes around `modal-detected` (with ANSI escape sequences left in) so the next developer can see what the rolling buffer actually contained. Source: the `modal-bytes-snapshot-path=` tempfile or the `tee`d stderr log.
   - **Stripped text** — same excerpt after `ansiRe.ReplaceAll(snap, nil)`. Shows what `extractModalText` operated on.
   - **Box-drawing characters used** — list every Unicode box-drawing codepoint that appeared (e.g. `╭`, `╮`, `╰`, `╯`, `│`, `─`).
   - **ANSI color codes used** — list the SGR sequences observed (e.g. `\x1b[33m` for yellow, `\x1b[0m` for reset). If colors are load-bearing for visual distinction the consumer would want to forward, note that.
   - **Cursor positioning** — does the modal use any `\x1b[<n>H` cursor moves that paint outside the buffer's recent-bytes window? If yes, that's a surprise and the README documents (this is the "modal that doesn't fit the rolling buffer" failure mode the AC anticipates).
   - **Layout** — sketch the modal's visual structure (header, body, option list, selection cursor) with column widths so the eventual layer-2 designer has the picture.
6. **JSONL events recorded during/around each modal** — for each probe, a table: row per event, columns `idx`, `type`, `stop_reason` (raw value), `msg_id`, `content block types in order`, `arrived-relative-to (probe-start / modal-detected / keystroke-sent / window-end)`. If nothing arrives during a modal-open window, say so explicitly — that's the load-bearing finding for the layer-2 design (PTY-side detection is the only signal we have).
7. **Pattern-detection regex shape** — what the spike actually used to detect the modal. Cheap predicate (any box-drawing char in stripped buffer), specific predicate (literal text), and the empirical false-positive baseline (does `hasModal` return true at idle, before any prompt?). Recommend which the eventual library should use.
8. **Post-response state machine timing** (Probe 2) — table: `prompt-written → modal-detected (ms)`, `modal-detected → keystroke-sent (ms)` (should be ~0), `keystroke-sent → modal-cleared (ms)`, `keystroke-sent → end-turn-detected (ms)`, `total (ms)`. At least 2 runs.
9. **Escalation contract sketch** — what data Probe 3 captured that a consumer callback would need. Concrete proposal (not implementation — the library API design happens later):
   - `Text string` — the cleaned-up modal text.
   - `RawBytes []byte` — the raw rolling-buffer snapshot, for fallback when extraction misses something.
   - `Tool string` — the tool name (best-effort; may be empty).
   - `Options []string` — the option list as extracted (best-effort; may be empty if layout-based extraction fails).
   - `SuggestedResponse []byte` — the bytes that auto-respond WOULD have written (e.g. `1\r`); lets the consumer surface "approve" as a one-click action if they don't want to do their own parsing.
   - For each field, note whether the spike could extract it reliably or whether it's "data the consumer would need but the spike couldn't always get."
10. **Surprises / findings** — numbered list, same shape as spike #11. Anything that contradicted ticket-body predictions, anything that surprised, anything that's a candidate follow-up ticket (e.g. multiselect modals, trust-workdir prompt, modals that don't fit the rolling buffer).

Post-hoc data the README needs (and how to get it without changing the tailer filter): once each session completes, `cat ~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl | jq -c '.type'` to bucket envelope types; `jq 'select(.type=="assistant") | {stop_reason: .message.stop_reason, msg_id: .message.id}'` to walk stop_reason values during/around the modal windows. The tempfile from `modal-bytes-snapshot-path=` is the source of truth for raw modal bytes.

## Open questions for the dev

The README answers these; this spec does not pre-decide.

- **Does the cheap box-drawing predicate produce false positives at idle?** Pre-prompt, after `idle-detected`, log `idle-predicate-check has_modal=%v` once for diagnostic. If true, the cheap predicate is unusable as-is and the dev falls back to literal text (or a region-restricted check that ignores the persistent status-bar borders).
- **Does Probe 1's `list the files in /tmp` actually trigger a modal under the locally-installed `claude` 2.1.x?** If claude has an allowlist that swallows the Bash invocation silently, swap to `read /etc/hostname` (Read tool) or `recursively list all files under /tmp` (heavier Bash; the prompt spike #11 used). README documents the actual prompt that worked.
- **Is the approve keystroke single-keystroke-plus-enter (`1\r`), or does claude require arrow navigation first (`\x1b[B\r` for "down then enter")?** The flag's four values cover the common cases; if none work, the dev tries `2\r` / `3\r` (in case the default option is "no" and "yes" is option 2 or 3) and documents in the README's *Approve keystroke that worked* table.
- **Does Probe 2's post-approve flow produce an `assistant(stop_reason=tool_use)` line followed by `user(tool_result)` followed by `assistant(stop_reason=end_turn)`, same as spike #11 Probe 2's pre-cancel shape?** Hypothesis: yes, identical (the bypass flag just skipped the modal; with the modal answered, the tool-use sequence is the same). Post-hoc `jq` inspection confirms or denies.
- **For Probe 3, does the second modal in Session B look identical to Probe 2's first modal?** Hypothesis: yes for shape; possibly different option layout if claude remembers Probe 2's approval and pre-selects a default option. The Read-vs-Bash tool difference is intentional to force a fresh permission; if Read fires the same Bash-shaped modal, document.
- **Does the modal stay open indefinitely (Probe 3 step 7), or does claude auto-timeout?** Hypothesis: stays open. If `modal-still-open=false` after the 3 s window, that's a surprise — claude has some auto-timeout we don't know about, which has implications for layer-2 (the consumer's escalation callback may need to respond within a deadline).
- **`modalDetectLimit = 30 s`** is a guess. If empirically modals appear within ~1–2 s of `prompt-written`, the spec value is mostly headroom. If they take 5–10 s (because claude has to plan before requesting the tool), the value is right-sized. Document.
- **Cursor positioning surprise.** If the modal uses `\x1b[<n>;<n>H` absolute cursor moves and paints into a buffer region the 4096-byte rolling buffer doesn't see (or the modal redraws so frequently that the rolling buffer churns past it during the detection wait), the predicate may fire flakily or never. Document the redraw cadence (count modal-detected fires per second during a probe — should be 1).

## Out of scope (reminder)

Same exclusions as spikes #1, #9, #11, plus the ones the ticket adds:

- `pkg/tuidriver/` library API extraction (primary post-spike next move)
- Multiselect modals (`/doctor`, slash-command pickers — sibling spike per ticket body)
- Layer 1 (settings validation, pre-trust workdir, version pinning — operational)
- Layer 3 (detect unknown modals + bail safely — trivial follow-up once layer-2 detection primitives exist)
- Network / connectivity error modals (rare, hard to trigger deterministically)
- Trust-workdir prompt (first-run in untrusted directory — fires once per workdir; operator runbook)
- Auto-update prompt (pinnable via claude version management)
- Parallel-tool-use stress test (still outstanding from spike #2)
- Slow-tool cancellation probe (does ESC kill a long-running tool? — open since spike #11)
- Modal-during-cancel race (cancel sent while modal is up — different shape; out of scope)
- Spinner-regex / ANSI-strip refinement (finding #8 still open since spike #1)

## Security review

**Verdict:** PASS

**Findings:**

- **[Trust boundaries]** No findings — the spike's input surface is hard-coded probe prompts plus a `-session-id` UUID flag (parsed by `uuid.Parse`, no shell metacharacters reachable). Data flow is single-trust-domain: `claude` subprocess → PTY → rolling buffer → log lines / 0600 tempfile. No network, no operator stdin, no untrusted file reads.
- **[Tokens, secrets, credentials]** No findings — `uuid.NewRandom()` in `resolveSession` / `newSessionID` uses `crypto/rand` internally. The UUID is a session identifier (used as a filename and `--session-id` argv), not a credential — visibility in logs and on-disk under `~/.claude/projects/` is the same affordance prior spikes already established. Claude's own auth tokens live in claude's config; the spike never sees or handles them.
- **[File operations]** Resolved inline — § *Per-probe drivers* `runObserve` step 5 and `runEscalate` step 5 explicitly require `os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)` for the modal-bytes tempfile under `/tmp` (rejecting `os.WriteFile`'s default 0644 mode). Path traversal: `encodeCwd` does byte-level `/` → `-` and `.` → `-` substitution on `os.Getwd()` output (not user input); `..` cannot appear. TOCTOU between `os.Stat` and `os.Open` in `openSessionJSONL`/`tailJSONL` is a single-user single-trust-domain race and is not exploitable in the spike's scope (same posture as predecessor spikes).
- **[Subprocess execution]** Resolved inline — § *Shutdown* explicitly acknowledges that process-group kill (`Setpgid` + `kill(-pid)`) would be more robust against double-fork escapes, but the predecessor spikes' SIGTERM-only pattern is reused for consistency and the `pgrep -lf 'claude --session-id <uuid>'` post-exit verification (§ *Testing strategy* step 6) is the safety net. The decision is deferred to the `pkg/tuidriver/` extraction where it belongs as a library-wide policy. `exec.Command("claude", "--session-id", sessionID)` passes args as a slice (no `sh -c`), so even with arbitrary `sessionID` bytes no shell interpretation happens.
- **[Cryptographic primitives]** No findings — no hashing, encryption, key derivation, or constant-time comparison surface. The only randomness consumer is `uuid.NewRandom()` (correct: `crypto/rand`).
- **[Network & I/O]** No findings on network (the spike has none). I/O is bounded: PTY read into the 4096-byte rolling buffer auto-rotates. JSONL tailer's `bufio.Reader.ReadBytes('\n')` has no per-line cap — consistent across spikes #1/#9/#11; deferred to the library extraction as a single policy decision (a giant `tool_result` line would buffer in memory, but claude doesn't emit such lines in practice).
- **[Error messages, logs, telemetry]** No findings — log lines contain probe transitions, session UUIDs (not credentials), JSONL paths, and modal text truncated to 200 bytes for the live log. Full modal bytes go to a 0600 tempfile. No stack traces; `fmt.Errorf` wrap pattern only. No telemetry / metrics.
- **[Concurrency]** No findings — single lock (`rollingBuffer.mu`), no multi-lock paths, no check-then-mutate on shared state. Every goroutine (PTY reader, watchdog, JSONL tailer) has a bounded exit path (PTY EOF, context cancellation) and `wg.Wait()` blocks shutdown until all exit. `sync.Once`-guarded shutdown defer prevents double-signal.
- **[Threat model alignment]** OUT OF SCOPE for the spike itself — this spike produces empirical data that the *consumer* of the eventual `pkg/tuidriver/` (the `pyry acp` server) will use to decide which permission modals to auto-approve and which to escalate to the host UI. The auto-approve policy is a security-sensitive decision that this spike does NOT pre-make; it only documents the modal's shape and the escalation contract sketch (§ *README requirements* item 9). The actual policy decision lives in the post-spike `pkg/tuidriver/` extraction ticket and the consumer-side ACP ticket. This spike's README must NAME the dangerous tools (Bash, Write, Edit) the modal can request so the downstream design treats them with appropriate caution.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-17

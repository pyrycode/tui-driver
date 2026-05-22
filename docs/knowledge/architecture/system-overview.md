# System overview

Where things live and how data flows. Update when modules, types, or data flows change.

## Current state

The library proper (`pkg/tuidriver/`) does **not exist yet**. The Go code in the repo is four throwaway spike binaries that exercise every primitive the eventual library will own without committing to an API:

- `cmd/spike-one-turn/` — single-turn happy path (idle → prompt → spinner → `end_turn` → SUCCESS). See [#1](../codebase/1.md), [#3](../codebase/3.md), [#4](../codebase/4.md), [#7](../codebase/7.md).
- `cmd/spike-multi-turn/` — three-turn loop (simple text → Bash tool use → slow thinking) with msg_id-grouped content extraction and a `runTurn` per-turn driver. See [#9](../codebase/9.md).
- `cmd/spike-cancel/` — three-probe cancellation binary (cancel during thinking → cancel during tool-use → recovery turn). Validates ESC as the cancel keystroke, documents the `user(text "[Request interrupted by user]")` JSONL cancellation marker, and verifies the same `--session-id` is recoverable post-cancel. See [#11](../codebase/11.md).
- `cmd/spike-permission/` — two-session three-probe permission-modal binary (observe modal with no response → auto-respond + complete turn → simulated escalation). First spike to drop `--permission-mode bypassPermissions`. Validates `1\r` as the approve keystroke, documents that permission modals have zero JSONL footprint (detection MUST be PTY-side), and sketches the consumer escalation-callback shape. See [#13](../codebase/13.md).

All four spikes share ~600 LOC of helpers under `// copied from cmd/spike-one-turn/main.go — keep in sync until library extraction` (or `…/spike-multi-turn/main.go` / `…/spike-cancel/main.go`) attribution comments. The duplication is deliberate — every spike binary deletes when `pkg/tuidriver/` lands.

## Intended modules (post-spike, not yet built)

- `pkg/tuidriver/` — public API: session lifecycle, state subscription, input writers. Shape will settle after multiple ticket cycles produce enough integration pressure to justify abstractions.

## Layout (today)

```
cmd/spike-one-turn/   # throwaway single-file spike (single turn)
  main.go             # PTY + reader + state machine + JSONL tailer + watchdog + shutdown
  README.md           # empirical observations log
cmd/spike-multi-turn/ # throwaway single-file spike (three turns)
  main.go             # adds runTurn per-turn driver + msg_id-grouped extractor + char-by-char typePrompt
  README.md           # multi-turn empirical observations log
cmd/spike-cancel/     # throwaway single-file spike (three cancellation probes)
  main.go             # adds runProbe (cancel-thinking/cancel-tool-use/recovery) + sendCancel
                      # + clearInputLine (Ctrl-U) + rollingBuffer.quietFor (PTY-quiescence)
  README.md           # cancellation empirical observations log
cmd/spike-permission/ # throwaway single-file spike (two sessions, three modal probes)
  main.go             # adds runSession (per-session orchestrator) + runObserve/runAutoRespond/
                      # runEscalate + hasModal (literal-text + box-drawing variants) +
                      # extractModalText (last-dash-run anchor) + extractToolName +
                      # sendKeystroke (renamed sendCancel) + oscRe
  README.md           # permission-modal empirical observations log
docs/
  specs/architecture/ # per-ticket specs from the architect
  knowledge/          # this directory (evergreen)
```

## Data flows

```
                            ┌──────────────────────────────┐
                            │ spawned `claude` process     │
                            │ (`--session-id <uuid>` pins  │
                            │  the JSONL filename)         │
                            └──┬──────────────────┬────────┘
                          PTY  │                  │  writes session JSONL to the
                       master  ▼                  ▼  deterministic path computed pre-spawn
              ┌───────────────────────┐    ┌─────────────────────┐
              │ PTY reader goroutine  │    │ JSONL tailer        │
              │ → rolling buffer (4K) │    │ → events channel    │
              │ → mirror to stderr    │    │   (assistant only)  │
              └──────────┬────────────┘    └──────────┬──────────┘
                  regex  │                            │  parsed JSON
                 matches │                            ▼
                         ▼                  ┌─────────────────────┐
              ┌───────────────────────┐     │ orchestrator        │
              │ state predicates:     │◄────┤ - waitUntil(...)    │
              │  - isIdle()           │     │ - waits for spinner-│
              │  - matchSpinner()     │     │   gone AND end_turn │
              └───────────────────────┘     │ - shutdown defer    │
                                            └─────────────────────┘
                                                      ▲
                                                      │  1 Hz tick
                                            ┌─────────┴───────────┐
                                            │ watchdog (60s/30s)  │
                                            └─────────────────────┘
```

## Concurrency model

Three goroutines per spike, coordinated by a single `context.WithCancelCause`:

| Goroutine | Owns | Exit |
|---|---|---|
| `main` orchestrator | linear state machine (single-turn) or per-turn driver loop (multi-turn), watchdog tick | state completes OR watchdog trips OR error |
| PTY reader | the rolling buffer (mutex-protected) | EOF from PTY master (happens when shutdown closes it) |
| JSONL tailer | the events channel (buffered, size 32) | context cancellation |

In `cmd/spike-multi-turn/` and `cmd/spike-cancel/` the events channel is **session-scoped, not turn-scoped** — one tailer goroutine services every turn/probe, and `runTurn` / `runProbe` non-blocking-drains residual events at turn start to discard any delta lines for the prior turn's msg_id that the tailer left buffered after `end_turn` was observed. Without the drain, the next turn's termination predicate could fire spuriously.

Shutdown is a `defer` with a `sync.Once`-guarded body: SIGTERM → 3 s grace race against `cmd.Wait()` → SIGKILL → close PTY → cancel context → `wg.Wait()`. Same sequence in all three spike binaries.

## Key signals

- **Idle:** `❯` glyph (UTF-8 `\xe2\x9d\xaf`) present in the ANSI-stripped rolling buffer AND the spinner regex does NOT match.
- **Thinking:** `✻\s+(\S+(?:\s+\S+)?)\s+for\s+(?:(\d+)m\s+)?(\d+)s` — captures a 1–2-word verb (variable per prompt) and a time-tail in `Ns` or `Nm Ns` form. Currently misses every observed verb in practice (CSI cursor-forward between glyph and verb eaten by ANSI strip; ellipsis-form verbs lack the `for Ns` counter); slow-path log lines are dormant pending a regex fix. A bare-glyph fallback (`hasSpinnerGlyph` — `bytes.Contains` against `\xe2\x9c\xbb` after ANSI strip) is the practical "claude has started processing" signal until the regex is fixed; introduced in `cmd/spike-cancel/main.go`.
- **Turn done (JSONL side):** at least one `type=="assistant"` line for the turn with `message.stop_reason=="end_turn"`. Note that this is on every delta of the message, not only the last line — see [JSONL layout § One Anthropic message ⇒ N JSONL lines](jsonl-layout.md).
- **Turn-complete predicate (multi-turn):** JSONL `end_turn` observed AND `❯` glyph present in the ANSI-stripped rolling buffer AND `rb.QuietFor() ≥ 1500 ms` (PTY-quiescence; `ptyQuietWindow` in `cmd/spike-multi-turn/main.go`). Composes the JSONL `end_turn` signal with the shared PTY-quiescence readiness predicate (below). The original `gotEndTurn ∧ isIdle for 250 ms` wedged against `claude 2.1.148` because a stuck `✻ Brewed for Ns` glyph stayed painted in the 4 KB rolling buffer after a fast assistant response, keeping `isIdle` false forever — quiescence sidesteps the buffer-residue problem by observing silence directly. See [#73](../codebase/73.md).
- **Post-approve readiness (permission modals):** in spike-permission's `runAutoRespond`, the post-approve return-to-idle is gated by `gotEndTurn ∧ ❯ glyph present in StripANSI(rb.Snapshot()) ∧ rb.QuietFor() ≥ ptyQuietWindow` (`cmd/spike-permission/main.go`). Same shape as the multi-turn turn-complete predicate (above), same empirical 1500 ms window, same buffer-residue failure mode it replaces — the original `gotEndTurn ∧ isIdle for 250 ms` wedged when a `✻` glyph painted during tool execution stayed in the 4 KB rolling buffer past `end_turn`. The approve keystroke and the JSONL-side end-of-turn signal are unchanged; only the PTY-side readiness predicate's shape changed. See [#70](../codebase/70.md).
- **Content extraction:** msg_id grouping — collect every assistant event whose `.message.id` equals the latest `end_turn`-tagged line's msg_id, concatenate `text`-type content blocks in JSONL arrival order. Skip `thinking` and `tool_use` blocks. See [JSONL layout § One Anthropic message ⇒ N JSONL lines](jsonl-layout.md).
- **Cancel keystroke:** ESC (`\x1b`) — single byte, single `pty.Write`, no `\r`. Double-ESC and Ctrl-C also work but ESC is the default (matches the on-screen `esc to interrupt` hint, simplest single-byte representation). The keystroke does NOT go through `typePrompt` — the inter-byte-delay / `\r` reasoning that drives `typePrompt` does not apply to a `\r`-less single byte. See [#11](../codebase/11.md).
- **PTY-quiescence readiness:** the general "claude has finished settling" signal — `❯` glyph present in the ANSI-stripped rolling buffer AND `rb.QuietFor() >= 1500 ms`, where `QuietFor` is "time since the last byte was appended to the rolling buffer." Directly observes silence instead of going through a proxy ("the spinner glyph has rolled out of the buffer"), which is unsound because claude doesn't emit enough bytes after a small redraw to roll the 4 KB rolling buffer past the stale spinner glyph. Introduced in `cmd/spike-cancel/main.go` as post-cancel readiness; now also drives the multi-turn turn-complete predicate (#73) and the spike-permission post-approve readiness predicate (#70), all against the same `✻ Verb for Ns` stuck-glyph-in-rolling-buffer failure mode. Three-consumer pattern; the predicate the library will lift post-extraction (#58–#62). See [#11](../codebase/11.md) for the empirical derivation, [#73](../codebase/73.md) for the multi-turn adoption, and [#70](../codebase/70.md) for the permission-modal adoption.
- **Cancellation acknowledged (JSONL side):** a `user`-role event with content `[{type:"text", text:"[Request interrupted by user]"}]` — NOT a new `stop_reason` value. The cancelled assistant message keeps its pre-cancel `stop_reason`. The current assistant-only tailer filter drops this marker; consumers needing JSONL-side acknowledgment must widen the filter or rely on PTY quiescence. See [JSONL layout § Cancellation signal](jsonl-layout.md).
- **Input-box state across cancels:** post-cancel, claude restores the cancelled prompt as drafted input. The next prompt write must `Ctrl-U` (`0x15`) the input line first; idempotent on an empty input box. See [#11](../codebase/11.md).
- **Permission modal present (PTY side):** literal-text predicate over the stripped buffer — match any of `Esctocancel`, `Doyouwanttoproceed`, `Do you want to proceed`. Two forms of the "proceed" phrase because claude renders permission-modal text differently per tool: Bash uses `\x1b[1C` (CSI cursor-forward) between words, so the stripped buffer has NO interword spaces; Read uses literal spaces. The dual-form match is mandatory. The cheap "any box-drawing char in the buffer" alternative is false-positive at idle (claude's own input box uses `╭ ╰ │`). Strip both CSI (`ansiRe`) AND OSC (`oscRe = \x1b\][^\x07]*\x07`) before matching; OSC payloads otherwise contaminate the buffer. See [#13](../codebase/13.md). Permission modals have **zero JSONL footprint** — detection MUST be PTY-side; see [JSONL layout § Permission modals (zero JSONL footprint)](jsonl-layout.md).
- **Modal text extraction:** last `─{20,}` run BEFORE the proceed-marker is the start anchor; the line containing `Esctocancel` is the end anchor. Per-line, strip leading/trailing box-drawing border chars + whitespace, drop empty lines, join with `\n`. Robust across both Bash and Read modal variants (the alternatives — `modalSepRe.Split`, walk-back-N-newlines from the marker — fail for one or the other). Implemented in `cmd/spike-permission/main.go` as `extractModalText`. See [#13](../codebase/13.md).
- **Approve keystroke (permission modals):** `1\r` (`0x31 0x0d`) — single bulk `pty.Write`, no inter-byte delay. Matches the on-screen `❯1.Yes` numbered default ("Yes, once" — narrowest grant). `y\r`, bare `\r`, and `\x1b[B\r` (down + enter) also work; the down-arrow variant selects option 2 (project- or session-scope grant, tool-dependent) and is ~1 s slower. Library default for "approve once" should be `1\r`; "approve broader scope" is a separate product affordance, not a fallback. Same single-byte single-write semantics as the cancel keystroke — both go through `sendKeystroke` (which is the same body spike #11 introduced as `sendCancel`, renamed because cancel and approve share semantics). See [#13](../codebase/13.md).

## e2e harness

Top-level orchestrator at `cmd/e2e-runner/`, invoked by `make e2e`. Operates orthogonally to the rest of the system — it does not import `pkg/tuidriver/`, just shells out to the prebuilt spike + probe binaries under `./bin/<name>` and emits a single-file `e2e-report.json` artifact. Runs sequentially in one goroutine; `exec.CommandContext` handles subprocess lifecycle (SIGKILL on cancel) so no manual signal plumbing is required.

Headless-CI plumbing rides a single seam in `pkg/tuidriver/pty.go`: when `TUIDRIVER_STRICT_MCP_CONFIG=1` is set in the env, `EnsureClaudeEnv(cmd)` additionally appends `--strict-mcp-config` to `cmd.Args` (idempotent — skipped if already present). Every spike+probe already calls `EnsureClaudeEnv` between `exec.Command("claude", ...)` and `pty.Start`; the runner sets the env var on each child process, so the flag flows transparently with zero spike-side changes. Exported constant `tuidriver.StrictMcpConfigEnv` is the contract for the env-var name.

See [features/e2e-harness.md](../features/e2e-harness.md) for the operator runbook, report schema, and CLI flags. See [#34](../codebase/34.md) for the build notes.

## Related

- [ADR-0001 — Hybrid JSONL + TUI](../decisions/0001-hybrid-jsonl-tui.md)
- [ADR-0002 — Pattern matching over emulation](../decisions/0002-pattern-matching-over-emulation.md)
- [JSONL layout](jsonl-layout.md)
- [e2e harness feature doc](../features/e2e-harness.md)

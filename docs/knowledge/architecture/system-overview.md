# System overview

Where things live and how data flows. Update when modules, types, or data flows change.

## Current state

The library proper (`pkg/tuidriver/`) does **not exist yet**. The Go code in the repo is two throwaway spike binaries that exercise every primitive the eventual library will own without committing to an API:

- `cmd/spike-one-turn/` — single-turn happy path (idle → prompt → spinner → `end_turn` → SUCCESS). See [#1](../codebase/1.md), [#3](../codebase/3.md), [#4](../codebase/4.md), [#7](../codebase/7.md).
- `cmd/spike-multi-turn/` — three-turn loop (simple text → Bash tool use → slow thinking) with msg_id-grouped content extraction and a `runTurn` per-turn driver. See [#9](../codebase/9.md).

Both spikes share ~600 LOC of helpers under `// copied from cmd/spike-one-turn/main.go — keep in sync until library extraction` attribution comments. The duplication is deliberate — both binaries delete when `pkg/tuidriver/` lands.

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

In `cmd/spike-multi-turn/` the events channel is **session-scoped, not turn-scoped** — one tailer goroutine services all three turns, and `runTurn` non-blocking-drains residual events at turn start to discard any delta lines for the prior turn's msg_id that the tailer left buffered after `end_turn` was observed. Without the drain, the next turn's termination predicate could fire spuriously.

Shutdown is a `defer` with a `sync.Once`-guarded body: SIGTERM → 3 s grace race against `cmd.Wait()` → SIGKILL → close PTY → cancel context → `wg.Wait()`. Same sequence in both spike binaries.

## Key signals

- **Idle:** `❯` glyph (UTF-8 `\xe2\x9d\xaf`) present in the ANSI-stripped rolling buffer AND the spinner regex does NOT match.
- **Thinking:** `✻\s+(\S+(?:\s+\S+)?)\s+for\s+(?:(\d+)m\s+)?(\d+)s` — captures a 1–2-word verb (variable per prompt) and a time-tail in `Ns` or `Nm Ns` form. Currently misses every observed verb in practice (CSI cursor-forward between glyph and verb eaten by ANSI strip; ellipsis-form verbs lack the `for Ns` counter); slow-path log lines are dormant pending a regex fix.
- **Turn done (JSONL side):** at least one `type=="assistant"` line for the turn with `message.stop_reason=="end_turn"`. Note that this is on every delta of the message, not only the last line — see [JSONL layout § One Anthropic message ⇒ N JSONL lines](jsonl-layout.md).
- **Turn-complete predicate (multi-turn):** JSONL `end_turn` observed AND `isIdle` true continuously for ≥250 ms (the `idleStableWindow` debounce in `cmd/spike-multi-turn/main.go`).
- **Content extraction:** msg_id grouping — collect every assistant event whose `.message.id` equals the latest `end_turn`-tagged line's msg_id, concatenate `text`-type content blocks in JSONL arrival order. Skip `thinking` and `tool_use` blocks. See [JSONL layout § One Anthropic message ⇒ N JSONL lines](jsonl-layout.md).

## Related

- [ADR-0001 — Hybrid JSONL + TUI](../decisions/0001-hybrid-jsonl-tui.md)
- [ADR-0002 — Pattern matching over emulation](../decisions/0002-pattern-matching-over-emulation.md)
- [JSONL layout](jsonl-layout.md)

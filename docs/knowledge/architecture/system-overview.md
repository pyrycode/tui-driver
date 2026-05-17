# System overview

Where things live and how data flows. Update when modules, types, or data flows change.

## Current state

The library proper (`pkg/tuidriver/`) does **not exist yet**. The only Go code in the repo is the throwaway spike at `cmd/spike-one-turn/`, which exercises every primitive the eventual library will own without committing to an API. See [#1](../codebase/1.md).

## Intended modules (post-spike, not yet built)

- `pkg/tuidriver/` — public API: session lifecycle, state subscription, input writers. Shape will settle after multiple ticket cycles produce enough integration pressure to justify abstractions.

## Layout (today)

```
cmd/spike-one-turn/   # throwaway single-file spike
  main.go             # PTY + reader + state machine + JSONL tailer + watchdog + shutdown
  README.md           # empirical observations log
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

Three goroutines for the spike, coordinated by a single `context.WithCancelCause`:

| Goroutine | Owns | Exit |
|---|---|---|
| `main` orchestrator | linear state machine, watchdog tick | state completes OR watchdog trips OR error |
| PTY reader | the rolling buffer (mutex-protected) | EOF from PTY master (happens when shutdown closes it) |
| JSONL tailer | the events channel (buffered, size 32) | context cancellation |

Shutdown is a `defer` with a `sync.Once`-guarded body: SIGTERM → 3 s grace race against `cmd.Wait()` → SIGKILL → close PTY → cancel context → `wg.Wait()`. See `cmd/spike-one-turn/main.go` for the canonical sequence.

## Key signals

- **Idle:** `❯` glyph (UTF-8 `\xe2\x9d\xaf`) present in the ANSI-stripped rolling buffer AND the spinner regex does NOT match.
- **Thinking:** `✻\s+(\S+(?:\s+\S+)?)\s+for\s+(?:(\d+)m\s+)?(\d+)s` — captures a 1–2-word verb (variable per prompt) and a time-tail in `Ns` or `Nm Ns` form.
- **Turn done (JSONL side):** a `type=="assistant"` line with `message.stop_reason=="end_turn"`. See [JSONL layout](jsonl-layout.md).

## Related

- [ADR-0001 — Hybrid JSONL + TUI](../decisions/0001-hybrid-jsonl-tui.md)
- [ADR-0002 — Pattern matching over emulation](../decisions/0002-pattern-matching-over-emulation.md)
- [JSONL layout](jsonl-layout.md)

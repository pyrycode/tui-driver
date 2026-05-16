# tui-driver — Project Memory for Claude Code

Go library for driving interactive `claude` CLI sessions via PTY. Pre-spike state — architecture decided 2026-05-16, no implementation yet.

## Vault context (read these first when picking up this project)

- `📋 Projects/2026-04-10 - Pyrycode/TUI Driver.md` — canonical architecture, scope, language choice, library set, spike plan
- `📋 Projects/2026-04-10 - Pyrycode/Drop-In Contract.md` — strategic context (subscription-billing preservation via PTY-driven interactive `claude`)
- `📋 Projects/2026-04-10 - Pyrycode/Pyrycode.md` — parent project

## Scope discipline

This library owns: **PTY allocation, byte-stream parsing, state detection, modal handling, keystroke injection, session lifecycle, watchdog.**

It does **NOT** own: JSONL parsing (consumer's job), ACP protocol (consumer's job), agent-stage logic (consumer's job), dispatcher coordination (consumer's job), cost telemetry (consumer extracts from JSONL).

When a feature idea surfaces, first ask: does this belong in tui-driver or in the consumer? If the answer is "the consumer needs to know about claude sessions specifically," it belongs in the consumer. tui-driver should work against any TUI-style CLI in principle.

## Architecture (one paragraph)

PTY allocation → spawn target binary → continuous read into rolling buffer → pattern matchers classify state (idle / thinking / modal / hung / errored) → consumer subscribes to state events and writes input via library APIs. The **thinking indicator** (`✻ Baked for Ns`) is the keystone state signal. State detection uses pattern matching, not full terminal emulation.

## Library choices

- PTY: `github.com/creack/pty`
- Regex: stdlib `regexp`
- Concurrency: goroutines + channels (natural fit for concurrent reader / writer / state observer)

## Spike plan

See `📋 Projects/2026-04-10 - Pyrycode/TUI Driver.md#Spike Plan (~4 hours)`.

## Pipeline

This project uses the pyrycode agentic dispatch pipeline. Agents repo: `pyrycode/tui-driver-agents`. Dispatcher submodule: `pyrycode/agent-dispatcher` (shared with all other pyrycode forks).

PO → Architect → Developer → Code Review → Documentation, gated by labels (`ready:*`, `wip:*`, `error:*`, `needs-rework:*`).

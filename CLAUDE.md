# tui-driver — Project Memory for Claude Code

Go library for driving interactive `claude` CLI sessions via PTY. Library shipped: `pkg/tuidriver/` (~25 files, ~80 tests). Consumer is `pyry agent-run` (consumer migration shipped to pyrycode `main` 2026-05-23).

## Vault context (read these first when picking up this project)

- `📋 Projects/2026-04-10 - Pyrycode/TUI Driver.md` — canonical architecture, scope, language choice, library set
- `📋 Projects/2026-04-10 - Pyrycode/Drop-In Contract.md` — strategic context (subscription-billing preservation via PTY-driven interactive `claude`)
- `📋 Projects/2026-04-10 - Pyrycode/Pyrycode.md` — parent project

## Scope discipline

This library owns: **PTY allocation, byte-stream parsing, state detection, modal handling, keystroke injection, session lifecycle, watchdog.**

It does **NOT** own: JSONL parsing (consumer's job), ACP protocol (consumer's job), agent-stage logic (consumer's job), dispatcher coordination (consumer's job), cost telemetry (consumer extracts from JSONL).

When a feature idea surfaces, first ask: does this belong in tui-driver or in the consumer? If the answer is "the consumer needs to know about claude sessions specifically," it belongs in the consumer. tui-driver should work against any TUI-style CLI in principle. The library shipping in `pkg/tuidriver/` reflects this split; the consumer (`pyry agent-run`) owns JSONL/ACP/agent-stage logic.

## Architecture (one paragraph)

PTY allocation → spawn target binary → continuous read into rolling buffer → pattern matchers classify state (idle / thinking / modal / hung / errored) → consumer subscribes to state events and writes input via library APIs. State detection uses pattern matching, not full terminal emulation.

**Spinner caveat (claude 2.1.158):** the class-A `✻ <verb> for Ns` spinner format matches **0/667 frames** (tui-driver#124), so the spinner-freeze watchdog arm was **retired** in #164 — the PTY-quiet arm and `Events()`'s `EventKindStallDetected` cover the freeze case. `ParseSpinner` is retained for verb telemetry (its seconds-counter now has no in-library consumer), and `SpinnerFreezeLimit` is a retained no-op. `IsThinking` (bare `✻` glyph presence) still works as a "claude has started processing" signal. Prefer the `"esc to interrupt"` hint as the reliable in-flight anchor.

## Use codegraph for symbol lookups

This repo is indexed for codegraph (`.codegraph/`, gitignored). Prefer `mcp__codegraph__codegraph_*` MCP tools over grep for symbol-level questions — where something is defined, what calls it, what breaks if it changes.

- **Before changing or removing an exported function** — run `codegraph_callers` first to find every call site.
- **"Where is X defined" / "what does X call"** — `codegraph_search`, `codegraph_node`, and `codegraph_callees` beat reading files end to end.
- **For a broader "how does this area work"** — `codegraph_context` or `codegraph_impact` before a cross-cutting change.
- Fall back to grep/Read for comments, string literals, and pending edits the index hasn't picked up yet.
- In Claude Code these are deferred tools: load them once with `ToolSearch` (e.g. `select:mcp__codegraph__codegraph_search`) before first use. Codex sees the same `mcp__codegraph__<tool>` names directly.

## Library choices

- PTY: `github.com/creack/pty`
- Regex: stdlib `regexp`
- Concurrency: goroutines + channels (natural fit for concurrent reader / writer / state observer)

## Spikes

The spikes graduated from throwaway prototypes to example consumers of the shipped library. The seven `cmd/spike-*` binaries import `pkg/tuidriver/` and compose its primitives into specific workflows; they double as regression-detection harnesses. Historical spike plan: `📋 Projects/2026-04-10 - Pyrycode/TUI Driver.md#Spike Plan (~4 hours)`.

## Pipeline

This project uses the pyrycode agentic dispatch pipeline. Agents repo: `pyrycode/tui-driver-agents`. Dispatcher submodule: `pyrycode/agent-dispatcher` (shared with all other pyrycode forks).

PO → Architect → Developer → Code Review → Documentation, gated by labels (`ready:*`, `wip:*`, `error:*`, `needs-rework:*`).

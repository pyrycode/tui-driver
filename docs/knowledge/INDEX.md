# Knowledge Index

One-line summary per evergreen doc. Cross-reference target — keep it terse.

## Architecture

- [System overview](architecture/system-overview.md) — modules, data flows, concurrency model (single-turn + multi-turn spike shapes)
- [JSONL layout](architecture/jsonl-layout.md) — where claude writes session logs, deterministic-path discovery via `--session-id`, the turn-terminator shape, and msg_id grouping for multi-block messages

## Decisions (ADRs)

- [0001 — Hybrid JSONL + TUI](decisions/0001-hybrid-jsonl-tui.md) — pair PTY pattern-matching for state with JSONL tailing for content
- [0002 — Pattern matching over terminal emulation](decisions/0002-pattern-matching-over-emulation.md) — regex on a rolling buffer instead of xterm-headless

## Features

(none yet — the code is two throwaway spikes in `cmd/spike-one-turn/` and `cmd/spike-multi-turn/`; see the per-ticket notes under `codebase/` for what each ticket touched)

## Per-ticket notes

`codebase/<N>.md`, one file per ticket. See [codebase/README.md](codebase/README.md) for the convention.

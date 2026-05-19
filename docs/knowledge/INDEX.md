# Knowledge Index

One-line summary per evergreen doc. Cross-reference target — keep it terse.

## Architecture

- [System overview](architecture/system-overview.md) — modules, data flows, concurrency model (single-turn + multi-turn + cancellation + permission-modal spike shapes); key signals including cancel keystroke (ESC), PTY-quiescence post-cancel predicate, input-box state across cancels, modal-detection literal-text predicate, modal-text extraction anchor, and `1\r` approve keystroke
- [JSONL layout](architecture/jsonl-layout.md) — where claude writes session logs, deterministic-path discovery via `--session-id`, the turn-terminator shape, msg_id grouping for multi-block messages, the `user(text "[Request interrupted by user]")` cancellation marker, and the zero-JSONL-footprint of permission-prompt modals

## Decisions (ADRs)

- [0001 — Hybrid JSONL + TUI](decisions/0001-hybrid-jsonl-tui.md) — pair PTY pattern-matching for state with JSONL tailing for content
- [0002 — Pattern matching over terminal emulation](decisions/0002-pattern-matching-over-emulation.md) — regex on a rolling buffer instead of xterm-headless

## Features

- [e2e harness](features/e2e-harness.md) — `make e2e` runs the in-process `claude-version-lock` check (asserts `claude --version` matches `claude-version.lock` and pinned flags still appear in `claude --help`; short-circuits the rest of the run on drift) first, then every spike + probe + `snapshot-drift` serially against real `claude`, emitting `e2e-report.json` (pass/fail/timeout per check, single CI artifact); headless-MCP plumbing rides the `EnsureClaudeEnv` seam via `TUIDRIVER_STRICT_MCP_CONFIG=1`; snapshot-drift byte-compares `pkg/tuidriver/testdata/{picker,mcp,agents}-snapshot.bin` against re-derived captures and is read-only by construction; CI runs the harness on push-to-main + `workflow_dispatch` only (cost-capped, `timeout-minutes: 20`) via `.github/workflows/e2e.yml`, caching the claude install on `claude-version.lock` and uploading `e2e-report.json` + probe recordings on `if: always()`

## Per-ticket notes

`codebase/<N>.md`, one file per ticket. See [codebase/README.md](codebase/README.md) for the convention.

# Knowledge Index

One-line summary per evergreen doc. Cross-reference target — keep it terse.

## Architecture

- [System overview](architecture/system-overview.md) — modules, data flows, concurrency model (single-turn + multi-turn + cancellation + permission-modal spike shapes); key signals including cancel keystroke (ESC), PTY-quiescence readiness predicate (four-consumer pattern across three binaries: post-cancel readiness #11, multi-turn turn-complete #73, spike-permission post-approve readiness #70, spike-cancel post-`end_turn` recovery readiness #69 + IsIdle-wrapper drop in `waitReappeared`), prompt-submit convention (`clearInputLine` + `typePrompt`; four-consumer pattern after #71 — survives claude 2.1.148 paste-detection), input-box state across cancels, modal-detection literal-text predicate, modal-text extraction anchor, and `1\r` approve keystroke
- [JSONL layout](architecture/jsonl-layout.md) — where claude writes session logs, deterministic-path discovery via `--session-id` (canonical library API since #58 — `tuidriver.SessionJSONLPath` for composition + `tuidriver.WaitForSessionJSONL` for the post-prompt appearance poll — the tail loop itself promoted in #59 as `tuidriver.TailJSONL(ctx, path, startOffset) (<-chan JSONLEntry, error)` returning a buffered (cap 32) channel of typed entries (library emits every envelope type, consumers filter), and the per-entry Phase-A discriminator + content walk promoted in #60 as `tuidriver.IsEndTurn(e) bool` (assistant + `stop_reason=="end_turn"` + non-empty concatenated `text` content; rejects `thinking`-only / `tool_use`-only delta lines) and `tuidriver.AssistantText(e) string` (concatenates `content[]` blocks where `Type=="text"` in JSONL arrival order; pure, total over `JSONLEntry` including zero value and `Message==nil`); all in `pkg/tuidriver/jsonl.go`), the turn-terminator shape, msg_id grouping for multi-block messages (still consumer-side), the `user(text "[Request interrupted by user]")` cancellation marker, and the zero-JSONL-footprint of permission-prompt modals

## Decisions (ADRs)

- [0001 — Hybrid JSONL + TUI](decisions/0001-hybrid-jsonl-tui.md) — pair PTY pattern-matching for state with JSONL tailing for content
- [0002 — Pattern matching over terminal emulation](decisions/0002-pattern-matching-over-emulation.md) — regex on a rolling buffer instead of xterm-headless

## Features

- [e2e harness](features/e2e-harness.md) — `make e2e` runs the in-process `claude-version-lock` check (asserts every `flag=` and `value=` pinned in `claude-version.lock` still appears in `claude --help`; the lock's `version=` is informational only since #64 — claude patch drift no longer gates the harness; short-circuits the rest of the run on flag/value drift) first, then every spike + probe + `snapshot-drift` serially against real `claude`, emitting `e2e-report.json` (pass/fail/timeout per check, single CI artifact); headless-MCP plumbing rides the `EnsureClaudeEnv` seam via `TUIDRIVER_STRICT_MCP_CONFIG=1`; `make e2e MODEL=haiku EFFORT=low` rides the same seam via `TUIDRIVER_CLAUDE_MODEL` / `TUIDRIVER_CLAUDE_EFFORT` to pin a cheap claude pair for CI (18–90× cheaper than the default Opus + high; unset = inherit operator config / Max-subscription path); snapshot-drift byte-compares `pkg/tuidriver/testdata/{picker,mcp,agents}-snapshot.bin` against re-derived captures and is read-only by construction; CI runs the harness on push-to-main + `workflow_dispatch` only (cost-capped, `timeout-minutes: 20`) via `.github/workflows/e2e.yml`, caching the claude install on `claude-version.lock` and uploading `e2e-report.json` + probe recordings on `if: always()`

## Per-ticket notes

`codebase/<N>.md`, one file per ticket. See [codebase/README.md](codebase/README.md) for the convention.

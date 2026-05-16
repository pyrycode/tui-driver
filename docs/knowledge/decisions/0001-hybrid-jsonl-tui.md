# ADR-0001 — Hybrid JSONL + TUI

**Date:** 2026-05-16 · **Status:** accepted · **Empirically validated by:** [#1](https://github.com/pyrycode/tui-driver/issues/1)

## Context

We need to programmatically drive `claude` in a way that (a) keeps the spawned process on Anthropic's subscription-eligible surface (interactive TTY, no `-p` flag — see repo `README.md` § *Why this exists*) and (b) gives a consumer like `pyry acp` both **live state** (is claude thinking? showing a modal? idle?) and **structured turn content** (assistant text, tool calls).

Neither side alone is sufficient:

- The session JSONL log (`~/.claude/projects/<encoded-cwd>/<session-id>.jsonl`) cleanly records what was said but does NOT signal multiselects, the thinking spinner, or the idle prompt — exactly the states a driver needs to know about.
- The TUI byte stream over the PTY shows all of those states clearly but is a churning ANSI redraw; extracting the assistant's actual text from it would require terminal emulation and brittle scraping.

## Decision

Pair both channels. The library reads PTY bytes for **state** (pattern matching over a rolling buffer); the consumer reads the session JSONL for **content**.

The two-channel split is reflected in `CLAUDE.md` § *Scope discipline*: tui-driver owns PTY/state; JSONL parsing is the consumer's job. This is a hard boundary, not a soft one.

## Rationale

- JSONL is missing exactly the signals the TUI shows clearly; the TUI is missing exactly the structured content JSONL records cleanly. Symmetric gaps → symmetric pairing.
- Pattern matching on the PTY (vs. full emulation) is cheap and good-enough for state detection — see [ADR-0002](0002-pattern-matching-over-emulation.md).
- Keeps the consumer in charge of content schema, which will drift across `claude` versions. tui-driver doesn't have to follow that drift.

## Alternatives considered

- **JSONL only.** Rejected: no thinking/modal/idle signals.
- **PTY only with full terminal emulation.** Rejected: brittle text extraction, larger dependency surface (xterm-headless etc.), and we'd still need a JSONL-equivalent to surface structured content reliably across `claude` versions.
- **`claude -p` (headless) mode.** Rejected on strategic grounds — falls under Anthropic's 2026-06-15 Agent SDK billing split, moving to a metered credit pool. The whole reason this library exists is to stay on subscription billing.

## Consequences

- The keystone state signals (`✻ <verb> for Ns` thinking, `❯` idle) become the empirical surface area we have to track if `claude`'s UI ever shifts.
- The JSONL turn-terminator (`type=="assistant"` AND `message.stop_reason=="end_turn"`) is what consumers wait for to know a turn is done — see [JSONL layout](../architecture/jsonl-layout.md).
- We don't ship a JSONL parser; consumers do. Multiple consumers may end up duplicating that code, which is fine — keeps schema drift out of this library.

## Links

- Vault: `📋 Projects/2026-04-10 - Pyrycode/TUI Driver.md` § *Hybrid JSONL + TUI*
- Repo: `README.md` (one-paragraph architecture)
- Validated: `cmd/spike-one-turn/main.go`

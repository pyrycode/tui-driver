# ADR-0002 — Pattern matching over terminal emulation

**Date:** 2026-05-16 · **Status:** accepted · **Empirically validated by:** [#1](https://github.com/pyrycode/tui-driver/issues/1)

## Context

The library has to classify the spawned `claude` process's state (idle / thinking / modal / hung / errored) from the byte stream coming back over the PTY master. Two broad approaches:

1. Feed the bytes into a headless terminal emulator (xterm-headless, tcell, vt10x) and inspect the resulting screen buffer.
2. Strip ANSI escapes from a rolling tail of the bytes and run a handful of regexes against the result.

## Decision

Use approach (2): regex matching on a 4 KB rolling buffer, with a single-pass ANSI strip (`\x1b\[[0-9;?]*[a-zA-Z]`) for the matcher's view. Mirror the raw bytes to stderr for human observability.

This is a **deliberate initial choice**, not a permanent one. If a future state signal genuinely requires cursor-position tracking, the deferral reopens.

## Rationale

- The keystone state signals — `✻ <verb> for Ns` thinking spinner, `❯` idle prompt, box-drawing chars for modals — are distinctive Unicode glyphs that survive a one-line ANSI strip. No screen geometry needed to recognize them.
- The state signal of interest is always in the last few hundred bytes of output. A 4 KB rolling window is plenty; older context is irrelevant.
- Avoids a heavy dependency (terminal emulators are non-trivial) and the latency of running the full emulation pipeline per byte.
- Pattern-matching code is auditable in a way emulator-state introspection isn't — one regex per state, easy to extend.

## Alternatives considered

- **xterm-headless / vt10x / tcell.** Rejected for now: too much machinery for the state signals we actually need to detect. Reopen if a signal turns out to need cursor position or scrollback fidelity.
- **Parse only the most recent line.** Considered, but the PTY output is a churning redraw — relevant glyphs land at unpredictable positions and may span partial reads. Buffer + tail-window regex handles partial reads naturally.

## Consequences

- We accept that some state edge cases may need extra heuristics (e.g. "idle = `❯ present AND spinner regex does NOT match`" — lone `❯` isn't reliable because the input line redraws below the spinner during thinking — see [#1 patterns](../codebase/1.md)).
- The verb in the thinking indicator is variable per prompt ("Baked", "Whipped up", "Cooking", …). Match-and-capture, don't whitelist — see [JSONL layout](../architecture/jsonl-layout.md) for the empirical observation surface.
- 4 KB rolling cap is a knob we may have to tune if `claude` ever changes its redraw cadence. Re-examine if state predicates start missing signals.

## Links

- Vault: `📋 Projects/2026-04-10 - Pyrycode/TUI Driver.md` § *Pattern Matching, Not Full Emulation (Initially)*
- Code: `cmd/spike-one-turn/main.go` — `rollingBuffer`, `spinnerRe`, `ansiRe`, `isIdle`
- Related: [ADR-0001](0001-hybrid-jsonl-tui.md)

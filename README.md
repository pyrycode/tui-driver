# tui-driver

Go library for driving interactive `claude` CLI sessions via PTY.

## Scope

**Owns:** PTY allocation, byte-stream parsing, state detection, modal handling, keystroke injection, session lifecycle, watchdog.

**Does not own:** JSONL parsing, ACP protocol, agent-stage logic, dispatcher coordination, cost telemetry.

Consumer pairs `tui-driver` output (live state) with the target binary's session JSONL log (structured content). This separation is intentional — JSONL is missing exactly the state signals (multiselects, thinking spinner, idle prompt) that the TUI shows clearly, and the TUI is missing exactly the structured content that the JSONL records cleanly.

## Status

Pre-spike. Architecture decided 2026-05-16; no code yet. See [vault: TUI Driver](../../obsidian-vault/Second%20Brain/📋%20Projects/2026-04-10%20-%20Pyrycode/TUI%20Driver.md) for the full design doc.

## Architecture (one-paragraph)

Allocates a pseudo-terminal pair; spawns target binary (e.g. `claude`) with the slave as its controlling terminal. Reads PTY output continuously into a rolling buffer; runs pattern matchers (regex over recent bytes) to classify state — `idle` / `thinking` / `modal-up` / `hung` / `errored`. Consumer subscribes to state events and writes user input via the driver's `WriteUserPrompt` / `WriteRawBytes` / `RespondToModal` APIs.

The **thinking indicator** (`✻ Baked for Ns`) is the keystone state signal — a distinctive Unicode glyph + incrementing counter that's trivially regex-detectable. Combined with `❯` (idle marker) and box-drawing characters (modal marker), the basic state machine emerges from a handful of pattern matchers, not a full virtual terminal emulator.

## Why this exists

Anthropic's 2026-06-15 Agent SDK billing split moves `claude -p`, the Agent SDK, and "third-party apps that authenticate with your Claude subscription through the Agent SDK" to a metered credit pool. *Interactive* Claude Code in the terminal stays on subscription. Driving `claude` via a real PTY puts the spawned process on the explicitly-subscription-eligible surface — TTY-attached stdin/stdout, no `-p` flag, normal interactive UI. This library is the substrate that makes that approach viable for programmatic consumers (`pyry acp`, etc.).

See [Drop-In Contract](../../obsidian-vault/Second%20Brain/📋%20Projects/2026-04-10%20-%20Pyrycode/Drop-In%20Contract.md) for the strategic context.

## Consumers

- `pyrycode/pyrycode` — `pyry acp` mode (planned)

## License

Private repository. No license declared.

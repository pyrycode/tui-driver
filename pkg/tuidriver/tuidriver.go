// Package tuidriver provides foundation primitives for driving interactive
// `claude` CLI sessions via PTY. Consumed by pyry acp (and reusable by other
// TUI-style CLI drivers).
//
// Scope: PTY allocation, rolling byte buffer with quiet-time tracking,
// projects-dir name encoding, ANSI/OSC stripping, vt10x-backed grid
// rendering, modal-class detection, modal parsers (picker / mcp / agents),
// trust-folder + MCP-failure banner detectors. Out of scope (consumer's
// job): JSONL parsing, ACP protocol, agent-stage logic, dispatcher
// coordination, cost telemetry.
//
// Extracted from the 6 spike binaries under cmd/ after loops 1-6 validated
// the architecture across 19 experiments. See the project's Findings.md in
// the vault for empirical context.
//
// # MCP server reliability
//
// claude renders a "N MCP server failed · /mcp" status banner when one or
// more configured MCP servers fail to connect at startup. Two strategies:
//
//   - Sidestep (recommended for non-interactive drivers): pass
//     --strict-mcp-config to claude. Configured MCP servers are skipped
//     entirely; banner never appears.
//   - Surface: allow MCP servers, and call HasMcpFailureBanner /
//     FailedMcpCount to detect failures and forward to the host UI.
//
// The banner is a terminal state — "I failed to connect," not "I'm still
// trying." There is no benefit to polling for it to clear, so the library
// does not expose a WaitForMcpReady primitive.
package tuidriver

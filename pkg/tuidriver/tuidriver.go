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
// more configured MCP servers fail to connect at startup. The user's MCP
// servers are part of what the driver is forwarding (e.g. pyry acp
// surfacing Gmail / GitHub / Figma tools to mobile), so the library does
// not strip them — consumers handle failures by detecting them and
// surfacing to the host UI.
//
// Primitives:
//
//   - HasMcpFailureBanner(snap) — boolean check for the banner
//   - FailedMcpCount(snap) — integer count, 0 if no banner
//   - ParseMcpStatus(snap) — full structured /mcp modal contents
//
// The banner is a terminal state — "I failed to connect," not "I'm still
// trying." There is no benefit to polling for it to clear, so the library
// does not expose a WaitForMcpReady primitive. Consumers either retry
// (out of library scope — relaunch claude, possibly with an adjusted
// --mcp-config) or proceed with partial MCP and surface the failure.
//
// --strict-mcp-config is a claude CLI flag that skips configured MCP
// servers entirely. It exists for reproducibility scenarios (CI, tests,
// containers) but is NOT the recommended path for an interactive driver:
// it discards the user's MCP servers, which are usually part of the value
// being driven.
package tuidriver

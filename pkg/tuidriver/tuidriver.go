// Package tuidriver provides foundation primitives for driving interactive
// `claude` CLI sessions via PTY. Consumed by pyry acp (and reusable by other
// TUI-style CLI drivers).
//
// Scope: PTY allocation, rolling byte buffer with quiet-time tracking,
// projects-dir name encoding, session JSONL path resolution, appearance
// polling, tail-with-parsed-entries, and per-entry assistant-text /
// end-of-turn helpers, ANSI/OSC stripping, vt10x-backed grid rendering,
// modal-class detection, modal parsers (picker / mcp / agents),
// trust-folder + MCP-failure banner detectors, unified PTY+JSONL event
// subscription. Out of scope (consumer's
// job): JSONL semantic interpretation across lines (msg_id grouping,
// cross-line content aggregation, cancellation-marker detection), ACP
// protocol, agent-stage logic, dispatcher coordination, cost telemetry.
//
// Extracted from the 6 spike binaries under cmd/ after loops 1-6 validated
// the architecture across 19 experiments. See the project's Findings.md in
// the vault for empirical context.
//
// # Attaching files (and images) to prompts
//
// claude's `@<path>` prompt syntax works for both text files AND images.
// The path is read from disk at prompt-submit time; the file's bytes get
// base64-encoded into the JSONL `attachment` event. Mobile / remote
// consumers can therefore attach images by:
//
//  1. Writing the image bytes to a temp file accessible from claude's cwd.
//  2. Injecting `@<absolute-path>` (or a relative path) into the prompt.
//  3. Sending the prompt + CR.
//
// JSONL attachment shape (out of library scope, documented for consumers):
//
//	{"type":"attachment","attachment":{"type":"file",
//	  "filename":"/tmp/x.png",
//	  "content":{"type":"image","file":{
//	    "base64":"...","type":"image/png","originalSize":515,
//	    "dimensions":{"originalWidth":100,"originalHeight":100,...}}},
//	  "displayPath":"../../tmp/x.png"}}
//
// Confirmed empirically 2026-05-18 with a 100×100 PNG.
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

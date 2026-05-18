// Package tuidriver provides foundation primitives for driving interactive
// `claude` CLI sessions via PTY. Consumed by pyry acp (and reusable by other
// TUI-style CLI drivers).
//
// Scope: PTY allocation, rolling byte buffer with quiet-time tracking,
// projects-dir name encoding, modal-class detection. Out of scope (consumer's
// job): JSONL parsing, ACP protocol, agent-stage logic, dispatcher
// coordination, cost telemetry.
//
// Extracted from the 6 spike binaries under cmd/ after loops 1-6 validated
// the architecture across 19 experiments. See the project's Findings.md in
// the vault for empirical context.
package tuidriver

package tuidriver

import (
	"os"
	"os/exec"
	"strings"

	"github.com/creack/pty"
)

// Default PTY dimensions used by the spike binaries. claude renders
// reasonably at this size; smaller is clipped, larger is unnecessary for
// state-detection.
const (
	DefaultPtyRows uint16 = 40
	DefaultPtyCols uint16 = 120
)

// ClaudeTermEnv is the TERM value claude expects for proper TUI rendering.
// Without it claude falls back to a plain-text mode that breaks state
// detection (no spinner glyphs, no box-drawing, no color codes).
const ClaudeTermEnv = "TERM=xterm-256color"

// StrictMcpConfigEnv is the env var the e2e harness sets to ask
// EnsureClaudeEnv to additionally append --strict-mcp-config to cmd.Args.
// Set to "1" to enable. Empty / unset / any other value: no-op. Lets the
// harness signal "headless / reproducible MCP" through the seam every
// spike+probe already uses, without touching individual spike binaries.
const StrictMcpConfigEnv = "TUIDRIVER_STRICT_MCP_CONFIG"

// strictMcpConfigFlag is the claude CLI flag that skips configured MCP
// servers entirely (see package doc). EnsureClaudeEnv appends it to
// cmd.Args when StrictMcpConfigEnv is "1" and the flag is not already
// present.
const strictMcpConfigFlag = "--strict-mcp-config"

// EnsureClaudeEnv prepares cmd for driving claude. It ensures cmd.Env
// contains TERM=xterm-256color (seeding from os.Environ when nil; replacing
// any pre-existing TERM=...; idempotent on repeat calls). When the
// StrictMcpConfigEnv env var is set to "1", it additionally appends
// --strict-mcp-config to cmd.Args unless that flag is already present;
// other values (including empty/unset) are a no-op for the args side.
// Returns cmd for chaining.
func EnsureClaudeEnv(cmd *exec.Cmd) *exec.Cmd {
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	termSet := false
	for i, kv := range cmd.Env {
		if strings.HasPrefix(kv, "TERM=") {
			cmd.Env[i] = ClaudeTermEnv
			termSet = true
			break
		}
	}
	if !termSet {
		cmd.Env = append(cmd.Env, ClaudeTermEnv)
	}

	if os.Getenv(StrictMcpConfigEnv) == "1" {
		alreadyPresent := false
		for _, a := range cmd.Args {
			if a == strictMcpConfigFlag {
				alreadyPresent = true
				break
			}
		}
		if !alreadyPresent {
			cmd.Args = append(cmd.Args, strictMcpConfigFlag)
		}
	}
	return cmd
}

// StartPTY spawns cmd inside a PTY and attempts to set the window size to
// DefaultPtyRows × DefaultPtyCols. The Setsize step is best-effort —
// non-fatal failure is silently ignored; the returned PTY file is usable
// either way. For custom dimensions, call pty.Setsize directly on the
// returned file after Start.
//
// Callers are responsible for cmd.Env (use EnsureClaudeEnv if driving
// claude), for cmd.Wait, and for closing the returned *os.File when done.
func StartPTY(cmd *exec.Cmd) (*os.File, error) {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: DefaultPtyRows, Cols: DefaultPtyCols})
	return ptmx, nil
}

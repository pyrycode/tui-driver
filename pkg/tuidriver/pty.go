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

// EnsureClaudeEnv ensures cmd.Env contains TERM=xterm-256color. If cmd.Env
// is nil it is seeded from os.Environ first. If TERM is already set to
// xterm-256color the call is a no-op; if set to something else, it is
// replaced. Returns cmd for chaining.
func EnsureClaudeEnv(cmd *exec.Cmd) *exec.Cmd {
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	for i, kv := range cmd.Env {
		if strings.HasPrefix(kv, "TERM=") {
			cmd.Env[i] = ClaudeTermEnv
			return cmd
		}
	}
	cmd.Env = append(cmd.Env, ClaudeTermEnv)
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

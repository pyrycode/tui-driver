package tuidriver

import (
	"fmt"
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

// ClaudeModelEnv is the env var the e2e harness sets to ask
// EnsureClaudeEnv to additionally append --model <value> to cmd.Args.
// Non-empty: appended verbatim. Empty / unset: no-op (preserves the
// operator's interactive Claude default — required for Max-subscription
// local development). Idempotent: if --model is already in cmd.Args,
// no second occurrence is added.
const ClaudeModelEnv = "TUIDRIVER_CLAUDE_MODEL"

// ClaudeEffortEnv — same contract as ClaudeModelEnv for --effort <level>.
// Accepts whatever string claude accepts (today: low/medium/high/xhigh/max
// — see `claude --help`). Validation is delegated to claude itself.
const ClaudeEffortEnv = "TUIDRIVER_CLAUDE_EFFORT"

const claudeModelFlag = "--model"
const claudeEffortFlag = "--effort"

// claudeNestingEnvVars are the environment markers Claude Code sets on a
// child process it spawns. When claude sees them it treats itself as a
// nested child session and skips writing its standalone session transcript,
// the JSONL the runner waits on. Since 2.1.199 that skip makes any spike
// launched from inside a Claude Code session hang at the first-prompt
// deadline. EnsureClaudeEnv removes them so the driven claude always starts
// as a clean top-level session. CLAUDE_CODE_OAUTH_TOKEN is deliberately not
// listed: it carries auth and must survive.
var claudeNestingEnvVars = []string{
	"CLAUDECODE",
	"CLAUDE_CODE_CHILD_SESSION",
	"CLAUDE_CODE_SESSION_ID",
	"CLAUDE_CODE_ENTRYPOINT",
}

func isClaudeNestingVar(key string) bool {
	for _, v := range claudeNestingEnvVars {
		if key == v {
			return true
		}
	}
	return false
}

// EnsureClaudeEnv prepares cmd for driving claude. It ensures cmd.Env
// contains TERM=xterm-256color (seeding from os.Environ when nil; replacing
// any pre-existing TERM=...; idempotent on repeat calls). It strips the
// Claude Code nesting markers (see claudeNestingEnvVars) so the child
// persists its session transcript even when the parent is itself a Claude
// Code session. When the StrictMcpConfigEnv env var is set to "1", it
// additionally appends --strict-mcp-config to cmd.Args unless that flag is
// already present; other values (including empty/unset) are a no-op for the
// args side. When ClaudeModelEnv / ClaudeEffortEnv are set to a non-empty
// value, it appends --model <value> / --effort <value> to cmd.Args unless
// the respective flag is already present. Returns cmd for chaining.
func EnsureClaudeEnv(cmd *exec.Cmd) *exec.Cmd {
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	// Strip Claude Code nesting markers so the driven claude launches as a
	// clean top-level session and persists its transcript. Without this a
	// spike run from inside a Claude Code session inherits them, acts as a
	// nested child, skips the transcript, and hangs the runner at the
	// first-prompt deadline on 2.1.199+.
	scrubbed := make([]string, 0, len(cmd.Env))
	for _, kv := range cmd.Env {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if !isClaudeNestingVar(key) {
			scrubbed = append(scrubbed, kv)
		}
	}
	cmd.Env = scrubbed

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

	if v := os.Getenv(ClaudeModelEnv); v != "" {
		alreadyPresent := false
		for _, a := range cmd.Args {
			if a == claudeModelFlag {
				alreadyPresent = true
				break
			}
		}
		if !alreadyPresent {
			cmd.Args = append(cmd.Args, claudeModelFlag, v)
		}
	}

	if v := os.Getenv(ClaudeEffortEnv); v != "" {
		alreadyPresent := false
		for _, a := range cmd.Args {
			if a == claudeEffortFlag {
				alreadyPresent = true
				break
			}
		}
		if !alreadyPresent {
			cmd.Args = append(cmd.Args, claudeEffortFlag, v)
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
//
// On Linux the child is given a hard parent-death SIGKILL (Pdeathsig) so it
// cannot outlive a crashed host that never runs Session.Close; other platforms
// have no kernel parent-death signal and rely on Close for shutdown (see
// setParentDeathSignal).
func StartPTY(cmd *exec.Cmd) (*os.File, error) {
	setParentDeathSignal(cmd) // linux: SIGKILL child on parent death; no-op elsewhere
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: DefaultPtyRows, Cols: DefaultPtyCols})
	return ptmx, nil
}

// Resize sets the hosted PTY's window size to rows×cols (TIOCSWINSZ on the held
// master FD), which the kernel turns into SIGWINCH on the hosted process so its
// TUI redraws. It is the sanctioned replacement for the sealed Session.PTY
// accessor, letting a consumer that holds only a *Session size a live session's
// PTY instead of the fixed DefaultPtyRows×DefaultPtyCols.
//
// rows/cols are passed through verbatim — not clamped, defaulted, or
// interpreted. Resize is callable the instant Spawn returns, so a size known up
// front can be set before the child's first render; a session that never calls
// Resize keeps StartPTY's 40×120 default unchanged.
//
// Returns a non-nil error if the size cannot be applied (e.g. after Close, where
// the closed master FD yields EBADF); never panics.
func (s *Session) Resize(rows, cols uint16) error {
	if err := pty.Setsize(s.pty, &pty.Winsize{Rows: rows, Cols: cols}); err != nil {
		return fmt.Errorf("tuidriver: resize to %dx%d: %w", rows, cols, err)
	}
	// Record the new size so detection renders at it (#226). Only on success:
	// a failed Setsize left the PTY at its previous size, so the tracked dims
	// must stay there too.
	s.sizeMu.Lock()
	s.curCols, s.curRows = int(cols), int(rows)
	s.sizeMu.Unlock()
	return nil
}

// gridDims returns the session's current terminal size as (cols, rows) — the
// dimensions detection should render at, matching the NewGrid / classify
// argument order. It is the seam the per-tick classifier (mergeEvents) reads so
// a resized interactive session's bytes are rendered at the size claude drew
// them for, instead of the fixed 120x40 default. A session that never calls
// Resize returns the StartPTY default seeded in Spawn.
func (s *Session) gridDims() (cols, rows int) {
	s.sizeMu.Lock()
	defer s.sizeMu.Unlock()
	return s.curCols, s.curRows
}

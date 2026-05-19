package tuidriver

import (
	"os/exec"
	"strings"
	"testing"
)

func TestEnsureClaudeEnvAddsWhenMissing(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{"FOO=bar", "BAZ=qux"}}
	EnsureClaudeEnv(cmd)
	if !contains(cmd.Env, ClaudeTermEnv) {
		t.Errorf("Env = %v, missing %q", cmd.Env, ClaudeTermEnv)
	}
}

func TestEnsureClaudeEnvReplacesExistingTerm(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{"TERM=dumb", "FOO=bar"}}
	EnsureClaudeEnv(cmd)
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "TERM=") && kv != ClaudeTermEnv {
			t.Errorf("TERM not replaced: %q in %v", kv, cmd.Env)
		}
	}
	if !contains(cmd.Env, ClaudeTermEnv) {
		t.Errorf("Env = %v, missing %q", cmd.Env, ClaudeTermEnv)
	}
	// No duplicate TERM entries.
	termCount := 0
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "TERM=") {
			termCount++
		}
	}
	if termCount != 1 {
		t.Errorf("got %d TERM entries, want 1: %v", termCount, cmd.Env)
	}
}

func TestEnsureClaudeEnvNilEnvSeedsFromOsEnviron(t *testing.T) {
	cmd := &exec.Cmd{Env: nil}
	EnsureClaudeEnv(cmd)
	if cmd.Env == nil {
		t.Fatal("cmd.Env still nil after EnsureClaudeEnv")
	}
	if !contains(cmd.Env, ClaudeTermEnv) {
		t.Errorf("Env = %v, missing %q", cmd.Env, ClaudeTermEnv)
	}
}

func TestEnsureClaudeEnvIdempotent(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{ClaudeTermEnv, "FOO=bar"}}
	EnsureClaudeEnv(cmd)
	EnsureClaudeEnv(cmd) // second call shouldn't add a duplicate.
	termCount := 0
	for _, kv := range cmd.Env {
		if kv == ClaudeTermEnv {
			termCount++
		}
	}
	if termCount != 1 {
		t.Errorf("got %d %q entries, want 1: %v", termCount, ClaudeTermEnv, cmd.Env)
	}
}

func TestEnsureClaudeEnvStrictMcpUnsetLeavesArgs(t *testing.T) {
	t.Setenv(StrictMcpConfigEnv, "")
	cmd := &exec.Cmd{Path: "claude", Args: []string{"claude", "--session-id", "x"}}
	EnsureClaudeEnv(cmd)
	for _, a := range cmd.Args {
		if a == "--strict-mcp-config" {
			t.Errorf("Args = %v, contains --strict-mcp-config but env unset", cmd.Args)
		}
	}
}

func TestEnsureClaudeEnvStrictMcpEnabledAppends(t *testing.T) {
	t.Setenv(StrictMcpConfigEnv, "1")
	cmd := &exec.Cmd{Path: "claude", Args: []string{"claude", "--session-id", "x"}}
	EnsureClaudeEnv(cmd)
	count := 0
	for _, a := range cmd.Args {
		if a == "--strict-mcp-config" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("got %d --strict-mcp-config entries, want 1: %v", count, cmd.Args)
	}
}

func TestEnsureClaudeEnvStrictMcpEnabledNoDoubleAppend(t *testing.T) {
	t.Setenv(StrictMcpConfigEnv, "1")
	cmd := &exec.Cmd{Path: "claude", Args: []string{"claude", "--strict-mcp-config", "--session-id", "x"}}
	EnsureClaudeEnv(cmd)
	count := 0
	for _, a := range cmd.Args {
		if a == "--strict-mcp-config" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("got %d --strict-mcp-config entries, want 1 (no double-append): %v", count, cmd.Args)
	}
}

func TestEnsureClaudeEnvStrictMcpOtherValueIgnored(t *testing.T) {
	t.Setenv(StrictMcpConfigEnv, "true")
	cmd := &exec.Cmd{Path: "claude", Args: []string{"claude", "--session-id", "x"}}
	EnsureClaudeEnv(cmd)
	for _, a := range cmd.Args {
		if a == "--strict-mcp-config" {
			t.Errorf("Args = %v, contains --strict-mcp-config but env was %q (want exact \"1\")", cmd.Args, "true")
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

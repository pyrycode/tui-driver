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

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

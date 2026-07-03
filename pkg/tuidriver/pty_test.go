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

func TestEnsureClaudeEnvModelUnsetLeavesArgs(t *testing.T) {
	t.Setenv(ClaudeModelEnv, "")
	cmd := &exec.Cmd{Path: "claude", Args: []string{"claude", "--session-id", "x"}}
	EnsureClaudeEnv(cmd)
	for _, a := range cmd.Args {
		if a == "--model" {
			t.Errorf("Args = %v, contains --model but env unset", cmd.Args)
		}
	}
}

func TestEnsureClaudeEnvModelSetAppends(t *testing.T) {
	t.Setenv(ClaudeModelEnv, "haiku")
	cmd := &exec.Cmd{Path: "claude", Args: []string{"claude", "--session-id", "x"}}
	EnsureClaudeEnv(cmd)
	flagCount, valueCount := 0, 0
	for _, a := range cmd.Args {
		if a == "--model" {
			flagCount++
		}
		if a == "haiku" {
			valueCount++
		}
	}
	if flagCount != 1 || valueCount != 1 {
		t.Errorf("got %d --model and %d haiku entries, want 1 each: %v", flagCount, valueCount, cmd.Args)
	}
}

func TestEnsureClaudeEnvModelAlreadyPresentNoDoubleAppend(t *testing.T) {
	t.Setenv(ClaudeModelEnv, "haiku")
	cmd := &exec.Cmd{Path: "claude", Args: []string{"claude", "--model", "sonnet", "--session-id", "x"}}
	EnsureClaudeEnv(cmd)
	flagCount, haikuCount, sonnetCount := 0, 0, 0
	for _, a := range cmd.Args {
		switch a {
		case "--model":
			flagCount++
		case "haiku":
			haikuCount++
		case "sonnet":
			sonnetCount++
		}
	}
	if flagCount != 1 {
		t.Errorf("got %d --model entries, want 1 (caller wins): %v", flagCount, cmd.Args)
	}
	if sonnetCount != 1 || haikuCount != 0 {
		t.Errorf("got sonnet=%d haiku=%d, want sonnet=1 haiku=0 (caller's value wins): %v", sonnetCount, haikuCount, cmd.Args)
	}
}

func TestEnsureClaudeEnvStrictMcpAndModelBothAppend(t *testing.T) {
	t.Setenv(StrictMcpConfigEnv, "1")
	t.Setenv(ClaudeModelEnv, "haiku")
	cmd := &exec.Cmd{Path: "claude", Args: []string{"claude", "--session-id", "x"}}
	EnsureClaudeEnv(cmd)
	if !contains(cmd.Args, "--strict-mcp-config") {
		t.Errorf("Args = %v, missing --strict-mcp-config", cmd.Args)
	}
	if !contains(cmd.Args, "--model") || !contains(cmd.Args, "haiku") {
		t.Errorf("Args = %v, missing --model haiku", cmd.Args)
	}
}

func TestEnsureClaudeEnvEffortUnsetLeavesArgs(t *testing.T) {
	t.Setenv(ClaudeEffortEnv, "")
	cmd := &exec.Cmd{Path: "claude", Args: []string{"claude", "--session-id", "x"}}
	EnsureClaudeEnv(cmd)
	for _, a := range cmd.Args {
		if a == "--effort" {
			t.Errorf("Args = %v, contains --effort but env unset", cmd.Args)
		}
	}
}

func TestEnsureClaudeEnvEffortSetAppends(t *testing.T) {
	t.Setenv(ClaudeEffortEnv, "low")
	cmd := &exec.Cmd{Path: "claude", Args: []string{"claude", "--session-id", "x"}}
	EnsureClaudeEnv(cmd)
	flagCount, valueCount := 0, 0
	for _, a := range cmd.Args {
		if a == "--effort" {
			flagCount++
		}
		if a == "low" {
			valueCount++
		}
	}
	if flagCount != 1 || valueCount != 1 {
		t.Errorf("got %d --effort and %d low entries, want 1 each: %v", flagCount, valueCount, cmd.Args)
	}
}

func TestEnsureClaudeEnvEffortAlreadyPresentNoDoubleAppend(t *testing.T) {
	t.Setenv(ClaudeEffortEnv, "low")
	cmd := &exec.Cmd{Path: "claude", Args: []string{"claude", "--effort", "high", "--session-id", "x"}}
	EnsureClaudeEnv(cmd)
	flagCount, lowCount, highCount := 0, 0, 0
	for _, a := range cmd.Args {
		switch a {
		case "--effort":
			flagCount++
		case "low":
			lowCount++
		case "high":
			highCount++
		}
	}
	if flagCount != 1 {
		t.Errorf("got %d --effort entries, want 1 (caller wins): %v", flagCount, cmd.Args)
	}
	if highCount != 1 || lowCount != 0 {
		t.Errorf("got high=%d low=%d, want high=1 low=0 (caller's value wins): %v", highCount, lowCount, cmd.Args)
	}
}

func TestEnsureClaudeEnvStrictMcpAndEffortBothAppend(t *testing.T) {
	t.Setenv(StrictMcpConfigEnv, "1")
	t.Setenv(ClaudeEffortEnv, "low")
	cmd := &exec.Cmd{Path: "claude", Args: []string{"claude", "--session-id", "x"}}
	EnsureClaudeEnv(cmd)
	if !contains(cmd.Args, "--strict-mcp-config") {
		t.Errorf("Args = %v, missing --strict-mcp-config", cmd.Args)
	}
	if !contains(cmd.Args, "--effort") || !contains(cmd.Args, "low") {
		t.Errorf("Args = %v, missing --effort low", cmd.Args)
	}
}

func TestEnsureClaudeEnvStrictMcpModelEffortAllAppendNoDuplicates(t *testing.T) {
	t.Setenv(StrictMcpConfigEnv, "1")
	t.Setenv(ClaudeModelEnv, "haiku")
	t.Setenv(ClaudeEffortEnv, "low")
	cmd := &exec.Cmd{Path: "claude", Args: []string{"claude", "--session-id", "x"}}
	EnsureClaudeEnv(cmd)
	counts := map[string]int{}
	for _, a := range cmd.Args {
		counts[a]++
	}
	for _, want := range []string{"--strict-mcp-config", "--model", "haiku", "--effort", "low"} {
		if counts[want] != 1 {
			t.Errorf("got %d %q entries, want 1: %v", counts[want], want, cmd.Args)
		}
	}
}

func TestEnsureClaudeEnvScrubsNestingVars(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{
		"CLAUDECODE=1",
		"CLAUDE_CODE_CHILD_SESSION=1",
		"CLAUDE_CODE_SESSION_ID=abc-123",
		"CLAUDE_CODE_ENTRYPOINT=sdk-ts",
		"PATH=/usr/bin",
	}}
	EnsureClaudeEnv(cmd)
	for _, key := range []string{
		"CLAUDECODE",
		"CLAUDE_CODE_CHILD_SESSION",
		"CLAUDE_CODE_SESSION_ID",
		"CLAUDE_CODE_ENTRYPOINT",
	} {
		if hasEnvKey(cmd.Env, key) {
			t.Errorf("nesting var %q not scrubbed: %v", key, cmd.Env)
		}
	}
	if !hasEnvKey(cmd.Env, "PATH") {
		t.Errorf("unrelated var PATH was dropped: %v", cmd.Env)
	}
	if !contains(cmd.Env, ClaudeTermEnv) {
		t.Errorf("Env = %v, missing %q", cmd.Env, ClaudeTermEnv)
	}
}

func TestEnsureClaudeEnvPreservesOAuthToken(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{
		"CLAUDECODE=1",
		"CLAUDE_CODE_OAUTH_TOKEN=sk-secret",
	}}
	EnsureClaudeEnv(cmd)
	if hasEnvKey(cmd.Env, "CLAUDECODE") {
		t.Errorf("CLAUDECODE not scrubbed: %v", cmd.Env)
	}
	if !contains(cmd.Env, "CLAUDE_CODE_OAUTH_TOKEN=sk-secret") {
		t.Errorf("OAuth token dropped: %v", cmd.Env)
	}
}

func TestEnsureClaudeEnvScrubsNestingVarsFromOsEnviron(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	cmd := &exec.Cmd{} // nil Env seeds from os.Environ.
	EnsureClaudeEnv(cmd)
	if hasEnvKey(cmd.Env, "CLAUDECODE") {
		// Do NOT print cmd.Env — it is the real os.Environ and may hold secrets.
		t.Error("CLAUDECODE from os.Environ not scrubbed")
	}
}

func hasEnvKey(env []string, key string) bool {
	for _, kv := range env {
		if kv == key || strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

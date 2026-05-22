package tuidriver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionJSONLPath(t *testing.T) {
	// Non-existent cwds so EncodeCwd's canonicalisation falls through and
	// the byte-transform alone is asserted — mirrors cwd_test.go's pattern
	// (deterministic across machines, no FS-shape dependency).
	const home = "/home/test"
	const sessionID = "00000000-0000-0000-0000-000000000000"

	cases := []struct {
		name      string
		cwd       string
		sessionID string
		want      string
	}{
		{
			name:      "path separators map to hyphens",
			cwd:       "/tui-driver-test-does-not-exist/a/b",
			sessionID: sessionID,
			want:      filepath.Join(home, ".claude", "projects", "-tui-driver-test-does-not-exist-a-b", sessionID+".jsonl"),
		},
		{
			name:      "dots map to hyphens",
			cwd:       "/tui-driver-test-does-not-exist/v1.2.3",
			sessionID: sessionID,
			want:      filepath.Join(home, ".claude", "projects", "-tui-driver-test-does-not-exist-v1-2-3", sessionID+".jsonl"),
		},
		{
			name:      "spaces map to hyphens",
			cwd:       "/tui-driver-test-does-not-exist/Second Brain",
			sessionID: sessionID,
			want:      filepath.Join(home, ".claude", "projects", "-tui-driver-test-does-not-exist-Second-Brain", sessionID+".jsonl"),
		},
		{
			name:      "underscore is non-alnum (drift catch)",
			cwd:       "/tui-driver-test-does-not-exist/snake_case",
			sessionID: sessionID,
			want:      filepath.Join(home, ".claude", "projects", "-tui-driver-test-does-not-exist-snake-case", sessionID+".jsonl"),
		},
		{
			name:      "mixed case preserved by byte-transform",
			cwd:       "/tui-driver-test-does-not-exist/CamelCase",
			sessionID: sessionID,
			want:      filepath.Join(home, ".claude", "projects", "-tui-driver-test-does-not-exist-CamelCase", sessionID+".jsonl"),
		},
		{
			name:      "adjacent specials produce adjacent hyphens (no run-collapse)",
			cwd:       "/tui-driver-test-does-not-exist/a) [b",
			sessionID: sessionID,
			want:      filepath.Join(home, ".claude", "projects", "-tui-driver-test-does-not-exist-a---b", sessionID+".jsonl"),
		},
		{
			name:      "opaque session id (library does not validate)",
			cwd:       "/tui-driver-test-does-not-exist/x",
			sessionID: "abc",
			want:      filepath.Join(home, ".claude", "projects", "-tui-driver-test-does-not-exist-x", "abc.jsonl"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SessionJSONLPath(home, tc.cwd, tc.sessionID)
			if got != tc.want {
				t.Errorf("SessionJSONLPath(%q, %q, %q) = %q, want %q", home, tc.cwd, tc.sessionID, got, tc.want)
			}
		})
	}
}

func TestSessionJSONLPath_RealpathHappyPath(t *testing.T) {
	// Real on-disk cwd → confirms EncodeCwd's canonicalisation flows
	// through (on darwin t.TempDir() lives under /var → /private/var, so
	// the canonical form differs from the literal path).
	home := t.TempDir()
	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	const sessionID = "abc"
	want := filepath.Join(home, ".claude", "projects", EncodeCwd(work), sessionID+".jsonl")
	got := SessionJSONLPath(home, work, sessionID)
	if got != want {
		t.Errorf("SessionJSONLPath(home=%q, cwd=%q, %q) = %q, want %q", home, work, sessionID, got, want)
	}
}

func TestWaitForSessionJSONL_FileAlreadyExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	start := time.Now()
	if err := WaitForSessionJSONL(context.Background(), path); err != nil {
		t.Fatalf("WaitForSessionJSONL = %v, want nil", err)
	}
	if d := time.Since(start); d > 5*time.Millisecond {
		t.Errorf("WaitForSessionJSONL(exists) took %v, want <5ms (no ticker setup on short-circuit)", d)
	}
}

func TestWaitForSessionJSONL_FileAppearsPartwayThrough(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.WriteFile(path, []byte("{}\n"), 0o644)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := WaitForSessionJSONL(ctx, path); err != nil {
		t.Fatalf("WaitForSessionJSONL = %v, want nil", err)
	}
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Errorf("WaitForSessionJSONL returned in %v, want >=100ms (file appears mid-poll)", d)
	}
}

func TestWaitForSessionJSONL_ContextCancellation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "never-appears.jsonl")
	sentinel := errors.New("operator hit cancel")
	ctx, cancel := context.WithCancelCause(context.Background())
	var cancelled atomic.Bool
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancelled.Store(true)
		cancel(sentinel)
	}()
	err := WaitForSessionJSONL(ctx, path)
	if err == nil {
		t.Fatalf("WaitForSessionJSONL = nil, want error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("WaitForSessionJSONL cause = %v, want errors.Is(_, sentinel)", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("WaitForSessionJSONL err = %q, want it to mention path %q", err.Error(), path)
	}
	if !cancelled.Load() {
		t.Errorf("returned before cancel goroutine ran — race in test or in implementation")
	}
}

func TestWaitForSessionJSONL_DeadlineExpiry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "never-appears.jsonl")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := WaitForSessionJSONL(ctx, path)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitForSessionJSONL = %v, want errors.Is(_, context.DeadlineExceeded)", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("WaitForSessionJSONL err = %q, want it to mention path %q", err.Error(), path)
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Errorf("WaitForSessionJSONL took %v, want close to 100ms", d)
	}
}

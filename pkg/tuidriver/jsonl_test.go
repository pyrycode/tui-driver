package tuidriver

import (
	"context"
	"errors"
	"io/fs"
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

// mustReceive blocks for at most timeout waiting for an entry on ch.
// Fails the test if no value arrives or if the channel closes early.
// Used to bound test runtime under deadlock — a stuck tail goroutine
// would otherwise hang until go test's default timeout.
func mustReceive(t *testing.T, ch <-chan JSONLEntry, timeout time.Duration) JSONLEntry {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed before receive")
		}
		return ev
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for entry (%v)", timeout)
		return JSONLEntry{}
	}
}

func mustAppend(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
}

func TestTailJSONL_AppendDuringTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"a"}`+"\n"+`{"type":"b"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := TailJSONL(ctx, path, 0)
	if err != nil {
		t.Fatalf("TailJSONL = %v, want nil", err)
	}
	if got := mustReceive(t, ch, 500*time.Millisecond).Type; got != "a" {
		t.Errorf("entry 1 Type = %q, want %q", got, "a")
	}
	if got := mustReceive(t, ch, 500*time.Millisecond).Type; got != "b" {
		t.Errorf("entry 2 Type = %q, want %q", got, "b")
	}
	mustAppend(t, path, `{"type":"c"}`+"\n"+`{"type":"d"}`+"\n")
	if got := mustReceive(t, ch, 500*time.Millisecond).Type; got != "c" {
		t.Errorf("entry 3 Type = %q, want %q", got, "c")
	}
	if got := mustReceive(t, ch, 500*time.Millisecond).Type; got != "d" {
		t.Errorf("entry 4 Type = %q, want %q", got, "d")
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Errorf("expected channel close after cancel, got entry")
		}
	case <-time.After(500 * time.Millisecond):
		t.Errorf("channel did not close within 500ms after cancel")
	}
}

func TestTailJSONL_PartialLineAcrossReads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"split"`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := TailJSONL(ctx, path, 0)
	if err != nil {
		t.Fatalf("TailJSONL = %v, want nil", err)
	}
	// Let the tail consume the partial bytes, hit EOF, park in retry.
	time.Sleep(150 * time.Millisecond)
	mustAppend(t, path, `,"id":"x"}`+"\n")
	ev := mustReceive(t, ch, 500*time.Millisecond)
	if ev.Type != "split" {
		t.Errorf("Type = %q, want %q", ev.Type, "split")
	}
	if got, _ := ev.Raw["id"].(string); got != "x" {
		t.Errorf("Raw[\"id\"] = %v, want %q", ev.Raw["id"], "x")
	}
	// Assert no second (spurious split) entry arrives.
	select {
	case extra, ok := <-ch:
		if ok {
			t.Errorf("unexpected second entry: %+v", extra)
		}
	case <-time.After(150 * time.Millisecond):
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Errorf("expected channel close after cancel, got entry")
		}
	case <-time.After(500 * time.Millisecond):
		t.Errorf("channel did not close within 500ms after cancel")
	}
}

func TestTailJSONL_MalformedLineNonFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte("not json at all\n"+`{"type":"recovers"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := TailJSONL(ctx, path, 0)
	if err != nil {
		t.Fatalf("TailJSONL = %v, want nil", err)
	}
	ev := mustReceive(t, ch, 500*time.Millisecond)
	if ev.Type != "recovers" {
		t.Errorf("Type = %q, want %q (malformed line should be silently dropped, valid line still emitted)", ev.Type, "recovers")
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Errorf("expected channel close after cancel, got entry")
		}
	case <-time.After(500 * time.Millisecond):
		t.Errorf("channel did not close within 500ms after cancel")
	}
}

func TestTailJSONL_ContextCancellationMidRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"x"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	sentinel := errors.New("test stop")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	ch, err := TailJSONL(ctx, path, 0)
	if err != nil {
		t.Fatalf("TailJSONL = %v, want nil", err)
	}
	// Drain pre-written entry, then let the goroutine park in EOF-sleep.
	_ = mustReceive(t, ch, 500*time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	cancel(sentinel)
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("got entry instead of close after cancel")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("channel did not close within 200ms after cancel (one tick is 50ms)")
	}
	if !errors.Is(context.Cause(ctx), sentinel) {
		t.Errorf("context.Cause(ctx) = %v, want errors.Is(_, sentinel)", context.Cause(ctx))
	}
}

func TestTailJSONL_EOFAppendCycles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"0"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := TailJSONL(ctx, path, 0)
	if err != nil {
		t.Fatalf("TailJSONL = %v, want nil", err)
	}
	if got := mustReceive(t, ch, 500*time.Millisecond).Type; got != "0" {
		t.Fatalf("entry 0 Type = %q, want %q", got, "0")
	}
	for i, want := range []string{"1", "2", "3"} {
		time.Sleep(150 * time.Millisecond)
		mustAppend(t, path, `{"type":"`+want+`"}`+"\n")
		if got := mustReceive(t, ch, 500*time.Millisecond).Type; got != want {
			t.Errorf("cycle %d Type = %q, want %q", i+1, got, want)
		}
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Errorf("expected channel close after cancel, got entry")
		}
	case <-time.After(500 * time.Millisecond):
		t.Errorf("channel did not close within 500ms after cancel")
	}
}

func TestTailJSONL_StartOffsetSkipsPrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	const lineA = `{"type":"a"}` + "\n"
	body := lineA + `{"type":"b"}` + "\n" + `{"type":"c"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	offset := int64(len(lineA))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := TailJSONL(ctx, path, offset)
	if err != nil {
		t.Fatalf("TailJSONL = %v, want nil", err)
	}
	if got := mustReceive(t, ch, 500*time.Millisecond).Type; got != "b" {
		t.Errorf("entry 1 Type = %q, want %q (offset must skip line a)", got, "b")
	}
	if got := mustReceive(t, ch, 500*time.Millisecond).Type; got != "c" {
		t.Errorf("entry 2 Type = %q, want %q", got, "c")
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Errorf("expected channel close after cancel, got entry")
		}
	case <-time.After(500 * time.Millisecond):
		t.Errorf("channel did not close within 500ms after cancel")
	}
}

func TestTailJSONL_OpenFailsWhenFileMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nonexistent", "x.jsonl")
	ch, err := TailJSONL(context.Background(), missing, 0)
	if err == nil {
		t.Fatalf("TailJSONL(missing) err = nil, want error")
	}
	if ch != nil {
		t.Errorf("TailJSONL(missing) ch != nil, want nil")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("err = %q, want it to mention path %q", err.Error(), missing)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want errors.Is(_, fs.ErrNotExist)", err)
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

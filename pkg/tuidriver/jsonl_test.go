package tuidriver

import (
	"bytes"
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

func TestTailJSONL_RawLineByteFidelity(t *testing.T) {
	// Each scenario is one source line whose verbatim bytes must round-trip
	// through TailJSONL unchanged. These four shapes are the realistic
	// worst-case for re-marshal drift via json.Marshal(Raw):
	//   - mixed-order keys: Go's encoder sorts map keys alphabetically
	//   - unicode: multi-byte UTF-8 must not be re-encoded
	//   - escaped newlines: \n inside a string stays escaped, not literal
	//   - embedded base64: long string with +/= must pass through verbatim
	lines := []string{
		`{"z":1,"a":2,"m":3}`,
		`{"type":"assistant","msg":"こんにちは 🎉 — ok"}`,
		`{"type":"text","text":"line1\nline2\twith\ttabs"}`,
		`{"type":"tool_use","input":{"payload":"SGVsbG8sIFdvcmxkISBUaGlzIGlzIGEgbG9uZ2lzaCBiYXNlNjQrLz09"}}`,
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	var buf bytes.Buffer
	for _, line := range lines {
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := TailJSONL(ctx, path, 0)
	if err != nil {
		t.Fatalf("TailJSONL = %v, want nil", err)
	}
	entries := make([]JSONLEntry, len(lines))
	for i, want := range lines {
		entries[i] = mustReceive(t, ch, 500*time.Millisecond)
		if !bytes.Equal(entries[i].RawLine, []byte(want)) {
			t.Errorf("entry %d RawLine =\n  %q\nwant\n  %q", i, entries[i].RawLine, want)
		}
	}
	// Defensive-copy pin: appending more lines must not clobber earlier
	// entries' RawLine. If parseEntry ever returned a slice aliasing the
	// rolling buffer, the next iteration's append would overwrite it.
	mustAppend(t, path, `{"type":"after"}`+"\n")
	_ = mustReceive(t, ch, 500*time.Millisecond)
	for i, want := range lines {
		if !bytes.Equal(entries[i].RawLine, []byte(want)) {
			t.Errorf("entry %d RawLine clobbered after later read =\n  %q\nwant\n  %q", i, entries[i].RawLine, want)
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

// textBlock builds a content block of type "text" with the given text.
func textBlock(text string) ContentBlock {
	return ContentBlock{Type: "text", Raw: map[string]any{"type": "text", "text": text}}
}

// blockWithRaw builds a content block of arbitrary type carrying the
// given raw payload. Used for malformed-shape scenarios (non-string
// "text" field, missing "text" field, non-text block types).
func blockWithRaw(kind string, raw map[string]any) ContentBlock {
	return ContentBlock{Type: kind, Raw: raw}
}

func TestIsEndTurn(t *testing.T) {
	cases := []struct {
		name string
		e    JSONLEntry
		want bool
	}{
		{
			name: "assistant + end_turn + non-empty text block",
			e: JSONLEntry{
				Type: "assistant",
				Message: &EntryMessage{
					StopReason: "end_turn",
					Content:    []ContentBlock{textBlock("hello")},
				},
			},
			want: true,
		},
		{
			name: "assistant + end_turn + empty text block",
			e: JSONLEntry{
				Type: "assistant",
				Message: &EntryMessage{
					StopReason: "end_turn",
					Content:    []ContentBlock{textBlock("")},
				},
			},
			want: false,
		},
		{
			name: "assistant + end_turn + only tool_use blocks",
			e: JSONLEntry{
				Type: "assistant",
				Message: &EntryMessage{
					StopReason: "end_turn",
					Content: []ContentBlock{
						blockWithRaw("tool_use", map[string]any{"type": "tool_use", "name": "bash"}),
					},
				},
			},
			want: false,
		},
		{
			name: "assistant + end_turn + only thinking blocks",
			e: JSONLEntry{
				Type: "assistant",
				Message: &EntryMessage{
					StopReason: "end_turn",
					Content: []ContentBlock{
						blockWithRaw("thinking", map[string]any{"type": "thinking", "thinking": "musing"}),
					},
				},
			},
			want: false,
		},
		{
			name: "assistant + end_turn + thinking AND text blocks",
			e: JSONLEntry{
				Type: "assistant",
				Message: &EntryMessage{
					StopReason: "end_turn",
					Content: []ContentBlock{
						blockWithRaw("thinking", map[string]any{"type": "thinking", "thinking": "musing"}),
						textBlock("hello"),
					},
				},
			},
			want: true,
		},
		{
			name: "assistant + stop_reason=tool_use + text block",
			e: JSONLEntry{
				Type: "assistant",
				Message: &EntryMessage{
					StopReason: "tool_use",
					Content:    []ContentBlock{textBlock("hello")},
				},
			},
			want: false,
		},
		{
			name: "assistant + empty stop_reason + text block",
			e: JSONLEntry{
				Type: "assistant",
				Message: &EntryMessage{
					StopReason: "",
					Content:    []ContentBlock{textBlock("hello")},
				},
			},
			want: false,
		},
		{
			name: "assistant + nil Message",
			e:    JSONLEntry{Type: "assistant", Message: nil},
			want: false,
		},
		{
			name: "type=user + end_turn + text block",
			e: JSONLEntry{
				Type: "user",
				Message: &EntryMessage{
					StopReason: "end_turn",
					Content:    []ContentBlock{textBlock("hello")},
				},
			},
			want: false,
		},
		{
			name: "type=system",
			e:    JSONLEntry{Type: "system"},
			want: false,
		},
		{
			name: "zero-value entry",
			e:    JSONLEntry{},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsEndTurn(tc.e); got != tc.want {
				t.Errorf("IsEndTurn = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAssistantText(t *testing.T) {
	cases := []struct {
		name string
		e    JSONLEntry
		want string
	}{
		{
			name: "assistant + one text block",
			e: JSONLEntry{
				Type:    "assistant",
				Message: &EntryMessage{Content: []ContentBlock{textBlock("hello")}},
			},
			want: "hello",
		},
		{
			name: "assistant + two text blocks concatenated in order",
			e: JSONLEntry{
				Type: "assistant",
				Message: &EntryMessage{Content: []ContentBlock{
					textBlock("hello"),
					textBlock(" world"),
				}},
			},
			want: "hello world",
		},
		{
			name: "assistant + text, thinking, text — thinking skipped",
			e: JSONLEntry{
				Type: "assistant",
				Message: &EntryMessage{Content: []ContentBlock{
					textBlock("hello"),
					blockWithRaw("thinking", map[string]any{"type": "thinking", "thinking": "musing"}),
					textBlock(" world"),
				}},
			},
			want: "hello world",
		},
		{
			name: "assistant + only tool_use block",
			e: JSONLEntry{
				Type: "assistant",
				Message: &EntryMessage{Content: []ContentBlock{
					blockWithRaw("tool_use", map[string]any{"type": "tool_use", "name": "bash"}),
				}},
			},
			want: "",
		},
		{
			name: "assistant + text block with non-string text field",
			e: JSONLEntry{
				Type: "assistant",
				Message: &EntryMessage{Content: []ContentBlock{
					blockWithRaw("text", map[string]any{"type": "text", "text": 42}),
				}},
			},
			want: "",
		},
		{
			name: "assistant + text block with missing text field",
			e: JSONLEntry{
				Type: "assistant",
				Message: &EntryMessage{Content: []ContentBlock{
					blockWithRaw("text", map[string]any{"type": "text"}),
				}},
			},
			want: "",
		},
		{
			name: "assistant + nil Message",
			e:    JSONLEntry{Type: "assistant", Message: nil},
			want: "",
		},
		{
			name: "type=user + text block",
			e: JSONLEntry{
				Type:    "user",
				Message: &EntryMessage{Content: []ContentBlock{textBlock("hello")}},
			},
			want: "",
		},
		{
			name: "zero-value entry",
			e:    JSONLEntry{},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AssistantText(tc.e); got != tc.want {
				t.Errorf("AssistantText = %q, want %q", got, tc.want)
			}
		})
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

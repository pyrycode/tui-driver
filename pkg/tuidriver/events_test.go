package tuidriver

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testSnap is a mutable snapshot source for mergeEvents tests. Set
// replaces the snapshot bytes; Snapshot returns a copy under lock —
// matching Buffer.Snapshot's contract so the merge loop can't tell
// the difference. Used in place of a *Buffer where tests need direct
// state replacement (e.g. transitioning modal class A → B by dropping
// A's anchor bytes; *Buffer's append-only semantics would require
// overflow-by-byte-count instead).
type testSnap struct {
	mu sync.Mutex
	b  []byte
}

func (s *testSnap) Snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]byte, len(s.b))
	copy(out, s.b)
	return out
}

func (s *testSnap) Set(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b = b
}

// mustReceiveEvent blocks for at most timeout waiting for an Event on
// ch. Fails the test if no value arrives or if the channel closes
// early. Mirrors the mustReceive helper in jsonl_test.go.
func mustReceiveEvent(t *testing.T, ch <-chan Event, timeout time.Duration) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed before receive")
		}
		return ev
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for event (%v)", timeout)
		return Event{}
	}
}

// assertEventChClosed asserts that ch closes within timeout and that
// no further event is delivered.
func assertEventChClosed(t *testing.T, ch <-chan Event, timeout time.Duration) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("expected channel close, got event %+v", ev)
		}
	case <-time.After(timeout):
		t.Fatalf("channel did not close within %v", timeout)
	}
}

func TestMergeEvents_PtyIdleAndThinkingTransitions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap := &testSnap{}
	jsonlCh := make(chan JSONLEntry)
	out := make(chan Event, defaultEventBuffer)
	go mergeEvents(ctx, snap.Snapshot, jsonlCh, out, DefaultPollInterval)

	// Phase 1: ❯ alone → PtyIdle.
	snap.Set([]byte("\xe2\x9d\xaf input"))
	ev := mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyIdle {
		t.Errorf("phase 1 Kind = %v, want EventKindPtyIdle", ev.Kind)
	}
	if ev.Source != EventSourcePty {
		t.Errorf("phase 1 Source = %v, want EventSourcePty", ev.Source)
	}
	if ev.Time.IsZero() {
		t.Errorf("phase 1 Time is zero, want non-zero wall-clock")
	}
	if ev.Modal != ModalClassUnknown {
		t.Errorf("phase 1 Modal = %q, want unset (zero)", ev.Modal)
	}

	// Phase 2: ✻ present → PtyThinking.
	snap.Set([]byte("\xe2\x9c\xbb Baked for 2s\n\xe2\x9d\xaf input"))
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyThinking {
		t.Errorf("phase 2 Kind = %v, want EventKindPtyThinking", ev.Kind)
	}

	// Phase 3: spinner gone, ❯ alone → PtyIdle again.
	snap.Set([]byte("\xe2\x9d\xaf input"))
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyIdle {
		t.Errorf("phase 3 Kind = %v, want EventKindPtyIdle", ev.Kind)
	}

	cancel()
	assertEventChClosed(t, out, 500*time.Millisecond)
}

func TestMergeEvents_ModalShowAndHide(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap := &testSnap{}
	jsonlCh := make(chan JSONLEntry)
	out := make(chan Event, defaultEventBuffer)
	go mergeEvents(ctx, snap.Snapshot, jsonlCh, out, DefaultPollInterval)

	// Phase 1: enter Permission modal. ❯ is present (modal still renders
	// the input line) but idle/thinking emissions are suppressed while a
	// modal is up — assert no spurious PtyIdle arrives.
	snap.Set([]byte("\xe2\x9d\xaf Do you want to proceed"))
	ev := mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyModalShown {
		t.Errorf("phase 1 Kind = %v, want EventKindPtyModalShown", ev.Kind)
	}
	if ev.Modal != ModalClassPermission {
		t.Errorf("phase 1 Modal = %q, want %q", ev.Modal, ModalClassPermission)
	}
	if ev.Source != EventSourcePty {
		t.Errorf("phase 1 Source = %v, want EventSourcePty", ev.Source)
	}

	// Phase 2: class change Permission → MCP. Two events in order:
	// Hidden(Permission) then Shown(MCP).
	snap.Set([]byte("ManageMCPservers"))
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyModalHidden {
		t.Errorf("phase 2a Kind = %v, want EventKindPtyModalHidden", ev.Kind)
	}
	if ev.Modal != ModalClassPermission {
		t.Errorf("phase 2a Modal = %q, want %q (the just-hidden class)", ev.Modal, ModalClassPermission)
	}
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyModalShown {
		t.Errorf("phase 2b Kind = %v, want EventKindPtyModalShown", ev.Kind)
	}
	if ev.Modal != ModalClassMCP {
		t.Errorf("phase 2b Modal = %q, want %q", ev.Modal, ModalClassMCP)
	}

	// Phase 3: clear modal anchors, expose idle. Two events: Hidden(MCP)
	// then PtyIdle (idle/thinking re-enabled once modal == Unknown).
	snap.Set([]byte("\xe2\x9d\xaf ready"))
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyModalHidden {
		t.Errorf("phase 3a Kind = %v, want EventKindPtyModalHidden", ev.Kind)
	}
	if ev.Modal != ModalClassMCP {
		t.Errorf("phase 3a Modal = %q, want %q", ev.Modal, ModalClassMCP)
	}
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyIdle {
		t.Errorf("phase 3b Kind = %v, want EventKindPtyIdle", ev.Kind)
	}

	cancel()
	assertEventChClosed(t, out, 500*time.Millisecond)
}

func TestMergeEvents_JsonlEntryAndSyntheticEndOfTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Empty snap → classify yields (false, false, Unknown) matching prev's
	// zero values: no PTY events fire, so only JSONL events show up.
	snap := &testSnap{}
	jsonlCh := make(chan JSONLEntry, 4)
	out := make(chan Event, defaultEventBuffer)
	go mergeEvents(ctx, snap.Snapshot, jsonlCh, out, DefaultPollInterval)

	// Non-assistant entry: emits one JsonlEntry, no EndOfTurn.
	userEntry := JSONLEntry{
		Type: "user",
		Message: &EntryMessage{
			StopReason: "end_turn",
			Content:    []ContentBlock{textBlock("hello")},
		},
	}
	jsonlCh <- userEntry
	ev := mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindJsonlEntry {
		t.Errorf("user Kind = %v, want EventKindJsonlEntry", ev.Kind)
	}
	if ev.Source != EventSourceJsonl {
		t.Errorf("user Source = %v, want EventSourceJsonl", ev.Source)
	}
	if ev.Entry.Type != "user" {
		t.Errorf("user Entry.Type = %q, want %q", ev.Entry.Type, "user")
	}

	// Assistant + end_turn + text: emits JsonlEntry then JsonlEndOfTurn,
	// both carrying the same entry.
	endTurnEntry := JSONLEntry{
		Type: "assistant",
		Message: &EntryMessage{
			StopReason: "end_turn",
			Content:    []ContentBlock{textBlock("done")},
		},
	}
	jsonlCh <- endTurnEntry
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindJsonlEntry {
		t.Errorf("end-turn pair 1 Kind = %v, want EventKindJsonlEntry", ev.Kind)
	}
	if ev.Entry.Type != "assistant" {
		t.Errorf("end-turn pair 1 Entry.Type = %q, want %q", ev.Entry.Type, "assistant")
	}
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindJsonlEndOfTurn {
		t.Errorf("end-turn pair 2 Kind = %v, want EventKindJsonlEndOfTurn", ev.Kind)
	}
	if ev.Source != EventSourceJsonl {
		t.Errorf("end-turn pair 2 Source = %v, want EventSourceJsonl", ev.Source)
	}
	if ev.Entry.Message == nil || ev.Entry.Message.StopReason != "end_turn" {
		t.Errorf("end-turn pair 2 Entry does not carry the entry (got %+v)", ev.Entry)
	}

	// Assistant + end_turn + thinking-only: emits JsonlEntry, NO
	// EndOfTurn within ~150 ms.
	thinkingEntry := JSONLEntry{
		Type: "assistant",
		Message: &EntryMessage{
			StopReason: "end_turn",
			Content: []ContentBlock{
				blockWithRaw("thinking", map[string]any{"type": "thinking", "thinking": "musing"}),
			},
		},
	}
	jsonlCh <- thinkingEntry
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindJsonlEntry {
		t.Errorf("thinking-only Kind = %v, want EventKindJsonlEntry", ev.Kind)
	}
	select {
	case extra, ok := <-out:
		if !ok {
			t.Fatalf("output closed unexpectedly after thinking-only entry")
		}
		if extra.Kind == EventKindJsonlEndOfTurn {
			t.Errorf("thinking-only fired EventKindJsonlEndOfTurn, want no follow-up")
		}
	case <-time.After(150 * time.Millisecond):
	}

	cancel()
	assertEventChClosed(t, out, 500*time.Millisecond)
}

func TestMergeEvents_InterleavedArrivalOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap := &testSnap{}
	jsonlCh := make(chan JSONLEntry, 4)
	out := make(chan Event, defaultEventBuffer)
	go mergeEvents(ctx, snap.Snapshot, jsonlCh, out, DefaultPollInterval)

	// 1. PTY: enter idle.
	snap.Set([]byte("\xe2\x9d\xaf input"))
	ev := mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyIdle {
		t.Fatalf("step 1 Kind = %v, want EventKindPtyIdle", ev.Kind)
	}

	// 2. JSONL entry.
	jsonlCh <- JSONLEntry{Type: "user"}
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindJsonlEntry {
		t.Fatalf("step 2 Kind = %v, want EventKindJsonlEntry", ev.Kind)
	}

	// 3. PTY: enter thinking.
	snap.Set([]byte("\xe2\x9c\xbb thinking"))
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyThinking {
		t.Fatalf("step 3 Kind = %v, want EventKindPtyThinking", ev.Kind)
	}

	// 4. JSONL entry.
	jsonlCh <- JSONLEntry{Type: "assistant"}
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindJsonlEntry {
		t.Fatalf("step 4 Kind = %v, want EventKindJsonlEntry", ev.Kind)
	}

	cancel()
	assertEventChClosed(t, out, 500*time.Millisecond)
}

func TestMergeEvents_CleanShutdown(t *testing.T) {
	sentinel := errors.New("test stop")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	// Empty snap → no startup events to drain.
	snap := &testSnap{}
	jsonlCh := make(chan JSONLEntry)
	out := make(chan Event, defaultEventBuffer)
	go mergeEvents(ctx, snap.Snapshot, jsonlCh, out, DefaultPollInterval)

	// Park briefly so the merge goroutine has set up its ticker and is
	// blocked in the select — the close-on-cancel timing test exercises
	// the ctx.Done() arm, not the pre-loop short-circuit.
	time.Sleep(75 * time.Millisecond)
	cancel(sentinel)
	select {
	case _, ok := <-out:
		if ok {
			t.Fatalf("got event instead of close after cancel")
		}
	case <-time.After(2 * DefaultPollInterval):
		t.Fatalf("channel did not close within %v after cancel", 2*DefaultPollInterval)
	}
	if !errors.Is(context.Cause(ctx), sentinel) {
		t.Errorf("context.Cause(ctx) = %v, want errors.Is(_, sentinel)", context.Cause(ctx))
	}
}

func TestMergeEvents_JsonlChClosureClosesOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap := &testSnap{}
	jsonlCh := make(chan JSONLEntry)
	out := make(chan Event, defaultEventBuffer)
	go mergeEvents(ctx, snap.Snapshot, jsonlCh, out, DefaultPollInterval)

	// Park briefly so the merge goroutine is in its select loop, then
	// close jsonlCh — that should drive the loop to exit and close out.
	time.Sleep(75 * time.Millisecond)
	close(jsonlCh)
	select {
	case _, ok := <-out:
		if ok {
			t.Fatalf("got event instead of close after jsonlCh closure")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("output did not close within 200ms after jsonlCh closure")
	}
}

func TestEvents_TailJSONLErrorBubbles(t *testing.T) {
	s := &Session{Buffer: NewBuffer(0)}
	missing := filepath.Join(t.TempDir(), "nonexistent", "x.jsonl")
	ch, err := s.Events(context.Background(), missing, 0)
	if err == nil {
		t.Fatalf("Events(missing) err = nil, want error")
	}
	if ch != nil {
		t.Errorf("Events(missing) ch != nil, want nil")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("err = %q, want it to mention path %q", err.Error(), missing)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want errors.Is(_, fs.ErrNotExist)", err)
	}
}

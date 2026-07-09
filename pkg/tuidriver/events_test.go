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

// neverQuiet is a quietFor closure that reports the PTY is never quiet
// (0 elapsed since the last byte). With it, the stall arm's condition
// (b) — quietFor() > ptyQuietLimit — can never hold, so the existing
// merge-loop tests exercise the idle/thinking/modal/banner/JSONL axes
// unchanged: no stall event ever fires.
func neverQuiet() time.Duration { return 0 }

// zeroDims is the dims closure the merge-loop tests pass: (0, 0) falls through
// to the package default grid size (NewGrid's convention), so these tests
// exercise detection at the default 120x40 exactly as before #226 added the
// session-size-aware render path. The session-resize path is covered in
// resize_test.go.
func zeroDims() (int, int) { return 0, 0 }

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
	go mergeEvents(ctx, snap.Snapshot, zeroDims, neverQuiet, DefaultPTYQuietLimit, jsonlCh, out, DefaultPollInterval)

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
	go mergeEvents(ctx, snap.Snapshot, zeroDims, neverQuiet, DefaultPTYQuietLimit, jsonlCh, out, DefaultPollInterval)

	// Phase 1: enter Permission modal (prompt + ❯-marked option row, the #242
	// dialog shape). ❯ is present (on the option row) but idle/thinking emissions
	// are suppressed while a modal is up — assert no spurious PtyIdle arrives.
	snap.Set([]byte("Do you want to proceed\r\n\xe2\x9d\xaf 1. Yes"))
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
	// Hidden(Permission) then Shown(MCP). Spaced form: DetectModalClass matches
	// the rendered grid since #152, so the on-screen "Manage MCP servers" (not
	// the old StripANSI-stripped variant) is what classifies. The 38;5;153
	// highlight escape supplies the #223 mcp co-signal (index 153 → 175,215,255).
	snap.Set([]byte("\x1b[38;5;153mManage MCP servers\x1b[39m"))
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

// TestMergeEvents_ModalClearRevealsSuppressedIdleThenThinking drives the
// direct Permission → idle path the masking TestMergeEvents_ModalShowAndHide
// misses (it detours through an MCP snapshot with no ❯, which resets prev.idle
// to false). Here the modal renders ❯ throughout its lifetime, so IsIdle is
// true across the whole modal-active window — the exact shape that leaked the
// idle rising edge before #157. Asserts the edge survives the modal window and
// fires on the clear tick, and (phases 3-4) that the freeze applies
// symmetrically to prev.thinking.
func TestMergeEvents_ModalClearRevealsSuppressedIdleThenThinking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap := &testSnap{}
	jsonlCh := make(chan JSONLEntry)
	out := make(chan Event, defaultEventBuffer)
	go mergeEvents(ctx, snap.Snapshot, zeroDims, neverQuiet, DefaultPTYQuietLimit, jsonlCh, out, DefaultPollInterval)

	// Phase 1: Permission modal with ❯ present. IsIdle is true throughout,
	// but idle emission is suppressed while a modal is up — only ModalShown
	// fires, no PtyIdle.
	snap.Set([]byte("Do you want to proceed\r\n\xe2\x9d\xaf 1. Yes"))
	ev := mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyModalShown {
		t.Errorf("phase 1 Kind = %v, want EventKindPtyModalShown", ev.Kind)
	}
	if ev.Modal != ModalClassPermission {
		t.Errorf("phase 1 Modal = %q, want %q", ev.Modal, ModalClassPermission)
	}
	select {
	case extra, ok := <-out:
		if !ok {
			t.Fatalf("phase 1 channel closed unexpectedly")
		}
		t.Fatalf("phase 1 unexpected follow-up event %+v (idle suppressed under modal)", extra)
	case <-time.After(2 * DefaultPollInterval):
	}

	// Phase 2: modal cleared, still idle (❯ present, no anchor, no spinner).
	// Two events in order: ModalHidden(Permission) then PtyIdle. The idle edge
	// is preserved because prev.idle was frozen at false across the modal
	// window. Against the pre-#157 code only ModalHidden arrives (prev.idle
	// had tracked the ❯-under-modal classification to true), so this receive
	// times out.
	snap.Set([]byte("\xe2\x9d\xaf ready"))
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyModalHidden {
		t.Errorf("phase 2a Kind = %v, want EventKindPtyModalHidden", ev.Kind)
	}
	if ev.Modal != ModalClassPermission {
		t.Errorf("phase 2a Modal = %q, want %q", ev.Modal, ModalClassPermission)
	}
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyIdle {
		t.Errorf("phase 2b Kind = %v, want EventKindPtyIdle", ev.Kind)
	}

	// Phase 3: re-enter the Permission modal (prev.idle is now true). Only
	// ModalShown fires — the modal did not introduce a new low→high idle
	// transition, and the freeze holds prev.idle/prev.thinking across the
	// window.
	snap.Set([]byte("Do you want to proceed\r\n\xe2\x9d\xaf 1. Yes"))
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyModalShown {
		t.Errorf("phase 3 Kind = %v, want EventKindPtyModalShown", ev.Kind)
	}
	select {
	case extra, ok := <-out:
		if !ok {
			t.Fatalf("phase 3 channel closed unexpectedly")
		}
		t.Fatalf("phase 3 unexpected follow-up event %+v (idle suppressed under modal)", extra)
	case <-time.After(2 * DefaultPollInterval):
	}

	// Phase 4: modal cleared to a spinner-present snapshot. Two events in
	// order: ModalHidden(Permission) then PtyThinking — confirming the freeze
	// held prev.thinking at false across the modal window (AC 2). No spurious
	// PtyIdle: cur.idle is false (busy anchor present).
	snap.Set([]byte("\xe2\x9c\xbb Baked for 2s\n\xe2\x9d\xaf input"))
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyModalHidden {
		t.Errorf("phase 4a Kind = %v, want EventKindPtyModalHidden", ev.Kind)
	}
	if ev.Modal != ModalClassPermission {
		t.Errorf("phase 4a Modal = %q, want %q", ev.Modal, ModalClassPermission)
	}
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyThinking {
		t.Errorf("phase 4b Kind = %v, want EventKindPtyThinking", ev.Kind)
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
	go mergeEvents(ctx, snap.Snapshot, zeroDims, neverQuiet, DefaultPTYQuietLimit, jsonlCh, out, DefaultPollInterval)

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
	go mergeEvents(ctx, snap.Snapshot, zeroDims, neverQuiet, DefaultPTYQuietLimit, jsonlCh, out, DefaultPollInterval)

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
	go mergeEvents(ctx, snap.Snapshot, zeroDims, neverQuiet, DefaultPTYQuietLimit, jsonlCh, out, DefaultPollInterval)

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

func TestMergeEvents_JsonlChClosureCtxLiveEmitsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap := &testSnap{}
	jsonlCh := make(chan JSONLEntry)
	out := make(chan Event, defaultEventBuffer)
	go mergeEvents(ctx, snap.Snapshot, zeroDims, neverQuiet, DefaultPTYQuietLimit, jsonlCh, out, DefaultPollInterval)

	// Park briefly so the merge goroutine is in its select loop, then
	// close jsonlCh with ctx STILL LIVE — the merge-seam simulation of the
	// tail's default: read-error exit. mergeEvents must emit one terminal
	// EventKindError before closing out.
	time.Sleep(75 * time.Millisecond)
	close(jsonlCh)
	ev := mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindError {
		t.Fatalf("Kind = %v, want EventKindError", ev.Kind)
	}
	if ev.Source != EventSourceJsonl {
		t.Errorf("Source = %v, want EventSourceJsonl", ev.Source)
	}
	if !errors.Is(ev.Err, ErrJSONLTailRead) {
		t.Errorf("Err = %v, want errors.Is(_, ErrJSONLTailRead)", ev.Err)
	}
	assertEventChClosed(t, out, 500*time.Millisecond)
}

// TestMergeEvents_JsonlChCloseAfterCancelEmitsNoError covers the
// EOF-then-shutdown shape: ctx cancels first (the clean tail-exit
// trigger), then the tail's deferred close(jsonlCh) fires. ctx.Err() is
// non-nil at the !ok branch, so mergeEvents emits NO EventKindError —
// the AC-#4 clean-path companion to the read-error case above.
func TestMergeEvents_JsonlChCloseAfterCancelEmitsNoError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap := &testSnap{}
	jsonlCh := make(chan JSONLEntry)
	out := make(chan Event, defaultEventBuffer)
	go mergeEvents(ctx, snap.Snapshot, zeroDims, neverQuiet, DefaultPTYQuietLimit, jsonlCh, out, DefaultPollInterval)

	time.Sleep(75 * time.Millisecond)
	cancel()
	close(jsonlCh)
	// A spurious EventKindError would surface here as an event, not a
	// close; assertEventChClosed fails on any event.
	assertEventChClosed(t, out, 500*time.Millisecond)
}

// TestMergeEvents_ReadFaultSurfacesTerminalError wires the full chain —
// faultReader → tailJSONLLoop → jsonlCh → mergeEvents → out — proving a
// read error injected into the running tail loop reaches the Events()
// consumer as a distinct terminal EventKindError (AC #1, #4).
func TestMergeEvents_ReadFaultSurfacesTerminalError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap := &testSnap{}
	r := &faultReader{data: []byte(`{"type":"good"}` + "\n")}
	jsonlCh := make(chan JSONLEntry, defaultJSONLTailBuffer)
	out := make(chan Event, defaultEventBuffer)
	go tailJSONLLoop(ctx, r, jsonlCh)
	go mergeEvents(ctx, snap.Snapshot, zeroDims, neverQuiet, DefaultPTYQuietLimit, jsonlCh, out, DefaultPollInterval)

	// The scripted good line surfaces as a normal entry event first...
	if ev := mustReceiveEvent(t, out, 500*time.Millisecond); ev.Kind != EventKindJsonlEntry {
		t.Fatalf("first Kind = %v, want EventKindJsonlEntry", ev.Kind)
	}
	// ...then the injected read fault surfaces end-to-end as one terminal
	// EventKindError before the stream closes.
	ev := mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindError {
		t.Fatalf("Kind = %v, want EventKindError", ev.Kind)
	}
	if ev.Source != EventSourceJsonl {
		t.Errorf("Source = %v, want EventSourceJsonl", ev.Source)
	}
	if !errors.Is(ev.Err, ErrJSONLTailRead) {
		t.Errorf("Err = %v, want errors.Is(_, ErrJSONLTailRead)", ev.Err)
	}
	assertEventChClosed(t, out, 500*time.Millisecond)
}

func TestMergeEvents_McpFailureBannerTransitions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap := &testSnap{}
	jsonlCh := make(chan JSONLEntry)
	out := make(chan Event, defaultEventBuffer)
	go mergeEvents(ctx, snap.Snapshot, zeroDims, neverQuiet, DefaultPTYQuietLimit, jsonlCh, out, DefaultPollInterval)

	// Phase 1: empty snap, no banner → no event within two ticks.
	select {
	case ev, ok := <-out:
		if ok {
			t.Fatalf("phase 1 got unexpected event %+v on empty snap", ev)
		}
		t.Fatalf("phase 1 channel closed unexpectedly")
	case <-time.After(2 * DefaultPollInterval):
	}

	// Phase 2: banner appears → McpFailureShown fires once.
	snap.Set([]byte("...1 MCP server failed · /mcp..."))
	ev := mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyMcpFailureShown {
		t.Errorf("phase 2 Kind = %v, want EventKindPtyMcpFailureShown", ev.Kind)
	}
	if ev.Source != EventSourcePty {
		t.Errorf("phase 2 Source = %v, want EventSourcePty", ev.Source)
	}
	if ev.Time.IsZero() {
		t.Errorf("phase 2 Time is zero, want non-zero wall-clock")
	}
	if ev.Modal != ModalClassUnknown {
		t.Errorf("phase 2 Modal = %q, want unset (zero)", ev.Modal)
	}

	// Phase 3: banner clears → McpFailureHidden fires.
	snap.Set([]byte("plain text"))
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyMcpFailureHidden {
		t.Errorf("phase 3 Kind = %v, want EventKindPtyMcpFailureHidden", ev.Kind)
	}
	if ev.Source != EventSourcePty {
		t.Errorf("phase 3 Source = %v, want EventSourcePty", ev.Source)
	}

	// Phase 4: banner reappears (different count, plural form) →
	// McpFailureShown fires again (rising-edge semantics).
	snap.Set([]byte("2 MCP servers failed"))
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyMcpFailureShown {
		t.Errorf("phase 4 Kind = %v, want EventKindPtyMcpFailureShown", ev.Kind)
	}

	cancel()
	assertEventChClosed(t, out, 500*time.Millisecond)
}

func TestMergeEvents_NetworkFailureTransitions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap := &testSnap{}
	jsonlCh := make(chan JSONLEntry)
	out := make(chan Event, defaultEventBuffer)
	go mergeEvents(ctx, snap.Snapshot, zeroDims, neverQuiet, DefaultPTYQuietLimit, jsonlCh, out, DefaultPollInterval)

	// Phase 1: empty snap → no event.
	select {
	case ev, ok := <-out:
		if ok {
			t.Fatalf("phase 1 got unexpected event %+v on empty snap", ev)
		}
		t.Fatalf("phase 1 channel closed unexpectedly")
	case <-time.After(2 * DefaultPollInterval):
	}

	// Phase 2: anchor appears → NetworkFailureShown fires once. #220: the
	// anchor is claude's real 2.1.199 status line, matched in the status region.
	snap.Set([]byte("Unable to connect to API (ConnectionRefused)"))
	ev := mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyNetworkFailureShown {
		t.Errorf("phase 2 Kind = %v, want EventKindPtyNetworkFailureShown", ev.Kind)
	}
	if ev.Source != EventSourcePty {
		t.Errorf("phase 2 Source = %v, want EventSourcePty", ev.Source)
	}
	if ev.Time.IsZero() {
		t.Errorf("phase 2 Time is zero, want non-zero wall-clock")
	}

	// Phase 3: anchor clears → NetworkFailureHidden fires.
	snap.Set([]byte("recovered"))
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyNetworkFailureHidden {
		t.Errorf("phase 3 Kind = %v, want EventKindPtyNetworkFailureHidden", ev.Kind)
	}

	// Phase 4: ANSI-wrapped reappearance → NetworkFailureShown again. The grid
	// render consumes the CSI and preserves the on-screen phrase.
	snap.Set([]byte("\x1b[31mUnable to connect to API (ConnectionRefused)\x1b[0m"))
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyNetworkFailureShown {
		t.Errorf("phase 4 Kind = %v, want EventKindPtyNetworkFailureShown", ev.Kind)
	}

	cancel()
	assertEventChClosed(t, out, 500*time.Millisecond)
}

func TestMergeEvents_BannerCoexistsWithIdleAndModal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap := &testSnap{}
	jsonlCh := make(chan JSONLEntry)
	out := make(chan Event, defaultEventBuffer)
	go mergeEvents(ctx, snap.Snapshot, zeroDims, neverQuiet, DefaultPTYQuietLimit, jsonlCh, out, DefaultPollInterval)

	// Phase 1: idle glyph AND banner in one snapshot. Expect two events
	// in the same tick: PtyIdle and McpFailureShown. Internal emission
	// order is not contract — collect both into a set before asserting.
	snap.Set([]byte("\xe2\x9d\xaf input ... 1 MCP server failed · /mcp"))
	got := map[EventKind]bool{}
	for i := 0; i < 2; i++ {
		ev := mustReceiveEvent(t, out, 500*time.Millisecond)
		if ev.Source != EventSourcePty {
			t.Errorf("phase 1 event %d Source = %v, want EventSourcePty", i, ev.Source)
		}
		got[ev.Kind] = true
	}
	if !got[EventKindPtyIdle] {
		t.Errorf("phase 1 missing EventKindPtyIdle, got %v", got)
	}
	if !got[EventKindPtyMcpFailureShown] {
		t.Errorf("phase 1 missing EventKindPtyMcpFailureShown, got %v", got)
	}

	// Phase 2: permission modal anchor + banner still present. Modal
	// axis flips Unknown → Permission (Shown fires); banner unchanged
	// (no Hidden); idle/thinking suppressed under modal. Expect exactly
	// one event: ModalShown(Permission). Critically, no
	// McpFailureHidden — modal does not suppress the banner axis.
	snap.Set([]byte("Do you want to proceed\r\n\xe2\x9d\xaf 1. Yes\r\n1 MCP server failed · /mcp"))
	ev := mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyModalShown {
		t.Errorf("phase 2 Kind = %v, want EventKindPtyModalShown", ev.Kind)
	}
	if ev.Modal != ModalClassPermission {
		t.Errorf("phase 2 Modal = %q, want %q", ev.Modal, ModalClassPermission)
	}
	select {
	case extra, ok := <-out:
		if !ok {
			t.Fatalf("phase 2 channel closed unexpectedly")
		}
		t.Fatalf("phase 2 unexpected follow-up event %+v (banner should still be shown)", extra)
	case <-time.After(2 * DefaultPollInterval):
	}

	// Phase 3: modal still up, banner gone. Expect exactly one event:
	// McpFailureHidden. Modal axis unchanged → no modal event.
	snap.Set([]byte("Do you want to proceed\r\n\xe2\x9d\xaf 1. Yes"))
	ev = mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyMcpFailureHidden {
		t.Errorf("phase 3 Kind = %v, want EventKindPtyMcpFailureHidden", ev.Kind)
	}

	cancel()
	assertEventChClosed(t, out, 500*time.Millisecond)
}

func TestEvents_TailJSONLErrorBubbles(t *testing.T) {
	s := &Session{buffer: NewBuffer(0)}
	missing := filepath.Join(t.TempDir(), "nonexistent", "x.jsonl")
	// The TailJSONL open/seek error fires before tr is dereferenced, so a
	// zero-opts tracker suffices to satisfy the new required parameter.
	tr := NewTracker(TrackerOpts{})
	ch, err := s.Events(context.Background(), missing, 0, tr)
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

// stallSnap is a mid-turn snapshot: no ❯ (so IsIdle is false → stall
// condition (a) holds) and no ✻/modal/banner anchors (so no other merge
// axis fires). It isolates the stall arm from the idle/thinking/modal/
// banner emissions.
var stallSnap = []byte("working")

func TestMergeEvents_StallRisingEdgeFiresOnceAndDoesNotRepeat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap := &testSnap{}
	jsonlCh := make(chan JSONLEntry)
	out := make(chan Event, defaultEventBuffer)
	const limit = 10 * time.Millisecond
	// The PTY has been quiet well beyond the limit on every tick.
	quietFor := func() time.Duration { return 5 * limit }
	go mergeEvents(ctx, snap.Snapshot, zeroDims, quietFor, limit, jsonlCh, out, DefaultPollInterval)

	// (a) not idle, (b) quietFor > limit, (c) no JSONL ever → stall.
	snap.Set(stallSnap)
	ev := mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindStallDetected {
		t.Fatalf("Kind = %v, want EventKindStallDetected", ev.Kind)
	}
	if ev.Source != EventSourcePty {
		t.Errorf("Source = %v, want EventSourcePty", ev.Source)
	}
	if ev.Time.IsZero() {
		t.Errorf("Time is zero, want non-zero wall-clock")
	}

	// Rising-edge: the condition still holds on every later tick, but the
	// event must not repeat while the stall persists.
	select {
	case extra, ok := <-out:
		if !ok {
			t.Fatalf("channel closed unexpectedly while stall persisted")
		}
		t.Fatalf("stall repeated: got %+v, want at most one EventKindStallDetected", extra)
	case <-time.After(5 * DefaultPollInterval):
	}

	cancel()
	assertEventChClosed(t, out, 500*time.Millisecond)
}

func TestMergeEvents_StallSuppressedAtIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap := &testSnap{}
	jsonlCh := make(chan JSONLEntry)
	out := make(chan Event, defaultEventBuffer)
	const limit = 10 * time.Millisecond
	quietFor := func() time.Duration { return 5 * limit }
	go mergeEvents(ctx, snap.Snapshot, zeroDims, quietFor, limit, jsonlCh, out, DefaultPollInterval)

	// Idle snapshot (❯, no ✻): (a) !idle is false even though the PTY is
	// quiet beyond the limit and no JSONL has arrived. The idle edge
	// fires once; the stall must never fire.
	snap.Set([]byte("\xe2\x9d\xaf input"))
	ev := mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindPtyIdle {
		t.Fatalf("Kind = %v, want EventKindPtyIdle", ev.Kind)
	}
	select {
	case extra, ok := <-out:
		if !ok {
			t.Fatalf("channel closed unexpectedly")
		}
		t.Fatalf("unexpected event %+v while idle, want no stall", extra)
	case <-time.After(5 * DefaultPollInterval):
	}

	cancel()
	assertEventChClosed(t, out, 500*time.Millisecond)
}

func TestMergeEvents_StallSuppressedByRecentPty(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap := &testSnap{}
	jsonlCh := make(chan JSONLEntry)
	out := make(chan Event, defaultEventBuffer)
	const limit = 50 * time.Millisecond
	// PTY bytes arrived recently — quiet window stays below the limit, so
	// (b) never holds.
	quietFor := func() time.Duration { return limit / 10 }
	go mergeEvents(ctx, snap.Snapshot, zeroDims, quietFor, limit, jsonlCh, out, DefaultPollInterval)

	snap.Set(stallSnap)
	select {
	case ev, ok := <-out:
		if !ok {
			t.Fatalf("channel closed unexpectedly")
		}
		t.Fatalf("unexpected event %+v with recent PTY bytes, want no stall", ev)
	case <-time.After(5 * DefaultPollInterval):
	}

	cancel()
	assertEventChClosed(t, out, 500*time.Millisecond)
}

func TestMergeEvents_StallSuppressedByRecentJsonlThenFires(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap := &testSnap{}
	jsonlCh := make(chan JSONLEntry, 1)
	out := make(chan Event, defaultEventBuffer)
	const limit = 200 * time.Millisecond
	// The PTY is quiet beyond the limit throughout; only JSONL progress
	// gates the stall in this scenario.
	quietFor := func() time.Duration { return 10 * limit }
	go mergeEvents(ctx, snap.Snapshot, zeroDims, quietFor, limit, jsonlCh, out, DefaultPollInterval)

	// (a) and (b) hold immediately. Record a JSONL arrival so (c) — no
	// JSONL within the window — is suppressed.
	snap.Set(stallSnap)
	jsonlCh <- JSONLEntry{Type: "assistant"}
	ev := mustReceiveEvent(t, out, 500*time.Millisecond)
	if ev.Kind != EventKindJsonlEntry {
		t.Fatalf("Kind = %v, want EventKindJsonlEntry", ev.Kind)
	}

	// Comfortably inside the window after the JSONL entry: stall suppressed.
	select {
	case extra, ok := <-out:
		if !ok {
			t.Fatalf("channel closed unexpectedly")
		}
		t.Fatalf("stall fired within the JSONL window: got %+v", extra)
	case <-time.After(limit / 2):
	}

	// Once the window elapses with no further JSONL, the stall fires.
	ev = mustReceiveEvent(t, out, 2*limit)
	if ev.Kind != EventKindStallDetected {
		t.Fatalf("Kind = %v, want EventKindStallDetected after window elapsed", ev.Kind)
	}

	cancel()
	assertEventChClosed(t, out, 500*time.Millisecond)
}

// TestClassifyRendersGridOncePerTick pins the #225 property: one classifier
// tick renders the snapshot through the terminal emulator exactly once, not
// once per predicate. gridForClassify is the sole render seam in the classify
// path; the test swaps it for a counting wrapper and asserts a single call.
// classify is driven synchronously (no merge goroutine), so the package-var
// swap races nothing. The snapshot exercises every axis (idle glyph, spinner,
// modal anchor, banner) so a predicate that rebuilt its own grid would push the
// count past one.
func TestClassifyRendersGridOncePerTick(t *testing.T) {
	orig := gridForClassify
	t.Cleanup(func() { gridForClassify = orig })
	var calls int
	gridForClassify = func(snap []byte, cols, rows int) *Grid {
		calls++
		return orig(snap, cols, rows)
	}
	snap := []byte("\xe2\x9d\xaf input ... 1 MCP server failed \xc2\xb7 /mcp")
	_ = classify(snap, 0, 0)
	if calls != 1 {
		t.Fatalf("classify rendered %d grids, want exactly 1", calls)
	}
}

// TestClassifyBehaviorUnchangedAfterSingleRender guards that threading one grid
// through the predicate variants keeps classify's per-axis output identical to
// calling the exported single-snapshot predicates independently. It is the
// behavior-equivalence net for the #225 refactor, over a spread of snapshots.
func TestClassifyBehaviorUnchangedAfterSingleRender(t *testing.T) {
	cases := map[string][]byte{
		"empty":       nil,
		"idle":        []byte("\xe2\x9d\xaf input"),
		"thinking":    []byte("\xe2\x9c\xbb Baked for 2s\n\xe2\x9d\xaf input"),
		"permission":  []byte("Do you want to proceed\r\n\xe2\x9d\xaf 1. Yes"),
		"mcp-banner":  []byte("\xe2\x9d\xaf input ... 1 MCP server failed \xc2\xb7 /mcp"),
		"network":     []byte("Unable to connect to API (ConnectionRefused)"),
		"idle+banner": []byte("\xe2\x9d\xaf input ... 2 MCP servers failed"),
	}
	for name, snap := range cases {
		t.Run(name, func(t *testing.T) {
			got := classify(snap, 0, 0)
			if got.idle != IsIdle(snap) {
				t.Errorf("idle = %v, want %v", got.idle, IsIdle(snap))
			}
			if got.thinking != IsThinking(snap) {
				t.Errorf("thinking = %v, want %v", got.thinking, IsThinking(snap))
			}
			if got.modal != DetectModalClass(snap) {
				t.Errorf("modal = %q, want %q", got.modal, DetectModalClass(snap))
			}
			if got.mcpFailure != HasMcpFailureBanner(snap) {
				t.Errorf("mcpFailure = %v, want %v", got.mcpFailure, HasMcpFailureBanner(snap))
			}
			if got.networkFailure != HasNetworkFailure(snap) {
				t.Errorf("networkFailure = %v, want %v", got.networkFailure, HasNetworkFailure(snap))
			}
		})
	}
}

func TestMergeEvents_StallReusesPtyQuietLimit(t *testing.T) {
	// quietFor reports a fixed quiet window on every tick. The stall
	// fires only when the configured ptyQuietLimit is below that window,
	// proving detection keys off the passed-in limit — not a hardcoded
	// constant (AC4).
	const quiet = 30 * time.Millisecond
	quietFor := func() time.Duration { return quiet }

	t.Run("limit below quiet window fires", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		snap := &testSnap{}
		jsonlCh := make(chan JSONLEntry)
		out := make(chan Event, defaultEventBuffer)
		go mergeEvents(ctx, snap.Snapshot, zeroDims, quietFor, quiet/3, jsonlCh, out, DefaultPollInterval)

		snap.Set(stallSnap)
		ev := mustReceiveEvent(t, out, 500*time.Millisecond)
		if ev.Kind != EventKindStallDetected {
			t.Fatalf("Kind = %v, want EventKindStallDetected (limit < quiet window)", ev.Kind)
		}
		cancel()
		assertEventChClosed(t, out, 500*time.Millisecond)
	})

	t.Run("limit above quiet window suppresses", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		snap := &testSnap{}
		jsonlCh := make(chan JSONLEntry)
		out := make(chan Event, defaultEventBuffer)
		go mergeEvents(ctx, snap.Snapshot, zeroDims, quietFor, 10*quiet, jsonlCh, out, DefaultPollInterval)

		snap.Set(stallSnap)
		select {
		case ev, ok := <-out:
			if !ok {
				t.Fatalf("channel closed unexpectedly")
			}
			t.Fatalf("unexpected event %+v: quiet window below limit must not fire", ev)
		case <-time.After(5 * DefaultPollInterval):
		}
		cancel()
		assertEventChClosed(t, out, 500*time.Millisecond)
	})
}

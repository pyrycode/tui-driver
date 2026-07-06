package main

import (
	"reflect"
	"testing"

	"github.com/pyrycode/tui-driver/pkg/tuidriver"
)

// TestClassify exercises the pure epistemic decision — one row per branch of
// the decision table in classify's doc comment. This is the AC's
// `main_test.go` assertion: it gives `make check` real teeth on the only
// logic that can run claude-free (the classification), while the live
// observation that feeds it runs on `make e2e`.
func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		obs  observation
		want finding
	}{
		{
			// B survived A's `1\r` → the `\r` did not commit it → atomic-safe.
			name: "parallel and modal B survived -> safe",
			obs:  observation{parallelToolUse: true, modalBObserved: true, modalsAnswered: 2, toolsExecuted: 2},
			want: findingSafe,
		},
		{
			// B vanished and a tool ran that the spike never approved → its
			// default was auto-accepted by the leaked `\r`.
			name: "parallel, B gone, extra tool executed -> dangerous",
			obs:  observation{parallelToolUse: true, modalBObserved: false, modalsAnswered: 1, toolsExecuted: 2},
			want: findingDangerous,
		},
		{
			// Boundary: queued but only as many tools executed as modals
			// answered → ambiguous, not dangerous.
			name: "parallel, B gone, tools == answered -> inconclusive",
			obs:  observation{parallelToolUse: true, modalBObserved: false, modalsAnswered: 1, toolsExecuted: 1},
			want: findingInconclusive,
		},
		{
			// Fewer tools executed than modals answered (e.g. B rejected /
			// turn cancelled) → ambiguous.
			name: "parallel, B gone, tools < answered -> inconclusive",
			obs:  observation{parallelToolUse: true, modalBObserved: false, modalsAnswered: 2, toolsExecuted: 1},
			want: findingInconclusive,
		},
		{
			// No parallel tool_use block → the two-queued-modal precondition
			// was never met. Undetermined, NOT safe.
			name: "no parallel tool use -> inconclusive",
			obs:  observation{parallelToolUse: false, modalBObserved: false, modalsAnswered: 1, toolsExecuted: 1},
			want: findingInconclusive,
		},
		{
			// Defensive: even if a later (sequential) modal was seen and
			// answered, without parallel queuing the run cannot conclude
			// anything about commit semantics under queuing. The
			// parallelToolUse gate wins over a stray modalBObserved.
			name: "sequential modal seen but no parallel use -> inconclusive",
			obs:  observation{parallelToolUse: false, modalBObserved: true, modalsAnswered: 2, toolsExecuted: 2},
			want: findingInconclusive,
		},
		{
			// Zero value: nothing observed → inconclusive.
			name: "zero observation -> inconclusive",
			obs:  observation{},
			want: findingInconclusive,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.obs); got != tc.want {
				t.Errorf("classify(%+v) = %q, want %q", tc.obs, got, tc.want)
			}
		})
	}
}

// asstToolUse builds an assistant JSONLEntry carrying one tool_use content
// block per name — the on-the-wire shape claude emits (one block per line).
func asstToolUse(msgID string, toolNames ...string) tuidriver.JSONLEntry {
	blocks := make([]tuidriver.ContentBlock, 0, len(toolNames))
	for _, n := range toolNames {
		blocks = append(blocks, tuidriver.ContentBlock{
			Type: "tool_use",
			Raw:  map[string]any{"name": n},
		})
	}
	return tuidriver.JSONLEntry{
		Type:    "assistant",
		Message: &tuidriver.EntryMessage{ID: msgID, Content: blocks},
	}
}

// TestAnalyzeToolUse pins the msg_id-grouped parallel-tool-use detection. The
// load-bearing row is "parallel calls across separate lines sharing one
// msg_id": that is exactly how claude serialises genuinely parallel tool calls
// (one content block per JSONL line, all under one msg_id —
// docs/knowledge/architecture/jsonl-layout.md), and a per-ENTRY count of
// tool_use blocks — the pre-fix implementation — would report `parallel=false`
// for it, leaving the spike permanently stuck at `inconclusive`. This test is
// the deterministic guard on the one measurement that gates every
// non-inconclusive verdict.
func TestAnalyzeToolUse(t *testing.T) {
	tests := []struct {
		name         string
		events       []tuidriver.JSONLEntry
		wantParallel bool
		wantDistinct []string
	}{
		{
			// THE regression case: two parallel tool calls, each its own JSONL
			// line, both under one msg_id. Per-entry counting sees 1+1 and
			// misses it; per-msg_id summing sees 2.
			name: "parallel across separate lines, shared msg_id -> parallel",
			events: []tuidriver.JSONLEntry{
				asstToolUse("msgA", "Bash"),
				asstToolUse("msgA", "Read"),
			},
			wantParallel: true,
			wantDistinct: []string{"Bash", "Read"},
		},
		{
			// Sequential: two tools, distinct msg_ids. No single message has 2.
			name: "sequential across distinct msg_ids -> not parallel",
			events: []tuidriver.JSONLEntry{
				asstToolUse("msgA", "Bash"),
				asstToolUse("msgB", "Read"),
			},
			wantParallel: false,
			wantDistinct: []string{"Bash", "Read"},
		},
		{
			// Belt: two tool_use blocks in ONE entry under one msg_id (should
			// claude ever emit that shape) is still parallel — the per-msg_id
			// sum subsumes the old per-entry check.
			name: "two blocks in one entry, one msg_id -> parallel",
			events: []tuidriver.JSONLEntry{
				asstToolUse("msgA", "Bash", "Read"),
			},
			wantParallel: true,
			wantDistinct: []string{"Bash", "Read"},
		},
		{
			name: "single tool_use -> not parallel",
			events: []tuidriver.JSONLEntry{
				asstToolUse("msgA", "Bash"),
			},
			wantParallel: false,
			wantDistinct: []string{"Bash"},
		},
		{
			// Non-assistant and message-less entries contribute nothing; the
			// two tool_use lines under msgA still make it parallel.
			name: "user and message-less entries ignored",
			events: []tuidriver.JSONLEntry{
				{Type: "user", Message: &tuidriver.EntryMessage{ID: "u1", Content: []tuidriver.ContentBlock{{Type: "tool_result"}}}},
				{Type: "assistant", Message: nil},
				asstToolUse("msgA", "Bash"),
				asstToolUse("msgA", "Read"),
			},
			wantParallel: true,
			wantDistinct: []string{"Bash", "Read"},
		},
		{
			// Text-only assistant message: no tool_use, not parallel, no names.
			name: "text-only assistant -> not parallel",
			events: []tuidriver.JSONLEntry{
				{Type: "assistant", Message: &tuidriver.EntryMessage{ID: "msgA", Content: []tuidriver.ContentBlock{{Type: "text", Raw: map[string]any{"text": "hi"}}}}},
			},
			wantParallel: false,
			wantDistinct: nil,
		},
		{
			name:         "no events -> not parallel",
			events:       nil,
			wantParallel: false,
			wantDistinct: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotParallel, gotDistinct := analyzeToolUse(tc.events)
			if gotParallel != tc.wantParallel {
				t.Errorf("analyzeToolUse parallel = %v, want %v", gotParallel, tc.wantParallel)
			}
			if !reflect.DeepEqual(gotDistinct, tc.wantDistinct) {
				t.Errorf("analyzeToolUse distinct = %v, want %v", gotDistinct, tc.wantDistinct)
			}
		})
	}
}

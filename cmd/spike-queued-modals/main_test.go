package main

import "testing"

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

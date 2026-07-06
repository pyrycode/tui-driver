package tuidriver

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestAnswerModal exercises the unexported driver against fake answer +
// dismissed seams (no live PTY): each supported class's answer-and-confirm path
// in both the dismissed (success) and still-present (error) outcomes, plus the
// unsupported-class / invalid-choice / send-failure reject branches. Mirrors
// deliver_test.go's TestDeliverPrompt_* boolean-seam shape.
func TestAnswerModal(t *testing.T) {
	tests := []struct {
		name string
		opts AnswerModalOpts
		// answerErr is returned by the fake answer seam (nil = success).
		answerErr error
		// dismissed is what the fake dismissed seam reports.
		dismissed bool
		// wantErr is a substring the returned error must contain; "" = no error.
		wantErr string
		// wantAnswerArg is the choice the answer seam must be called with once;
		// "" means the answer seam must NOT be called.
		wantAnswerArg string
		// wantDismissed is whether the dismissed seam should be consulted.
		wantDismissed bool
	}{
		{
			name:          "permission dismissed (success)",
			opts:          AnswerModalOpts{Class: ModalClassPermission, Choice: 2},
			dismissed:     true,
			wantAnswerArg: "2",
			wantDismissed: true,
		},
		{
			name:          "permission still present (error)",
			opts:          AnswerModalOpts{Class: ModalClassPermission, Choice: 2},
			dismissed:     false,
			wantErr:       "still present",
			wantAnswerArg: "2",
			wantDismissed: true,
		},
		{
			// Choice 1 sends the same "1\r" as AcceptTrust.
			name:          "trust-folder dismissed (success)",
			opts:          AnswerModalOpts{Class: ModalClassTrustFolder, Choice: 1},
			dismissed:     true,
			wantAnswerArg: "1",
			wantDismissed: true,
		},
		{
			name:          "trust-folder still present (error)",
			opts:          AnswerModalOpts{Class: ModalClassTrustFolder, Choice: 1},
			dismissed:     false,
			wantErr:       "still present",
			wantAnswerArg: "1",
			wantDismissed: true,
		},
		{
			// A number-select keystroke must not reach a non-number-select class.
			name:    "unsupported class sends nothing",
			opts:    AnswerModalOpts{Class: ModalClassMCP, Choice: 1},
			wantErr: "unsupported",
		},
		{
			// Index is 1-based; never send a "0\r" to a live claude.
			name:    "invalid choice sends nothing",
			opts:    AnswerModalOpts{Class: ModalClassPermission, Choice: 0},
			wantErr: "1-based",
		},
		{
			name:          "send failure is not confirmed",
			opts:          AnswerModalOpts{Class: ModalClassPermission, Choice: 1},
			answerErr:     os.ErrClosed,
			wantErr:       "send keystroke",
			wantAnswerArg: "1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var answerCalls []string
			var dismissedCalled bool
			deps := answerDeps{
				answer: func(c string) error {
					answerCalls = append(answerCalls, c)
					return tt.answerErr
				},
				dismissed: func(time.Duration) bool {
					dismissedCalled = true
					return tt.dismissed
				},
			}

			err := answerModal(tt.opts, deps)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("answerModal: unexpected error %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("answerModal err = %v, want it to contain %q", err, tt.wantErr)
			}

			if tt.wantAnswerArg == "" {
				if len(answerCalls) != 0 {
					t.Errorf("answer seam called %v, want it NOT called", answerCalls)
				}
			} else if len(answerCalls) != 1 || answerCalls[0] != tt.wantAnswerArg {
				t.Errorf("answer calls = %v, want exactly one call with %q", answerCalls, tt.wantAnswerArg)
			}

			if dismissedCalled != tt.wantDismissed {
				t.Errorf("dismissed seam consulted = %v, want %v", dismissedCalled, tt.wantDismissed)
			}
		})
	}
}

// TestModalDismissed drives the confirm-by-re-read poll by injecting the
// snapshot sequence (no live PTY), mirroring deliver_test.go's
// TestPromptDidCommit real-buffer poll. Reuses #146's committed fixtures as the
// "modal still present" re-read; idle bytes are the dismissed re-read.
func TestModalDismissed(t *testing.T) {
	t.Run("dismissed immediately when class is gone", func(t *testing.T) {
		snap := func() []byte { return []byte("idle TUI") } // DetectModalClass → Unknown
		start := time.Now()
		if !modalDismissed(context.Background(), ModalClassPermission, snap, time.Second) {
			t.Error("modalDismissed = false, want true (no modal on screen)")
		}
		if time.Since(start) >= answerConfirmPoll {
			t.Errorf("returned in %v, want it to short-circuit before the first poll tick", time.Since(start))
		}
	})

	t.Run("still present times out to false", func(t *testing.T) {
		fixture := loadFixture(t, "permission-snapshot.bin")
		snap := func() []byte { return fixture }
		start := time.Now()
		if modalDismissed(context.Background(), ModalClassPermission, snap, 60*time.Millisecond) {
			t.Error("modalDismissed = true, want false (modal still present)")
		}
		if time.Since(start) < 50*time.Millisecond {
			t.Errorf("returned in %v, want it to honor the timeout", time.Since(start))
		}
	})

	t.Run("history-only permission anchor counts as dismissed", func(t *testing.T) {
		// #161: the answered permission modal's anchor ("Do you want to
		// proceed") lingers in scrolled-up transcript history after dismissal.
		// Pre-#152 the whole-buffer substring match reported the class still
		// present, firing a spurious "still present" error that re-drove an
		// already-committed turn. Post-#152 DetectModalClass region-scopes the
		// permission anchor to the bottom overlay window, so an anchor pushed
		// above that window by transcript body renders as Unknown → dismissed.
		// Same forged-above-region shape as modal_test.go's
		// TestDetectModalClassPermissionRegion, asserted one layer up at the
		// dismissal seam (returns dismissed, not the detection-level Unknown).
		body := strings.Repeat("transcript body line\r\n", 25)
		forged := []byte("Do you want to proceed?\r\n" + body)
		// Sanity: the anchor IS in the buffer — a naive whole-buffer match would
		// forge "still present". Region-scoping is exactly what rejects it.
		if !strings.Contains(string(forged), "Do you want to proceed") {
			t.Fatal("fixture lost the forged phrase")
		}
		snap := func() []byte { return forged }
		start := time.Now()
		if !modalDismissed(context.Background(), ModalClassPermission, snap, time.Second) {
			t.Error("modalDismissed = false, want true (anchor only in above-region history)")
		}
		if time.Since(start) >= answerConfirmPoll {
			t.Errorf("returned in %v, want it to short-circuit before the first poll tick", time.Since(start))
		}
	})

	t.Run("different modal counts as dismissed", func(t *testing.T) {
		fixture := loadFixture(t, "trust-folder-snapshot.bin")
		snap := func() []byte { return fixture }
		// The permission modal the operator answered is gone; a trust-folder
		// modal that popped after is a fresh parse→answer cycle for the daemon.
		if !modalDismissed(context.Background(), ModalClassPermission, snap, time.Second) {
			t.Error("modalDismissed = false, want true (trust-folder ≠ permission)")
		}
	})

	t.Run("ctx cancelled returns false promptly", func(t *testing.T) {
		fixture := loadFixture(t, "permission-snapshot.bin")
		snap := func() []byte { return fixture }
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if modalDismissed(ctx, ModalClassPermission, snap, 5*time.Second) {
			t.Error("modalDismissed = true, want false (ctx cancelled)")
		}
	})
}

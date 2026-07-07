package main

import "testing"

// TestFormatAutoRespondObservation pins the single ^OBSERVED line Probe 2 emits
// on claude 2.1.199 (#180). The e2e-runner gates spike-permission on
// ^OBSERVED, so the leading marker and the field set are load-bearing: they
// must let a reader distinguish "keystroke not accepted" (modal_cleared=false)
// from "no end_turn signal" (modal_cleared=true, end_turn=false) from
// "completed but readiness predicate unmet" (end_turn=true, idle/quiet false)
// without a live re-run.
func TestFormatAutoRespondObservation(t *testing.T) {
	tests := []struct {
		name string
		obs  autoRespondObservation
		want string
	}{
		{
			name: "clean completion within settle window",
			obs: autoRespondObservation{
				approveHex:   "31 0d",
				modalCleared: true,
				endTurn:      true,
				idlePresent:  true,
				ptyQuiet:     true,
				snapshotPath: "/tmp/spike-permission-probe2-1/post-approve-pty.bin",
			},
			want: "OBSERVED: spike-permission post-approve modal_cleared=true end_turn=true idle_present=true pty_quiet=true approve_keystroke=31 0d snapshot=/tmp/spike-permission-probe2-1/post-approve-pty.bin",
		},
		{
			name: "post-approve hang: modal cleared, never reached end_turn",
			obs: autoRespondObservation{
				approveHex:   "31 0d",
				modalCleared: true,
				endTurn:      false,
				idlePresent:  false,
				ptyQuiet:     false,
				snapshotPath: "/tmp/x.bin",
			},
			want: "OBSERVED: spike-permission post-approve modal_cleared=true end_turn=false idle_present=false pty_quiet=false approve_keystroke=31 0d snapshot=/tmp/x.bin",
		},
		{
			name: "keystroke not accepted: nothing cleared, snapshot persist failed",
			obs: autoRespondObservation{
				approveHex:   "31 0d",
				modalCleared: false,
				endTurn:      false,
				idlePresent:  false,
				ptyQuiet:     false,
				snapshotPath: "",
			},
			want: "OBSERVED: spike-permission post-approve modal_cleared=false end_turn=false idle_present=false pty_quiet=false approve_keystroke=31 0d snapshot=",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatAutoRespondObservation(tc.obs); got != tc.want {
				t.Errorf("formatAutoRespondObservation() =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

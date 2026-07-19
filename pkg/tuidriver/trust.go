package tuidriver

import "strings"

// trustDialogLookahead bounds how many rows below the "Quick safety check"
// header the pointer-marked option row may sit and still count as the real
// dialog. Claude 2.1.199 renders the header, a wrapped second header line, an
// explanatory line, a "Security guide" link, and blank separators before the
// "❯ 1. Yes, I trust this folder" option row — 7 rows below the header at 120
// cols (trust-folder-2.1.199-snapshot.bin). #300 raised this from 3, which was
// sized for the compact pre-2.1.199 layout (trust-folder-snapshot.bin, option
// on the next row) and silently missed the current dialog, so the startup
// safety net never fired. 8 covers the measured gap of 7 with one row of slack;
// it stays far tighter than a whole-panel match, so the header-plus-nearby-
// option-row co-signal that rejects a bare header quotation (#219) still holds.
const trustDialogLookahead = 8

// gridHasTrustDialog reports whether g renders claude's real trust-folder
// dialog: the "Quick safety check" header AND, within the next
// trustDialogLookahead rows, a pointer-marked numbered option row (the shape
// of "❯ 1. Yes, I trust this folder").
//
// The structural co-signal is the #219 fix. Before it, both entry points
// classified the trust modal from the header phrase matched anywhere on the
// grid, so a ticket body or a source file that merely quoted the header
// classified as the modal. Because the runner treats a mid-run trust detection
// as fatal, such a quotation aborted the run mid-turn (live: #217, and the
// 2026-07-06 developer abort). Requiring the pointer-marked option row directly
// below the header keeps the real startup dialog matching — it always renders
// that shape — while rejecting a bare quotation, which does not.
//
// modalOptionRe (permission.go) is the shared option-row shape; group 1 is the
// ❯ marker, required here so only the selected (pointer-marked) option row
// qualifies. Leading indentation is trimmed first, so an unselected "  2. No…"
// row — which trims to "2. No…" with no ❯ — does not satisfy it.
//
// Both HasTrustModal (the startup safety net) and the DetectModalClass trust
// arm call this, so the two can never drift (the #163 invariant).
func gridHasTrustDialog(g *Grid) bool {
	rows := g.Rows()
	for i, row := range rows {
		if !strings.Contains(row, string(anchorTrustHeaderSpaced)) {
			continue
		}
		end := min(i+1+trustDialogLookahead, len(rows))
		for j := i + 1; j < end; j++ {
			if m := modalOptionRe.FindStringSubmatch(strings.TrimLeft(rows[j], " ")); m != nil && m[1] != "" {
				return true
			}
		}
	}
	return false
}

// HasTrustModal reports whether snap shows claude's trust-folder dialog
// ("Quick safety check: Is this a project you created or one you trust?").
// Without explicit detection the idle predicate matches inside the modal
// (claude renders ❯ in the dialog's input field), so a consumer would type its
// first prompt into the modal and time out opaquely. This is the STARTUP trust
// safety net: Readiness.TrustModal (ready.go) is set from it, and the consumer
// aborts the run when it fires.
//
// It matches on the RENDERED SCREEN GRID via gridHasTrustDialog, which requires
// both the header anchor (space-preserved anchorTrustHeaderSpaced) AND a
// pointer-marked numbered option row directly below it. #152 moved this and
// DetectModalClass onto the grid, keyed on the space-preserved header, so the
// two agree (before #163 the startup match scanned StripANSI(snap) for a
// space-stripped "Quicksafetycheck" and drifted from the grid path). #219 then
// added the option-row co-signal to both, because the header phrase alone
// classified any on-screen quotation of it as the modal — a fatal false
// positive under the runner's abort policy.
//
// The header-plus-shape requirement also excludes the post-accept confirmation
// ("Yes, I trust this folder ✔") that lingers in the buffer after acceptance:
// it carries no "Quick safety check" header, so the post-accept wait still
// clears (loop 3 C-1, 2026-05-18).
func HasTrustModal(snap []byte) bool {
	return gridHasTrustDialog(NewGrid(snap, 0, 0))
}

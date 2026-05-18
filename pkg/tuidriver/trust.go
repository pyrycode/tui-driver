package tuidriver

import "bytes"

// TrustModalAnchor is the literal that uniquely identifies claude's
// trust-folder modal in a stripped PTY snapshot. claude renders the dialog
// as "Quick safety check: Is this a project you created or one you trust?"
// on first use of a previously-unseen cwd. Anchored on the space-stripped
// header (Bash-style CSI cursor-forward stripping eats inter-word spaces in
// some renderings).
//
// Critical: this is NOT in the post-accept confirmation text ("Yes, I trust
// this folder ✔") that lands in the rolling buffer after acceptance. Loop 3
// C-1's initial broader predicate matched both, breaking the post-accept
// wait. The header-only anchor avoids that.
const TrustModalAnchor = "Quicksafetycheck"

// HasTrustModal reports whether snap contains claude's trust-folder dialog.
// Without explicit detection the spike's idle predicate matches inside the
// modal (claude renders ❯ in the dialog's input field), so consumers would
// type their first prompt into the modal and time out opaquely.
//
// Loop 2 B-5 (2026-05-18) introduced the predicate; loop 3 C-1 (2026-05-18)
// refined it to header-only matching.
func HasTrustModal(snap []byte) bool {
	return bytes.Contains(StripANSI(snap), []byte(TrustModalAnchor))
}

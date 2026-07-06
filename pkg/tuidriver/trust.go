package tuidriver

// HasTrustModal reports whether snap shows claude's trust-folder dialog
// ("Quick safety check: Is this a project you created or one you trust?").
// Without explicit detection the idle predicate matches inside the modal
// (claude renders ❯ in the dialog's input field), so a consumer would type its
// first prompt into the modal and time out opaquely. This is the STARTUP trust
// safety net: Readiness.TrustModal (ready.go) is set from it, and the consumer
// aborts the run when it fires.
//
// It matches the trust header on the RENDERED SCREEN GRID, keyed on the
// space-preserved anchorTrustHeaderSpaced ("Quick safety check") — the same
// anchor and whole-grid path DetectModalClass uses to classify
// ModalClassTrustFolder (#152). Before #163 this scanned StripANSI(snap) for a
// space-stripped "Quicksafetycheck": that only matched a rendering where CSI
// cursor-forward moves had eaten the inter-word spaces, and MISSED a header
// rendered with literal spaces (which StripANSI leaves as "Quick safety check").
// The grid reconstructs the on-screen spacing regardless of how claude encoded
// it, so matching it is both encoding-independent and consistent with the
// modal-class detector — the two no longer drift (the gap #163 closed).
//
// The header-only anchor deliberately excludes the post-accept confirmation
// ("Yes, I trust this folder ✔") that lingers in the buffer after acceptance:
// that text carries no "Quick safety check" header, so the post-accept wait
// still clears (loop 3 C-1, 2026-05-18).
func HasTrustModal(snap []byte) bool {
	return gridContains(NewGrid(snap, 0, 0), anchorTrustHeaderSpaced)
}

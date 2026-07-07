package tuidriver

import (
	"testing"
)

// mcpEnablementDialog reconstructs the claude 2.1.199 startup MCP-enablement
// modal from the 2026-07-03 probe recording (ANSI-stripped) in the session log
// and [[2.1.199 MCP-Enablement Modal — Fix Design]]. It is a checkbox
// multiselect, not a number-select, so its option rows are `❯ [✔] name` rather
// than `❯ 1. …`. This is a reconstruction, not a live capture: the modal is a
// one-time-per-project gate, so it cannot be re-opened in an already-enabled
// repo. \r\n so vt10x renders flat rows.
func mcpEnablementDialog() []byte {
	return gridRows(
		"2 new MCP servers found in this project",
		"Select any you wish to enable.",
		"",
		"❯ [✔] smart-connections",
		"  [✔] qmd",
		"Space to select · Enter to confirm · Esc to reject all",
	)
}

// TestGridHasSelectionDialogPositiveControls: the committed trust and permission
// fixtures are real blocking dialogs, so the structural shape predicate must
// recognise them (#224 AC — the known-dialog positive controls).
func TestGridHasSelectionDialogPositiveControls(t *testing.T) {
	for _, f := range []string{"trust-folder-snapshot.bin", "permission-snapshot.bin"} {
		snap := loadFixture(t, f)
		if !gridHasSelectionDialog(NewGrid(snap, 0, 0)) {
			t.Errorf("%s: gridHasSelectionDialog = false, want true (real dialog has the shape)", f)
		}
	}
}

// TestHasUnknownDialogDetectsMcpEnablement is the core #224 case: a novel dialog
// that DetectModalClass does not recognise, but whose selection shape is present,
// must surface as an unknown dialog. The checkbox option row `❯ [✔] …` is the
// shape. This is the 2.1.199 startup modal that hung the runner on 2026-07-03.
func TestHasUnknownDialogDetectsMcpEnablement(t *testing.T) {
	snap := mcpEnablementDialog()
	if got := DetectModalClass(snap); got != ModalClassUnknown {
		t.Fatalf("DetectModalClass(mcp-enablement) = %q, want Unknown (no anchor should match)", got)
	}
	if !HasUnknownDialog(snap) {
		t.Errorf("HasUnknownDialog(mcp-enablement) = false, want true")
	}
	if !isUnexpectedStartupModal(snap) {
		t.Errorf("isUnexpectedStartupModal(mcp-enablement) = false, want true (WaitReady must fail loudly on it)")
	}
}

// TestHasUnknownDialogRejectsContentForgery: the negative-suite content forms —
// a transcript echo and a source read of a dialog anchor — carry no
// pointer-marked option row, so the shape predicate must reject them and
// HasUnknownDialog must stay false. Otherwise the #224 net would re-introduce the
// forgery class it exists to catch.
func TestHasUnknownDialogRejectsContentForgery(t *testing.T) {
	for _, anchor := range []string{
		"Quick safety check",
		"Do you want to proceed",
		"Select any you wish to enable",
		"new MCP servers found in this project",
	} {
		for _, f := range forgedBodyForms(anchor) {
			t.Run(anchor+"/"+f.name, func(t *testing.T) {
				if gridHasSelectionDialog(NewGrid(f.snap, 0, 0)) {
					t.Errorf("%s as %s content matched the dialog shape, want no match", anchor, f.name)
				}
				if HasUnknownDialog(f.snap) {
					t.Errorf("%s as %s content set HasUnknownDialog, want false", anchor, f.name)
				}
			})
		}
	}
	// A prose line that merely mentions options, and the idle input line, must
	// not match: neither begins with the pointer glyph plus an option token.
	for _, in := range [][]byte{
		[]byte("the dialog offers 1. Yes and 2. No as choices\r\n"),
		[]byte("❯ Try \"refactor <filepath>\"\r\n"),
		[]byte("❯ /model to switch models\r\n"),
	} {
		if gridHasSelectionDialog(NewGrid(in, 0, 0)) {
			t.Errorf("gridHasSelectionDialog(%q) = true, want false", in)
		}
	}
}

// TestHasUnknownDialogFalseForKnownClasses: a KNOWN dialog is the consumer's own
// to handle, so HasUnknownDialog must stay false for it even though it carries
// the selection shape. This pins the "unknown only" contract and the "no change
// to known-class outcomes" AC.
func TestHasUnknownDialogFalseForKnownClasses(t *testing.T) {
	for _, f := range []string{
		"trust-folder-snapshot.bin",
		"permission-snapshot.bin",
		"model-select-snapshot.bin",
		"ask-user-question-screen-snapshot.bin",
		"mcp-snapshot.bin",
	} {
		snap := loadFixture(t, f)
		if HasUnknownDialog(snap) {
			t.Errorf("%s: HasUnknownDialog = true, want false (known class, not unknown)", f)
		}
	}
}

// TestHasUnknownDialogIdleAndEmpty: no dialog shape on an idle screen or empty
// input, so no unknown dialog.
func TestHasUnknownDialogIdleAndEmpty(t *testing.T) {
	idle := []byte("\xe2\x9d\xaf Try \"refactor <filepath>\"\r\n")
	if HasUnknownDialog(idle) {
		t.Errorf("HasUnknownDialog(idle) = true, want false")
	}
	if HasUnknownDialog(nil) || HasUnknownDialog([]byte{}) {
		t.Errorf("HasUnknownDialog(empty) = true, want false")
	}
	if isUnexpectedStartupModal(idle) {
		t.Errorf("isUnexpectedStartupModal(idle) = true, want false")
	}
}

// TestReadinessUnexpectedModalTrustExcluded: the trust modal is surfaced by
// Readiness.TrustModal (report-only, driver decides), so isUnexpectedStartupModal
// must stay false for it — WaitReady must not fail loudly on trust — even though
// trust carries the selection shape.
func TestReadinessUnexpectedModalTrustExcluded(t *testing.T) {
	snap := loadFixture(t, "trust-folder-snapshot.bin")
	if isUnexpectedStartupModal(snap) {
		t.Errorf("isUnexpectedStartupModal(trust) = true, want false (surfaced by TrustModal, not a loud error)")
	}
}

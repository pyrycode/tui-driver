package tuidriver

import (
	"os"
	"path/filepath"
	"testing"
)

// TestHasTrustModalDetectsRealFixture pins the real trust-folder snapshot:
// claude renders the dialog header as "Quick safety check" (spaced) on the
// screen grid, and HasTrustModal must detect it. This is the startup safety net
// Readiness.TrustModal (ready.go) rides on. #163 also requires HasTrustModal and
// DetectModalClass to AGREE on the real modal — their prior disagreement
// (grid+spaced class detection vs a space-stripped startup match) was the gap.
func TestHasTrustModalDetectsRealFixture(t *testing.T) {
	snap, err := os.ReadFile(filepath.Join("testdata", "trust-folder-snapshot.bin"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if !HasTrustModal(snap) {
		t.Errorf("HasTrustModal(trust-folder-snapshot.bin) = false, want true")
	}
	if got := DetectModalClass(snap); got != ModalClassTrustFolder {
		t.Errorf("DetectModalClass(trust-folder-snapshot.bin) = %q, want %q — HasTrustModal and DetectModalClass must agree", got, ModalClassTrustFolder)
	}
}

// TestHasTrustModalDetectsLiteralSpacedHeader is the #163 RED→GREEN driver.
// A trust header that renders with LITERAL spaces (not CSI cursor-forward
// moves) strips to "Quick safety check" under StripANSI, which the pre-#163
// space-stripped anchor "Quicksafetycheck" missed entirely. Matching the
// rendered grid catches the header regardless of how claude encoded the spacing.
func TestHasTrustModalDetectsLiteralSpacedHeader(t *testing.T) {
	snap := []byte("Quick safety check: Is this a project you created or one you trust?\r\n" +
		"❯ 1. Yes, I trust this folder\r\n" +
		"  2. No, I selected this folder by mistake\r\n" +
		"(Esc to cancel)\r\n")
	if !HasTrustModal(snap) {
		t.Errorf("HasTrustModal missed a literal-spaced trust header: %q", snap)
	}
}

// TestHasTrustModalMatchesControlSequenceWrappedAnchor: CSI/SGR color noise
// around the header must not block detection. The grid render consumes the
// control sequences and preserves the on-screen spacing. Sibling of modal.go's
// TestDetectModalClassMatchesControlSequenceWrappedAnchor.
func TestHasTrustModalMatchesControlSequenceWrappedAnchor(t *testing.T) {
	snap := []byte("\x1b[1m\x1b[34mQuick safety check\x1b[0m: Is this a project you trust?\r\n")
	if !HasTrustModal(snap) {
		t.Errorf("HasTrustModal missed a CSI-wrapped header: %q", snap)
	}
}

// TestHasTrustModalIgnoresPostAcceptConfirmation: the acceptance confirmation
// ("Yes, I trust this folder ✔") lingers in the buffer after the modal clears.
// It carries no "Quick safety check" header, so it must NOT match — else the
// post-accept wait never satisfies (loop 3 C-1, 2026-05-18).
func TestHasTrustModalIgnoresPostAcceptConfirmation(t *testing.T) {
	snap := []byte("Yes, I trust this folder ✔\r\nReady for input\r\n")
	if HasTrustModal(snap) {
		t.Errorf("HasTrustModal falsely matched post-accept confirmation: %q", snap)
	}
}

// TestHasTrustModalEmpty: no snapshot, no modal.
func TestHasTrustModalEmpty(t *testing.T) {
	if HasTrustModal(nil) {
		t.Errorf("HasTrustModal(nil) = true, want false")
	}
	if HasTrustModal([]byte{}) {
		t.Errorf("HasTrustModal([]byte{}) = true, want false")
	}
}

func TestStripANSIString(t *testing.T) {
	in := "\x1b[1m\x1b[34mhello\x1b[0m world"
	got := StripANSIString(in)
	want := "hello world"
	if got != want {
		t.Errorf("StripANSIString(%q) = %q, want %q", in, got, want)
	}
}

package tuidriver

import (
	"bytes"
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
//
// #219: the fixture now carries the pointer-marked option row too, because the
// header alone no longer classifies (see the forgery test below). This test
// still pins the thing it always pinned — SGR noise around the header does not
// block detection — just on a structurally complete dialog.
func TestHasTrustModalMatchesControlSequenceWrappedAnchor(t *testing.T) {
	snap := []byte("\x1b[1m\x1b[34mQuick safety check\x1b[0m: Is this a project you trust?\r\n" +
		"❯ 1. Yes, I trust this folder\r\n" +
		"  2. No, I selected this folder by mistake\r\n")
	if !HasTrustModal(snap) {
		t.Errorf("HasTrustModal missed a CSI-wrapped header: %q", snap)
	}
}

// TestHasTrustModalRejectsHeaderQuotedAsContent is the #219 RED→GREEN driver.
// The trust header rendered as ordinary screen content — a transcript echo or a
// source-file read — must NOT classify as the trust modal. Before #219 the
// header phrase matched anywhere on the grid was enough, so a ticket body or a
// source file that merely quoted "Quick safety check" fired the detector; the
// consumer treats a mid-run trust detection as fatal, so the quotation aborted
// the run (live: ticket 217, and the 2026-07-06 developer abort). The fix
// requires a pointer-marked numbered option row directly below the header —
// the real dialog's shape, which prose content does not reproduce.
//
// \r\n so vt10x renders flat rows (the #150 grid fixture lesson).
func TestHasTrustModalRejectsHeaderQuotedAsContent(t *testing.T) {
	// (a) Transcript-echo form: the header quoted inside a prose line, no
	// dialog-shaped option row anywhere below it.
	echo := []byte("user: what does the folder-trust prompt say?\r\n" +
		"assistant: it reads \"Quick safety check: Is this a project you created or one you trust?\"\r\n" +
		"and then offers a yes/no choice.\r\n")
	if !bytes.Contains(echo, anchorTrustHeaderSpaced) {
		t.Fatal("fixture lost the header phrase — the forgery contrast is void")
	}
	if HasTrustModal(echo) {
		t.Errorf("transcript-echo forgery: HasTrustModal = true, want false")
	}

	// (b) Source-read form: the anchor inside a quoted Go string, with a
	// line-number gutter, as an agent reading permission.go would see it.
	source := []byte("52\t// anchorTrustHeaderSpaced is the trust-folder header line as it renders\r\n" +
		"54\tvar anchorTrustHeaderSpaced = []byte(\"Quick safety check\")\r\n")
	if !bytes.Contains(source, anchorTrustHeaderSpaced) {
		t.Fatal("fixture lost the header phrase — the forgery contrast is void")
	}
	if HasTrustModal(source) {
		t.Errorf("source-read forgery: HasTrustModal = true, want false")
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

package tuidriver

import "testing"

func TestHasTrustModalMatchesHeader(t *testing.T) {
	// Realistic-ish stripped form of claude's trust dialog header.
	snap := []byte("Quicksafetycheck\nIsthisaprojectyoucreatedoronethatyoutrust?")
	if !HasTrustModal(snap) {
		t.Errorf("HasTrustModal missed the header in: %q", snap)
	}
}

func TestHasTrustModalIgnoresPostAcceptConfirmation(t *testing.T) {
	// Loop 3 C-1 regression: the confirmation lingers in the rolling buffer
	// after acceptance. It must NOT match the trust predicate or the
	// post-accept wait never satisfies.
	snap := []byte("Yes, I trust this folder ✔\nReadyforinput")
	if HasTrustModal(snap) {
		t.Errorf("HasTrustModal falsely matched post-accept confirmation: %q", snap)
	}
}

func TestHasTrustModalStripsANSIBeforeMatching(t *testing.T) {
	// Anchor wrapped in CSI color codes — the strip should expose it.
	snap := []byte("\x1b[1m\x1b[34mQuicksafetycheck\x1b[0m")
	if !HasTrustModal(snap) {
		t.Errorf("HasTrustModal missed anchor wrapped in CSI: %q", snap)
	}
}

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

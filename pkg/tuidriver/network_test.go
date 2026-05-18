package tuidriver

import "testing"

func TestHasNetworkFailureEmpty(t *testing.T) {
	if HasNetworkFailure(nil) {
		t.Errorf("HasNetworkFailure(nil) = true, want false")
	}
	if HasNetworkFailure([]byte("idle TUI bytes")) {
		t.Errorf("HasNetworkFailure(idle) = true, want false")
	}
}

func TestHasNetworkFailureFailedToOpenSocket(t *testing.T) {
	// Captured 2026-05-18 from spawning claude with bogus
	// ANTHROPIC_BASE_URL=https://broken.invalid:9999 — claude rendered
	// the spinner indefinitely AND emitted FailedToOpenSocket in the
	// PTY stream.
	cases := [][]byte{
		[]byte("...FailedToOpenSocket..."),
		[]byte("error: FailedToOpenSocket (connection refused)"),
		[]byte("\x1b[31mFailedToOpenSocket\x1b[0m"), // ANSI-wrapped
	}
	for _, in := range cases {
		if !HasNetworkFailure(in) {
			t.Errorf("HasNetworkFailure(%q) = false, want true", in)
		}
	}
}

func TestHasNetworkFailureDoesNotMatchUnrelatedText(t *testing.T) {
	// The detector should NOT match incidental occurrences of "Socket" or
	// "Failed" alone. Anchor is the full identifier.
	cases := [][]byte{
		[]byte("Socket initialized"),
		[]byte("Build failed: compile error"),
		[]byte("FailedToParseJSON"), // different error class
	}
	for _, in := range cases {
		if HasNetworkFailure(in) {
			t.Errorf("HasNetworkFailure(%q) = true, want false", in)
		}
	}
}

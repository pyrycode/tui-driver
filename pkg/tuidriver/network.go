package tuidriver

import "bytes"

// networkFailureAnchors lists the literal substrings claude renders when
// the API is unreachable. Observed 2026-05-18 by spawning with a bogus
// ANTHROPIC_BASE_URL: claude shows the thinking spinner indefinitely AND
// emits the error name in the PTY stream:
//
//	FailedToOpenSocket
//
// More variants likely exist (DNS failures, TLS errors, rate limits).
// Add them as they're observed; do not speculatively pattern-match.
var networkFailureAnchors = [][]byte{
	[]byte("FailedToOpenSocket"),
}

// HasNetworkFailure reports whether snap contains evidence of a network-
// reachability failure to the claude API. Detects the same condition the
// watchdog would eventually catch (spinner-freeze / PTY-quiet) but
// earlier — useful for consumers that want to surface the failure to a
// host UI before falling back to the watchdog timeout.
//
// Does NOT cover authentication failures (those render differently —
// "Please run /login" appears at startup, before the welcome banner).
// Does NOT cover model-side rate limits or capacity errors (those have
// distinct error shapes).
//
// Returns false on nil/empty snap.
func HasNetworkFailure(snap []byte) bool {
	stripped := StripANSI(snap)
	for _, anchor := range networkFailureAnchors {
		if bytes.Contains(stripped, anchor) {
			return true
		}
	}
	return false
}

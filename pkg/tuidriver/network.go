package tuidriver

import "strings"

// networkFailureAnchors lists the literal status-line phrases claude renders
// when the API is unreachable. Captured live 2026-07-07 against claude 2.1.199
// (bogus ANTHROPIC_BASE_URL, connection refused): claude draws its thinking
// status line as
//
//	✻ Unable to connect to API (ConnectionRefused) · Retrying in 1s · attempt 1/10
//
// and retries on a backoff. The stable part is the lead phrase; the reason in
// parentheses (ConnectionRefused, DNS, TLS, …) and the retry counter vary, so
// only the phrase is anchored. The pre-2.1.199 token "FailedToOpenSocket"
// (2026-05-18) does not render on 2.1.199 at all and was removed: the corpus
// shows it only ever appeared as quoted content, never as claude's own chrome,
// so as an anchor it could only false-fire (#173/#220). Add more phrases here
// only as they are observed on screen; each is matched the same region-scoped
// way.
//
// Scope: network-unreachability only. An authentication failure renders a
// different line, "API Error: 401 …", owned by the dispatcher's own 401 retry
// and disjoint from this phrase — this detector deliberately does not cover it.
//
// ⚠️ Adding a phrase here? Add it to the #221 negative regression suite
// (anchor_forgery_test.go) so a transcript quotation of it stays non-firing.
var networkFailureAnchors = []string{
	"Unable to connect to API",
}

// HasNetworkFailure reports whether snap shows claude's network-unreachable
// status line in the bottom status region of the rendered screen.
//
// ADVISORY signal, not a fatal condition. Claude retries a network failure
// itself (ten attempts on a backoff), and the PTY-quiet watchdog is the real
// net for a genuinely hung session — a consumer should surface this to a host
// UI, not abort on it. Matching the rendered grid and scoping to the status
// region (bannerRegionRows) is what stops a quotation of the phrase in the
// on-screen transcript from firing it (#220); the earlier whole-buffer scan for
// a stale token aborted runs whose content merely mentioned it (#173).
//
// Returns false on nil/empty snap.
func HasNetworkFailure(snap []byte) bool {
	return hasNetworkFailureGrid(NewGrid(snap, 0, 0))
}

// hasNetworkFailureGrid is HasNetworkFailure over a Grid the caller already
// rendered. The per-tick classifier renders the snapshot once and threads that
// grid here (#225); HasNetworkFailure stays the thin single-snapshot wrapper.
func hasNetworkFailureGrid(g *Grid) bool {
	for _, row := range g.LastRows(bannerRegionRows) {
		for _, anchor := range networkFailureAnchors {
			if strings.Contains(row, anchor) {
				return true
			}
		}
	}
	return false
}

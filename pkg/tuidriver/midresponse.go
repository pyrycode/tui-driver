package tuidriver

import "strings"

// midResponseErrorAnchors lists the literal status-line phrase claude renders
// when its API connection drops mid-turn and the visible output is truncated.
// Per the ticket's description of the 2.1.199 render, claude draws
//
//	API Error: Connection closed mid-response. The response above may be incomplete.
//
// just above the input box. The stable, distinctive token is the partial-output
// phrase; the surrounding sentence punctuation, casing, and trailing clause all
// drift across claude versions, so only the phrase is anchored — never the bare
// "API Error:" prefix, which the out-of-scope 401 auth line ("API Error: 401 …")
// shares. "Connection closed mid-response" appears in none of the three
// neighbouring API-error lines — the 401 auth line, #220's "Unable to connect to
// API" network line, or #303's "API error … Retrying in" retry line — so it is
// disjoint from all of them on its own.
//
// The equally-distinctive alternative "The response above may be incomplete" is a
// safe second phrase to add here if a live capture shows the first drifts: the
// OR-match widens drift tolerance at no disjointness cost. One phrase satisfies
// every current requirement, so only the first is anchored for now.
//
// ⚠️ Adding a phrase here? Add it to the #221 negative regression suite
// (anchor_forgery_test.go) so a transcript quotation of it stays non-firing.
var midResponseErrorAnchors = []string{
	"Connection closed mid-response",
}

// HasMidResponseError reports whether snap shows claude's mid-response
// partial-output error line in the bottom status region of the rendered screen —
// the line claude draws when its API connection drops mid-turn, leaving the
// visible output above it truncated.
//
// ADVISORY signal, not a fatal condition. It means the turn's output is PARTIAL;
// a consumer surfaces "response incomplete" to a host UI rather than aborting on
// it — matching HasNetworkFailure / HasApiRetry framing. Matching the rendered
// grid and scoping to the status region (bannerRegionRows) is what stops a
// quotation of the phrase in the on-screen transcript from firing it (#220/#221).
//
// Returns false on nil/empty snap. Independent of the modal/idle/thinking axes,
// like HasNetworkFailure and HasMcpFailureBanner — the banner coexists with the
// dominant UI axis in the lower status area.
func HasMidResponseError(snap []byte) bool {
	return hasMidResponseErrorGrid(NewGrid(snap, 0, 0))
}

// hasMidResponseErrorGrid is HasMidResponseError over a Grid the caller already
// rendered. The per-tick classifier renders the snapshot once and threads that
// grid here (#225); HasMidResponseError stays the thin single-snapshot wrapper,
// exactly as hasNetworkFailureGrid relates to HasNetworkFailure.
func hasMidResponseErrorGrid(g *Grid) bool {
	for _, row := range g.LastRows(bannerRegionRows) {
		for _, anchor := range midResponseErrorAnchors {
			if strings.Contains(row, anchor) {
				return true
			}
		}
	}
	return false
}

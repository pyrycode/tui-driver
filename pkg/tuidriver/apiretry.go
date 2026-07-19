package tuidriver

import (
	"regexp"
	"strconv"
)

// ApiRetryAttempt is the parsed `attempt N/M` counter carried by claude's
// live API-error retry status line. Current is N (the attempt in flight),
// Total is M (the retry ceiling). The zero value {0, 0} is the
// "count unavailable" sentinel — a real counter is always Current>=1,
// Total>=1 — returned when the retry row is present but its counter is
// malformed or absent.
type ApiRetryAttempt struct {
	Current int
	Total   int
}

// apiRetryRowRe matches claude's live API-error retry status line: a generic
// transient API error that claude retries on a backoff, rendered (per the
// ticket's description of the 2.1.199 form) as
//
//	✻ API error · Retrying in 1s · attempt 3/10
//
// The stable structure is the two tokens "API error" and "Retrying in", in
// that order; the seconds value, the `·` separators, and the exact spacing all
// drift across claude versions and are deliberately NOT anchored. The two-token
// structural anchor (not the full literal) is what keeps this disjoint from the
// two neighbouring status lines that share the same retry suffix:
//
//   - #220's network-unreachable line renders the same
//     "· Retrying in Ns · attempt N/M" suffix but reads "Unable to connect to
//     API" (owned by network.go), NOT "API error" — so it never matches here.
//   - the auth line "API Error: 401 …" has a capital-E "Error" and no
//     "Retrying in" retry structure, so it is excluded on both tokens — matching
//     network.go's deliberate "network-only, auth is the dispatcher's 401 retry"
//     scope boundary.
//
// ⚠️ The "API error" casing and the "Retrying in" phrasing are taken from the
// ticket's description of the live render; no live .bin capture exists yet. If a
// real capture surfaces and a token drifts, this regex is the single place to
// adjust — the two-token structural anchor is chosen precisely so minor drift in
// the seconds/separators does not break it. Add any new anchor to the #221
// negative regression suite (anchor_forgery_test.go) so a transcript quotation
// of it stays non-firing.
var apiRetryRowRe = regexp.MustCompile(`API error.*Retrying in`)

// apiRetryAttemptRe extracts the `attempt N/M` counter. Capture group 1 is N
// (current attempt), group 2 is M (total). Applied to the matched retry ROW
// only (never the whole region) so it can never scrape the #220 network line's
// own `attempt 1/10` when both lines are on screen at once.
var apiRetryAttemptRe = regexp.MustCompile(`attempt\s+(\d+)\s*/\s*(\d+)`)

// HasApiRetry reports whether snap shows claude's live API-error retry status
// line in the bottom status region of the rendered screen.
//
// ADVISORY signal, not a fatal condition. Claude retries a transient API error
// itself (the `attempt N/M` counter is its own retry progress), and the
// PTY-quiet watchdog is the real net for a genuinely hung session — a consumer
// should surface this to a host UI (e.g. "retrying, 3 of 10"), not abort on it.
// Matching the rendered grid and scoping to the status region (bannerRegionRows)
// is what stops a quotation of the phrase in the on-screen transcript from firing
// it (#220/#221).
//
// Returns false on nil/empty snap. Independent of the modal/idle/thinking axes,
// like HasNetworkFailure and HasMcpFailureBanner — the banner coexists with the
// dominant UI axis in the lower status area.
func HasApiRetry(snap []byte) bool {
	present, _, _ := apiRetryInRegion(NewGrid(snap, 0, 0))
	return present
}

// ParseApiRetry returns the parsed attempt counter from claude's API-error retry
// status line. Returns (attempt, true) when the retry row is present AND its
// `attempt N/M` counter parses; returns ({0,0}, false) when no retry row is
// present, or the row is present but the counter is malformed or absent. Never
// panics. Follows the ParseSpinner / ParseSpinnerTokens (value, ok) idiom.
func ParseApiRetry(snap []byte) (ApiRetryAttempt, bool) {
	present, attempt, parsed := apiRetryInRegion(NewGrid(snap, 0, 0))
	if !present {
		return ApiRetryAttempt{}, false
	}
	return attempt, parsed
}

// apiRetryInRegion scans the bottom status region of the rendered grid for
// claude's API-error retry row and, if found, parses its attempt counter.
// Returns (present, attempt, parsed): present is whether the retry row sits in
// the region; attempt is the parsed counter, zero-value when the row is absent or
// its counter did not parse; parsed is whether the counter parsed. The per-tick
// classifier renders the snapshot once and threads that grid here (#225);
// HasApiRetry / ParseApiRetry stay the thin single-snapshot wrappers, exactly as
// hasNetworkFailureGrid / mcpBannerMatchInRegion relate to their exported
// wrappers.
//
// The counter regex is applied to the matched row only, not the region, so it
// can never scrape the #220 network line's own `attempt 1/10` when both rows are
// on screen at once.
func apiRetryInRegion(g *Grid) (present bool, attempt ApiRetryAttempt, parsed bool) {
	for _, row := range g.LastRows(bannerRegionRows) {
		if !apiRetryRowRe.MatchString(row) {
			continue
		}
		m := apiRetryAttemptRe.FindStringSubmatch(row)
		if m == nil {
			return true, ApiRetryAttempt{}, false
		}
		current, errN := strconv.Atoi(m[1])
		total, errM := strconv.Atoi(m[2])
		if errN != nil || errM != nil {
			// Digits that overflow int — never a real counter; treat as malformed.
			return true, ApiRetryAttempt{}, false
		}
		return true, ApiRetryAttempt{Current: current, Total: total}, true
	}
	return false, ApiRetryAttempt{}, false
}

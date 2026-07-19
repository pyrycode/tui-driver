package tuidriver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHasApiRetryEmpty(t *testing.T) {
	if HasApiRetry(nil) {
		t.Errorf("HasApiRetry(nil) = true, want false")
	}
	if HasApiRetry([]byte("idle TUI bytes")) {
		t.Errorf("HasApiRetry(idle) = true, want false")
	}
	if attempt, ok := ParseApiRetry(nil); ok || attempt != (ApiRetryAttempt{}) {
		t.Errorf("ParseApiRetry(nil) = (%+v, %v), want ({0 0}, false)", attempt, ok)
	}
}

// TestHasApiRetryRegion is the content-forgery regression, mirroring
// TestHasNetworkFailureRegion: the retry phrase quoted in the on-screen
// transcript body, pushed above the bottom status region by content below it,
// must NOT fire; the same phrase in the live status region must, and its
// counter must parse. \r\n so vt10x renders flat rows.
func TestHasApiRetryRegion(t *testing.T) {
	body := strings.Repeat("transcript body line\r\n", 25)

	// Forged above the region: the full phrase at the top, pushed far above the
	// bottom status rows by the transcript body below it.
	forged := []byte("✻ API error · Retrying in 1s · attempt 3/10\r\n" + body)
	if !strings.Contains(string(forged), "API error") {
		t.Fatal("fixture lost the forged phrase — the forgery contrast is void")
	}
	if HasApiRetry(forged) {
		t.Errorf("forged-above-region: HasApiRetry = true, want false")
	}

	// Live status line: the phrase sits in the bottom region, just above the
	// input box, exactly as the real retry renders.
	live := []byte(body + "✻ API error · Retrying in 1s · attempt 3/10\r\n────\r\n❯ \r\n")
	if !HasApiRetry(live) {
		t.Errorf("live-status: HasApiRetry = false, want true")
	}
	attempt, ok := ParseApiRetry(live)
	if !ok {
		t.Fatalf("live-status: ParseApiRetry ok = false, want true")
	}
	if attempt != (ApiRetryAttempt{Current: 3, Total: 10}) {
		t.Errorf("live-status: ParseApiRetry = %+v, want {Current:3 Total:10}", attempt)
	}
}

// TestApiRetryDisjointFromNetworkFailure is the AC2 crux (both directions): the
// #220 network-unreachable line and the #303 api-error retry line carry the same
// `· Retrying in Ns · attempt N/M` suffix, so the two detectors must never
// classify the same rendered row. Each requires a token the other's line lacks —
// `API error` vs `Unable to connect to API`. Asserting both directions locks the
// boundary so a future anchor loosening that reintroduces the overlap fails here.
func TestApiRetryDisjointFromNetworkFailure(t *testing.T) {
	body := strings.Repeat("transcript body line\r\n", 25)

	// The real 2.1.199 network line (loaded from the committed fixture, the
	// strongest form): fires NetworkFailure, must NOT fire ApiRetry (no
	// "API error" token).
	netSnap, err := os.ReadFile(filepath.Join("testdata", "network-failure-snapshot.bin"))
	if err != nil {
		t.Fatalf("read network fixture: %v", err)
	}
	if !HasNetworkFailure(netSnap) {
		t.Fatal("network fixture: HasNetworkFailure = false — precondition void")
	}
	if HasApiRetry(netSnap) {
		t.Errorf("network line: HasApiRetry = true, want false (lacks \"API error\")")
	}

	// The api-error retry line in-region: fires ApiRetry, must NOT fire
	// NetworkFailure (no "Unable to connect to API" token).
	apiLine := []byte(body + "✻ API error · Retrying in 1s · attempt 3/10\r\n────\r\n❯ \r\n")
	if !HasApiRetry(apiLine) {
		t.Fatal("api-error line: HasApiRetry = false — precondition void")
	}
	if HasNetworkFailure(apiLine) {
		t.Errorf("api-error line: HasNetworkFailure = true, want false (lacks \"Unable to connect to API\")")
	}
}

// TestHasApiRetryIgnoresAuthError pins the scope boundary, mirroring
// TestHasNetworkFailureIgnoresAuthError: the auth failure line renders
// "API Error: 401 …" (capital-E Error, no "Retrying in" retry structure), so it
// must NOT set ApiRetry — the #303 anchor requires the lowercase "API error"
// token AND the "Retrying in" co-token.
func TestHasApiRetryIgnoresAuthError(t *testing.T) {
	live := []byte("some transcript\r\nAPI Error: 401 Invalid authentication credentials\r\n────\r\n❯ \r\n")
	if HasApiRetry(live) {
		t.Errorf("auth-error: HasApiRetry = true, want false (capital-E Error, no Retrying in)")
	}
}

// TestParseApiRetryValidMalformedAbsent covers AC3: a valid counter parses to
// its integers; a malformed or absent counter on an otherwise-present retry row
// yields ({0,0}, false) and never panics.
func TestParseApiRetryValidMalformedAbsent(t *testing.T) {
	body := strings.Repeat("transcript body line\r\n", 25)
	live := func(row string) []byte {
		return []byte(body + row + "\r\n────\r\n❯ \r\n")
	}
	cases := []struct {
		name    string
		snap    []byte
		want    ApiRetryAttempt
		wantOk  bool
		wantHas bool
	}{
		{"valid", live("✻ API error · Retrying in 1s · attempt 3/10"), ApiRetryAttempt{Current: 3, Total: 10}, true, true},
		{"malformed-no-total", live("✻ API error · Retrying in 1s · attempt 3/"), ApiRetryAttempt{}, false, true},
		{"malformed-no-current", live("✻ API error · Retrying in 1s · attempt /10"), ApiRetryAttempt{}, false, true},
		{"malformed-nonnumeric", live("✻ API error · Retrying in 1s · attempt x/y"), ApiRetryAttempt{}, false, true},
		{"absent-counter", live("✻ API error · Retrying in 2s"), ApiRetryAttempt{}, false, true},
		{"no-retry-row", live("❯ ready"), ApiRetryAttempt{}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attempt, ok := ParseApiRetry(tc.snap)
			if attempt != tc.want || ok != tc.wantOk {
				t.Errorf("ParseApiRetry = (%+v, %v), want (%+v, %v)", attempt, ok, tc.want, tc.wantOk)
			}
			if got := HasApiRetry(tc.snap); got != tc.wantHas {
				t.Errorf("HasApiRetry = %v, want %v", got, tc.wantHas)
			}
		})
	}
}

// TestParseApiRetryBindsCounterToApiRow guards that the counter regex is applied
// to the matched api-error row only, never the region — so it can never scrape
// the network line's own `attempt 1/10` when both rows are on screen at once. The
// api-error row here carries attempt 4/10; a network line above it carries 1/10.
func TestParseApiRetryBindsCounterToApiRow(t *testing.T) {
	snap := []byte(strings.Repeat("transcript body line\r\n", 20) +
		"✻ Unable to connect to API (ConnectionRefused) · Retrying in 1s · attempt 1/10\r\n" +
		"✻ API error · Retrying in 2s · attempt 4/10\r\n────\r\n❯ \r\n")
	attempt, ok := ParseApiRetry(snap)
	if !ok {
		t.Fatalf("ParseApiRetry ok = false, want true")
	}
	if attempt != (ApiRetryAttempt{Current: 4, Total: 10}) {
		t.Errorf("ParseApiRetry = %+v, want {Current:4 Total:10} (bound to the api-error row, not the network line)", attempt)
	}
}

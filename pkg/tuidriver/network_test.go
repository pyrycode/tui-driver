package tuidriver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHasNetworkFailureEmpty(t *testing.T) {
	if HasNetworkFailure(nil) {
		t.Errorf("HasNetworkFailure(nil) = true, want false")
	}
	if HasNetworkFailure([]byte("idle TUI bytes")) {
		t.Errorf("HasNetworkFailure(idle) = true, want false")
	}
}

// TestHasNetworkFailureRealFixture pins the real 2.1.199 rendering. Captured
// live by spawning claude against a dead endpoint (bogus ANTHROPIC_BASE_URL):
// claude draws "✻ Unable to connect to API (ConnectionRefused) · Retrying in
// 1s · attempt 1/10" as its status line, just above the input box. The old
// token FailedToOpenSocket (2026-05-18, older claude) does not appear on
// 2.1.199 at all — #220.
func TestHasNetworkFailureRealFixture(t *testing.T) {
	snap, err := os.ReadFile(filepath.Join("testdata", "network-failure-snapshot.bin"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if !HasNetworkFailure(snap) {
		t.Errorf("HasNetworkFailure(network-failure-snapshot.bin) = false, want true")
	}
}

// TestHasNetworkFailureRejectsStaleToken: the old FailedToOpenSocket token is
// no longer an anchor, so it must not fire from anywhere — it can only appear
// as content on 2.1.199 (a ticket body, a source read of the old detector).
// This is the #173 abort class, closed.
func TestHasNetworkFailureRejectsStaleToken(t *testing.T) {
	for _, in := range [][]byte{
		[]byte("error: FailedToOpenSocket (connection refused)"),
		[]byte("\x1b[31mFailedToOpenSocket\x1b[0m"),
		[]byte("52\t// networkFailureAnchors lists FailedToOpenSocket ...\r\n"),
	} {
		if HasNetworkFailure(in) {
			t.Errorf("HasNetworkFailure(%q) = true, want false (stale token is not an anchor)", in)
		}
	}
}

// TestHasNetworkFailureRegion is the content-forgery regression: the real
// anchor phrase quoted in the on-screen transcript body, pushed above the
// bottom status region by content below it, must NOT fire; the same phrase in
// the live status region must. Mirrors TestDetectModalClassPermissionRegion.
// \r\n so vt10x renders flat rows.
func TestHasNetworkFailureRegion(t *testing.T) {
	const anchor = "Unable to connect to API (ConnectionRefused)"
	body := strings.Repeat("transcript body line\r\n", 25)

	// Forged above the region: the phrase at the top, pushed far above the
	// bottom status rows by the transcript body below it.
	forged := []byte(anchor + "\r\n" + body)
	if !strings.Contains(string(forged), "Unable to connect to API") {
		t.Fatal("fixture lost the forged phrase — the forgery contrast is void")
	}
	if HasNetworkFailure(forged) {
		t.Errorf("forged-above-region: HasNetworkFailure = true, want false")
	}

	// Live status line: the phrase sits in the bottom region, just above the
	// input box, exactly as the real failure renders.
	live := []byte(body + "✻ " + anchor + " · Retrying in 1s · attempt 1/10\r\n────\r\n❯ \r\n")
	if !HasNetworkFailure(live) {
		t.Errorf("live-status: HasNetworkFailure = false, want true")
	}
}

// TestHasNetworkFailureIgnoresAuthError pins the scope decision (#220): the
// detector covers network-unreachability only, not authentication. Claude
// renders an auth failure as "API Error: 401 …", handled by the dispatcher's
// own 401 retry, and disjoint from the network phrase. So an auth line in the
// status region must NOT set NetworkFailure.
func TestHasNetworkFailureIgnoresAuthError(t *testing.T) {
	live := []byte("some transcript\r\nAPI Error: 401 Invalid authentication credentials\r\n────\r\n❯ \r\n")
	if HasNetworkFailure(live) {
		t.Errorf("auth-error: HasNetworkFailure = true, want false (network-only scope)")
	}
}

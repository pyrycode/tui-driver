package tuidriver

import (
	"strings"
	"testing"
)

func TestHasMidResponseErrorEmpty(t *testing.T) {
	if HasMidResponseError(nil) {
		t.Errorf("HasMidResponseError(nil) = true, want false")
	}
	if HasMidResponseError([]byte("idle TUI bytes")) {
		t.Errorf("HasMidResponseError(idle) = true, want false")
	}
}

// TestHasMidResponseErrorNormalScreen covers AC1's "false on a normal
// (non-error) screen": an ordinary idle snapshot (❯ prompt + transcript, no
// error line) must not fire.
func TestHasMidResponseErrorNormalScreen(t *testing.T) {
	snap := gridRows(
		"assistant: here is the refactor you asked for",
		"────",
		"\xe2\x9d\xaf ",
	)
	if HasMidResponseError(snap) {
		t.Errorf("normal idle screen: HasMidResponseError = true, want false")
	}
}

// TestHasMidResponseErrorRegion is the content-forgery regression, mirroring
// TestHasNetworkFailureRegion: the mid-response error line quoted in the
// on-screen transcript body, pushed above the bottom status region by content
// below it, must NOT fire; the same line in the live status region must. \r\n so
// vt10x renders flat rows.
func TestHasMidResponseErrorRegion(t *testing.T) {
	const line = "API Error: Connection closed mid-response. The response above may be incomplete."
	body := strings.Repeat("transcript body line\r\n", 25)

	// Forged above the region: the full error line at the top, pushed far above
	// the bottom status rows by the transcript body below it.
	forged := []byte(line + "\r\n" + body)
	if !strings.Contains(string(forged), "Connection closed mid-response") {
		t.Fatal("fixture lost the forged phrase — the forgery contrast is void")
	}
	if HasMidResponseError(forged) {
		t.Errorf("forged-above-region: HasMidResponseError = true, want false")
	}

	// Live status line: the error line sits in the bottom region, just above the
	// input box, exactly as the real mid-response failure renders.
	live := []byte(body + line + "\r\n────\r\n\xe2\x9d\xaf \r\n")
	if !HasMidResponseError(live) {
		t.Errorf("live-status: HasMidResponseError = false, want true")
	}
}

// TestHasMidResponseErrorIgnoresAuthError is AC1's teeth, mirroring
// TestHasNetworkFailureIgnoresAuthError: the auth failure line renders
// "API Error: 401 …", which shares only the "API Error:" prefix and lacks the
// partial-output token. It is owned by the dispatcher's own 401 retry and out of
// scope, so it must NOT fire this detector.
func TestHasMidResponseErrorIgnoresAuthError(t *testing.T) {
	live := []byte("some transcript\r\nAPI Error: 401 Invalid authentication credentials\r\n────\r\n\xe2\x9d\xaf \r\n")
	if HasMidResponseError(live) {
		t.Errorf("auth-error: HasMidResponseError = true, want false (shares only the API Error: prefix)")
	}
}

// TestMidResponseErrorDisjointFromNeighbours is the AC1/AC2 crux (both
// directions): the mid-response line and the two neighbouring API-error lines it
// borders (#220 network, #303 api-retry) must never classify the same rendered
// row. Each requires a token the others lack. Asserting both directions locks the
// boundary so a future anchor loosening that reintroduces overlap fails here.
func TestMidResponseErrorDisjointFromNeighbours(t *testing.T) {
	body := strings.Repeat("transcript body line\r\n", 25)

	// The mid-response error line in-region: fires MidResponseError, must NOT fire
	// the network-failure or api-retry detectors (it lacks their tokens).
	midLine := []byte(body + "API Error: Connection closed mid-response. The response above may be incomplete.\r\n────\r\n\xe2\x9d\xaf \r\n")
	if !HasMidResponseError(midLine) {
		t.Fatal("mid-response line: HasMidResponseError = false — precondition void")
	}
	if HasNetworkFailure(midLine) {
		t.Errorf("mid-response line: HasNetworkFailure = true, want false (lacks \"Unable to connect to API\")")
	}
	if HasApiRetry(midLine) {
		t.Errorf("mid-response line: HasApiRetry = true, want false (lacks \"API error … Retrying in\")")
	}

	// The real 2.1.199 network line in-region: fires NetworkFailure, must NOT fire
	// MidResponseError (it lacks "Connection closed mid-response").
	netLine := []byte(body + "✻ Unable to connect to API (ConnectionRefused) · Retrying in 1s · attempt 1/10\r\n────\r\n\xe2\x9d\xaf \r\n")
	if !HasNetworkFailure(netLine) {
		t.Fatal("network line: HasNetworkFailure = false — precondition void")
	}
	if HasMidResponseError(netLine) {
		t.Errorf("network line: HasMidResponseError = true, want false (lacks \"Connection closed mid-response\")")
	}
}

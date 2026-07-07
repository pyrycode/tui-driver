package tuidriver

import (
	"strings"
	"testing"
)

func TestHasMcpFailureBannerEmpty(t *testing.T) {
	if HasMcpFailureBanner(nil) {
		t.Errorf("HasMcpFailureBanner(nil) = true, want false")
	}
	if HasMcpFailureBanner([]byte("idle TUI bytes")) {
		t.Errorf("HasMcpFailureBanner(plain) = true, want false")
	}
}

func TestHasMcpFailureBannerSyntheticVariants(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"singular", []byte("...1 MCP server failed · /mcp..."), true},
		{"plural", []byte("...2 MCP servers failed · /mcp..."), true},
		{"high count", []byte("12 MCP servers failed"), true},
		{"no count", []byte("MCP server failed"), false},
		{"unrelated mcp text", []byte("Manage MCP servers"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasMcpFailureBanner(tc.in); got != tc.want {
				t.Errorf("HasMcpFailureBanner(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestHasMcpFailureBannerStripsANSI(t *testing.T) {
	// Banner wrapped in CSI noise — predicate must still match. Since #220 the
	// grid render (not StripANSI) consumes the control sequences; the banner
	// renders as one row and the regex matches it.
	in := []byte("\x1b[38;5;211m1 MCP server failed\x1b[38;5;246m · /mcp\x1b[39m")
	if !HasMcpFailureBanner(in) {
		t.Errorf("HasMcpFailureBanner missed CSI-wrapped banner: %q", in)
	}
}

// TestHasMcpFailureBannerRegion is the #220 content-forgery regression. The
// banner renders in the lower status area, so it is matched only within the
// bottom status region. A "N MCP servers failed" phrase quoted in the
// transcript body, pushed above that region by content below, must NOT fire;
// the same phrase in the live status region must. FailedMcpCount must agree
// with HasMcpFailureBanner (both region-scoped). \r\n so vt10x renders flat rows.
func TestHasMcpFailureBannerRegion(t *testing.T) {
	body := strings.Repeat("transcript body line\r\n", 25)

	// Forged above the region.
	forged := []byte("3 MCP servers failed · /mcp\r\n" + body)
	if !strings.Contains(string(forged), "MCP servers failed") {
		t.Fatal("fixture lost the forged phrase — the forgery contrast is void")
	}
	if HasMcpFailureBanner(forged) {
		t.Errorf("forged-above-region: HasMcpFailureBanner = true, want false")
	}
	if got := FailedMcpCount(forged); got != 0 {
		t.Errorf("forged-above-region: FailedMcpCount = %d, want 0", got)
	}

	// Live banner in the bottom status region.
	live := []byte(body + "2 MCP servers failed · /mcp\r\n────\r\n❯ \r\n")
	if !HasMcpFailureBanner(live) {
		t.Errorf("live-region: HasMcpFailureBanner = false, want true")
	}
	if got := FailedMcpCount(live); got != 2 {
		t.Errorf("live-region: FailedMcpCount = %d, want 2", got)
	}
}

func TestFailedMcpCount(t *testing.T) {
	cases := []struct {
		in   []byte
		want int
	}{
		{[]byte("1 MCP server failed"), 1},
		{[]byte("12 MCP servers failed"), 12},
		{[]byte("\x1b[1m3 MCP servers failed\x1b[0m"), 3},
		{[]byte("no banner here"), 0},
		{nil, 0},
		{[]byte{}, 0},
	}
	for _, tc := range cases {
		got := FailedMcpCount(tc.in)
		if got != tc.want {
			t.Errorf("FailedMcpCount(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

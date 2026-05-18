package tuidriver

import "testing"

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
	// Banner wrapped in CSI noise — predicate must still match.
	in := []byte("\x1b[38;5;211m1 MCP server failed\x1b[38;5;246m · /mcp\x1b[39m")
	if !HasMcpFailureBanner(in) {
		t.Errorf("HasMcpFailureBanner missed CSI-wrapped banner: %q", in)
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

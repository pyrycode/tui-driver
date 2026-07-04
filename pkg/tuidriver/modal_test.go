package tuidriver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectModalClassUnknownOnEmpty(t *testing.T) {
	if got := DetectModalClass(nil); got != ModalClassUnknown {
		t.Errorf("DetectModalClass(nil) = %q, want %q", got, ModalClassUnknown)
	}
	if got := DetectModalClass([]byte("idle TUI bytes")); got != ModalClassUnknown {
		t.Errorf("DetectModalClass(idle) = %q, want %q", got, ModalClassUnknown)
	}
}

func TestDetectModalClassSyntheticAnchors(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want ModalClass
	}{
		{"mcp space-stripped", []byte("...ManageMCPservers..."), ModalClassMCP},
		{"mcp spaced title", []byte("...Manage MCP servers..."), ModalClassMCP},
		{"mcp empty-state spaced", []byte("...No MCP servers configured. Please run /doctor..."), ModalClassMCP},
		{"mcp empty-state stripped", []byte("...NoMCPserversconfigured.Pleaserun/doctor..."), ModalClassMCP},
		{"agents header + running tab", []byte("...Agents...Running..."), ModalClassAgents},
		{"agents header + library tab", []byte("...Agents...Library..."), ModalClassAgents},
		{
			// #151: a `/`-row with no picker highlight chrome is NOT a
			// picker — a lone command-shaped line (or an absolute path) at
			// idle must not phantom-picker. Chrome is now load-bearing.
			"slash-picker bare line without chrome is NOT a picker",
			[]byte("noise\n/figma-use description\n"),
			ModalClassUnknown,
		},
		{
			// #151: normal indexed foreground (index 246 → gray 148) is not
			// a highlight shade, so this fails the chrome check → Unknown.
			"slash-picker row in normal indexed color (no highlight) is NOT a picker",
			[]byte("noise\n\x1b[38;5;246m/figma-use\x1b[39m\n"),
			ModalClassUnknown,
		},
		{
			// #151: normal truecolor foreground (148,148,148) is not a
			// highlight shade → fails chrome → Unknown.
			"slash-picker row in normal truecolor (no highlight) is NOT a picker",
			[]byte("noise\n\x1b[38;2;148;148;148m/figma-use\x1b[39m\n"),
			ModalClassUnknown,
		},
		{
			// Stays a picker: the mid-row 38;5;153 is a real highlight shade
			// (index 153 → 175,215,255), so chrome is present.
			"slash-picker filtered (multiple colors mid-row)",
			[]byte("noise\n\x1b[38;5;246m/\x1b[38;5;153mp\x1b[38;5;246mlugin desc\n"),
			ModalClassSlashPicker,
		},
		{
			"slash-picker truecolor highlighted row",
			[]byte("noise\n\x1b[38;2;177;185;249m/code-review desc\n"),
			ModalClassSlashPicker,
		},
		{
			"hint-bar text alone is NOT a picker (welcome banner false-positive guard)",
			[]byte("...? for shortcuts · ← for agents..."),
			ModalClassUnknown,
		},
		{
			"slash mid-line is NOT a picker (markdown body false-positive guard)",
			[]byte("see /usr/local/bin for binaries"),
			ModalClassUnknown,
		},
		{"ask-user stripped", []byte("...Entertoselect..."), ModalClassAskUserQuestion},
		{"ask-user spaced", []byte("...Enter to select..."), ModalClassAskUserQuestion},
		{"trust-folder", []byte("...Quicksafetycheck..."), ModalClassTrustFolder},
		{"permission stripped", []byte("...Doyouwanttoproceed..."), ModalClassPermission},
		{"permission spaced", []byte("...Do you want to proceed..."), ModalClassPermission},
		{"model-select stripped", []byte("...Selectmodel..."), ModalClassModelSelect},
		{"model-select spaced", []byte("...Select model..."), ModalClassModelSelect},
		{"permissions-config + Allow tab", []byte("Permissions header...Allow rules"), ModalClassPermissionsConfig},
		{"permissions-config + Ask tab", []byte("Permissions...Ask before"), ModalClassPermissionsConfig},
		{"permissions-config + Deny tab", []byte("Permissions...Deny list"), ModalClassPermissionsConfig},
		{"permissions header alone is NOT a config modal", []byte("see Permissions docs for details"), ModalClassUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectModalClass(tc.in); got != tc.want {
				t.Errorf("DetectModalClass(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDetectModalClassSlashPickerContract covers #151's two guarantees: the
// slash-picker check runs LAST (a specific anchor wins even when picker signals
// are present), and it requires picker chrome (a highlight color), not just an
// on-screen `/`-row. Fixtures use \r\n so vt10x renders flat rows, not a
// staircase (the #150 grid lesson).
func TestDetectModalClassSlashPickerContract(t *testing.T) {
	const (
		hlTrue = "\x1b[38;2;177;185;249m" // truecolor highlight shade
		hlIdx  = "\x1b[38;5;153m"         // indexed highlight (→ 175,215,255)
		reset  = "\x1b[39m"
	)
	cases := []struct {
		name string
		in   []byte
		want ModalClass
	}{
		{
			// Lone absolute path on screen, no highlight → not a picker.
			// Grid-region alone wouldn't reject it (the path IS on screen);
			// chrome is what rejects it.
			"lone absolute path with no chrome is not a picker",
			[]byte("/Users/x/file.go\r\n"),
			ModalClassUnknown,
		},
		{
			// A real permission modal that also has a `/`-path on screen
			// painted in the highlight shade — i.e. BOTH picker signals
			// present. The reorder must let the Permission anchor win.
			"permission modal wins over co-present picker signals",
			[]byte("Do you want to proceed?\r\n" + hlTrue + "/Users/x/file.go" + reset + "\r\n"),
			ModalClassPermission,
		},
		{
			// Single-match filtered picker (one row) painted in the highlight
			// shade → still a picker. Truecolor + indexed twins.
			"single-match picker with truecolor chrome is a picker",
			[]byte(hlTrue + "/figma-use" + reset + "\r\n"),
			ModalClassSlashPicker,
		},
		{
			"single-match picker with indexed chrome is a picker",
			[]byte(hlIdx + "/figma-use" + reset + "\r\n"),
			ModalClassSlashPicker,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectModalClass(tc.in); got != tc.want {
				t.Errorf("DetectModalClass(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// ParsePicker must stay behaviourally unchanged for a real one-item
	// picker (AC: findPickerRows/ParsePicker untouched).
	t.Run("ParsePicker output unchanged for single-match picker", func(t *testing.T) {
		items := ParsePicker([]byte(hlTrue + "/figma-use" + reset + "\r\n"))
		if len(items) != 1 {
			t.Fatalf("ParsePicker returned %d items, want 1", len(items))
		}
		if items[0].Command != "/figma-use" {
			t.Errorf("ParsePicker command = %q, want %q", items[0].Command, "/figma-use")
		}
		if !items[0].Highlighted {
			t.Errorf("ParsePicker highlighted = false, want true")
		}
	})
}

func TestDetectModalClassStripsANSIAndOSCBeforeMatching(t *testing.T) {
	// Anchor wrapped in CSI + OSC noise — predicate should still match.
	in := []byte("\x1b[1m\x1b]0;title text\x07ManageMCPservers\x1b[0m")
	if got := DetectModalClass(in); got != ModalClassMCP {
		t.Errorf("DetectModalClass(wrapped) = %q, want %q", got, ModalClassMCP)
	}
}

func TestDetectModalClassAgentsNeedsHeaderAndTab(t *testing.T) {
	// "Agents" header alone is NOT enough — claude renders the word in
	// many contexts (e.g. `← for agents` status bar). Requires Running or
	// Library tab adjacency.
	in := []byte("...press ← for agents...")
	if got := DetectModalClass(in); got == ModalClassAgents {
		t.Errorf("DetectModalClass(status-bar only) = %q, want NOT agents", got)
	}
}

// Fixture-based regression: real captured /mcp + /agents + slash-picker
// snapshots from spike-multiselect runs must classify correctly.
func TestDetectModalClassRealFixtures(t *testing.T) {
	cases := []struct {
		fixture string
		want    ModalClass
	}{
		{"mcp-snapshot.bin", ModalClassMCP},
		{"mcp-empty-snapshot.bin", ModalClassMCP},
		{"agents-snapshot.bin", ModalClassAgents},
		{"picker-snapshot.bin", ModalClassSlashPicker},
		{"picker-truecolor-snapshot.bin", ModalClassSlashPicker},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			snap, err := os.ReadFile(filepath.Join("testdata", tc.fixture))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			if got := DetectModalClass(snap); got != tc.want {
				t.Errorf("DetectModalClass(%s) = %q, want %q", tc.fixture, got, tc.want)
			}
		})
	}
}

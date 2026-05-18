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
		{"agents header + running tab", []byte("...Agents...Running..."), ModalClassAgents},
		{"agents header + library tab", []byte("...Agents...Library..."), ModalClassAgents},
		{
			"slash-picker SGR row 246",
			[]byte("noise\x1b[38;5;246m/figma-use more"),
			ModalClassSlashPicker,
		},
		{
			"slash-picker SGR row 153 highlighted",
			[]byte("noise\x1b[38;5;153m/clear more"),
			ModalClassSlashPicker,
		},
		{
			"slash-picker filtered (color + nested color)",
			[]byte("noise\x1b[38;5;246m/\x1b[38;5;153mp\x1b[38;5;246mlugin more"),
			ModalClassSlashPicker,
		},
		{
			"hint-bar text alone is NOT a picker (welcome banner false-positive guard)",
			[]byte("...? for shortcuts · ← for agents..."),
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
		{"agents-snapshot.bin", ModalClassAgents},
		{"picker-snapshot.bin", ModalClassSlashPicker},
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

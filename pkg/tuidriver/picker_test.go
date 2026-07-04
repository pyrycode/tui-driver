package tuidriver

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSelectCommandStripsLeadingSlash(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"with slash", "/figma-use", "figma-use\r"},
		{"without slash", "figma-use", "figma-use\r"},
		{"empty", "", "\r"},
		{"just slash", "/", "\r"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(SelectCommand(tc.in))
			if got != tc.want {
				t.Errorf("SelectCommand(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParsePickerReturnsNilOnNonPicker(t *testing.T) {
	if got := ParsePicker(nil); got != nil {
		t.Errorf("ParsePicker(nil) = %v, want nil", got)
	}
	if got := ParsePicker([]byte("idle TUI bytes with no picker")); got != nil {
		t.Errorf("ParsePicker(non-picker) = %v, want nil", got)
	}
}

func TestParsePickerRealFixture(t *testing.T) {
	// Both fixtures are from default unfiltered "/" probes captured at
	// different points in claude's renderer history. Same structural
	// assertions for both — the parser is renderer-encoding-agnostic.
	// First-command differs per capture because claude's plugin set
	// (and therefore the alphabetically-first skill) changes; the
	// parser must report whichever row claude paints first.
	cases := []struct {
		fixture     string
		firstCmd    string
		minSGRShape string // expected raw-byte substring proving the SGR shape
	}{
		{
			fixture:     "picker-snapshot.bin",
			firstCmd:    "/figma-use",
			minSGRShape: "\x1b[38;5;",
		},
		{
			fixture:     "picker-truecolor-snapshot.bin",
			firstCmd:    "/code-review",
			minSGRShape: "\x1b[38;2;",
		},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			snap, err := os.ReadFile(filepath.Join("testdata", tc.fixture))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			// Sanity: confirm the fixture really does carry the SGR shape
			// we claim it does (guards against future re-records
			// silently swapping encodings).
			if !bytes.Contains(snap, []byte(tc.minSGRShape)) {
				t.Fatalf("fixture %s missing expected SGR shape %q", tc.fixture, tc.minSGRShape)
			}

			items := ParsePicker(snap)
			if len(items) == 0 {
				t.Fatalf("ParsePicker(%s) returned 0 items", tc.fixture)
			}

			first := items[0]
			if first.Command != tc.firstCmd {
				t.Errorf("first item Command = %q, want %q", first.Command, tc.firstCmd)
			}
			if !first.Highlighted {
				t.Errorf("first item Highlighted = false, want true")
			}

			sawFigmaCategory := false
			for _, it := range items {
				if it.Category == "figma" {
					sawFigmaCategory = true
				}
				if !startsWithSlash(it.Command) {
					t.Errorf("item %q does not start with /", it.Command)
				}
			}
			if !sawFigmaCategory {
				t.Errorf("no item had category=figma in fixture; items=%+v", items)
			}

			highlightedCount := 0
			for _, it := range items {
				if it.Highlighted {
					highlightedCount++
				}
			}
			if highlightedCount != 1 {
				t.Errorf("highlighted count = %d, want 1 (unfiltered mode)", highlightedCount)
			}
		})
	}
}

// TestParsePickerHypotheticalThirdEncoding is the AC #5 seam proof. A
// made-up SGR shape (\x1b[38;6;…) that parseForegroundSGR doesn't
// recognise must still produce parsed rows — the row anchor is
// renderer-encoding-agnostic. The rows just come back unhighlighted
// (the highlight predicate can't classify a color it can't read).
//
// To support a third encoding for real, add one case to
// parseForegroundSGR. No edits to the anchor or classifier are needed.
func TestParsePickerHypotheticalThirdEncoding(t *testing.T) {
	snap := []byte("\x1b[38;6;1;2;3m/foo description one\n\x1b[38;6;9;9;9m/bar description two\n")
	items := ParsePicker(snap)
	if len(items) != 2 {
		t.Fatalf("ParsePicker returned %d items, want 2 (anchor must not depend on color encoding); items=%+v", len(items), items)
	}
	if items[0].Command != "/foo" {
		t.Errorf("items[0].Command = %q, want /foo", items[0].Command)
	}
	if items[1].Command != "/bar" {
		t.Errorf("items[1].Command = %q, want /bar", items[1].Command)
	}
	// No row's open color is in pickerHighlightedRGBs (encoding is
	// unknown), so the classifier falls back to "first row highlighted".
	if !items[0].Highlighted {
		t.Errorf("items[0].Highlighted = false; want true (fallback when no row matches known highlight set)")
	}
	if items[1].Highlighted {
		t.Errorf("items[1].Highlighted = true; want false")
	}
}

func startsWithSlash(s string) bool { return len(s) > 0 && s[0] == '/' }

// #151 chrome helper: true only for a known picker-highlight shade, in either
// encoding; false for normal colors and no color.
func TestSnapHasPickerHighlight(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"indexed highlight (153)", []byte("\x1b[38;5;153m/figma-use\x1b[39m"), true},
		{"truecolor highlight (177,185,249)", []byte("\x1b[38;2;177;185;249m/code-review\x1b[39m"), true},
		{"indexed highlight mid-stream after normal", []byte("\x1b[38;5;246m/\x1b[38;5;153mp\x1b[39m"), true},
		{"normal indexed color (246 → gray 148)", []byte("\x1b[38;5;246m/figma-use\x1b[39m"), false},
		{"normal truecolor (148,148,148)", []byte("\x1b[38;2;148;148;148m/figma-use\x1b[39m"), false},
		{"no color at all", []byte("/figma-use description"), false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := snapHasPickerHighlight(tc.in); got != tc.want {
				t.Errorf("snapHasPickerHighlight(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// #151: isSlashPicker is the AND of grid-region row location and chrome. Truth
// table over {on-screen /-row?, chrome?}. Fixtures use \r\n (the #150 grid
// staircase lesson).
func TestIsSlashPicker(t *testing.T) {
	const hl = "\x1b[38;2;177;185;249m"
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"row + chrome", []byte(hl + "/figma-use\x1b[39m\r\n"), true},
		{"row, no chrome (lone path)", []byte("/Users/x/file.go\r\n"), false},
		{"chrome, no /-row", []byte(hl + "hello\x1b[39m\r\n"), false},
		{"neither (idle)", []byte("just some idle text\r\n"), false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSlashPicker(tc.in); got != tc.want {
				t.Errorf("isSlashPicker(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

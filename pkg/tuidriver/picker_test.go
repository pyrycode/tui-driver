package tuidriver

import (
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
	snap, err := os.ReadFile(filepath.Join("testdata", "picker-snapshot.bin"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	items := ParsePicker(snap)
	if len(items) == 0 {
		t.Fatal("ParsePicker(picker-snapshot.bin) returned 0 items")
	}

	// The fixture is from a default unfiltered "/" probe. Spot-check
	// known structural facts:
	//   - First item should be /figma-use (highlighted by default — at
	//     time of capture the figma plugin owned the alphabetically-
	//     first skill).
	//   - At least one item should have category=="figma".
	//   - All items must start with "/".
	first := items[0]
	if first.Command != "/figma-use" {
		t.Errorf("first item Command = %q, want /figma-use", first.Command)
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

	// Exactly one item should be highlighted in unfiltered mode.
	highlightedCount := 0
	for _, it := range items {
		if it.Highlighted {
			highlightedCount++
		}
	}
	if highlightedCount != 1 {
		t.Errorf("highlighted count = %d, want 1 (unfiltered mode)", highlightedCount)
	}
}

func startsWithSlash(s string) bool { return len(s) > 0 && s[0] == '/' }

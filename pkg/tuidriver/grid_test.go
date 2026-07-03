package tuidriver

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRenderPlainText(t *testing.T) {
	got := Render([]byte("hello world"), 80, 24)
	want := "hello world"
	if got != want {
		t.Errorf("Render(plain) = %q, want %q", got, want)
	}
}

func TestRenderCursorForwardAdvancesGrid(t *testing.T) {
	// "claude.ai" + cursor-forward 5 + "Gmail" should land "Gmail" at
	// column 14 (after 9 chars of "claude.ai" + 5 forward), NOT eat any
	// characters. The naive ANSI-strip would replace `\x1b[5C` with a
	// single space, producing "claude.ai Gmail" (one space). vt10x renders
	// it as "claude.ai" + 5 actual spaces + "Gmail" = 19 chars.
	in := []byte("claude.ai\x1b[5CGmail")
	got := Render(in, 80, 24)
	want := "claude.ai     Gmail"
	if got != want {
		t.Errorf("Render(cursor-forward) = %q, want %q", got, want)
	}
}

func TestRenderAbsoluteColumnPositioning(t *testing.T) {
	// `\x1b[15G` = move cursor to absolute column 15.
	// Writing "Gmail" there should land it at column 15 regardless of what
	// comes before.
	in := []byte("name:\x1b[15GGmail")
	got := Render(in, 80, 24)
	want := "name:" + strings.Repeat(" ", 9) + "Gmail" // cols 1-5 "name:", 6-14 spaces, 15+ "Gmail"
	if got != want {
		t.Errorf("Render(absolute-col) = %q, want %q", got, want)
	}
}

func TestRenderCarriageReturnRewrites(t *testing.T) {
	// CR alone moves cursor to column 1 without advancing row. Writing
	// after CR overwrites earlier content on the same row.
	in := []byte("first\rsecond")
	got := Render(in, 80, 24)
	want := "second"
	if got != want {
		t.Errorf("Render(CR-overwrite) = %q, want %q", got, want)
	}
}

func TestRenderStripsColorCodesAtTextLevel(t *testing.T) {
	// SGR color codes should be applied to the cell's attributes but not
	// appear in the text output.
	in := []byte("\x1b[1m\x1b[31mhello\x1b[0m world")
	got := Render(in, 80, 24)
	want := "hello world"
	if got != want {
		t.Errorf("Render(color-codes) = %q, want %q", got, want)
	}
}

func TestRenderTrimsTrailingPadding(t *testing.T) {
	// vt10x's String() pads every row to full width with spaces. Render
	// trims trailing whitespace + trailing empty rows so downstream
	// parsers don't drown in padding.
	got := Render([]byte("hi"), 80, 24)
	if strings.Contains(got, "  ") {
		t.Errorf("Render did not trim trailing padding: %q", got)
	}
	if strings.HasSuffix(got, "\n") {
		t.Errorf("Render kept a trailing newline: %q", got)
	}
}

func TestRenderDefaultsForZeroDims(t *testing.T) {
	// cols=0 or rows=0 should fall through to the package defaults.
	got1 := Render([]byte("x"), 0, 0)
	got2 := Render([]byte("x"), DefaultGridCols, DefaultGridRows)
	if got1 != got2 {
		t.Errorf("Render with zero dims (%q) differs from explicit defaults (%q)", got1, got2)
	}
}

// TestRenderMCPSnapshotRegression locks in the loop 6 F-1 truncation bug
// fix. The fixture is a real PTY snapshot from spike-multiselect's /mcp
// probe (2026-05-18) where claude renders MCP server names using cursor
// positioning. The naive StripANSI path produced truncated names like
// "claude.ai Gmail" → "laude.ai Gmail" and
// "claude.ai Google Calendar" → "caude.ai Google Calndar". The grid-
// rendered output must contain the full names.
func TestRenderMCPSnapshotRegression(t *testing.T) {
	path := filepath.Join("testdata", "mcp-snapshot.bin")
	snap, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	rendered := Render(snap, DefaultGridCols, DefaultGridRows)

	wantSubstrings := []string{
		"claude.ai Gmail",
		"claude.ai Google Calendar",
		"claude.ai Google Drive",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered output missing %q", want)
		}
	}

	// And the previously-observed truncations must NOT appear as
	// standalone lines. (They may appear as substrings of the correct
	// names, e.g. "laude.ai Gmail" is inside "claude.ai Gmail", so we
	// guard against the row-start truncation specifically.)
	dontWantPrefixes := []string{
		"\nlaude.ai Gmail",
		"\ncaude.ai Google Cal",
	}
	for _, dontWant := range dontWantPrefixes {
		if strings.Contains(rendered, dontWant) {
			t.Errorf("rendered output still contains truncation %q", dontWant)
		}
	}
}

func TestNewGridZeroDimsParity(t *testing.T) {
	// Zero dims must fall through to the package defaults exactly like Render
	// does — NewGrid delegates the fallthrough rather than reimplementing it.
	snap := []byte("alpha\r\nbeta\r\ngamma")
	got := NewGrid(snap, 0, 0).Rows()
	want := NewGrid(snap, DefaultGridCols, DefaultGridRows).Rows()
	if !slices.Equal(got, want) {
		t.Errorf("NewGrid zero-dims Rows() = %q, differ from explicit defaults %q", got, want)
	}
}

func TestGridRowsShape(t *testing.T) {
	// Content on row 0, blank row 1, content on row 2. Interior empties are
	// preserved; trailing empties are already dropped by Render.
	grid := NewGrid([]byte("top\r\n\r\nbottom"), 0, 0)
	got := grid.Rows()
	want := []string{"top", "", "bottom"}
	if !slices.Equal(got, want) {
		t.Errorf("Rows() = %q, want %q", got, want)
	}
}

func TestNewGridEmptyRenderZeroRows(t *testing.T) {
	// An empty screen has zero rendered rows — NOT [""], which is what a naive
	// strings.Split("", "\n") would yield.
	cases := map[string][]byte{
		"empty snapshot":       []byte(""),
		"whitespace-only snap": []byte("   \r\n   "),
	}
	for name, snap := range cases {
		t.Run(name, func(t *testing.T) {
			if got := NewGrid(snap, 0, 0).Rows(); len(got) != 0 {
				t.Errorf("Rows() = %q (len %d), want zero rows", got, len(got))
			}
		})
	}
}

func TestGridContainsInLastRowsRegionDistinction(t *testing.T) {
	// The headline slice: screen-region semantics vs. raw-history substring.
	const modal = "Do you trust the files in this folder?"
	// Snapshot A renders the modal into the grid's bottom region.
	snapA := []byte("header line\r\ncontext line\r\n" + modal)
	// Snapshot B is A plus content rendered *below* the modal, pushing it
	// above the last-k window — i.e. it has "scrolled up" off the bottom.
	snapB := []byte("header line\r\ncontext line\r\n" + modal + "\r\nfooter one\r\nfooter two")

	const k = 2

	if got := NewGrid(snapA, 0, 0).ContainsInLastRows(modal, k); !got {
		t.Errorf("snapshot A: ContainsInLastRows(%q, %d) = false, want true (modal is in the bottom region)", modal, k)
	}
	if got := NewGrid(snapB, 0, 0).ContainsInLastRows(modal, k); got {
		t.Errorf("snapshot B: ContainsInLastRows(%q, %d) = true, want false (modal scrolled above the last-%d window)", modal, k, k)
	}

	// The whole point: a raw substring match over the full snapshot bytes
	// finds the modal in BOTH cases — only the grid region predicate distinguishes.
	if !strings.Contains(string(snapA), modal) || !strings.Contains(string(snapB), modal) {
		t.Fatalf("test premise broken: raw snapshot bytes must contain %q in both A and B", modal)
	}
}

func TestGridContainsInLastRowsBoundaries(t *testing.T) {
	// 3-row grid: top row "alpha", middle "beta", bottom "gamma".
	grid := NewGrid([]byte("alpha\r\nbeta\r\ngamma"), 0, 0)
	rowCount := len(grid.Rows())
	if rowCount != 3 {
		t.Fatalf("fixture rendered %d rows, want 3", rowCount)
	}

	tests := []struct {
		name string
		sub  string
		n    int
		want bool
	}{
		{"n zero returns false", "gamma", 0, false},
		{"n negative returns false", "gamma", -1, false},
		{"exact row count reaches the top row", "alpha", rowCount, true},
		{"n far exceeds row count clamps to all rows", "alpha", rowCount + 100, true},
		{"top row is above the last-1 window", "alpha", 1, false},
		{"bottom row is within the last-1 window", "gamma", 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := grid.ContainsInLastRows(tt.sub, tt.n); got != tt.want {
				t.Errorf("ContainsInLastRows(%q, %d) = %v, want %v", tt.sub, tt.n, got, tt.want)
			}
		})
	}
}

func TestGridRowHasPrefix(t *testing.T) {
	grid := NewGrid([]byte("alpha one\r\nbeta two"), 0, 0)
	n := len(grid.Rows())

	tests := []struct {
		name   string
		i      int
		prefix string
		want   bool
	}{
		{"in-range matching prefix", 0, "alpha", true},
		{"in-range non-matching prefix", 0, "beta", false},
		{"second row matching prefix", 1, "beta", true},
		{"negative index does not panic", -1, "alpha", false},
		{"index at length does not panic", n, "beta", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := grid.RowHasPrefix(tt.i, tt.prefix); got != tt.want {
				t.Errorf("RowHasPrefix(%d, %q) = %v, want %v", tt.i, tt.prefix, got, tt.want)
			}
		})
	}
}

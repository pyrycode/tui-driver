package tuidriver

import (
	"os"
	"path/filepath"
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

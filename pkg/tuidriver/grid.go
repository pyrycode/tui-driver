package tuidriver

import (
	"strings"

	"github.com/hinshun/vt10x"
)

// Default grid dimensions for Render. Match DefaultPtyRows × DefaultPtyCols
// so a Buffer snapshot taken from a PTY allocated at those dimensions
// renders 1:1 with what claude actually drew.
const (
	DefaultGridRows = int(DefaultPtyRows)
	DefaultGridCols = int(DefaultPtyCols)
)

// Render interprets snap as a VT100 terminal byte stream and returns the
// grid-rendered text — what a real terminal would display after applying
// every escape sequence (cursor positioning, line clearing, scrolling,
// etc.). Trailing whitespace on each row is trimmed; rows are joined with
// "\n".
//
// Use this for parsing column-aligned content where naive ANSI stripping
// (StripANSI / StripANSIString) loses cursor-positioning information. The
// canonical failure mode: claude renders MCP server names via cursor-
// positioning escapes like `\x1b[<col>G`, and naive stripping produces
// truncated names like "claude.ai Gmail" → "laude.ai Gmail" (loop 6 F-1).
//
// For state predicates (HasTrustModal, isIdle) the cheaper StripANSI path
// remains correct — those match on text content, not column layout.
//
// cols, rows are the grid dimensions to render at. Pass 0 for either to
// use the package defaults (DefaultGridCols / DefaultGridRows). Render with
// the same dimensions the PTY was allocated at; mismatched dimensions can
// cause word-wrap differences vs. what the user saw.
func Render(snap []byte, cols, rows int) string {
	if cols <= 0 {
		cols = DefaultGridCols
	}
	if rows <= 0 {
		rows = DefaultGridRows
	}
	term := vt10x.New(vt10x.WithSize(cols, rows))
	_, _ = term.Write(snap)
	return trimGridText(term.String())
}

// trimGridText collapses trailing whitespace on each row and drops trailing
// empty rows. vt10x's String() pads every row to full width with spaces;
// without trimming the output is dominated by padding which is noise for
// downstream parsers.
func trimGridText(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	// Drop trailing empty lines.
	end := len(lines)
	for end > 0 && lines[end-1] == "" {
		end--
	}
	return strings.Join(lines[:end], "\n")
}

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

// Grid is an addressable view over a rendered VT100 screen. Construct once per
// snapshot with NewGrid; it renders the snapshot via Render and caches the
// resulting rows. Every accessor operates on the rendered *screen* rows — the
// text a terminal would actually display — never the raw append-only history
// buffer. That distinction is what lets later detectors avoid classifying
// scrolled-off text as on-screen.
type Grid struct {
	rows []string // rendered rows; see NewGrid for the empty-render contract
}

// NewGrid renders snap at the given grid dimensions and returns a Grid over the
// resulting rows. Passing 0 for either dimension falls through to
// DefaultGridCols / DefaultGridRows (same convention as Render); non-zero
// dimensions override them. An empty render (whitespace-only or empty snapshot)
// yields a Grid with zero rows — Render returns "" for such input, and an empty
// screen has no rendered rows (not [""]).
func NewGrid(snap []byte, cols, rows int) *Grid {
	text := Render(snap, cols, rows)
	if text == "" {
		return &Grid{}
	}
	return &Grid{rows: strings.Split(text, "\n")}
}

// Rows returns the grid's rendered rows in top-to-bottom order: one entry per
// row, each right-trimmed, trailing empty rows dropped (exactly what Render
// emits per line). The returned slice is the grid's own backing store —
// read-only; callers must not mutate it.
func (g *Grid) Rows() []string {
	return g.rows
}

// ContainsInLastRows reports whether sub appears within the last n rendered
// rows. n <= 0 returns false; n greater than the row count is clamped to all
// rows. A match must fall inside a single row — sub is never matched across a
// row boundary. That is the point: off-screen text that has scrolled above the
// last-n window does not count.
func (g *Grid) ContainsInLastRows(sub string, n int) bool {
	if n <= 0 {
		return false
	}
	n = min(n, len(g.rows))
	for _, row := range g.rows[len(g.rows)-n:] {
		if strings.Contains(row, sub) {
			return true
		}
	}
	return false
}

// RowHasPrefix reports whether row i (0-based, top-down) starts with prefix.
// An out-of-range i returns false rather than panicking.
func (g *Grid) RowHasPrefix(i int, prefix string) bool {
	if i < 0 || i >= len(g.rows) {
		return false
	}
	return strings.HasPrefix(g.rows[i], prefix)
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

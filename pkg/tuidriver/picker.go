package tuidriver

import (
	"regexp"
	"strings"
)

// PickerItem represents one row of claude's `/` slash-command picker as
// extracted from a PTY snapshot. Loop 4 D-1 deliverable shape, used by
// consumers (e.g. pyry acp) to forward command suggestions to mobile host
// UIs.
type PickerItem struct {
	Command     string `json:"command"`
	Category    string `json:"category,omitempty"`
	Description string `json:"description"`
	Highlighted bool   `json:"highlighted"`
}

// Picker row structure as claude paints it:
//
// UNFILTERED (user typed `/` only) — each row is a single color block:
//
//	\x1b[38;…m/<cmd><CSI-fwd>[(<cat>) ]<desc>\x1b[39m
//	- highlighted row's open color sits in pickerHighlightedRGBs.
//	- all other rows open in a normal gray (RGB ~153 or ~148).
//	- CSI cursor-forward (\x1b[<N>C) substitutes for spaces between words.
//
// FILTERED (user typed e.g. `/fi`) — each row has MULTIPLE color switches
// within it. Claude wraps each occurrence of the typed filter substring in
// the highlight color; the rest stays in the normal color. So `/figma-use`
// filtered by `fi` renders with the `fi` substring highlighted mid-row:
//
//	\x1b[38;…246m/\x1b[38;…153mfi\x1b[38;…246mgma-use\x1b[39m
//
// The whole-row highlight semantic is lost in filtered mode; claude
// instead surfaces the SELECTED row's description in a separate right-
// column block, and the default selection is always the first match
// (claude convention). Row finding works in both modes because every row
// — filtered or not — begins with `/<letter>` on its line after ANSI
// stripping.

// pickerColorCodeRe matches foreground-color SGR codes we strip during
// description reconstruction. Indexed + truecolor + reset shapes all
// covered. CSI cursor-forward (\x1b[NC) is handled separately because
// it's positional, not stylistic.
var pickerColorCodeRe = regexp.MustCompile(`\x1b\[(?:38;5;\d+|38;2;\d+;\d+;\d+|39)m`)

// pickerCsiCursorFwdRe matches CSI sequences that claude uses for
// inter-word spacing: cursor-forward-N (`\x1b[<N>C`, used in the
// indexed-color era) and cursor-horizontal-absolute-N (`\x1b[<N>G`,
// observed in claude 2.1.150's truecolor renderer). Both collapse to a
// single space during description reconstruction.
var pickerCsiCursorFwdRe = regexp.MustCompile(`\x1b\[(\d+)[CG]`)

// pickerCategoryRe matches the optional "(category)" prefix on a row's
// description: e.g. `/figma-use (figma) MANDATORY prerequisite — …`.
var pickerCategoryRe = regexp.MustCompile(`^\(([^)]+)\)\s*(.*)`)

// pickerRow is the internal projection of one picker line: the raw byte
// slice covering line-start..newline plus the foreground color that was
// active when the leading `/` was painted. openColor.known distinguishes
// "color set explicitly" from "default/unknown" (the walker's zero value)
// so unrecognised SGR encodings produce unhighlighted rows rather than
// matching some sentinel constant.
type pickerRow struct {
	raw       []byte
	openColor rgb
	colorSet  bool
}

// pickerRowStartRe locates the start of a picker row's content within a
// stripped line: optional leading whitespace (including `\r` left over
// from `\r\r\n` line separators getting split on `\n`), then `/`, then
// a name character. The anchor is intentionally renderer-agnostic — it
// does not know about colors at all. Whatever shape claude paints
// (indexed, truecolor, hypothetical-third-encoding, none), the
// underlying text is `/<letter>…`.
var pickerRowStartRe = regexp.MustCompile(`^[ \t\r]*/[a-zA-Z]`)

// findPickerRows scans snap for picker rows. Returns one entry per line
// whose stripped content begins with `/<letter>`. Highlight color is
// captured by walking the line's raw bytes through a foreground-SGR
// state machine — the rows themselves are located structurally, with no
// reference to SGR encoding.
//
// Line segmentation treats BOTH `\n` and `\r` as separators. claude
// emits `\r\r\n` between rows in some renders and uses bare `\r` to
// overwrite the cursor's column (e.g. `\r/ \r/code-review` — the
// prompt's `/ ` is overprinted by the picker row). Splitting on `\r`
// puts the post-carriage-return content on its own logical line so the
// row anchor matches.
func findPickerRows(snap []byte) []pickerRow {
	if len(snap) == 0 {
		return nil
	}
	cleaned := StripOSC(snap)
	var rows []pickerRow
	start := 0
	for i := 0; i <= len(cleaned); i++ {
		isSep := i == len(cleaned) || cleaned[i] == '\n' || cleaned[i] == '\r'
		if !isSep {
			continue
		}
		line := cleaned[start:i]
		start = i + 1
		if len(line) == 0 {
			continue
		}
		stripped := StripANSI(line)
		if !pickerRowStartRe.Match(stripped) {
			continue
		}
		color, ok := pickerRowOpenColor(line)
		rows = append(rows, pickerRow{
			raw:       line,
			openColor: color,
			colorSet:  ok,
		})
	}
	return rows
}

// pickerRowOpenColor walks raw byte-by-byte tracking the active
// foreground color. When the walker reaches the first `/` followed by a
// letter, it returns the color in effect at that point. ok=false means
// no explicit foreground color was set when the `/` was painted (treat
// as unhighlighted by default).
func pickerRowOpenColor(raw []byte) (rgb, bool) {
	var (
		cur    rgb
		curSet bool
	)
	i := 0
	for i < len(raw) {
		if raw[i] == 0x1b && i+1 < len(raw) && raw[i+1] == '[' {
			c, consumed, isFg, isReset := parseForegroundSGR(raw[i:])
			if consumed > 0 {
				if isFg {
					cur, curSet = c, true
				} else if isReset {
					cur, curSet = rgb{}, false
				}
				i += consumed
				continue
			}
		}
		if raw[i] == '/' && i+1 < len(raw) && isASCIILetter(raw[i+1]) {
			return cur, curSet
		}
		i++
	}
	return rgb{}, false
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// snapHasPickerHighlight reports whether snap paints any foreground in one of
// claude's picker-highlight shades (pickerHighlightedRGBs). This is the "chrome"
// signal for slash-picker classification (#151): a real picker always paints its
// selected row in a highlight color, whereas a benign absolute path or a lone
// command-shaped line at idle carries no such highlight.
//
// It mirrors pickerRowOpenColor's foreground-SGR walk but does NOT stop at the
// first `/`: it scans the whole snapshot for any highlighted foreground.
// parseForegroundSGR resolves both indexed (38;5;153) and truecolor
// (38;2;177;185;249) encodings to a comparable rgb, so this is encoding-agnostic
// by construction — the two real fixtures carry the highlight in different
// encodings and both match. One linear, allocation-free pass.
func snapHasPickerHighlight(snap []byte) bool {
	i := 0
	for i < len(snap) {
		if snap[i] == 0x1b && i+1 < len(snap) && snap[i+1] == '[' {
			c, consumed, isFg, _ := parseForegroundSGR(snap[i:])
			if consumed > 0 {
				if isFg && rgbIsHighlighted(c) {
					return true
				}
				i += consumed
				continue
			}
		}
		i++
	}
	return false
}

// gridHasPickerRow reports whether any on-screen rendered row begins with a
// picker row start (`/<letter>` after optional leading whitespace, via
// pickerRowStartRe). Location only: it reads from #150's Grid, so rows that
// have scrolled off into raw history don't count. Grid rows are StripANSI'd and
// carry no color, so highlight chrome is checked separately (see isSlashPicker).
func gridHasPickerRow(g *Grid) bool {
	for _, row := range g.Rows() {
		if pickerRowStartRe.MatchString(row) {
			return true
		}
	}
	return false
}

// isSlashPicker reports whether snap renders claude's `/` slash-command picker.
// It is the last-resort classifier in DetectModalClass (#151), reached only
// when no specific modal anchor matched. Two independent signals of different
// fabric, ANDed:
//
//  1. Grid-region location — at least one on-screen grid row begins `/<letter>`
//     (off-screen `/`-lines in raw history are excluded by the rendered grid).
//  2. Chrome — the snapshot carries a picker highlight color.
//
// Requiring both stops a benign on-screen absolute path (/Users/x/file.go) from
// phantom-pickering, while a genuine single-match filtered picker (one `/`-row
// painted in the highlight shade) still classifies.
func isSlashPicker(snap []byte) bool {
	if !gridHasPickerRow(NewGrid(snap, 0, 0)) {
		return false
	}
	return snapHasPickerHighlight(snap)
}

// ParsePicker extracts structured items from a raw PTY snapshot of
// claude's `/` slash-command picker. Returns items in render order
// (top-to-bottom). Handles both unfiltered and filtered modes (see the
// row-structure doc comment above for the difference).
//
// Items past the visible-window cutoff in the rolling buffer won't appear;
// picker scrolling requires a follow-up snapshot after arrow-down keys —
// see loop 4 D-3 for why arrow navigation isn't a library primitive.
//
// Returns nil if no picker rows are detected (snap is not the picker UI,
// or the buffer has only fragments).
func ParsePicker(snap []byte) []PickerItem {
	rows := findPickerRows(snap)
	if len(rows) == 0 {
		return nil
	}

	// Count rows opening in a known-highlighted color. Unfiltered mode
	// has ≥1; filtered mode has 0 (substring highlights live mid-row).
	highlightedCount := 0
	for _, r := range rows {
		if r.colorSet && rgbIsHighlighted(r.openColor) {
			highlightedCount++
		}
	}

	var items []PickerItem
	for i, r := range rows {
		// Reconstruct command + description text:
		//  1. Strip SGR foreground colors (preserve adjacency).
		//  2. Convert cursor-forward to single space.
		//  3. Strip any remaining CSI noise.
		//  4. Collapse multi-spaces; trim.
		text := pickerColorCodeRe.ReplaceAllString(string(r.raw), "")
		text = pickerCsiCursorFwdRe.ReplaceAllString(text, " ")
		text = StripANSIString(text)
		for strings.Contains(text, "  ") {
			text = strings.ReplaceAll(text, "  ", " ")
		}
		text = strings.TrimSpace(text)

		if !strings.HasPrefix(text, "/") {
			continue
		}
		spaceIdx := strings.IndexAny(text, " \t")
		var cmd, rest string
		if spaceIdx < 0 {
			cmd, rest = text, ""
		} else {
			cmd, rest = text[:spaceIdx], strings.TrimSpace(text[spaceIdx+1:])
		}

		var category, description string
		if catMatch := pickerCategoryRe.FindStringSubmatch(rest); catMatch != nil {
			category = catMatch[1]
			description = catMatch[2]
		} else {
			description = rest
		}

		// Highlight detection:
		//  - UNFILTERED: ≥1 row opens in a highlighted color → match by RGB.
		//  - FILTERED: every row opens in normal color → default is first.
		var highlighted bool
		if highlightedCount >= 1 {
			highlighted = r.colorSet && rgbIsHighlighted(r.openColor)
		} else {
			highlighted = i == 0
		}

		items = append(items, PickerItem{
			Command:     cmd,
			Category:    category,
			Description: description,
			Highlighted: highlighted,
		})
	}
	return items
}

// SelectCommand returns the keystrokes to commit a slash-command from
// claude's picker, assuming the picker is open at its unfiltered state
// (user has typed `/` and the picker is showing). Sends the rest of the
// command name followed by CR (`\r`).
//
// claude auto-expands plugin-namespaced commands after CR — e.g. typing
// `figma-use\r` becomes `/figma:figma-use` once committed (loop 4 D-5).
//
// The leading `/` in name is tolerated and stripped. For filtered-mode
// selection (the picker has been narrowed by prior keystrokes), the
// caller must pass only the remaining suffix.
//
// Caller writes the returned bytes to the PTY:
//
//	ptmx.Write(tuidriver.SelectCommand("/figma-use"))
func SelectCommand(name string) []byte {
	name = strings.TrimPrefix(name, "/")
	return []byte(name + "\r")
}

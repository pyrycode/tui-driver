package tuidriver

import (
	"bytes"
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

// pickerItemStartRe matches the start of a picker row: a color-code
// followed by `/` followed by a name character.
//
// Claude renders the `/` picker in two structurally different modes:
//
// UNFILTERED (user typed `/` only): each row is a single color block.
//
//	\x1b[38;5;<color>m/<cmd><CSI-fwd>[(<cat>) ]<desc>\x1b[39m
//	- color=153 (light blue) marks the HIGHLIGHTED row (Enter default).
//	- color=246 (gray) marks every other row.
//	- CSI cursor-forward (\x1b[<N>C) substitutes for spaces between words.
//
// FILTERED (user typed e.g. `/fi`): each row has MULTIPLE color
// switches within it. Claude wraps each occurrence of the typed filter
// substring in 153 to highlight matching characters; the rest stays 246.
// So `/figma-use` filtered by `fi` renders as:
//
//	\x1b[38;5;246m/\x1b[38;5;153mfi\x1b[38;5;246mgma-use\x1b[39m
//
// The whole-row highlight semantic is lost in filtered mode; claude
// instead surfaces the SELECTED row's full description in a separate
// right-column 153 block. Default selection in filtered mode is always
// the first match (claude convention).
//
// The trailing `(?:\x1b\[38;5;\d+m)?[a-zA-Z]` clause matches both shapes
// without false-positiving on description-embedded slashes (those never
// have the SGR-foreground prefix).
var pickerItemStartRe = regexp.MustCompile(
	`\x1b\[38;5;(246|153)m/(?:\x1b\[38;5;\d+m)?[a-zA-Z]`,
)

// pickerColorCodeRe matches the foreground-color SGR codes we strip
// during command-name + description reconstruction. CSI cursor-forward
// (\x1b[NC) is handled separately because it's positional, not stylistic.
var pickerColorCodeRe = regexp.MustCompile(`\x1b\[(?:38;5;\d+|39)m`)

// pickerCsiCursorFwdRe matches CSI cursor-forward N (\x1b[<N>C).
var pickerCsiCursorFwdRe = regexp.MustCompile(`\x1b\[(\d+)C`)

// pickerCategoryRe matches the optional "(category)" prefix on a row's
// description: e.g. `/figma-use (figma) MANDATORY prerequisite — …`.
var pickerCategoryRe = regexp.MustCompile(`^\(([^)]+)\)\s*(.*)`)

// ParsePicker extracts structured items from a raw PTY snapshot of
// claude's `/` slash-command picker. Returns items in render order
// (top-to-bottom). Handles both unfiltered and filtered modes (see
// pickerItemStartRe doc for the structural difference).
//
// Items past the visible-window cutoff in the rolling buffer won't appear;
// picker scrolling requires a follow-up snapshot after arrow-down keys —
// see loop 4 D-3 for why arrow navigation isn't a library primitive.
//
// Returns nil if no picker rows are detected (snap is not the picker UI,
// or the buffer has only fragments).
func ParsePicker(snap []byte) []PickerItem {
	cleaned := StripOSC(snap)

	starts := pickerItemStartRe.FindAllSubmatchIndex(cleaned, -1)
	if len(starts) == 0 {
		return nil
	}

	// Mode detection: unfiltered has 0 or 1 item-starts with color=153
	// (the highlighted row). Filtered has every item-start at 246
	// (substring highlights live MID-row, not at the start). So we can
	// detect highlighting by counting color=153 openings.
	highlightedCount153 := 0
	for _, m := range starts {
		color := string(cleaned[m[2]:m[3]])
		if color == "153" {
			highlightedCount153++
		}
	}

	var items []PickerItem
	for i, m := range starts {
		itemStart := m[0]
		var itemEnd int
		if i+1 < len(starts) {
			itemEnd = starts[i+1][0]
		} else {
			itemEnd = len(cleaned)
		}
		// Cap at first newline — left-column items are single-line; the
		// trailing right-pane description must not leak into the last item.
		if nl := bytes.IndexAny(cleaned[itemStart:itemEnd], "\r\n"); nl >= 0 {
			itemEnd = itemStart + nl
		}
		raw := cleaned[itemStart:itemEnd]
		openColor := string(cleaned[m[2]:m[3]])

		// Reconstruct command + description text:
		//  1. Strip SGR foreground colors (preserve adjacency).
		//  2. Convert cursor-forward to single space.
		//  3. Strip any remaining CSI noise.
		//  4. Collapse multi-spaces; trim.
		text := pickerColorCodeRe.ReplaceAllString(string(raw), "")
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
		//  - UNFILTERED: exactly one row opens with 153.
		//  - FILTERED: every row opens with 246; default is the first row.
		var highlighted bool
		if highlightedCount153 >= 1 {
			highlighted = openColor == "153"
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

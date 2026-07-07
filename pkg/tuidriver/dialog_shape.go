package tuidriver

import (
	"regexp"
	"strings"
)

// selectionOptionRe matches a rendered row whose SELECTED option is marked by
// claude's ❯ pointer glyph. After leading indentation is trimmed the row starts
// with ❯, whitespace, then an option token of either fabric claude uses:
//
//	❯ 1. Yes, I trust this folder      number-select (trust, permission, model-select, ask-user)
//	❯ [✔] smart-connections            checkbox      (the 2.1.199 MCP-enablement modal)
//
// This is the structural shape shared by every blocking selection dialog claude
// draws. Content almost never begins a line with the pointer glyph followed by a
// numbered or checkbox token, so the shape is a reliable "a dialog is up" signal
// that needs no per-dialog anchor text. The unmarked sibling rows ("  2. No",
// "  [✔] qmd") are not required: a real dialog always renders exactly one
// pointer-marked selected row, which is signal enough. Sibling of modalOptionRe
// (permission.go), which stays number-only because it also EXTRACTS option
// labels; this one only DETECTS the shape, so it also admits the checkbox form.
var selectionOptionRe = regexp.MustCompile(`^❯\s+(?:\d+\.|\[.\])`)

// gridHasSelectionDialog reports whether any rendered row of g is a
// pointer-marked selection-option row (selectionOptionRe). It is the structural
// dialog-shape predicate: it says "a blocking selection dialog is on screen"
// without naming which one.
func gridHasSelectionDialog(g *Grid) bool {
	for _, row := range g.Rows() {
		if selectionOptionRe.MatchString(strings.TrimLeft(row, " ")) {
			return true
		}
	}
	return false
}

// HasUnknownDialog reports whether snap shows a blocking selection dialog that
// DetectModalClass does NOT recognise: the dialog shape is present, yet no known
// class matches. This is the #224 defence for a novel claude dialog.
//
// It closes the class the 2.1.199 MCP-enablement modal walked through on
// 2026-07-03: a startup dialog matched no anchor, so DetectModalClass returned
// unknown, the idle predicate matched the ❯ the dialog painted, and the runner
// delivered its first prompt into the dialog and hung opaquely. The original
// architecture called for a detect-unknown-and-bail layer; it was never built,
// and the readiness gate previously rode only the known class set. This adds
// the missing structural layer: no per-dialog anchor, so it survives a claude
// update that introduces a brand-new dialog.
//
// The library only reports. WaitReady consumes this so a novel startup dialog
// surfaces as an *UnexpectedModalError (#173) rather than an ignorable flag; a
// mid-run consumer can also call it directly and decide policy — typically bail
// rather than deliver a prompt into an unrecognised dialog. Returns false when a
// known class matches (that class is
// the consumer's own to handle) and when no dialog shape is present (the common
// idle screen).
func HasUnknownDialog(snap []byte) bool {
	if DetectModalClass(snap) != ModalClassUnknown {
		return false
	}
	return gridHasSelectionDialog(NewGrid(snap, 0, 0))
}

package tuidriver

import (
	"strings"
)

// ModalClass identifies which of claude's modal/picker UI states a PTY
// snapshot is currently rendering. Returned by DetectModalClass.
//
// Eight classes have been mapped empirically:
//   - permission         (spike #13 / loop 1)        — tool-permission prompt
//   - trust-folder       (loop 2 B-5)                — first-use folder trust
//   - slash-picker       (loop 3 C-2 / 4 D-1)        — `/` command picker
//   - ask-user-question  (loop 5 E-1)                — AskUserQuestion tool modal
//   - mcp                (loop 6 F-1)                — `/mcp` status display
//   - agents             (loop 6 F-1)                — `/agents` subagent list;
//     classifier arm retired in #245 (not a live class; see ModalClassAgents)
//   - model-select       (2026-05-18 evening probes) — `/model` model picker
//   - permissions-config (2026-05-18 evening probes) — `/permissions` settings
type ModalClass string

const (
	ModalClassUnknown ModalClass = ""
	ModalClassMCP     ModalClass = "mcp"
	// ModalClassAgents identified claude's pre-2.1.199 tabbed `/agents` modal.
	// claude 2.1.199 removed the `/agents` wizard, so on the pinned claude the
	// modal can no longer render and the class could only ever fire falsely. The
	// #227 corpus-replay harness confirmed the observed cost that #182 had not yet
	// seen: a healthy production recording classified agents 6× on plain
	// transcript content. The classifier arm was therefore RETIRED in #245 —
	// detectModalClassWithGrid no longer returns this class, so transcript content
	// can never classify as agents on the pinned claude.
	//
	// The constant is retained for source compatibility (the external consumer and
	// cmd/spike-multiselect's dispatch still reference it), as are
	// ParseAgentList/AgentList and the frozen agents-snapshot.bin fixture (which
	// still parses a captured pre-2.1.199 modal host-independently). This comment
	// is the single source of truth for the retirement; a future consumer needing
	// pre-2.1.199 classification can reconstruct the ~5-line arm from git history
	// against the retained fixture.
	ModalClassAgents            ModalClass = "agents"
	ModalClassSlashPicker       ModalClass = "slash-picker"
	ModalClassAskUserQuestion   ModalClass = "ask-user-question"
	ModalClassTrustFolder       ModalClass = "trust-folder"
	ModalClassPermission        ModalClass = "permission"
	ModalClassModelSelect       ModalClass = "model-select"
	ModalClassPermissionsConfig ModalClass = "permissions-config"
)

// Modal-class detection anchors. Each is unique to its class — verified
// empirically across loops 1-6 plus the 2026-05-18 API-extending probes, and
// re-verified against the rendered grid on 2026-07-06 (#152).
//
// Anchors key on the SPACE-PRESERVED form — the text as it appears in the
// rendered screen grid. #152 moved DetectModalClass from a StripANSI substring
// scan of the raw history buffer onto the #150 rendered Grid. Rendering (vt10x)
// reconstructs the column spacing that StripANSI destroyed by eating CSI
// cursor-forward moves, so the on-screen form is what matches. The old
// space-stripped variants ("ManageMCPservers", "Doyouwanttoproceed", …) were an
// artifact of that stripping — they can never appear in a rendered grid — and
// are removed here rather than carried as dead no-op checks.
//
// Anchors are unexported — consumers call DetectModalClass rather than
// matching directly. Since #223 every class also requires a STRUCTURAL
// co-signal of a different fabric, not the anchor text alone, so one on-screen
// content line quoting the text no longer forges the class (the F3 forgery the
// negative suite pins). Literal forms plus co-signal, matched against the
// rendered grid:
//
//	mcp                 → "Manage MCP servers" (modal title) AND the picker
//	                      highlight color (snapHasPickerHighlight). #223 removed
//	                      the inline empty-state "No MCP servers configured"
//	                      (#128) anchor entirely: it is a /mcp result echoed on an
//	                      idle screen, not a modal, and matching it held the modal
//	                      axis on idle and suppressed idle/thinking edge events.
//	ask-user-question   → "Enter to select" AND a pointer-marked option row
//	                      (gridHasSelectionDialog, #223). The footer phrase is
//	                      generic — the agents modal's footer carries it too — so
//	                      the option-row shape plus the #223 re-order (this class
//	                      runs AFTER the header-specific ones) keep it honest.
//	trust-folder        → "Quick safety check" (anchorTrustHeaderSpaced, defined
//	                      in permission.go) AND a pointer-marked numbered option
//	                      row directly below it — the dialog shape, via
//	                      gridHasTrustDialog (trust.go). #219: the header alone
//	                      classified any on-screen quotation of it as the modal,
//	                      a fatal false positive under the runner's abort policy.
//	permission          → the proceed prompt (anchorPermissionSpaced) AND a
//	                      pointer-marked numbered option row directly below it —
//	                      the dialog shape, region-scoped to the bottom overlay
//	                      window (permissionRegionRows), via gridHasPermissionDialog
//	                      (below). #242: the prompt alone classified any in-region
//	                      quotation of it as the modal — a forgeable surface for the
//	                      modal_shown / modal_answer consumers.
//	model-select        → "Select model" AND a pointer-marked option row
//	                      (gridHasSelectionDialog, #223) — the `/model` modal.
//	permissions-config  → "Permissions" header + one of Allow/Ask/Deny tabs AND
//	                      the picker highlight color (#223).
//
// slash-picker classification (isSlashPicker in picker.go) combines two signals
// of different fabric: an on-screen rendered row that begins `/<letter>`
// (located from #150's Grid, so scrolled-off history is excluded) AND picker
// highlight chrome (a foreground color in pickerHighlightedRGBs). Both are
// required. It runs LAST (see DetectModalClass) and reuses the grid this
// function already built.
//
// ⚠️ When you add or change an anchor here, add it to the #221 negative
// regression suite (anchor_forgery_test.go). That suite renders every anchor as
// screen CONTENT and asserts the detector does not fire — an anchor without a
// structural co-signal fails it by default. Skipping this step is how the 173
// and 217 forgeries shipped.
var (
	anchorMCPSpaced           = []byte("Manage MCP servers")
	anchorAskUserSpaced       = []byte("Enter to select")
	anchorPermissionSpaced    = []byte("Do you want to proceed")
	anchorModelSelectSpaced   = []byte("Select model")
	anchorPermissionsHeader   = []byte("Permissions")
	anchorPermissionsTabAllow = []byte("Allow")
	anchorPermissionsTabAsk   = []byte("Ask")
	anchorPermissionsTabDeny  = []byte("Deny")
)

// permissionRegionRows bounds how far up from the bottom of the rendered screen
// the permission overlay's "Do you want to proceed?" line may sit and still
// count. The real overlay renders that line ~5 rows from the bottom
// (permission-snapshot.bin); the prompt always sits just above its numbered
// options and the "(Esc to cancel)" footer, so this window covers it with
// slack even for a modal with several options. Scoping the match to this bottom
// window is what rejects an identical "Do you want to proceed?" phrase forged
// higher up in the on-screen transcript body — the CRITICAL B case that a
// whole-buffer or whole-grid match would misclassify as Permission.
const permissionRegionRows = 12

// permissionDialogLookahead bounds how many rows below anchorPermissionSpaced the
// pointer-marked numbered option row may sit and still count. Mirrors
// trustDialogLookahead: claude renders the ❯-marked option directly below the
// prompt (permission-snapshot.bin: prompt at bottom-5, option at bottom-4); the
// slack tolerates a blank or wrapped prompt line between them.
const permissionDialogLookahead = 3

// gridHasPermissionDialog reports whether g renders claude's real permission
// overlay: anchorPermissionSpaced inside the bottom permissionRegionRows window
// AND, within the next permissionDialogLookahead rows of that same window, a
// pointer-marked numbered option row (modalOptionRe, group 1 = ❯ marker).
//
// The structural co-signal is the #242 fix, mirroring gridHasTrustDialog (#219).
// Before it, the permission arm classified from the anchor phrase alone anywhere
// in the bottom window, so fresh transcript content scrolling through that window
// and quoting the phrase forged a live permission dialog — which a consumer would
// surface to an operator or answer with a keystroke into a live turn.
//
// It keeps BOTH guards, and the difference from gridHasTrustDialog is exactly one
// line: it iterates g.LastRows(permissionRegionRows), not g.Rows(). Permission is
// a bottom-region overlay and that region scope is load-bearing — it is what
// rejects an above-window transcript forgery (TestModalPhraseAnchorsRejectBodyForgery).
// Because the window is already the bottom region and the downward lookahead can
// only move toward the screen bottom, both the anchor and the option row are
// inside permissionRegionRows by construction. A whole-grid match here would
// silently drop the region guard — do not.
func gridHasPermissionDialog(g *Grid) bool {
	rows := g.LastRows(permissionRegionRows)
	for i, row := range rows {
		if !strings.Contains(row, string(anchorPermissionSpaced)) {
			continue
		}
		end := min(i+1+permissionDialogLookahead, len(rows))
		for j := i + 1; j < end; j++ {
			if m := modalOptionRe.FindStringSubmatch(strings.TrimLeft(rows[j], " ")); m != nil && m[1] != "" {
				return true
			}
		}
	}
	return false
}

// gridContains reports whether sub appears within any rendered screen row of g.
// This is the whole-visible-grid match used by the full-panel modal classes
// (mcp, agents, model-select, permissions-config, trust-folder): the grid
// already excludes scrolled-off history, so a panel anchor cannot be forged by
// text that scrolled above the screen. The bottom-overlay class (permission)
// scopes to the bottom window instead (gridHasPermissionDialog, via
// Grid.LastRows), to additionally reject an on-screen transcript forgery
// rendered above the overlay.
func gridContains(g *Grid, sub []byte) bool {
	s := string(sub)
	for _, row := range g.Rows() {
		if strings.Contains(row, s) {
			return true
		}
	}
	return false
}

// DetectModalClass classifies the modal/picker currently rendered in snap.
// It renders snap once via the #150 Grid and matches each class's anchor
// against the rendered screen rows — never a StripANSI substring over the raw
// append-only history buffer. Full-panel classes match anywhere in the visible
// grid; the permission overlay is region-scoped to the bottom window
// (permissionRegionRows). Use Render/ParseModalContent when you need to extract
// content from the modal, not just classify it.
//
// Order of checks is significant (#151, #223): the header-specific classes run
// first, the generic-footer ask-user class runs after them, and the slash-picker
// check runs LAST. Two reasons. The picker's signal is the most permissive (a
// bottom-region `/`-row plus a highlight color), so a real modal that merely has
// a `/`-path on screen must match its own anchor first. And ask-user's anchor is a
// generic list footer that other dialogs draw too (the agents modal's footer
// carries it), so the classes with a specific header must be tried before it.
// The single grid built here is threaded into the picker check so the snapshot
// is rendered only once.
//
// Since #223 each class also requires a structural co-signal of a different
// fabric — a pointer-marked option row (gridHasSelectionDialog) for the option
// dialogs, or the picker highlight color (snapHasPickerHighlight) for the
// full panels — so one on-screen content line quoting a class's header text no
// longer forges it.
//
// Returns ModalClassUnknown when no class matches (the common case at idle — no
// modal currently rendered).
func DetectModalClass(snap []byte) ModalClass {
	return detectModalClassWithGrid(NewGrid(snap, 0, 0), snap)
}

// detectModalClassWithGrid is DetectModalClass over a Grid the caller already
// rendered. The per-tick classifier renders the snapshot once and threads that
// grid here (#225), so a tick classifies every axis off one render;
// DetectModalClass stays the thin single-snapshot wrapper. snap is still needed
// for the raw-color picker-highlight check (snapHasPickerHighlight) and the
// slash-picker chrome check, which read color bytes the grid discards.
func detectModalClassWithGrid(g *Grid, snap []byte) ModalClass {
	switch {
	case gridContains(g, anchorMCPSpaced) && snapHasPickerHighlight(snap):
		return ModalClassMCP
	case gridHasTrustDialog(g):
		return ModalClassTrustFolder
	case gridHasPermissionDialog(g):
		return ModalClassPermission
	case gridContains(g, anchorModelSelectSpaced) && gridHasSelectionDialog(g):
		return ModalClassModelSelect
	case gridContains(g, anchorPermissionsHeader) &&
		(gridContains(g, anchorPermissionsTabAllow) ||
			gridContains(g, anchorPermissionsTabAsk) ||
			gridContains(g, anchorPermissionsTabDeny)) &&
		snapHasPickerHighlight(snap):
		return ModalClassPermissionsConfig
	case gridContains(g, anchorAskUserSpaced) && gridHasSelectionDialog(g):
		return ModalClassAskUserQuestion
	}
	// Slash-picker is the last resort: reached only when no specific anchor
	// matched. It requires a bottom-region `/`-row AND picker highlight chrome
	// (see isSlashPicker), so a real modal carrying a `/`-path wins above, a lone
	// path at idle never phantom-pickers, and a `/`-line up in the transcript body
	// is out of the region window (#237). Reuses the grid built above.
	if isSlashPickerWithGrid(g, snap) {
		return ModalClassSlashPicker
	}
	return ModalClassUnknown
}

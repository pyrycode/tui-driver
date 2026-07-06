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
//   - agents             (loop 6 F-1)                — `/agents` subagent list
//     (removed in claude 2.1.199 — matches pre-2.1.199 builds only; see ModalClassAgents)
//   - model-select       (2026-05-18 evening probes) — `/model` model picker
//   - permissions-config (2026-05-18 evening probes) — `/permissions` settings
type ModalClass string

const (
	ModalClassUnknown ModalClass = ""
	ModalClassMCP     ModalClass = "mcp"
	// ModalClassAgents matches claude's pre-2.1.199 tabbed `/agents` modal.
	// claude 2.1.199 removed the `/agents` wizard: `/agents` now prints a
	// one-line "The /agents wizard has been removed…" notice that carries
	// neither the "Agents" header nor a Running/Library tab, so
	// DetectModalClass returns ModalClassUnknown for it (verified live against
	// 2.1.199 while working #178). This class and its anchors therefore match
	// pre-2.1.199 builds only (≤2.1.158 still render the real modal). Retained
	// deliberately — not dead code — per claude-version.lock's tolerated-drift
	// policy and the #129/#178 retain precedent; kept exercised host-
	// independently by agents-snapshot.bin. See #182.
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
// matching directly. Literal forms, matched against the rendered grid:
//
//	mcp                 → "Manage MCP servers" (modal title) or the strict-mcp
//	                      inline empty-state "No MCP servers configured" (#128)
//	agents              → "Agents" header + "Running" or "Library" tab
//	                      (pre-2.1.199 only — see ModalClassAgents)
//	ask-user-question   → "Enter to select"
//	trust-folder        → "Quick safety check" (anchorTrustHeaderSpaced, defined
//	                      in permission.go beside the trust-modal extractor)
//	permission          → "Do you want to proceed" — region-scoped to the
//	                      bottom overlay window (permissionRegionRows)
//	model-select        → "Select model" (the `/model` modal)
//	permissions-config  → "Permissions" header + one of Allow/Ask/Deny tabs
//
// slash-picker classification (isSlashPicker in picker.go) combines two signals
// of different fabric: an on-screen rendered row that begins `/<letter>`
// (located from #150's Grid, so scrolled-off history is excluded) AND picker
// highlight chrome (a foreground color in pickerHighlightedRGBs). Both are
// required. It runs LAST (see DetectModalClass) and reuses the grid this
// function already built.
var (
	anchorMCPSpaced           = []byte("Manage MCP servers")
	anchorMCPEmptySpaced      = []byte("No MCP servers configured")
	anchorAgentsHeader        = []byte("Agents")
	anchorAgentsTabRunning    = []byte("Running")
	anchorAgentsTabLibrary    = []byte("Library")
	anchorAskUserSpaced       = []byte("Enter to select")
	anchorPermissionSpaced    = []byte("Do you want to proceed")
	// anchorPermissionStripped is no longer used by DetectModalClass (which now
	// matches the rendered grid). It is retained because permission.go's
	// raw-bytes hasPermissionPrompt path still matches both forms.
	anchorPermissionStripped  = []byte("Doyouwanttoproceed")
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

// gridContains reports whether sub appears within any rendered screen row of g.
// This is the whole-visible-grid match used by the full-panel modal classes
// (mcp, agents, model-select, permissions-config, trust-folder): the grid
// already excludes scrolled-off history, so a panel anchor cannot be forged by
// text that scrolled above the screen. The bottom-overlay class (permission)
// uses Grid.ContainsInLastRows instead, to additionally reject an on-screen
// transcript forgery rendered above the overlay.
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
// Order of checks is significant: the specific anchors run first and the
// slash-picker check runs LAST (#151). The picker's signal is the most
// permissive (any on-screen `/`-row plus a highlight color), so a real modal
// that merely has a `/`-path on screen — e.g. a permission prompt showing a
// file path — must match its own anchor before the picker is even considered.
// The single grid built here is threaded into the picker check so the snapshot
// is rendered only once.
//
// Returns ModalClassUnknown when no anchor matches (the common case at idle —
// no modal currently rendered).
func DetectModalClass(snap []byte) ModalClass {
	g := NewGrid(snap, 0, 0)
	switch {
	case gridContains(g, anchorMCPSpaced) || gridContains(g, anchorMCPEmptySpaced):
		return ModalClassMCP
	case gridContains(g, anchorAgentsHeader) &&
		(gridContains(g, anchorAgentsTabRunning) ||
			gridContains(g, anchorAgentsTabLibrary)):
		return ModalClassAgents
	case gridContains(g, anchorAskUserSpaced):
		return ModalClassAskUserQuestion
	case gridContains(g, anchorTrustHeaderSpaced):
		return ModalClassTrustFolder
	case g.ContainsInLastRows(string(anchorPermissionSpaced), permissionRegionRows):
		return ModalClassPermission
	case gridContains(g, anchorModelSelectSpaced):
		return ModalClassModelSelect
	case gridContains(g, anchorPermissionsHeader) &&
		(gridContains(g, anchorPermissionsTabAllow) ||
			gridContains(g, anchorPermissionsTabAsk) ||
			gridContains(g, anchorPermissionsTabDeny)):
		return ModalClassPermissionsConfig
	}
	// Slash-picker is the last resort: reached only when no specific anchor
	// matched. It requires an on-screen `/`-row AND picker highlight chrome
	// (see isSlashPicker), so a real modal carrying a `/`-path wins above and a
	// lone path at idle never phantom-pickers. Reuses the grid built above.
	if isSlashPickerWithGrid(g, snap) {
		return ModalClassSlashPicker
	}
	return ModalClassUnknown
}

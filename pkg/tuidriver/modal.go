package tuidriver

import (
	"bytes"
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
// empirically across loops 1-6 plus the 2026-05-18 API-extending probes.
// CSI cursor-forward stripping eats inter-word spaces in some renderings
// (Bash-style), so predicates match both space-stripped and space-
// preserved forms where claude varies.
//
// Anchors are unexported — consumers call DetectModalClass rather than
// matching directly. The literal forms are documented here for readers.
//
//	mcp                 → "ManageMCPservers" or "Manage MCP servers" (the
//	                      modal title), or the strict-mcp inline empty-state
//	                      "No MCP servers configured" / "NoMCPserversconfigured"
//	                      (claude 2.1.158 renders the empty-state instead of
//	                      the modal under --strict-mcp-config — see #128)
//	agents              → "Agents" header + "Running" or "Library" tab
//	                      (pre-2.1.199 only — see ModalClassAgents)
//	slash-picker        → an on-screen rendered row that starts with `/<letter>`
//	                      AND picker highlight chrome (a foreground in
//	                      pickerHighlightedRGBs) — see isSlashPicker
//	ask-user-question   → "Entertoselect" or "Enter to select"
//	trust-folder        → "Quicksafetycheck"
//	permission          → "Doyouwanttoproceed" or "Do you want to proceed"
//	model-select        → "Selectmodel" or "Select model" (the `/model` modal)
//	permissions-config  → "Permissions" header + one of Allow/Ask/Deny tabs
//
// slash-picker classification (isSlashPicker in picker.go) combines two
// signals of different fabric: an on-screen rendered row that begins
// `/<letter>` (located from #150's Grid, so scrolled-off history is
// excluded) AND picker highlight chrome (a foreground color in
// pickerHighlightedRGBs). Both are required. Row location alone
// false-positived on benign content — the "? for shortcuts" hint bar at
// idle (guarded by the `/`-prefix, which the hint line lacks) and, more
// insidiously, a lone absolute path like `/Users/x/file.go` on screen.
// Requiring chrome rejects the path; requiring an on-screen row rejects
// off-screen matches. This is a separate path from ParsePicker, which
// still uses findPickerRows on the raw color-preserving bytes; the two
// diverge deliberately (ParsePicker runs only after classification
// confirms a picker), so a grid/color split here does not touch parsing.
var (
	anchorMCP                 = []byte("ManageMCPservers")
	anchorMCPSpaced           = []byte("Manage MCP servers")
	anchorMCPEmptySpaced      = []byte("No MCP servers configured")
	anchorMCPEmptyStripped    = []byte("NoMCPserversconfigured")
	anchorAgentsHeader        = []byte("Agents")
	anchorAgentsTabRunning    = []byte("Running")
	anchorAgentsTabLibrary    = []byte("Library")
	anchorAskUserStripped     = []byte("Entertoselect")
	anchorAskUserSpaced       = []byte("Enter to select")
	anchorTrustFolder         = []byte("Quicksafetycheck")
	anchorPermissionStripped  = []byte("Doyouwanttoproceed")
	anchorPermissionSpaced    = []byte("Do you want to proceed")
	anchorModelSelectStripped = []byte("Selectmodel")
	anchorModelSelectSpaced   = []byte("Select model")
	anchorPermissionsHeader   = []byte("Permissions")
	anchorPermissionsTabAllow = []byte("Allow")
	anchorPermissionsTabAsk   = []byte("Ask")
	anchorPermissionsTabDeny  = []byte("Deny")
)

// DetectModalClass classifies the modal/picker currently rendered in snap.
// Cheap predicate: StripANSI + StripOSC + substring matches for the specific
// anchors, then a grid-plus-chrome slash-picker check (isSlashPicker) as the
// last resort. Use Render (vt10x-backed) when you need to extract content from
// the modal, not just classify it.
//
// Order of checks is significant: the specific anchors run first and the
// slash-picker check runs LAST (#151). The picker's signal is the most
// permissive (any on-screen `/`-row plus a highlight color), so a real modal
// that merely has a `/`-path on screen — e.g. a permission prompt showing a
// file path — must match its own anchor before the picker is even considered.
// Checking the picker first misclassified such modals and suppressed their
// auto-answer.
//
// Returns ModalClassUnknown when no anchor matches (the common case at
// idle — no modal currently rendered).
func DetectModalClass(snap []byte) ModalClass {
	stripped := StripOSC(StripANSI(snap))
	switch {
	case bytes.Contains(stripped, anchorMCP) ||
		bytes.Contains(stripped, anchorMCPSpaced) ||
		bytes.Contains(stripped, anchorMCPEmptySpaced) ||
		bytes.Contains(stripped, anchorMCPEmptyStripped):
		return ModalClassMCP
	case bytes.Contains(stripped, anchorAgentsHeader) &&
		(bytes.Contains(stripped, anchorAgentsTabRunning) ||
			bytes.Contains(stripped, anchorAgentsTabLibrary)):
		return ModalClassAgents
	case bytes.Contains(stripped, anchorAskUserStripped) ||
		bytes.Contains(stripped, anchorAskUserSpaced):
		return ModalClassAskUserQuestion
	case bytes.Contains(stripped, anchorTrustFolder):
		return ModalClassTrustFolder
	case bytes.Contains(stripped, anchorPermissionStripped) ||
		bytes.Contains(stripped, anchorPermissionSpaced):
		return ModalClassPermission
	case bytes.Contains(stripped, anchorModelSelectStripped) ||
		bytes.Contains(stripped, anchorModelSelectSpaced):
		return ModalClassModelSelect
	case bytes.Contains(stripped, anchorPermissionsHeader) &&
		(bytes.Contains(stripped, anchorPermissionsTabAllow) ||
			bytes.Contains(stripped, anchorPermissionsTabAsk) ||
			bytes.Contains(stripped, anchorPermissionsTabDeny)):
		return ModalClassPermissionsConfig
	}
	// Slash-picker is the last resort: reached only when no specific anchor
	// matched. It requires an on-screen `/`-row AND picker highlight chrome
	// (see isSlashPicker), so a real modal carrying a `/`-path wins above and a
	// lone path at idle never phantom-pickers.
	if isSlashPicker(snap) {
		return ModalClassSlashPicker
	}
	return ModalClassUnknown
}

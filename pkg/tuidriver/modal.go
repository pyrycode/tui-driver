package tuidriver

import (
	"bytes"
	"regexp"
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
//   - model-select       (2026-05-18 evening probes) — `/model` model picker
//   - permissions-config (2026-05-18 evening probes) — `/permissions` settings
type ModalClass string

const (
	ModalClassUnknown           ModalClass = ""
	ModalClassMCP               ModalClass = "mcp"
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
//	mcp                 → "ManageMCPservers"
//	agents              → "Agents" header + "Running" or "Library" tab
//	slash-picker        → SGR-colored picker row (`\x1b[38;5;{246|153}m/<letter>`)
//	ask-user-question   → "Entertoselect" or "Enter to select"
//	trust-folder        → "Quicksafetycheck"
//	permission          → "Doyouwanttoproceed" or "Do you want to proceed"
//	model-select        → "Selectmodel" or "Select model" (the `/model` modal)
//	permissions-config  → "Permissions" header + one of Allow/Ask/Deny tabs
//
// slash-picker uses the SGR-row pattern rather than the "? for shortcuts"
// hint-bar text. The hint-bar text appears at idle too (it's part of the
// welcome banner's input-line hint), causing false-positive picker
// classifications. The SGR row pattern only matches when actual picker
// rows are rendered. The same regex doubles as the parser's item-start
// matcher in picker.go.
var (
	anchorMCP                = []byte("ManageMCPservers")
	anchorAgentsHeader       = []byte("Agents")
	anchorAgentsTabRunning   = []byte("Running")
	anchorAgentsTabLibrary   = []byte("Library")
	anchorAskUserStripped    = []byte("Entertoselect")
	anchorAskUserSpaced      = []byte("Enter to select")
	anchorTrustFolder        = []byte("Quicksafetycheck")
	anchorPermissionStripped = []byte("Doyouwanttoproceed")
	anchorPermissionSpaced   = []byte("Do you want to proceed")
	anchorModelSelectStripped = []byte("Selectmodel")
	anchorModelSelectSpaced   = []byte("Select model")
	anchorPermissionsHeader   = []byte("Permissions")
	anchorPermissionsTabAllow = []byte("Allow")
	anchorPermissionsTabAsk   = []byte("Ask")
	anchorPermissionsTabDeny  = []byte("Deny")
)

// slashPickerRowRe matches a picker item-start. Identical pattern to
// pickerItemStartRe in picker.go (kept independent to avoid coupling the
// detector to the parser's internals; the patterns are documented as
// the same in both files).
var slashPickerRowRe = regexp.MustCompile(
	`\x1b\[38;5;(246|153)m/(?:\x1b\[38;5;\d+m)?[a-zA-Z]`,
)

// DetectModalClass classifies the modal/picker currently rendered in snap.
// Cheap predicate: StripANSI + StripOSC + substring matches. Use Render
// (vt10x-backed) when you need to extract content from the modal, not just
// classify it.
//
// Order of checks is significant: more-specific anchors first. The
// ask-user-question modal's "Enter to select" hint-bar phrase could
// otherwise be confused with future modals reusing the same hint, so
// classify it after mcp/agents/slash-picker which have stronger anchors.
//
// Returns ModalClassUnknown when no anchor matches (the common case at
// idle — no modal currently rendered).
func DetectModalClass(snap []byte) ModalClass {
	// slash-picker is detected on the RAW snapshot — the row anchor is an
	// SGR sequence which StripANSI would remove. Check it first so the
	// unstripped path doesn't get hit by the other anchors' matching on
	// the welcome-banner text.
	if slashPickerRowRe.Match(snap) {
		return ModalClassSlashPicker
	}
	stripped := StripOSC(StripANSI(snap))
	switch {
	case bytes.Contains(stripped, anchorMCP):
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
	default:
		return ModalClassUnknown
	}
}

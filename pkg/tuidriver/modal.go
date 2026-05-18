package tuidriver

import "bytes"

// ModalClass identifies which of claude's modal/picker UI states a PTY
// snapshot is currently rendering. Returned by DetectModalClass.
//
// Six classes have been mapped empirically across loops 1-6:
//   - permission         (spike #13 / loop 1) — tool-permission prompt
//   - trust-folder       (loop 2 B-5)         — first-use folder trust
//   - slash-picker       (loop 3 C-2 / 4 D-1) — `/` command picker
//   - ask-user-question  (loop 5 E-1)         — AskUserQuestion tool modal
//   - mcp                (loop 6 F-1)         — `/mcp` status display
//   - agents             (loop 6 F-1)         — `/agents` subagent list
type ModalClass string

const (
	ModalClassUnknown         ModalClass = ""
	ModalClassMCP             ModalClass = "mcp"
	ModalClassAgents          ModalClass = "agents"
	ModalClassSlashPicker     ModalClass = "slash-picker"
	ModalClassAskUserQuestion ModalClass = "ask-user-question"
	ModalClassTrustFolder     ModalClass = "trust-folder"
	ModalClassPermission      ModalClass = "permission"
)

// Modal-class detection anchors. Each is unique to its class — verified
// empirically across loops 1-6. CSI cursor-forward stripping eats
// inter-word spaces in some renderings (Bash-style), so the predicates
// match both space-stripped and space-preserved forms where claude varies.
//
// Anchors are unexported — consumers call DetectModalClass rather than
// matching directly. The literal forms are documented here for readers.
//
//	mcp                → "ManageMCPservers"
//	agents             → "Agents" header + "Running" or "Library" tab
//	slash-picker       → "?forshortcuts" or "? for shortcuts"
//	ask-user-question  → "Entertoselect" or "Enter to select"
//	trust-folder       → "Quicksafetycheck"
//	permission         → "Doyouwanttoproceed" or "Do you want to proceed"
var (
	anchorMCP                = []byte("ManageMCPservers")
	anchorAgentsHeader       = []byte("Agents")
	anchorAgentsTabRunning   = []byte("Running")
	anchorAgentsTabLibrary   = []byte("Library")
	anchorSlashPickerStripped = []byte("?forshortcuts")
	anchorSlashPickerSpaced   = []byte("? for shortcuts")
	anchorAskUserStripped     = []byte("Entertoselect")
	anchorAskUserSpaced       = []byte("Enter to select")
	anchorTrustFolder         = []byte("Quicksafetycheck")
	anchorPermissionStripped  = []byte("Doyouwanttoproceed")
	anchorPermissionSpaced    = []byte("Do you want to proceed")
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
	stripped := StripOSC(StripANSI(snap))
	switch {
	case bytes.Contains(stripped, anchorMCP):
		return ModalClassMCP
	case bytes.Contains(stripped, anchorAgentsHeader) &&
		(bytes.Contains(stripped, anchorAgentsTabRunning) ||
			bytes.Contains(stripped, anchorAgentsTabLibrary)):
		return ModalClassAgents
	case bytes.Contains(stripped, anchorSlashPickerStripped) ||
		bytes.Contains(stripped, anchorSlashPickerSpaced):
		return ModalClassSlashPicker
	case bytes.Contains(stripped, anchorAskUserStripped) ||
		bytes.Contains(stripped, anchorAskUserSpaced):
		return ModalClassAskUserQuestion
	case bytes.Contains(stripped, anchorTrustFolder):
		return ModalClassTrustFolder
	case bytes.Contains(stripped, anchorPermissionStripped) ||
		bytes.Contains(stripped, anchorPermissionSpaced):
		return ModalClassPermission
	default:
		return ModalClassUnknown
	}
}

package tuidriver

import "strings"

// AgentList represents claude's `/agents` modal contents, parsed from a
// PTY snapshot. Loop 6 F-1 deliverable.
//
// The modal has a tabbed shape: Running shows currently-active subagents
// (empty most of the time); Library shows configured agents that can be
// invoked. ParseAgentList captures whichever tab is currently visible.
type AgentList struct {
	Tabs       []string `json:"tabs"`                 // always ["Running", "Library"]
	CurrentTab string   `json:"current_tab"`          // "Running" or "Library"
	Items      []Agent  `json:"items"`                // items in the current tab
	EmptyText  string   `json:"empty_text,omitempty"` // populated when the tab is empty
}

// Agent is one row in the /agents Library or Running tab.
type Agent struct {
	Name        string `json:"name"`
	Highlighted bool   `json:"highlighted"` // ❯ marker present
}

// ParseAgentList extracts /agents modal contents into structured form.
//
// Layout observed (loop 6 F-1):
//
//	Agents  Running   Library                  ← tab bar
//	No subagents are currently running.        ← empty-state for Running tab
//	←/→ to switch · ↑/↓ to navigate · Enter to select · Esc to close
//
// At F-1 capture time the Running tab was empty. Library-tab item parsing
// is sketched but requires sending `→` and re-snapshotting to populate;
// deferred to a future probe.
//
// Uses Render (vt10x-backed) so the tab-bar's column-aligned layout
// renders correctly.
//
// Returns a non-nil AgentList even for non-modal input; check CurrentTab
// or Tabs to detect "no parse."
func ParseAgentList(snap []byte) *AgentList {
	text := Render(snap, DefaultGridCols, DefaultGridRows)
	lines := strings.Split(text, "\n")
	for i := range lines {
		l := lines[i]
		for strings.Contains(l, "  ") {
			l = strings.ReplaceAll(l, "  ", " ")
		}
		lines[i] = strings.TrimSpace(l)
	}

	agents := &AgentList{Tabs: []string{"Running", "Library"}}

	for _, line := range lines {
		if line == "" {
			continue
		}
		// Tab bar — contains "Agents" + at least one of Running / Library.
		// The tab listed first after the header is the currently-selected
		// one (empirical rendering convention).
		if strings.HasPrefix(line, "Agents") &&
			(strings.Contains(line, "Running") || strings.Contains(line, "Library")) {
			if strings.Index(line, "Running") < strings.Index(line, "Library") {
				agents.CurrentTab = "Running"
			} else {
				agents.CurrentTab = "Library"
			}
			continue
		}
		// Empty-state text for the Running tab.
		if strings.Contains(line, "subagents") && strings.Contains(line, "running") {
			agents.EmptyText = line
			continue
		}
	}
	return agents
}

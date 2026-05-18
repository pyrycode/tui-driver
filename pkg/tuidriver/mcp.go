package tuidriver

import (
	"fmt"
	"regexp"
	"strings"
)

// McpStatus represents claude's `/mcp` modal contents, parsed from a PTY
// snapshot. Loop 6 F-1 deliverable.
type McpStatus struct {
	TotalServers int        `json:"total_servers"`
	Categories   []McpGroup `json:"categories"`
}

// McpGroup is one category in the /mcp display — claude organises MCP
// servers into "Project MCPs", "User MCPs", and "Built-in MCPs" sections.
type McpGroup struct {
	Name    string      `json:"name"`           // canonical: "Project MCPs", "User MCPs", "Built-in MCPs"
	Path    string      `json:"path,omitempty"` // parenthetical context ("/path/to/.mcp.json", "(always available)")
	Servers []McpServer `json:"servers"`
}

// McpServer is one MCP server in the /mcp listing.
//
// Status values:
//
//	"connecting"     — connection in progress (◯ connecting…)
//	"connected"      — ready, tool count populated (✔ed · N tools)
//	"disabled"       — explicitly disabled (◯ disabled)
//	"auth_required"  — needs authentication (△ needs authentic…)
//	"failed"         — connection failed (✘ failed)
//	"unknown"        — status line didn't match any known indicator
type McpServer struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	ToolCount   int    `json:"tool_count,omitempty"` // populated when status="connected"
	Highlighted bool   `json:"highlighted"`          // ❯ marker present
}

// Pre-compiled regexes used by ParseMcpStatus.
var (
	mcpTotalServersRe   = regexp.MustCompile(`^(\d+)\s*servers?$`)
	mcpCategoryHeaderRe = regexp.MustCompile(`^(Project|User|Built-in)\s*MCPs\s*(.*)$`)
	mcpToolCountRe      = regexp.MustCompile(`(\d+)\s*tools?`)
)

// ParseMcpStatus extracts /mcp modal contents into structured form.
//
// Layout observed (loop 6 F-1):
//
//	Manage MCP servers
//	N servers                                ← total count
//	Project MCPs (/path/to/.mcp.json)        ← category header
//	❯ <name> · <status indicator>            ← ❯ marker = highlighted
//	  <name> · <status indicator>
//	User MCPs (/path/to/.claude.json)        ← next category
//	  <name> · <status indicator>
//	Built-in MCPs (always available)
//	  <name> · <status indicator>
//	https://...                              ← footer URL (ignored)
//	↑/↓ to navigate · Enter to confirm · Esc to cancel (ignored)
//
// Uses Render (vt10x-backed) so column-aligned server names survive
// claude's cursor-positioning escapes. The previous naive ANSI-strip path
// produced truncated names like "claude.ai Gmail" → "laude.ai Gmail"
// (Open Question resolved 2026-05-18).
//
// Returns a non-nil McpStatus even for empty input; check len(Categories)
// or TotalServers to detect "no parse."
func ParseMcpStatus(snap []byte) *McpStatus {
	text := Render(snap, DefaultGridCols, DefaultGridRows)
	lines := strings.Split(text, "\n")
	for i := range lines {
		l := lines[i]
		for strings.Contains(l, "  ") {
			l = strings.ReplaceAll(l, "  ", " ")
		}
		lines[i] = strings.TrimSpace(l)
	}

	status := &McpStatus{}
	var currentGroup *McpGroup

	// Re-renders happen as MCP servers progressively connect. Dedup by
	// canonical category name and by server name within a category.
	seenCategory := map[string]bool{}

	for _, line := range lines {
		if line == "" {
			continue
		}
		// Skip hint-bar lines and URL footers — they're noise relative to
		// the structured content.
		if strings.Contains(line, "↑/↓") || strings.Contains(line, "Esc to cancel") ||
			strings.HasPrefix(line, "https://") {
			continue
		}
		// Total-server count: last-wins so the final count after all
		// re-renders is captured.
		if m := mcpTotalServersRe.FindStringSubmatch(line); m != nil {
			fmt.Sscanf(m[1], "%d", &status.TotalServers)
			continue
		}
		// Category header — tolerate missing space between word and "MCPs"
		// from cursor-forward rendering.
		if m := mcpCategoryHeaderRe.FindStringSubmatch(line); m != nil {
			canonical := m[1] + " MCPs"
			if !seenCategory[canonical] {
				seenCategory[canonical] = true
				status.Categories = append(status.Categories, McpGroup{
					Name: canonical,
					Path: strings.TrimSpace(m[2]),
				})
			}
			for i := range status.Categories {
				if status.Categories[i].Name == canonical {
					currentGroup = &status.Categories[i]
					break
				}
			}
			continue
		}
		// Item line: optional ❯, name, ·, status.
		if strings.Contains(line, "·") && currentGroup != nil {
			highlighted := strings.HasPrefix(line, "❯")
			body := strings.TrimPrefix(line, "❯")
			body = strings.TrimSpace(body)
			idx := strings.Index(body, "·")
			if idx < 0 {
				continue
			}
			name := strings.TrimSpace(body[:idx])
			statusPart := strings.TrimSpace(body[idx+len("·"):])

			srv := McpServer{Name: name, Highlighted: highlighted}
			switch {
			case strings.Contains(statusPart, "connecting"):
				srv.Status = "connecting"
			case strings.Contains(statusPart, "disabled"):
				srv.Status = "disabled"
			case strings.Contains(statusPart, "✔"):
				srv.Status = "connected"
				if tm := mcpToolCountRe.FindStringSubmatch(statusPart); tm != nil {
					fmt.Sscanf(tm[1], "%d", &srv.ToolCount)
				}
			case strings.Contains(statusPart, "△"):
				srv.Status = "auth_required"
			case strings.Contains(statusPart, "✘"):
				srv.Status = "failed"
			default:
				srv.Status = "unknown"
			}
			if srv.Name == "" {
				continue
			}
			// Phantom rows: claude sometimes re-renders just the status
			// glyph string for a server whose name appeared earlier.
			if strings.HasPrefix(srv.Name, "✔") || strings.HasPrefix(srv.Name, "✘") ||
				strings.HasPrefix(srv.Name, "△") || strings.HasPrefix(srv.Name, "◯") {
				continue
			}
			// Dedupe within group; latest observation wins (most-resolved
			// status).
			replaced := false
			for i := range currentGroup.Servers {
				if currentGroup.Servers[i].Name == srv.Name {
					currentGroup.Servers[i] = srv
					replaced = true
					break
				}
			}
			if !replaced {
				currentGroup.Servers = append(currentGroup.Servers, srv)
			}
		}
	}
	return status
}

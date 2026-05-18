package tuidriver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseMcpStatusEmpty(t *testing.T) {
	got := ParseMcpStatus(nil)
	if got == nil {
		t.Fatal("ParseMcpStatus(nil) = nil, want non-nil empty status")
	}
	if got.TotalServers != 0 || len(got.Categories) != 0 {
		t.Errorf("ParseMcpStatus(nil) = %+v, want empty", got)
	}
}

func TestParseMcpStatusRealFixture(t *testing.T) {
	snap, err := os.ReadFile(filepath.Join("testdata", "mcp-snapshot.bin"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	status := ParseMcpStatus(snap)
	if status == nil {
		t.Fatal("ParseMcpStatus = nil")
	}

	// Fixture captured 2026-05-18 from a vault-cwd run. Expected shape:
	// 10 servers across 3 categories. Most servers connecting/connected
	// at capture time; one auth_required, one failed, one disabled.
	if status.TotalServers != 10 {
		t.Errorf("TotalServers = %d, want 10", status.TotalServers)
	}

	if len(status.Categories) != 3 {
		t.Fatalf("Categories count = %d, want 3; categories=%+v", len(status.Categories), status.Categories)
	}
	wantCategories := []string{"Project MCPs", "User MCPs", "Built-in MCPs"}
	for i, want := range wantCategories {
		if status.Categories[i].Name != want {
			t.Errorf("Categories[%d].Name = %q, want %q", i, status.Categories[i].Name, want)
		}
	}

	// Critical: column-positioning truncation regression. The User MCPs
	// category contains the "claude.ai *" family. The names must NOT be
	// truncated.
	var userMcps *McpGroup
	for i := range status.Categories {
		if status.Categories[i].Name == "User MCPs" {
			userMcps = &status.Categories[i]
			break
		}
	}
	if userMcps == nil {
		t.Fatal("User MCPs category missing")
	}
	wantNames := map[string]bool{
		"claude.ai Gmail":           false,
		"claude.ai Google Calendar": false,
		"claude.ai Google Drive":    false,
	}
	for _, srv := range userMcps.Servers {
		if _, ok := wantNames[srv.Name]; ok {
			wantNames[srv.Name] = true
		}
	}
	for name, found := range wantNames {
		if !found {
			t.Errorf("expected User-MCP server %q not found; got %+v", name, userMcps.Servers)
		}
	}

	// Built-in MCPs should have at least one connected server with
	// ToolCount > 0 (e.g. plugin:figma:figma · 18 tools at capture time).
	var builtIn *McpGroup
	for i := range status.Categories {
		if status.Categories[i].Name == "Built-in MCPs" {
			builtIn = &status.Categories[i]
			break
		}
	}
	if builtIn == nil {
		t.Fatal("Built-in MCPs category missing")
	}
	sawConnectedWithTools := false
	for _, srv := range builtIn.Servers {
		if srv.Status == "connected" && srv.ToolCount > 0 {
			sawConnectedWithTools = true
			break
		}
	}
	if !sawConnectedWithTools {
		t.Errorf("Built-in MCPs had no connected server with tool count; servers=%+v", builtIn.Servers)
	}

	// Phantom-row filter: no server name should start with a status glyph.
	for _, cat := range status.Categories {
		for _, srv := range cat.Servers {
			for _, prefix := range []string{"✔", "✘", "△", "◯"} {
				if len(srv.Name) > 0 && srv.Name[:len(prefix)] == prefix {
					t.Errorf("phantom server name %q leaked through filter", srv.Name)
				}
			}
		}
	}
}

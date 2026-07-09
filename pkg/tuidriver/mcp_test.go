package tuidriver

import (
	"fmt"
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

// statusRepaints builds ~2 KB of representative in-place status repaints: the
// small, cursor-addressed frames a live panel emits while it sits on screen
// (spinner / token-counter updates). Each frame addresses only the bottom
// status row (row 40) — move-to-bottom + clear-line + a short spinner line —
// so a rendered grid still shows the panel body above. Deliberately NO
// screen-home / full-screen redraw: a full redraw would reconstruct the panel
// even after eviction and destroy the negative-control property. Variation is a
// plain counter (no time/rand) so the payload is deterministic.
func statusRepaints(frames int) []byte {
	var b []byte
	for i := 0; i < frames; i++ {
		// \x1b[40;1H — cursor to bottom row, col 1; \x1b[2K — clear that line.
		frame := fmt.Sprintf("\x1b[40;1H\x1b[2K✻ Thinking… (%ds · esc to interrupt)", i)
		b = append(b, frame...)
	}
	return b
}

// TestParseMcpStatusSurvivesRepaintsAtDefaultCap is the #246 regression guard:
// a full-screen mcp panel that fills the window must keep classifying for as
// long as it is physically on screen while ordinary status repaints trickle in.
// It drives bytes through the real Buffer (exercising the actual trim path) at
// two caps — the new default (16384, via NewBuffer(0)) and the pre-change 4096
// — with an identical append sequence. The two-cap structure is what gives the
// test value: the panel survives only at the larger cap, proving the ~2 KB of
// repaints genuinely crosses the eviction boundary. A future revert of
// DefaultBufferCap to 4096 makes the positive case fail too, since it goes
// through NewBuffer(0).
func TestParseMcpStatusSurvivesRepaintsAtDefaultCap(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "mcp-snapshot.bin"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	repaints := statusRepaints(40)
	if len(repaints) < 1400 {
		t.Fatalf("repaint payload too small (%d B) to cross the 4096 eviction boundary", len(repaints))
	}

	// Positive: the new default cap (16384). The repaints overwrite only the
	// bottom row (which ParseMcpStatus ignores); the panel body survives.
	b := NewBuffer(0) // DefaultBufferCap = 16384
	b.Append(fixture)
	b.Append(repaints)
	if got, want := len(b.Snapshot()), len(fixture)+len(repaints); got != want {
		t.Errorf("default cap trimmed unexpectedly: len(Snapshot) = %d, want %d", got, want)
	}
	status := ParseMcpStatus(b.Snapshot())
	if status.TotalServers != 10 {
		t.Errorf("at default cap: TotalServers = %d, want 10 (panel should still classify)", status.TotalServers)
	}
	if len(status.Categories) != 3 {
		t.Errorf("at default cap: Categories = %d, want 3", len(status.Categories))
	}

	// Negative control: the pre-change cap (4096, as a literal). Same appended
	// bytes. The ~2 KB of repaints evicts the front of the 4096-B fixture —
	// including the header and "N servers" count — before Render sees them, so
	// classification degrades. This is what proves the boundary is crossed; if
	// the panel classified at both caps the test would prove nothing.
	b4 := NewBuffer(4096)
	b4.Append(fixture)
	b4.Append(repaints)
	if got := len(b4.Snapshot()); got != 4096 {
		t.Errorf("old cap did not trim to 4096: len(Snapshot) = %d", got)
	}
	status4 := ParseMcpStatus(b4.Snapshot())
	t.Logf("negative control at 4096: TotalServers=%d Categories=%d", status4.TotalServers, len(status4.Categories))
	if status4.TotalServers == 10 {
		t.Errorf("at old 4096 cap: TotalServers = 10, want degraded — the repaint payload did not cross the eviction boundary")
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

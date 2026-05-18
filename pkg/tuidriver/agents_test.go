package tuidriver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAgentListEmpty(t *testing.T) {
	got := ParseAgentList(nil)
	if got == nil {
		t.Fatal("ParseAgentList(nil) = nil, want non-nil")
	}
	wantTabs := []string{"Running", "Library"}
	if len(got.Tabs) != len(wantTabs) {
		t.Errorf("Tabs = %v, want %v", got.Tabs, wantTabs)
	}
	for i := range wantTabs {
		if got.Tabs[i] != wantTabs[i] {
			t.Errorf("Tabs[%d] = %q, want %q", i, got.Tabs[i], wantTabs[i])
		}
	}
	if got.CurrentTab != "" {
		t.Errorf("CurrentTab = %q, want \"\" (no parse from nil input)", got.CurrentTab)
	}
}

func TestParseAgentListRealFixture(t *testing.T) {
	snap, err := os.ReadFile(filepath.Join("testdata", "agents-snapshot.bin"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	got := ParseAgentList(snap)
	if got == nil {
		t.Fatal("ParseAgentList = nil")
	}

	if got.CurrentTab != "Running" {
		t.Errorf("CurrentTab = %q, want Running (fixture captured on Running tab)", got.CurrentTab)
	}
	if !strings.Contains(got.EmptyText, "subagents") || !strings.Contains(got.EmptyText, "running") {
		t.Errorf("EmptyText = %q, want a message containing 'subagents' + 'running'", got.EmptyText)
	}
}

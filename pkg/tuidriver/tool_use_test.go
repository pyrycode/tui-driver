package tuidriver

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestParseToolUseReturnsNilOnNon(t *testing.T) {
	cases := []struct {
		name string
		snap []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"garbage", []byte("not json at all")},
		{"json non-object", []byte(`[1,2,3]`)},
		{"user envelope", []byte(`{"type":"user","message":{"content":[{"type":"text","text":"hi"}]}}`)},
		{"assistant nil message", []byte(`{"type":"assistant"}`)},
		{"assistant plain text", []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"reply"}]}}`)},
		{"assistant thinking only", []byte(`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"}]}}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseToolUse(tc.snap); got != nil {
				t.Errorf("ParseToolUse(%s) = %+v, want nil", tc.name, got)
			}
		})
	}
}

func TestParseToolUseProjectsFields(t *testing.T) {
	cases := []struct {
		name     string
		snap     []byte
		wantID   string
		wantName string
		check    func(t *testing.T, tu *ToolUse)
	}{
		{
			name:     "bash tool_use carries id/name/input",
			snap:     []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls /tmp"}}]}}`),
			wantID:   "toolu_1",
			wantName: "Bash",
			check: func(t *testing.T, tu *ToolUse) {
				if got, _ := tu.Input["command"].(string); got != "ls /tmp" {
					t.Errorf("Input[command] = %q, want %q", got, "ls /tmp")
				}
			},
		},
		{
			// Name-agnostic: any tool projects, not just AskUserQuestion.
			name:     "read tool_use projects too",
			snap:     []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_r","name":"Read","input":{"file_path":"/etc/hosts"}}]}}`),
			wantID:   "toolu_r",
			wantName: "Read",
			check: func(t *testing.T, tu *ToolUse) {
				if got, _ := tu.Input["file_path"].(string); got != "/etc/hosts" {
					t.Errorf("Input[file_path] = %q, want %q", got, "/etc/hosts")
				}
			},
		},
		{
			name:     "missing name yields zero-value Name",
			snap:     []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_2","input":{"command":"ls"}}]}}`),
			wantID:   "toolu_2",
			wantName: "",
		},
		{
			name:     "missing id yields zero-value ID",
			snap:     []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`),
			wantID:   "",
			wantName: "Bash",
		},
		{
			name:     "missing input yields nil Input",
			snap:     []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_3","name":"Bash"}]}}`),
			wantID:   "toolu_3",
			wantName: "Bash",
			check: func(t *testing.T, tu *ToolUse) {
				if tu.Input != nil {
					t.Errorf("Input = %v, want nil", tu.Input)
				}
			},
		},
		{
			// Type-mismatch must not panic; Name falls to "".
			name:     "non-string name yields zero-value Name",
			snap:     []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_4","name":42,"input":{"command":"ls"}}]}}`),
			wantID:   "toolu_4",
			wantName: "",
		},
		{
			// A text block ahead of the tool_use must be skipped, not matched.
			name:     "text block before tool_use is skipped",
			snap:     []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"sure"},{"type":"tool_use","id":"toolu_5","name":"Bash","input":{"command":"pwd"}}]}}`),
			wantID:   "toolu_5",
			wantName: "Bash",
		},
		{
			// First tool_use block wins; later blocks are ignored.
			name:     "first tool_use wins",
			snap:     []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_first","name":"Bash","input":{"command":"a"}},{"type":"tool_use","id":"toolu_second","name":"Read","input":{"file_path":"b"}}]}}`),
			wantID:   "toolu_first",
			wantName: "Bash",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseToolUse(tc.snap)
			if got == nil {
				t.Fatalf("ParseToolUse returned nil, want non-nil")
			}
			if got.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", got.ID, tc.wantID)
			}
			if got.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tc.wantName)
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

// Safe on the zero value — never panics (AC4).
func TestParseToolUseZeroValueInputSafe(t *testing.T) {
	if got := ParseToolUse(nil); got != nil {
		t.Errorf("ParseToolUse(nil) = %+v, want nil", got)
	}
}

func TestParseToolUseRealFixture(t *testing.T) {
	snap, err := os.ReadFile(filepath.Join("testdata", "tool-use-snapshot.bin"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// Sanity: confirm the fixture carries the wire-shape substrings we
	// depend on. Guards against a future re-record silently dropping the
	// tool_use block or renaming its fields. A non-AskUserQuestion tool
	// proves name-agnostic projection.
	for _, marker := range []string{`"type":"tool_use"`, `"name":"Bash"`, `"id":`} {
		if !bytes.Contains(snap, []byte(marker)) {
			t.Fatalf("fixture missing wire marker %s", marker)
		}
	}

	got := ParseToolUse(snap)
	if got == nil {
		t.Fatalf("ParseToolUse returned nil on real fixture")
	}
	if got.Name == "" {
		t.Errorf("Name is empty, want the tool name")
	}
	if got.ID == "" {
		t.Errorf("ID is empty, want the block id")
	}
	if got.Input == nil {
		t.Fatalf("Input is nil, want the tool's input object")
	}
	if cmd, _ := got.Input["command"].(string); cmd == "" {
		t.Errorf("Input[command] is empty, want the Bash command string")
	}
}

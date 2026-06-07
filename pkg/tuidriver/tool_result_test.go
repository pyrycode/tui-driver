package tuidriver

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestParseToolResultReturnsNilOnNon(t *testing.T) {
	cases := []struct {
		name string
		snap []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"garbage", []byte("not json at all")},
		{"json non-object", []byte(`[1,2,3]`)},
		// The user-gate divergence: a well-formed tool_result block on an
		// assistant envelope must NOT match (tool_result rides user envelopes).
		{"assistant carrying tool_result", []byte(`{"type":"assistant","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":false,"content":"out"}]}}`)},
		{"user nil message", []byte(`{"type":"user"}`)},
		{"user plain text only", []byte(`{"type":"user","message":{"content":[{"type":"text","text":"hi"}]}}`)},
		// The cancel marker is a user(text) envelope — the content-type walk,
		// not an envelope-only gate, is what rejects it.
		{"user cancel marker", []byte(`{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseToolResult(tc.snap); got != nil {
				t.Errorf("ParseToolResult(%s) = %+v, want nil", tc.name, got)
			}
		})
	}
}

func TestParseToolResultProjectsFields(t *testing.T) {
	cases := []struct {
		name          string
		snap          []byte
		wantToolUseID string
		wantIsError   bool
		check         func(t *testing.T, tr *ToolResult)
	}{
		{
			name:          "string content carries fields verbatim",
			snap:          []byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":false,"content":"file1\nfile2\n"}]}}`),
			wantToolUseID: "toolu_1",
			wantIsError:   false,
			check: func(t *testing.T, tr *ToolResult) {
				if got, _ := tr.Content.(string); got != "file1\nfile2\n" {
					t.Errorf("Content = %q, want %q", got, "file1\nfile2\n")
				}
			},
		},
		{
			// The union's array arm is preserved as []any, not flattened.
			name:          "array content preserved as []any",
			snap:          []byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_a","is_error":false,"content":[{"type":"text","text":"out"}]}]}}`),
			wantToolUseID: "toolu_a",
			wantIsError:   false,
			check: func(t *testing.T, tr *ToolResult) {
				arr, ok := tr.Content.([]any)
				if !ok {
					t.Fatalf("Content type = %T, want []any", tr.Content)
				}
				if len(arr) != 1 {
					t.Errorf("len(Content) = %d, want 1", len(arr))
				}
			},
		},
		{
			name:          "is_error true projects true",
			snap:          []byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_e","is_error":true,"content":"boom"}]}}`),
			wantToolUseID: "toolu_e",
			wantIsError:   true,
		},
		{
			name:          "missing tool_use_id yields zero-value ID",
			snap:          []byte(`{"type":"user","message":{"content":[{"type":"tool_result","is_error":false,"content":"x"}]}}`),
			wantToolUseID: "",
			wantIsError:   false,
		},
		{
			name:          "missing is_error yields false",
			snap:          []byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_n","content":"x"}]}}`),
			wantToolUseID: "toolu_n",
			wantIsError:   false,
		},
		{
			name:          "missing content yields nil Content",
			snap:          []byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_c","is_error":false}]}}`),
			wantToolUseID: "toolu_c",
			wantIsError:   false,
			check: func(t *testing.T, tr *ToolResult) {
				if tr.Content != nil {
					t.Errorf("Content = %v, want nil", tr.Content)
				}
			},
		},
		{
			// Type-mismatch must not panic; IsError falls to false.
			name:          "non-bool is_error yields false",
			snap:          []byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_b","is_error":"yes","content":"x"}]}}`),
			wantToolUseID: "toolu_b",
			wantIsError:   false,
		},
		{
			// Type-mismatch must not panic; ToolUseID falls to "".
			name:          "non-string tool_use_id yields empty ID",
			snap:          []byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":42,"is_error":false,"content":"x"}]}}`),
			wantToolUseID: "",
			wantIsError:   false,
		},
		{
			// A text block ahead of the tool_result must be skipped, not matched.
			name:          "text block before tool_result is skipped",
			snap:          []byte(`{"type":"user","message":{"content":[{"type":"text","text":"sure"},{"type":"tool_result","tool_use_id":"toolu_s","is_error":false,"content":"x"}]}}`),
			wantToolUseID: "toolu_s",
			wantIsError:   false,
		},
		{
			// First tool_result block wins; later blocks are ignored.
			name:          "first tool_result wins",
			snap:          []byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_first","is_error":false,"content":"a"},{"type":"tool_result","tool_use_id":"toolu_second","is_error":true,"content":"b"}]}}`),
			wantToolUseID: "toolu_first",
			wantIsError:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseToolResult(tc.snap)
			if got == nil {
				t.Fatalf("ParseToolResult returned nil, want non-nil")
			}
			if got.ToolUseID != tc.wantToolUseID {
				t.Errorf("ToolUseID = %q, want %q", got.ToolUseID, tc.wantToolUseID)
			}
			if got.IsError != tc.wantIsError {
				t.Errorf("IsError = %v, want %v", got.IsError, tc.wantIsError)
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

// Safe on the zero value — never panics (AC4).
func TestParseToolResultZeroValueInputSafe(t *testing.T) {
	if got := ParseToolResult(nil); got != nil {
		t.Errorf("ParseToolResult(nil) = %+v, want nil", got)
	}
}

func TestParseToolResultRealFixture(t *testing.T) {
	snap, err := os.ReadFile(filepath.Join("testdata", "tool-result-snapshot.bin"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// Sanity: confirm the fixture carries the wire-shape substrings we
	// depend on. Guards against a future re-record silently dropping the
	// tool_result block or renaming its fields.
	for _, marker := range []string{`"type":"tool_result"`, `"tool_use_id":`, `"is_error":`} {
		if !bytes.Contains(snap, []byte(marker)) {
			t.Fatalf("fixture missing wire marker %s", marker)
		}
	}

	got := ParseToolResult(snap)
	if got == nil {
		t.Fatalf("ParseToolResult returned nil on real fixture")
	}
	if got.ToolUseID == "" {
		t.Errorf("ToolUseID is empty, want the referenced tool_use id")
	}
	if got.Content == nil {
		t.Fatalf("Content is nil, want the tool result payload")
	}
	if s, _ := got.Content.(string); s == "" {
		t.Errorf("Content is not the expected non-empty string, got %T", got.Content)
	}
}

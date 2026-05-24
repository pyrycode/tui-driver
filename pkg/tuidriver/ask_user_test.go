package tuidriver

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAskUserQuestionReturnsNilOnNon(t *testing.T) {
	cases := []struct {
		name string
		snap []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"garbage", []byte("not json at all")},
		{"json non-object", []byte(`[1,2,3]`)},
		{"user envelope", []byte(`{"type":"user","message":{"content":[{"type":"text","text":"hi"}]}}`)},
		{"assistant plain text", []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"reply"}]}}`)},
		{"assistant other tool", []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`)},
		{"assistant ask-user input missing", []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"AskUserQuestion"}]}}`)},
		{"assistant ask-user questions empty", []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"AskUserQuestion","input":{"questions":[]}}]}}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseAskUserQuestion(tc.snap); got != nil {
				t.Errorf("ParseAskUserQuestion(%s) = %+v, want nil", tc.name, got)
			}
		})
	}
}

func TestParseAskUserQuestionRealFixture(t *testing.T) {
	snap, err := os.ReadFile(filepath.Join("testdata", "ask-user-question-snapshot.bin"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// Sanity: confirm the fixture really does carry the wire-shape
	// substrings we depend on (camelCase multiSelect, the tool name).
	// Guards against future re-records silently swapping field names.
	if !bytes.Contains(snap, []byte(`"name":"AskUserQuestion"`)) {
		t.Fatalf("fixture missing AskUserQuestion tool_use marker")
	}
	if !bytes.Contains(snap, []byte(`"multiSelect"`)) {
		t.Fatalf("fixture missing camelCase multiSelect field marker")
	}

	got := ParseAskUserQuestion(snap)
	if got == nil {
		t.Fatalf("ParseAskUserQuestion returned nil on real fixture")
	}
	if len(got.Questions) < 1 {
		t.Fatalf("len(Questions) = %d, want >= 1", len(got.Questions))
	}

	q := got.Questions[0]
	if q.Question == "" {
		t.Errorf("Questions[0].Question is empty")
	}
	if !strings.Contains(strings.ToLower(q.Question), "language") {
		t.Errorf("Questions[0].Question = %q, want substring %q (the spike prompt asks about programming languages)", q.Question, "language")
	}
	if len(q.Options) < 2 {
		t.Fatalf("len(Questions[0].Options) = %d, want >= 2 (the spike prompt asks about three languages)", len(q.Options))
	}
	for i, opt := range q.Options {
		if opt.Label == "" {
			t.Errorf("Questions[0].Options[%d].Label is empty", i)
		}
	}
	// The default spike prompt produces a single-select. Asserting the
	// concrete value (rather than just the field's presence) means a
	// fixture re-record with a multi-select prompt forces a deliberate
	// re-examination of this assertion instead of silently passing.
	if q.MultiSelect {
		t.Errorf("Questions[0].MultiSelect = true, want false (default spike prompt is single-select)")
	}
}

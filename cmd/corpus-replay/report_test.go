package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// TestReport_RendersTransitionEdges runs the aggregate report over the flapping
// fixture and asserts the additive transition-edges section renders: its header,
// the top-flappers sub-section, and the flapping cast attributed to its detector.
func TestReport_RendersTransitionEdges(t *testing.T) {
	r, err := replayCast(filepath.Join("testdata", "flap-err.cast"), 1)
	if err != nil {
		t.Fatalf("replayCast: %v", err)
	}
	var buf bytes.Buffer
	report(&buf, []castResult{r}, "testdata", 1, false)
	out := buf.String()

	for _, want := range []string{
		"transition edges",   // section header
		"top flappers",       // top-flappers sub-header
		"flap-err",           // the flapping cast is named
		"modal:trust-folder", // flapping attributed to the class
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report output missing %q:\n%s", want, out)
		}
	}
}

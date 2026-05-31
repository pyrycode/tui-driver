package main

import (
	"errors"
	"strings"
	"testing"
)

func TestEnrichMissingSidecar(t *testing.T) {
	base := errors.New("read parsed-json /tmp/x.parsed.json: no such file")
	cases := []struct {
		name      string
		stderr    string
		wantClass string // substring expected in the enriched message; "" means no enrichment
	}{
		{
			name:      "mcp class scraped",
			stderr:    "2026/05/31 16:46:23 modal-class detected=mcp\n",
			wantClass: `"mcp"`,
		},
		{
			name:      "unknown class (empty string) renders as Unknown",
			stderr:    "2026/05/31 16:46:23 modal-class detected=\n",
			wantClass: `"Unknown"`,
		},
		{
			name:      "slash-picker class scraped",
			stderr:    "noise\nmodal-class detected=slash-picker\nmore noise\n",
			wantClass: `"slash-picker"`,
		},
		{
			name:      "no modal-class line falls back to bare error",
			stderr:    "spike crashed before classifying\n",
			wantClass: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := enrichMissingSidecar(base, []byte(tc.stderr))
			if tc.wantClass == "" {
				// No scrape match — must return the original error verbatim.
				if got != base {
					t.Errorf("enrichMissingSidecar() = %q, want unchanged base %q", got, base)
				}
				return
			}
			msg := got.Error()
			if !strings.Contains(msg, tc.wantClass) {
				t.Errorf("enriched message %q does not contain class token %q", msg, tc.wantClass)
			}
			// The enrichment must preserve the original error text and the
			// classifier-drift framing, and must wrap (not replace) base.
			if !strings.Contains(msg, base.Error()) {
				t.Errorf("enriched message %q dropped the original read error %q", msg, base.Error())
			}
			if !strings.Contains(msg, "classifier drift, not a content diff") {
				t.Errorf("enriched message %q missing the classifier-drift framing", msg)
			}
			if !errors.Is(got, base) {
				t.Errorf("enriched error does not wrap base (errors.Is == false)")
			}
		})
	}
}

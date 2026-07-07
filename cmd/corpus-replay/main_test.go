package main

import (
	"path/filepath"
	"testing"
)

// TestReplayCast_RealTrustDialogFires replays a fixture whose middle frame is a
// genuine trust dialog (header plus a pointer-marked numbered option row). The
// structural trust class must fire, idle must fire, and the trust header anchor
// must be seen in content.
func TestReplayCast_RealTrustDialogFires(t *testing.T) {
	r, err := replayCast(filepath.Join("testdata", "sample-ok.cast"), 1)
	if err != nil {
		t.Fatalf("replayCast: %v", err)
	}
	if r.tag != "ok" {
		t.Errorf("tag = %q, want ok", r.tag)
	}
	if r.cols != 120 || r.rows != 40 {
		t.Errorf("dims = %dx%d, want 120x40", r.cols, r.rows)
	}
	if !r.fired["idle"] {
		t.Errorf("idle did not fire")
	}
	if !r.fired["modal:trust-folder"] {
		t.Errorf("genuine trust dialog did not classify as trust-folder")
	}
	if !r.anchors["trust-header"] {
		t.Errorf("trust-header anchor not seen in content")
	}
	if r.fired["network-failure"] {
		t.Errorf("network-failure fired unexpectedly")
	}
}

// TestReplayCast_ForgedHeaderAndRetiredTokenSuppressed replays a fixture that
// only ever quotes the trust header (no option row) and the retired network
// token, exactly the shape of the two 2026-07-07 aborts. Both anchors must be
// seen in content, but neither structural detector may fire — the co-signal /
// re-anchor suppression the review's fixes installed.
func TestReplayCast_ForgedHeaderAndRetiredTokenSuppressed(t *testing.T) {
	r, err := replayCast(filepath.Join("testdata", "forgery-err.cast"), 1)
	if err != nil {
		t.Fatalf("replayCast: %v", err)
	}
	if r.tag != "err" {
		t.Errorf("tag = %q, want err", r.tag)
	}
	if !r.fired["idle"] {
		t.Errorf("idle did not fire")
	}
	if r.fired["modal:trust-folder"] {
		t.Errorf("forged trust header (no option row) classified as trust — #219 suppression failed")
	}
	if r.fired["network-failure"] {
		t.Errorf("retired network token classified as network-failure — #220 suppression failed")
	}
	if !r.anchors["trust-header"] {
		t.Errorf("trust-header anchor not seen in content")
	}
	if !r.anchors["network-retired"] {
		t.Errorf("network-retired anchor not seen in content")
	}
}

func TestTagFromName(t *testing.T) {
	cases := map[string]string{
		"20260707T-uuid-ok.cast":  "ok",
		"20260707T-uuid-err.cast": "err",
		"weird-name.cast":         "untagged",
	}
	for in, want := range cases {
		if got := tagFromName(in); got != want {
			t.Errorf("tagFromName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSegmentOf(t *testing.T) {
	cases := map[string]string{
		"running /private/tmp/claude-501/TestRealClaude_Foo/001": "e2e",
		"cwd /Users/x/Workspace/Projects/foo-agents/.worktree-3": "prod",
		"nothing distinctive here":                               "unknown",
	}
	for in, want := range cases {
		if got := segmentOf(in); got != want {
			t.Errorf("segmentOf(%q) = %q, want %q", in, got, want)
		}
	}
}

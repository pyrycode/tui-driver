package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pure core (classifyCapture + diagnose) is the claude-free deliverable
// gated by `make check`. Each case reads a committed synthetic capture under
// testdata/ (the corpus-replay/testdata shape) and asserts the outcome + the
// root-cause fork. The live observation — an actually-reproduced flake — is the
// operator's post-merge job; it cannot run here (no live claude).

func classifyFixture(t *testing.T, name string) instanceOutcome {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return classifyCapture(data)
}

// A capture whose stdout printed ^OBSERVED is a pass.
func TestClassify_Pass(t *testing.T) {
	o := classifyFixture(t, "pass.capture")
	if o.outcome != outcomePass {
		t.Errorf("outcome = %q, want %q", o.outcome, outcomePass)
	}
	if got := diagnose(o); got.cause != causeInconclusive {
		t.Errorf("diagnose(pass).cause = %q, want %q (nothing to diagnose)", got.cause, causeInconclusive)
	}
}

// A flake whose PTY stream drained a modal anchor well before the deadline is a
// detection bug (c) — the mirror==buffer guarantee means the detector, not the
// render, failed. This is the security-relevant fork.
func TestClassify_DetectionBugC(t *testing.T) {
	o := classifyFixture(t, "flake-detection-c.capture")
	if o.outcome != outcomeFlake {
		t.Fatalf("outcome = %q, want %q", o.outcome, outcomeFlake)
	}
	if o.modalSeenAtMs < 0 {
		t.Errorf("modalSeenAtMs = never, want an early ms (anchor was in the stream)")
	}
	if got := diagnose(o); got.cause != causeDetectionC {
		t.Errorf("diagnose.cause = %q, want %q; evidence: %s", got.cause, causeDetectionC, got.evidence)
	}
	if got := sharedWith185(causeDetectionC); got == "" {
		t.Errorf("sharedWith185(detection-c) returned empty")
	}
}

// A CSI-split render (Do\x1b[1Cyou\x1b[1C…) must match the no-space
// Doyouwanttoproceed anchor after StripANSI — the Bash-tool path the spike
// handles at :150-168. This pins the ANSI-strip step in the classifier.
func TestClassify_CSISplitAnchorMatches(t *testing.T) {
	o := classifyFixture(t, "flake-detection-c-csi.capture")
	if o.outcome != outcomeFlake {
		t.Fatalf("outcome = %q, want %q", o.outcome, outcomeFlake)
	}
	if o.modalSeenAtMs < 0 {
		t.Errorf("modalSeenAtMs = never; the CSI-split anchor should match after StripANSI")
	}
	if got := diagnose(o); got.cause != causeDetectionC {
		t.Errorf("diagnose.cause = %q, want %q", got.cause, causeDetectionC)
	}
}

// A flake with no anchor and steady small gaps is contention (a): claude too
// starved to render, byte flow steady but slow.
func TestClassify_ContentionA(t *testing.T) {
	o := classifyFixture(t, "flake-contention-a.capture")
	if o.outcome != outcomeFlake {
		t.Fatalf("outcome = %q, want %q", o.outcome, outcomeFlake)
	}
	if o.modalSeenAtMs >= 0 {
		t.Errorf("modalSeenAtMs = %d, want never (no anchor in this capture)", o.modalSeenAtMs)
	}
	if got := diagnose(o); got.cause != causeContentionA {
		t.Errorf("diagnose.cause = %q, want %q; maxByteGapMs=%d", got.cause, causeContentionA, o.maxByteGapMs)
	}
}

// A flake with an isolated large byte-gap is PTY starvation (b): bytes pooled
// behind a starved reader, then burst.
func TestClassify_PtyStarvationB(t *testing.T) {
	o := classifyFixture(t, "flake-pty-starvation-b.capture")
	if o.outcome != outcomeFlake {
		t.Fatalf("outcome = %q, want %q", o.outcome, outcomeFlake)
	}
	if got := diagnose(o); got.cause != causePtyStarvationB {
		t.Errorf("diagnose.cause = %q, want %q; maxByteGapMs=%d", got.cause, causePtyStarvationB, o.maxByteGapMs)
	}
}

// A flake with a middling gap and no early anchor is load-induced but the
// (a)-vs-(b) split is not separable from a black-box capture.
func TestClassify_LoadAmbiguous(t *testing.T) {
	o := classifyFixture(t, "flake-ambiguous.capture")
	if o.outcome != outcomeFlake {
		t.Fatalf("outcome = %q, want %q", o.outcome, outcomeFlake)
	}
	if got := diagnose(o); got.cause != causeLoadAmbiguous {
		t.Errorf("diagnose.cause = %q, want %q; maxByteGapMs=%d", got.cause, causeLoadAmbiguous, o.maxByteGapMs)
	}
}

func TestParseCaptureLine(t *testing.T) {
	cases := []struct {
		line    string
		wantMs  int64
		wantTxt string
		wantOK  bool
	}{
		{"1000\tprobe=1 prompt-written", 1000, "probe=1 prompt-written", true},
		{"0\tOBSERVED: x", 0, "OBSERVED: x", true},
		{"no-tab-here", 0, "", false},
		{"notanumber\ttext", 0, "", false},
	}
	for _, c := range cases {
		ms, txt, ok := parseCaptureLine(c.line)
		if ok != c.wantOK || ms != c.wantMs || txt != c.wantTxt {
			t.Errorf("parseCaptureLine(%q) = (%d,%q,%t), want (%d,%q,%t)", c.line, ms, txt, ok, c.wantMs, c.wantTxt, c.wantOK)
		}
	}
}

func TestSharedWith185(t *testing.T) {
	for _, c := range []string{causeContentionA, causePtyStarvationB, causeLoadAmbiguous} {
		got := sharedWith185(c)
		if !strings.Contains(got, "shared") {
			t.Errorf("sharedWith185(%q) = %q, want a 'shared family' mapping", c, got)
		}
	}
	if got := sharedWith185(causeDetectionC); !strings.Contains(got, "distinct") {
		t.Errorf("sharedWith185(detection-c) = %q, want a 'distinct' mapping", got)
	}
}

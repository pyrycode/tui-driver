package main

import (
	"bytes"
	"path/filepath"
	"testing"
)

// prodOK builds a production ok-tagged castResult (bucketOf == "prod-ok") with the
// given fired keys and edge counts, for the pure invariant tests — no bytes.
func prodOK(name string, fired map[string]bool, edges map[string]int) castResult {
	if fired == nil {
		fired = map[string]bool{}
	}
	if edges == nil {
		edges = map[string]int{}
	}
	return castResult{name: name, tag: "ok", segment: "prod", fired: fired, edges: edges}
}

func kindCounts(vs []assertViolation) map[assertViolationKind]int {
	m := map[assertViolationKind]int{}
	for _, v := range vs {
		m[v.kind]++
	}
	return m
}

func TestEvaluateAssert_CleanCorpusNoViolation(t *testing.T) {
	results := []castResult{
		prodOK("a-ok", map[string]bool{"idle": true, "thinking": true}, map[string]int{"idle": 1, "thinking": 1}),
	}
	if v := evaluateAssert(results, nil, defaultEdgeCeiling); len(v) != 0 {
		t.Fatalf("clean corpus produced %d violation(s): %+v", len(v), v)
	}
}

func TestEvaluateAssert_StrayModalFires(t *testing.T) {
	results := []castResult{
		prodOK("a-ok",
			map[string]bool{"idle": true, "modal:trust-folder": true},
			map[string]int{"idle": 1, "modal:trust-folder": 1}),
	}
	v := evaluateAssert(results, nil, defaultEdgeCeiling)
	if kindCounts(v)[kindStrayFire] != 1 {
		t.Fatalf("want exactly 1 stray-fire violation, got %+v", v)
	}
}

func TestEvaluateAssert_MissingIdle(t *testing.T) {
	results := []castResult{
		prodOK("a-ok", map[string]bool{"thinking": true}, map[string]int{"thinking": 1}),
	}
	v := evaluateAssert(results, nil, defaultEdgeCeiling)
	if kindCounts(v)[kindMissingIdle] != 1 {
		t.Fatalf("want exactly 1 missing-idle violation, got %+v", v)
	}
}

func TestEvaluateAssert_OverCeiling(t *testing.T) {
	results := []castResult{
		prodOK("a-ok", map[string]bool{"idle": true}, map[string]int{"idle": 5}),
	}
	if v := evaluateAssert(results, nil, 3); kindCounts(v)[kindOverCeiling] != 1 {
		t.Fatalf("want 1 over-ceiling violation at ceiling 3, got %+v", v)
	}
	if v := evaluateAssert(results, nil, 10); len(v) != 0 {
		t.Fatalf("want 0 violations at ceiling 10, got %+v", v)
	}
}

func TestEvaluateAssert_AllowlistExemptsStrayFire(t *testing.T) {
	results := []castResult{
		prodOK("a-ok",
			map[string]bool{"idle": true, "modal:trust-folder": true},
			map[string]int{"idle": 1, "modal:trust-folder": 1}),
	}
	allow := map[string]string{"a-ok": "known benign, per baseline run"}
	if v := evaluateAssert(results, allow, defaultEdgeCeiling); len(v) != 0 {
		t.Fatalf("allowlisted cast still violated: %+v", v)
	}
}

// The allowlist forgives only invariant (a). A missing idle or a flap in an
// allowlisted cast must still fail the gate.
func TestEvaluateAssert_AllowlistForgivesOnlyStrayFire(t *testing.T) {
	results := []castResult{
		prodOK("a-ok", map[string]bool{"modal:trust-folder": true}, map[string]int{"modal:trust-folder": 9}),
	}
	allow := map[string]string{"a-ok": "benign modal fire"}
	k := kindCounts(evaluateAssert(results, allow, 3))
	if k[kindStrayFire] != 0 {
		t.Errorf("allowlist should have exempted the stray fire, got %d", k[kindStrayFire])
	}
	if k[kindMissingIdle] != 1 {
		t.Errorf("missing-idle should still fire despite allowlist, got %d", k[kindMissingIdle])
	}
	if k[kindOverCeiling] != 1 {
		t.Errorf("over-ceiling should still fire despite allowlist, got %d", k[kindOverCeiling])
	}
}

// Only production ok-tagged casts are gated: a stray fire or missing idle in an
// e2e-ok, prod-err, or untagged cast is out of scope.
func TestEvaluateAssert_IgnoresNonProdOk(t *testing.T) {
	mk := func(name, seg, tag string) castResult {
		return castResult{name: name, segment: seg, tag: tag,
			fired: map[string]bool{"modal:trust-folder": true},
			edges: map[string]int{"modal:trust-folder": 9}}
	}
	results := []castResult{
		mk("e2e-ok", "e2e", "ok"),
		mk("prod-err", "prod", "err"),
		mk("unknown-untagged", "unknown", "untagged"),
	}
	if v := evaluateAssert(results, nil, 3); len(v) != 0 {
		t.Fatalf("non-prod-ok casts should be ignored, got %+v", v)
	}
}

func TestRunAssert_ExitCodeAndContentSafeOutput(t *testing.T) {
	results := []castResult{
		prodOK("a-ok", map[string]bool{"idle": true, "modal:trust-folder": true}, map[string]int{"idle": 1, "modal:trust-folder": 1}),
	}
	var b bytes.Buffer
	if rc := runAssert(&b, results, nil, defaultEdgeCeiling); rc != 1 {
		t.Fatalf("rc = %d, want 1", rc)
	}
	out := b.String()
	// The verdict names the cast and the detector key but must never echo a matched
	// anchor phrase (the self-reference constraint).
	if !bytes.Contains(b.Bytes(), []byte("a-ok")) || !bytes.Contains(b.Bytes(), []byte("modal:trust-folder")) {
		t.Errorf("verdict missing cast/key:\n%s", out)
	}
	if bytes.Contains(b.Bytes(), []byte("Quick safety check")) || bytes.Contains(b.Bytes(), []byte("Yes, I trust")) {
		t.Errorf("verdict leaked an anchor phrase:\n%s", out)
	}
}

// replayDir replays every .cast in dir at stride 1, the fidelity assert forces.
func replayDir(t *testing.T, dir string) []castResult {
	t.Helper()
	paths, err := castPaths(dir, "")
	if err != nil {
		t.Fatalf("castPaths(%s): %v", dir, err)
	}
	if len(paths) == 0 {
		t.Fatalf("no casts in %s", dir)
	}
	var results []castResult
	for _, p := range paths {
		r, err := replayCast(p, 1)
		if err != nil {
			t.Fatalf("replayCast(%s): %v", p, err)
		}
		results = append(results, r)
	}
	return results
}

// TestAssert_FixtureCorpora drives the gate end-to-end over the committed synthetic
// fixtures under testdata/assert, one mini-corpus per violation kind, proving the
// gate claude-free under `make check`.
func TestAssert_FixtureCorpora(t *testing.T) {
	adir := func(s string) string { return filepath.Join("testdata", "assert", s) }

	t.Run("clean exits 0", func(t *testing.T) {
		var b bytes.Buffer
		if rc := runAssert(&b, replayDir(t, adir("clean")), nil, defaultEdgeCeiling); rc != 0 {
			t.Fatalf("clean corpus rc=%d, want 0\n%s", rc, b.String())
		}
	})

	t.Run("stray modal exits non-zero", func(t *testing.T) {
		var b bytes.Buffer
		if rc := runAssert(&b, replayDir(t, adir("stray-modal")), nil, defaultEdgeCeiling); rc == 0 {
			t.Fatalf("stray-modal corpus rc=0, want non-zero\n%s", b.String())
		}
	})

	t.Run("missing idle exits non-zero", func(t *testing.T) {
		var b bytes.Buffer
		if rc := runAssert(&b, replayDir(t, adir("missing-idle")), nil, defaultEdgeCeiling); rc == 0 {
			t.Fatalf("missing-idle corpus rc=0, want non-zero\n%s", b.String())
		}
	})

	t.Run("over ceiling fails at a low ceiling, passes at a high one", func(t *testing.T) {
		results := replayDir(t, adir("over-ceiling"))
		var lo bytes.Buffer
		if rc := runAssert(&lo, results, nil, 3); rc == 0 {
			t.Fatalf("over-ceiling rc=0 at ceiling 3, want non-zero\n%s", lo.String())
		}
		var hi bytes.Buffer
		if rc := runAssert(&hi, results, nil, 100); rc != 0 {
			t.Fatalf("over-ceiling rc=%d at ceiling 100, want 0\n%s", rc, hi.String())
		}
	})

	t.Run("allowlisted exception exits 0", func(t *testing.T) {
		results := replayDir(t, adir("allowlisted"))
		// Without the allowlist the same fixture must fail, so the exemption is what
		// flips it — otherwise the test would pass vacuously.
		var without bytes.Buffer
		if rc := runAssert(&without, results, nil, defaultEdgeCeiling); rc == 0 {
			t.Fatalf("allowlisted fixture should FAIL without the allowlist, rc=0\n%s", without.String())
		}
		allow := map[string]string{"allowlisted-ok": "fixture: benign trust modal, exempt"}
		var with bytes.Buffer
		if rc := runAssert(&with, results, allow, defaultEdgeCeiling); rc != 0 {
			t.Fatalf("allowlisted corpus rc=%d, want 0\n%s", rc, with.String())
		}
	})
}

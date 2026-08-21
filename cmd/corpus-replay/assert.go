package main

import (
	"fmt"
	"io"
	"sort"
)

// Assert mode (#259) turns the corpus-replay report into a pass-fail detection
// gate. It reads only the classified castResult maps (fired / edges) the replay
// already produced, never raw bytes, so it routes nothing new — it stays the same
// report-only audit, now with an exit code. Its whole job is invariant evaluation
// over the production ok-tagged casts, so a detection regression fails a gate
// automatically instead of depending on a human reading the aggregate correctly.

// defaultEdgeCeiling is the per-key transition-edge ceiling invariant (c) enforces:
// a healthy production run's detector should fire and settle, not flap. It is a
// BASELINE-DERIVED TUNABLE shipped as a documented placeholder, NOT an empirically
// fixed constant. The real value comes from a baseline run over the operator's
// external corpus (make corpus-assert), which is out of the claude-free scope of
// #259. Override it with -assert-edge-ceiling. The synthetic fixtures under
// testdata/assert prove the comparison mechanism, never this number.
const defaultEdgeCeiling = 12

// assertAllowlist names production ok-tagged casts whose modal-class or banner
// detector fire is an accepted exception to invariant (a), each with the reason it
// is allowed. It is COMMITTED and starts EMPTY: a real exception can only be
// justified by a baseline run over the external corpus (make corpus-assert), which
// the claude-free dev turn cannot do. Add an entry as `"short-name": "why"` once
// such a run surfaces a legitimate benign fire. The short-name is the cast filename
// without its .cast suffix, as printed by the report.
var assertAllowlist = map[string]string{}

// assertViolationKind categorises why a cast failed the gate. It is a fixed
// category label, never matched content or an anchor phrase.
type assertViolationKind string

const (
	kindStrayFire   assertViolationKind = "stray-modal-or-banner-fire" // invariant (a)
	kindMissingIdle assertViolationKind = "missing-idle"               // invariant (b)
	kindOverCeiling assertViolationKind = "edges-over-ceiling"         // invariant (c)
)

// assertViolation is one invariant breach. It carries ONLY the cast short-name, the
// detector key, and the kind — never matched content or an anchor phrase (#259), so
// printing the gate result cannot itself forge live detection the way echoing an
// anchor would.
type assertViolation struct {
	cast string
	key  string
	kind assertViolationKind
}

// evaluateAssert applies the three detection-health invariants to the replayed
// results and returns every violation, sorted deterministically. A cast is in scope
// only when it is production-segment and ok-tagged (bucketOf == "prod-ok"): those
// are the runs a healthy detector set must classify as calm.
//
//	(a) No modal-class or banner detector fires. "Suspect" reuses the report's own
//	    definition — every detector key except idle and thinking — so a stray
//	    modal:<class>, mcp-failure, network-failure, or unknown-dialog fire is a
//	    violation, unless the whole cast is named in allow.
//	(b) idle fires at least once. A production run that never reaches the idle
//	    prompt means the idle detector went blind (the #282 shape).
//	(c) Every detector key's transition-edge count is at or below ceiling, so a
//	    flapping regression (a class shown/hidden repeatedly, a busy axis toggling)
//	    fails loudly instead of hiding in the aggregate.
//
// It evaluates the classified fired/edges maps, so it is deterministic and needs no
// claude.
func evaluateAssert(results []castResult, allow map[string]string, ceiling int) []assertViolation {
	var v []assertViolation
	for _, r := range results {
		if bucketOf(r) != "prod-ok" {
			continue
		}
		short := shortName(r.name)
		// (a) stray modal/banner fire — the whole cast is exempt when allowlisted.
		if _, exempt := allow[short]; !exempt {
			for key := range r.fired {
				if isSuspect(key) {
					v = append(v, assertViolation{short, key, kindStrayFire})
				}
			}
		}
		// (b) idle must fire at least once.
		if !r.fired["idle"] {
			v = append(v, assertViolation{short, "idle", kindMissingIdle})
		}
		// (c) no key may flap past the ceiling.
		for key, n := range r.edges {
			if n > ceiling {
				v = append(v, assertViolation{short, key, kindOverCeiling})
			}
		}
	}
	sort.Slice(v, func(i, j int) bool {
		if v[i].cast != v[j].cast {
			return v[i].cast < v[j].cast
		}
		if v[i].kind != v[j].kind {
			return v[i].kind < v[j].kind
		}
		return v[i].key < v[j].key
	})
	return v
}

// runAssert evaluates the invariants, writes the verdict, and returns the process
// exit code: 0 when the corpus is clean, 1 on any violation. Each violation line
// names only the cast short-name, the detector key, and the kind — no matched
// content — keeping the self-reference constraint intact.
func runAssert(w io.Writer, results []castResult, allow map[string]string, ceiling int) int {
	scoped := 0
	for _, r := range results {
		if bucketOf(r) == "prod-ok" {
			scoped++
		}
	}
	violations := evaluateAssert(results, allow, ceiling)
	fmt.Fprintf(w, "\nassert: %d production ok-tagged cast(s) gated, edge ceiling %d, allowlist %d entr(ies)\n",
		scoped, ceiling, len(allow))
	if len(violations) == 0 {
		fmt.Fprintln(w, "assert: PASS — no detection-health invariant violated")
		return 0
	}
	fmt.Fprintf(w, "assert: FAIL — %d violation(s) [cast  detector-key  kind]:\n", len(violations))
	for _, vi := range violations {
		fmt.Fprintf(w, "  %s  %s  %s\n", vi.cast, vi.key, vi.kind)
	}
	return 1
}

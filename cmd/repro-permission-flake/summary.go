package main

import (
	"fmt"
	"io"
	"sort"
)

// writeSummary renders the aggregate diagnosis readout: totals, then one block
// per reproduced flake with its instanceOutcome fields, root-cause hint, and the
// shared-with-#185 mapping that selects slice B's branch. This readout (and the
// per-instance captures beside it) is the durable artifact PO authors slice B
// against. Kept out of the pure-classify file because it is presentation, not
// classification.
func writeSummary(w io.Writer, runs []instanceRun, outDir string) {
	var pass, flake, other int
	for _, r := range runs {
		switch r.outcome.outcome {
		case outcomePass:
			pass++
		case outcomeFlake:
			flake++
		default:
			other++
		}
	}

	fmt.Fprintf(w, "repro-permission-flake summary\n")
	fmt.Fprintf(w, "  artifact dir: %s\n", outDir)
	fmt.Fprintf(w, "  total instances: %d  (%d pass, %d flake, %d other)\n", len(runs), pass, flake, other)

	if flake == 0 {
		fmt.Fprintf(w, "\n  no flake reproduced — the \"modal not detected within 30s\" timeout was not observed.\n")
		fmt.Fprintf(w, "  Escalate -concurrency or add -cpu-burn (headroom below GOMAXPROCS) and re-run; #185's rule is to observe the natural failure, not force a fixed rate.\n")
		return
	}

	// Roll up the root-cause verdicts so the headline names the fork.
	causeCount := map[string]int{}
	for _, r := range runs {
		if r.outcome.outcome == outcomeFlake {
			causeCount[r.hint.cause]++
		}
	}
	fmt.Fprintf(w, "\n  flake root-cause tally:\n")
	for _, cause := range sortedCauses(causeCount) {
		fmt.Fprintf(w, "    %-26s %d  → %s\n", cause, causeCount[cause], sharedWith185(cause))
	}

	fmt.Fprintf(w, "\n  per-flake detail:\n")
	for _, r := range runs {
		if r.outcome.outcome != outcomeFlake {
			continue
		}
		fmt.Fprintf(w, "    wave%d-inst%d (%s):\n", r.wave, r.inst, r.path)
		fmt.Fprintf(w, "      modalSeenAtMs: %s\n", msOrNever(r.outcome.modalSeenAtMs))
		fmt.Fprintf(w, "      stageTimeline: %s\n", formatStages(r.outcome.stageTimeline))
		fmt.Fprintf(w, "      maxByteGapMs:  %d\n", r.outcome.maxByteGapMs)
		fmt.Fprintf(w, "      root cause:    %s\n", r.hint.cause)
		fmt.Fprintf(w, "      evidence:      %s\n", r.hint.evidence)
		fmt.Fprintf(w, "      #185 mapping:  %s\n", sharedWith185(r.hint.cause))
	}

	fmt.Fprintf(w, "\n  Transcribe the tally and the shared-vs-#185 line into the README Empirical-log + Diagnosis subsections; that recorded golden unblocks slice B.\n")
}

func sortedCauses(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func msOrNever(ms int64) string {
	if ms < 0 {
		return "never (no modal anchor drained in the capture)"
	}
	return fmt.Sprintf("%d", ms)
}

func formatStages(stages map[string]int64) string {
	if len(stages) == 0 {
		return "(none captured)"
	}
	keys := make([]string, 0, len(stages))
	for k := range stages {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := ""
	for i, k := range keys {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%s=%dms", k, stages[k])
	}
	return s
}

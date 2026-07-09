// Command corpus-label runs a resumable, subscription-login labeling queue over
// the corpus-sampler output JSONL (#255/#272–#274): a judge independent of the
// structural detectors classifies each sampled screen ahead of fixture promotion
// (#258). A presweep that classified with the code under test could only
// re-discover what the detectors already see — a blind spot would dedupe into the
// boring bucket — so the judge is a headless `claude` reading the raw grids, with a
// free-text `unusual:` escape hatch that surfaces situations no enum value covers
// (the dot-frame spinner gap #243 was found exactly this way).
//
// The run is resumable: the operator's `claude` subscription window can cap
// mid-run, so a capped run just pauses and the next run continues from the first
// unlabeled screen. Labels are flushed per batch, so a mid-run kill loses at most
// the in-flight batch. Sequential by design (no goroutines): the subscription login
// is a single serialized resource and resume correctness wants ordered,
// flush-per-batch persistence — a parallel pool could leave a mid-run gap resume
// can't reason about.
//
// Billing safety: the child `claude` runs on the ambient subscription login with
// ANTHROPIC_API_KEY REMOVED from its environment — its presence flips billing to
// metered, the exact outcome this design avoids. Default model haiku; a non-default
// -model forces a re-pass over the low-confidence and unusual screens.
//
// Local audit tool — no CI workflow (org rule); run BY HAND off the agent pipeline.
//
// ⚠️ Self-reference: sample grids quote detection anchors, and displaying them on
// screen mid-run can false-fire live detection (#152/#154/#155). Grids flow ONLY to
// the child claude's stdin; -out stores hashes (never grids); stdout carries
// aggregate counts only; test failures reference hashes and cast names, never grid
// content.
package main

import (
	"flag"
	"fmt"
	"os"
)

// defaultModel is the labeling model. A non-default -model switches to re-pass mode
// (re-label the low-confidence and unusual screens).
const defaultModel = "haiku"

func main() {
	in := flag.String("in", "", "sampler output JSONL to label (required)")
	out := flag.String("out", "", "labels JSONL; also the resume source (required)")
	park := flag.String("park", "", "park JSONL for batches that fail to label twice (required)")
	batch := flag.Int("batch", 15, "screens per claude -p call (10-20)")
	model := flag.String("model", defaultModel, "model for claude -p; a non-default value switches to re-pass mode")
	minConf := flag.Float64("min-confidence", 0.7, "re-pass selects labels below this confidence (or unusual:)")
	claudePath := flag.String("claude", "claude", "claude binary path (test injection seam)")
	flag.Parse()

	if *in == "" || *out == "" || *park == "" {
		fmt.Fprintln(os.Stderr, "corpus-label: -in, -out and -park are required")
		flag.Usage()
		os.Exit(2)
	}
	if !batchInRange(*batch) {
		fmt.Fprintf(os.Stderr, "corpus-label: -batch must be in [10,20], got %d\n", *batch)
		os.Exit(2)
	}

	if err := run(*in, *out, *park, *batch, *model, *minConf, *claudePath); err != nil {
		fmt.Fprintf(os.Stderr, "corpus-label: %v\n", err)
		os.Exit(1)
	}
}

// run wires the pipeline: read -in → select (resume or re-pass) → prioritize →
// batch → label loop with per-batch persist-or-park → aggregate counts.
func run(inPath, outPath, parkPath string, batchSize int, model string, minConf float64, claudePath string) error {
	samples, err := readSamples(inPath)
	if err != nil {
		return fmt.Errorf("reading -in: %w", err)
	}

	l := &labeler{claudePath: claudePath, model: model, labels: taxonomyLabels(), tax: taxonomySet()}
	repass := model != defaultModel

	queue, persist, err := plan(samples, outPath, repass, minConf)
	if err != nil {
		return err
	}

	queue = prioritize(queue)
	batches := batchesOf(queue, batchSize)

	labeled, parked := 0, 0
	for _, b := range batches {
		recs, park := l.labelBatch(b)
		if park != nil {
			if err := appendPark(parkPath, *park); err != nil {
				return fmt.Errorf("writing -park: %w", err)
			}
			parked += len(park.Hashes)
			continue
		}
		if err := persist(recs); err != nil {
			return fmt.Errorf("writing -out: %w", err)
		}
		labeled += len(recs)
	}

	// stdout: aggregate counts ONLY — never grid content or a label's free text.
	fmt.Printf("mode=%s in=%d queued=%d batches=%d labeled=%d parked=%d\n",
		modeName(repass), len(samples), len(queue), len(batches), labeled, parked)
	return nil
}

// plan builds the work queue and the mode-specific persistence callback.
//
// Resume mode (default model): queue = the unlabeled screens; persist appends each
// batch's labels to -out (per-batch flush → resume loses at most the in-flight
// batch).
//
// Re-pass mode (non-default model): queue = the existing -out records that are
// low-confidence or unusual, joined to their -in grids; persist keeps the full -out
// set in memory keyed by hash, merges each batch's updated records, and rewrites
// -out per batch (keeps re-pass resumable; the file is low-thousands lines). Parked
// re-pass screens are not persisted, so their old low-confidence entries survive and
// a later run re-attempts them.
func plan(samples []sample, outPath string, repass bool, minConf float64) ([]sample, func([]labelRecord) error, error) {
	if repass {
		recs, err := readLabels(outPath)
		if err != nil {
			return nil, nil, fmt.Errorf("reading -out for re-pass: %w", err)
		}
		queue := selectRepass(samples, recs, minConf)

		byHash := make(map[string]labelRecord, len(recs))
		order := make([]string, 0, len(recs))
		for _, r := range recs {
			if _, ok := byHash[r.Hash]; !ok {
				order = append(order, r.Hash)
			}
			byHash[r.Hash] = r
		}
		persist := func(updated []labelRecord) error {
			for _, u := range updated {
				byHash[u.Hash] = u
			}
			merged := make([]labelRecord, 0, len(order))
			for _, h := range order {
				merged = append(merged, byHash[h])
			}
			return rewriteLabels(outPath, merged)
		}
		return queue, persist, nil
	}

	labeled := map[string]bool{}
	if recs, err := readLabels(outPath); err == nil {
		labeled = labeledSet(recs)
	} else if !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("reading -out for resume: %w", err)
	}
	queue := selectUnlabeled(samples, labeled)
	persist := func(recs []labelRecord) error { return appendLabels(outPath, recs) }
	return queue, persist, nil
}

func modeName(repass bool) string {
	if repass {
		return "repass"
	}
	return "resume"
}

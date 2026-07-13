// Command corpus-label runs a resumable, subscription-login labeling queue over
// the corpus-sampler output JSONL (#255/#272–#274): a judge independent of the
// structural detectors classifies each sampled screen ahead of fixture promotion
// (#258). A presweep that classified with the code under test could only
// re-discover what the detectors already see — a blind spot would dedupe into the
// boring bucket — so the judge reads the raw grids, with a free-text `unusual:`
// escape hatch that surfaces situations no enum value covers (the dot-frame spinner
// gap #243 was found exactly this way).
//
// Each batch is judged by one fresh `pyry agent-run`, which spawns interactive
// claude through pyry's pseudo-terminal on the subscription login and ends when the
// turn completes. This is the one blessed way to run claude on the subscription
// without metered spend — the same path the agent dispatchers use every day. A
// print-mode `claude -p` call would bill per token against the metered API, the
// exact outcome this design avoids, so it is never used.
//
// The run is resumable: the subscription window can cap mid-run, so a capped run
// just pauses and the next run continues from the first unlabeled screen. Labels are
// flushed per batch, so a mid-run kill loses at most the in-flight batch. Ctrl-C
// (SIGINT/SIGTERM) cancels the in-flight batch and exits cleanly; resume then
// continues from the first unlabeled screen. Sequential by design (no goroutines):
// the subscription login is a single serialized resource and resume correctness
// wants ordered, flush-per-batch persistence — a parallel pool could leave a mid-run
// gap resume can't reason about.
//
// Billing safety: the child inherits the environment with ANTHROPIC_API_KEY and
// PYRY_USE_STREAMJSON both REMOVED — the first flips claude to metered billing, the
// second routes pyry through its metered print-mode fallback; either defeats the
// design. The durable subscription auth (CLAUDE_CODE_OAUTH_TOKEN) is preserved.
// Default model haiku; a non-default -model forces a re-pass over the low-confidence
// and unusual screens.
//
// Local audit tool — no CI workflow (org rule); run BY HAND off the agent pipeline.
//
// ⚠️ Self-reference: sample grids quote detection anchors, and displaying them on
// screen mid-run can false-fire live detection (#152/#154/#155). Grids flow ONLY to
// the judge's prompt file; -out stores hashes (never grids); stdout carries
// aggregate counts only; a park record carries only the model's reply or a
// grid-free error; test failures reference hashes and cast names, never grid content.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// defaultModel is the labeling model. A non-default -model switches to re-pass mode
// (re-label the low-confidence and unusual screens).
const defaultModel = "haiku"

// options is the parsed flag set threaded into run.
type options struct {
	in, out, park          string
	batch                  int
	model, effort, workdir string
	pyryPath               string
	minConf                float64
}

func main() {
	in := flag.String("in", "", "sampler output JSONL to label (required)")
	out := flag.String("out", "", "labels JSONL; also the resume source (required)")
	park := flag.String("park", "", "park JSONL for batches that fail to label twice (required)")
	batch := flag.Int("batch", 15, "screens per pyry agent-run batch (10-20)")
	model := flag.String("model", defaultModel, "model for the judge; a non-default value switches to re-pass mode")
	minConf := flag.Float64("min-confidence", 0.7, "re-pass selects labels below this confidence (or unusual:)")
	pyryPath := flag.String("pyry", "pyry", "pyry binary path; must support agent-run (test injection seam)")
	effort := flag.String("effort", "low", "thinking effort for pyry agent-run: low|medium|high|xhigh|max")
	workdir := flag.String("workdir", ".", "working directory for pyry agent-run (must exist; default is the already-trusted current dir)")
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

	// Ctrl-C cancels the in-flight batch; run exits cleanly and resume continues
	// from the first unlabeled screen next launch. Suits the fanless-Air multi-day run.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	opt := options{
		in: *in, out: *out, park: *park, batch: *batch,
		model: *model, effort: *effort, workdir: *workdir,
		pyryPath: *pyryPath, minConf: *minConf,
	}
	if err := run(ctx, opt); err != nil {
		fmt.Fprintf(os.Stderr, "corpus-label: %v\n", err)
		os.Exit(1)
	}
}

// run wires the pipeline: read -in → select (resume or re-pass) → prioritize →
// batch → label loop with per-batch persist-or-park → aggregate counts.
func run(ctx context.Context, opt options) error {
	samples, err := readSamples(opt.in)
	if err != nil {
		return fmt.Errorf("reading -in: %w", err)
	}

	// One minimal system prompt, written once and reused across every batch: it
	// steers the judge to a pure text answer and needs no per-batch rebuild.
	sysPrompt, cleanup, err := writeSystemPrompt()
	if err != nil {
		return fmt.Errorf("writing system prompt: %w", err)
	}
	defer cleanup()

	l := &labeler{
		runner: &pyryRunner{
			pyryPath:         opt.pyryPath,
			model:            opt.model,
			effort:           opt.effort,
			workdir:          opt.workdir,
			systemPromptPath: sysPrompt,
			maxTurns:         labelMaxTurns,
			timeout:          batchTimeout,
		},
		model:  opt.model,
		labels: taxonomyLabels(),
		tax:    taxonomySet(),
	}
	repass := opt.model != defaultModel

	queue, persist, err := plan(samples, opt.out, repass, opt.minConf)
	if err != nil {
		return err
	}

	queue = prioritize(queue)
	batches := batchesOf(queue, opt.batch)

	labeled, parked := 0, 0
	interrupted := false
	for _, b := range batches {
		if ctx.Err() != nil {
			interrupted = true
			break
		}
		recs, park := l.labelBatch(ctx, b)
		if ctx.Err() != nil {
			// Interrupted mid-batch: the in-flight batch was cancelled, not failed.
			// Don't persist or park it — resume re-does it next run.
			interrupted = true
			break
		}
		if park != nil {
			if err := appendPark(opt.park, *park); err != nil {
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
	if interrupted {
		fmt.Fprintln(os.Stderr, "corpus-label: interrupted — re-run to resume from the first unlabeled screen")
	}
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

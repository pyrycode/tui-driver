// Command repro-permission-flake reproduces and diagnoses spike-permission's
// "modal not detected within 30s" load flake (#253, slice A of the
// spike-from-implement split).
//
// The flake: under full-suite `make e2e` load, spike-permission's probe 1 can
// hit its internal 30s modalDetectLimit before claude renders the permission
// modal, printing "modal not detected within 30s" on stderr with no ^OBSERVED
// line — so the runner sees a fail. It passes 3/3 in isolation. That
// "passes alone, reddens under load" signature matches #185's spike-one-turn
// flake.
//
// This is a hand-run diagnostic, NOT a make-e2e check. It drives the prebuilt
// ./bin/spike-permission black-box in concurrent waves under artificial load,
// timestamps each instance's output, classifies the outcome with the pure
// functions in classify.go, and writes a durable artifact so slice B (the fix)
// is authored against a recorded root cause rather than a guess. Every instance
// is both load and subject.
//
// It reproduces; it fixes nothing. A reproduced flake is success (exit 0),
// exactly as #206's rig ships green on a successful observation. The three
// candidate root causes — (a) contention, (b) PTY starvation, (c) a detection
// bug — fork slice B's branch and its security posture; the single highest-stakes
// job here is to confirm-or-refute (c), the only cause that pushes the fix into
// pkg/tuidriver.
//
// ⚠️ Self-reference: this file and its testdata quote claude's modal anchors, so
// build and run it BY HAND, off the agent pipeline, as #152/#154/#155 were.
//
// Local diagnostic only — no CI workflow (org rule). See README.md.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

func main() {
	concurrency := flag.Int("concurrency", 4, "spike-permission instances per wave (2 is the real PYRY_MAX_CONCURRENT floor)")
	waves := flag.Int("waves", 20, "max waves to run")
	stopOnFlake := flag.Bool("stop-on-flake", true, "stop after the first wave that produces a flake (set false to accumulate the slice-B >=20-run sample)")
	cpuBurn := flag.Int("cpu-burn", 0, "synthetic CPU pressure: N busy-loop goroutines (leave headroom below GOMAXPROCS; 0 = concurrency alone)")
	bin := flag.String("bin", "./bin/spike-permission", "path to the prebuilt spike-permission binary")
	out := flag.String("out", "", "artifact root (default a timestamped dir under the temp dir)")
	flag.Parse()

	if err := run(*concurrency, *waves, *stopOnFlake, *cpuBurn, *bin, *out); err != nil {
		fmt.Fprintf(os.Stderr, "repro-permission-flake: %v\n", err)
		os.Exit(1)
	}
}

func run(concurrency, waves int, stopOnFlake bool, cpuBurn int, bin, out string) error {
	if concurrency < 1 {
		concurrency = 1
	}
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("spike-permission binary not found at %s (run `make spike-permission` first): %w", bin, err)
	}

	if out == "" {
		out = filepath.Join(os.TempDir(), fmt.Sprintf("repro-permission-flake-%d", time.Now().Unix()))
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("create artifact dir %s: %w", out, err)
	}

	// One root context, cancelled on SIGINT/SIGTERM. exec.CommandContext then
	// SIGKILLs every live spike child, and the cpu-burn workers exit.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var burnWG sync.WaitGroup
	for n := 0; n < cpuBurn; n++ {
		burnWG.Add(1)
		go cpuBurnWorker(ctx, &burnWG)
	}

	fmt.Printf("repro-permission-flake: bin=%s concurrency=%d waves<=%d stop-on-flake=%t cpu-burn=%d\n", bin, concurrency, waves, stopOnFlake, cpuBurn)
	fmt.Printf("repro-permission-flake: artifacts in %s\n", out)

	var all []instanceRun
	sawFlake := false
	for w := 1; w <= waves; w++ {
		if ctx.Err() != nil {
			fmt.Println("repro-permission-flake: interrupted, writing partial artifact")
			break
		}
		runs := runWave(ctx, w, concurrency, bin, out)
		all = append(all, runs...)

		pass, flake, other := tally(runs)
		fmt.Printf("wave %d: %d pass, %d flake, %d other\n", w, pass, flake, other)
		if flake > 0 {
			sawFlake = true
			if stopOnFlake {
				fmt.Println("repro-permission-flake: flake reproduced, stopping (set -stop-on-flake=false to accumulate a full sample)")
				break
			}
		}
	}

	stop()             // stop receiving signals; let cpu-burn workers drain on ctx cancel
	summaryPath := filepath.Join(out, "summary")
	f, err := os.Create(summaryPath)
	if err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	writeSummary(f, all, out)
	f.Close()

	// Echo to stdout so a terminal run is legible; the file is the deliverable.
	writeSummary(os.Stdout, all, out)
	fmt.Printf("\nrepro-permission-flake: summary written to %s\n", summaryPath)

	if !sawFlake {
		fmt.Println("repro-permission-flake: no flake reproduced this run — escalate -concurrency / -cpu-burn (see README Open questions)")
	}
	return nil
}

// instanceRun is one spike instance: its capture path, classified outcome, and
// diagnosis.
type instanceRun struct {
	wave    int
	inst    int
	path    string
	outcome instanceOutcome
	hint    rootCauseHint
}

// runWave launches concurrency spike instances at once (every one both load and
// subject), captures and classifies each, and returns their runs. It joins the
// whole wave before returning so the next wave's contention starts clean.
func runWave(ctx context.Context, wave, concurrency int, bin, outDir string) []instanceRun {
	runs := make([]instanceRun, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runs[i] = runInstance(ctx, wave, i, bin, outDir)
		}(i)
	}
	wg.Wait()
	return runs
}

// runInstance spawns one ./bin/spike-permission -trust-folder=accept (the same
// arg the e2e-runner passes), captures its stdout and stderr as timestamped
// lines into wave<w>-inst<i>.log, then classifies and diagnoses the capture.
// spike-permission runs with MirrorStderr, so its PTY byte stream and its
// probe=... stage lines both land on stderr; the ^OBSERVED line lands on stdout.
func runInstance(ctx context.Context, wave, inst int, bin, outDir string) instanceRun {
	path := filepath.Join(outDir, fmt.Sprintf("wave%d-inst%d.log", wave, inst))
	r := instanceRun{wave: wave, inst: inst, path: path}

	f, err := os.Create(path)
	if err != nil {
		r.outcome = instanceOutcome{outcome: outcomeOther, modalSeenAtMs: -1, stageTimeline: map[string]int64{}}
		r.hint = rootCauseHint{causeInconclusive, "could not create capture file: " + err.Error()}
		return r
	}
	lw := &lineWriter{w: bufio.NewWriter(f)}

	cmd := exec.CommandContext(ctx, bin, "-trust-folder=accept")
	stderr, err1 := cmd.StderrPipe()
	stdout, err2 := cmd.StdoutPipe()
	if err1 != nil || err2 != nil {
		lw.flush()
		f.Close()
		return classifiedRun(r, path)
	}

	lw.start = time.Now()
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(bufio.NewWriter(f), "0\tspawn failed: %v\n", err) // best-effort
		lw.flush()
		f.Close()
		r.outcome = instanceOutcome{outcome: outcomeOther, modalSeenAtMs: -1, stageTimeline: map[string]int64{}}
		r.hint = rootCauseHint{causeInconclusive, "instance failed to spawn: " + err.Error()}
		return r
	}

	var cg sync.WaitGroup
	cg.Add(2)
	go drain(stderr, lw, &cg)
	go drain(stdout, lw, &cg)
	cg.Wait()
	_ = cmd.Wait() // a flake exits non-zero; that is the target observation, not a harness error

	lw.flush()
	f.Close()
	return classifiedRun(r, path)
}

// classifiedRun reads back a written capture file and fills outcome + hint.
func classifiedRun(r instanceRun, path string) instanceRun {
	data, err := os.ReadFile(path)
	if err != nil {
		r.outcome = instanceOutcome{outcome: outcomeOther, modalSeenAtMs: -1, stageTimeline: map[string]int64{}}
		r.hint = rootCauseHint{causeInconclusive, "could not read capture: " + err.Error()}
		return r
	}
	r.outcome = classifyCapture(data)
	r.hint = diagnose(r.outcome)
	return r
}

// drain reads one pipe line-by-line and hands each line to the shared
// timestamping writer. It parses nothing — classification is a pure post-pass.
func drain(rc io.Reader, lw *lineWriter, wg *sync.WaitGroup) {
	defer wg.Done()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		lw.writeLine(sc.Text())
	}
}

// lineWriter timestamps each captured line with harness-owned elapsed-ms
// (monotonic from the instance's spawn) and writes "<ms>\t<text>". Both drain
// goroutines share one writer, so the mutex serialises their interleaved lines.
type lineWriter struct {
	mu    sync.Mutex
	start time.Time
	w     *bufio.Writer
}

func (lw *lineWriter) writeLine(text string) {
	elapsed := time.Since(lw.start).Milliseconds()
	lw.mu.Lock()
	fmt.Fprintf(lw.w, "%d\t%s\n", elapsed, text)
	lw.mu.Unlock()
}

func (lw *lineWriter) flush() {
	lw.mu.Lock()
	lw.w.Flush()
	lw.mu.Unlock()
}

// cpuBurnWorker busy-loops until the context is cancelled, applying synthetic
// CPU pressure. Kept trivial so it never starves the capture goroutines (the
// (b)-confound; see README Open questions).
func cpuBurnWorker(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func tally(runs []instanceRun) (pass, flake, other int) {
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
	return
}

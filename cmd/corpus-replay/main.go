// Command corpus-replay replays asciinema .cast recordings through tui-driver's
// screen-state detectors and reports which detectors fired in which runs.
//
// Every screen detector keys on literals claude renders, and those drift on a
// claude self-update with no compile-time or test signal. The PTY recording
// corpus (the recorder behind SpawnOpts.RecordTo, tagged -ok / -err) has cracked
// three detection problems in a row — the 2.1.199 recalibration, the wedge
// taxonomy, and the 2026-07-07 detection review's two content-forgery aborts —
// but every scan was re-derived ad hoc with shell one-liners. This turns that
// method into a repeatable make target (#227).
//
// For each .cast it feeds the recorded output through a rolling buffer, runs the
// per-tick classifier at a sampling stride, and records, per cast:
//
//   - which structural detectors fired at least once (idle, thinking, each modal
//     class, the mcp-failure and network-failure banners, the unknown-dialog
//     shape);
//   - which detection anchors appeared in the run's content at all, whether or
//     not the structural detector classified — the forgery surface (see
//     contentAnchors);
//   - the -ok / -err tag and a prod / e2e / unknown segment.
//
// Reading the aggregate:
//
//   - A structural detector firing in production -ok runs is a FALSE-POSITIVE
//     suspect: a healthy production run should not trip a modal/banner detector.
//   - A cast where an anchor appears in content but the matching structural
//     detector did NOT fire is a correctly-SUPPRESSED forgery — the shape the
//     #219 / #220 co-signals were built to reject, and the shape of the two
//     2026-07-07 aborts.
//   - A chrome anchor that never appears across a version's corpus where it
//     should is a DRIFT suspect (e.g. the retired network token).
//
// The corpus mixes production agent runs with the make-e2e test runs; they are
// segmented by content cue (a TestRealClaude_* workdir / temp-dir path for e2e,
// a git worktree under Workspace for production), per the 2026-07-04 sweep.
//
// Local audit tool — no CI workflow (org rule). See README.md for usage.
//
// ⚠️ Self-reference: this file and its testdata quote detector anchors, and the
// tool's output prints matched content. Displaying either on screen mid-run can
// false-fire the live detection, so build/run BY HAND, as #152/#154/#155 were.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/pyrycode/tui-driver/pkg/tuidriver"
)

// detector is one structural screen predicate the replay records a fire for. All
// are exported library predicates over a single snapshot; the corpus is recorded
// at the default 120x40, so rendering at the package default matches the cast.
type detector struct {
	name string
	fire func(snap []byte) bool
}

// flatDetectors are the non-modal-class predicates. The modal classes are
// captured separately (one "modal:<class>" key per class DetectModalClass
// returns) so the report shows which specific modal a run tripped.
var flatDetectors = []detector{
	{"idle", tuidriver.IsIdle},
	{"thinking", tuidriver.IsThinking},
	{"mcp-failure", tuidriver.HasMcpFailureBanner},
	{"network-failure", tuidriver.HasNetworkFailure},
	{"unknown-dialog", tuidriver.HasUnknownDialog},
	{"compacting", tuidriver.HasCompacting},
}

// contentAnchors are detection-relevant literals the tool scans for in each
// cast's raw content, INDEPENDENT of whether the structural detector classified.
// A cast where an anchor appears but the matching detector did not fire is a
// correctly-suppressed forgery; a "retired" anchor still appearing is a drift
// marker for an anchor a past claude rendered and the current detector no longer
// matches. This is a short, documented mirror of the library anchors, not an
// exhaustive copy: the structural detectors above are the truth, this is the
// coarse forgery/drift surface the review turned on.
var contentAnchors = []struct {
	label   string
	phrase  string
	retired bool
}{
	{"trust-header", "Quick safety check", false},
	{"network-current", "Unable to connect to API", false},
	{"network-retired", "FailedToOpenSocket", true},
	{"permission-prompt", "Do you want to proceed", false},
	// The compaction phrase appears in ordinary agent transcripts (ticket bodies,
	// this detector's own source), so a large phrase-present count against a
	// ~zero "compacting" detector count is the forgery-resistance proof: the ▱▰
	// bar co-signal + region scoping suppress every quotation.
	{"compacting-phrase", "Compacting conversation", false},
}

// castResult is one replayed recording's outcome.
type castResult struct {
	name    string
	tag     string // "ok" | "err" | "untagged"
	segment string // "prod" | "e2e" | "unknown"
	cols    int
	rows    int
	events  int
	fired   map[string]bool // detector key -> fired at least once
	edges   map[string]int  // detector key -> transition-edge count within this cast
	anchors map[string]bool // anchor label -> appeared in content
}

func main() {
	dir := flag.String("dir", "", "directory of .cast recordings to replay (required)")
	stride := flag.Int("stride", 1, "run the classifier every Nth output event (1 = every event)")
	perCast := flag.Bool("per-cast", false, "print one line per cast (fires, anchors, segment, tag)")
	only := flag.String("only", "", "only replay casts whose filename contains this substring")
	workers := flag.Int("workers", runtime.NumCPU(), "number of concurrent cast-replay workers")
	noCache := flag.Bool("no-cache", false, "bypass the result cache entirely (always replay; never read or write the cache)")
	cacheDir := flag.String("cache-dir", "", "override the cache directory (default: a corpus-replay subdirectory under the user cache dir)")
	flag.Parse()

	if *dir == "" {
		fmt.Fprintln(os.Stderr, "corpus-replay: -dir is required")
		flag.Usage()
		os.Exit(2)
	}
	if *stride < 1 {
		*stride = 1
	}
	if *workers < 1 {
		*workers = 1
	}

	paths, err := castPaths(*dir, *only)
	if err != nil {
		fmt.Fprintf(os.Stderr, "corpus-replay: %v\n", err)
		os.Exit(1)
	}
	if len(paths) == 0 {
		fmt.Fprintf(os.Stderr, "corpus-replay: no .cast files in %s\n", *dir)
		os.Exit(1)
	}

	// The cache is an optimisation; the tool's core value is replay + report. If
	// cache construction fails, degrade gracefully to a nil cache (an uncached run)
	// so stdout stays byte-identical to a -no-cache run.
	var cache *resultCache
	if !*noCache {
		c, err := newResultCache(*cacheDir, versionSourceDirs...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "corpus-replay: cache disabled: %v\n", err)
		} else {
			cache = c
		}
	}

	results, stats := replayAll(paths, *stride, *workers, cache, os.Stderr)
	if cache != nil {
		// Hit/miss summary to stderr — makes "only the new cast replays" observable
		// without touching the stdout report.
		fmt.Fprintf(os.Stderr, "corpus-replay: cache %d hit(s), %d miss(es)\n", stats.hits, stats.misses)
	}
	report(os.Stdout, results, *dir, *stride, *perCast)
}

// replayOutcome carries one cast's replay result or its error from a worker to
// the collector. Each castResult is created and mutated by exactly one worker,
// then handed off via the channel — after the send the collector is its sole
// owner, so there is no concurrent map access.
type replayOutcome struct {
	path string
	res  castResult
	hit  bool // the result came from the cache (no replay was performed)
	err  error
}

// replayAll replays every path through the detectors using `workers` concurrent
// workers and returns the results sorted by cast name — so report output is
// byte-identical across worker counts (-workers 1 is the sequential baseline it
// preserves). A cast that fails to replay prints one skip line to errOut and is
// omitted from the results (identical semantics to the sequential path; the
// relative ordering of skip lines under parallelism is out of scope for the
// byte-identical criterion, as stderr is not report output).
//
// When cache != nil each cast is served from the cache on a hit (no replay) or
// replayed and stored on a miss; the returned cacheStats tally hits and misses
// (an errored cast is a skip, counted as neither). Distinct keys map to distinct
// files, so the pool stays lockless — see replayOrLoad and cache.go. With
// cache == nil every cast is replayed and cacheStats is zero.
func replayAll(paths []string, stride, workers int, cache *resultCache, errOut io.Writer) ([]castResult, cacheStats) {
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan string)
	out := make(chan replayOutcome)

	// Feeder: stream the sorted paths into jobs, then close so the workers drain
	// and exit. It runs as its own goroutine so it can send concurrently with the
	// collector reading out — otherwise the workers would fill out with no reader.
	go func() {
		for _, p := range paths {
			jobs <- p
		}
		close(jobs)
	}()

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for p := range jobs {
				res, hit, err := replayOrLoad(cache, p, stride)
				out <- replayOutcome{path: p, res: res, hit: hit, err: err}
			}
		}()
	}

	// Closer: once every worker has drained jobs, close out so the collector's
	// range terminates.
	go func() {
		wg.Wait()
		close(out)
	}()

	// Collector: the single reader of out, and the only goroutine that writes
	// errOut and appends to results — so skip lines never interleave mid-byte and
	// no mutex is needed.
	results := make([]castResult, 0, len(paths))
	var stats cacheStats
	for o := range out {
		if o.err != nil {
			fmt.Fprintf(errOut, "corpus-replay: skip %s: %v\n", filepath.Base(o.path), o.err)
			continue
		}
		if cache != nil {
			if o.hit {
				stats.hits++
			} else {
				stats.misses++
			}
		}
		results = append(results, o.res)
	}

	// Casts complete in arbitrary order under the pool, so re-sort by name to
	// restore the sequential (-workers 1) order report() walks. os.ReadDir yields
	// unique names within one directory, so the comparator is a strict total order
	// and a non-stable sort is deterministic.
	sort.Slice(results, func(i, j int) bool { return results[i].name < results[j].name })
	return results, stats
}

// castPaths lists the .cast files in dir, optionally filtered to those whose
// base name contains only. Sorted for deterministic output.
func castPaths(dir, only string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".cast") {
			continue
		}
		if only != "" && !strings.Contains(e.Name(), only) {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	sort.Strings(paths)
	return paths, nil
}

// replayCast replays one recording through the detectors. It feeds each output
// event's bytes into a rolling buffer (the same DefaultBufferCap window the live
// detector sees) and, every stride events, runs every detector on the current
// snapshot. Anchor and segment scanning run over the full decoded output so a
// phrase that scrolled out of the 4 KB window is still counted as "appeared".
func replayCast(path string, stride int) (castResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return castResult{}, err
	}
	defer f.Close()

	name := filepath.Base(path)
	res := castResult{
		name:    name,
		tag:     tagFromName(name),
		fired:   map[string]bool{},
		edges:   map[string]int{},
		anchors: map[string]bool{},
	}

	buf := tuidriver.NewBuffer(0) // DefaultBufferCap rolling window
	var full strings.Builder      // full decoded output, for anchor + segment scans

	prev := map[string]bool{} // previous sampled tick's active-key set, for edges

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // cast event lines can be large
	header := true
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if header {
			// First line is the asciinema header: {"version":2,"width":W,"height":H}.
			var h struct {
				Width  int `json:"width"`
				Height int `json:"height"`
			}
			if err := json.Unmarshal(line, &h); err == nil {
				res.cols, res.rows = h.Width, h.Height
			}
			header = false
			continue
		}
		data, ok := parseOutputEvent(line)
		if !ok {
			continue // non-output event ("i" input, resize, or a parse miss)
		}
		res.events++
		buf.Append(data)
		full.Write(data)
		if res.events%stride == 0 {
			prev = res.classify(buf.Snapshot(), prev)
		}
	}
	if err := sc.Err(); err != nil {
		return castResult{}, err
	}
	// Always classify the final frame, even if the stride skipped it, so a modal
	// that is up on the last event is not missed. When the stride already sampled
	// the last event, this re-renders the same frame: cur == prev, so the
	// symmetric difference is empty and no spurious edge is recorded.
	res.classify(buf.Snapshot(), prev)

	text := full.String()
	res.segment = segmentOf(text)
	for _, a := range contentAnchors {
		if strings.Contains(text, a.phrase) {
			res.anchors[a.label] = true
		}
	}
	return res, nil
}

// activeKeys returns the set of detector keys active for snap: every flat
// detector that fired, plus "modal:<class>" when DetectModalClass returns a
// class. This is the same membership the fires table records; returning it as a
// set lets classify both OR it into the monotonic fires map and diff it against
// the previous tick for transition-edge counting.
func activeKeys(snap []byte) map[string]bool {
	keys := map[string]bool{}
	for _, d := range flatDetectors {
		if d.fire(snap) {
			keys[d.name] = true
		}
	}
	if mc := tuidriver.DetectModalClass(snap); mc != tuidriver.ModalClassUnknown {
		keys["modal:"+string(mc)] = true
	}
	return keys
}

// classify runs the detectors on snap, folds the result into this cast's
// monotonic fires (r.fired) and per-key transition-edge counts (r.edges), and
// returns the active-key set for use as the next tick's prev. A transition edge
// on a key increments whenever its membership flips between consecutive sampled
// ticks — for every key in the symmetric difference cur △ prev. A first
// appearance ({} -> {key}) counts one edge, so a detector that fires once and
// stays ends at exactly 1; a detector that flaps climbs past 1. prev is empty
// for a cast's first tick.
func (r *castResult) classify(snap []byte, prev map[string]bool) map[string]bool {
	cur := activeKeys(snap)
	for k := range cur {
		r.fired[k] = true // preserve monotonic fires: OR cur into fired
		if !prev[k] {
			r.edges[k]++ // key appeared this tick
		}
	}
	for k := range prev {
		if !cur[k] {
			r.edges[k]++ // key disappeared this tick
		}
	}
	return cur
}

// parseOutputEvent decodes one asciinema event line [t, code, data] and returns
// data when code is "o" (terminal output). Any other code, or a malformed line,
// returns ok=false.
func parseOutputEvent(line []byte) (data []byte, ok bool) {
	var ev []json.RawMessage
	if err := json.Unmarshal(line, &ev); err != nil || len(ev) < 3 {
		return nil, false
	}
	var code string
	if err := json.Unmarshal(ev[1], &code); err != nil || code != "o" {
		return nil, false
	}
	var s string
	if err := json.Unmarshal(ev[2], &s); err != nil {
		return nil, false
	}
	return []byte(s), true
}

// tagFromName reads the -ok / -err suffix the recorder writes into the filename.
func tagFromName(name string) string {
	base := strings.TrimSuffix(name, ".cast")
	switch {
	case strings.HasSuffix(base, "-ok"):
		return "ok"
	case strings.HasSuffix(base, "-err"):
		return "err"
	default:
		return "untagged"
	}
}

// segmentOf classifies a recording as a production agent run or a make-e2e test
// run from content cues, per the 2026-07-04 corpus sweep. The e2e cues are the
// most specific and win: the e2e suite drives real claude from a temp directory
// with a TestRealClaude_* workdir. Production agent runs execute in a per-ticket
// git worktree under the operator's Workspace. Neither present -> unknown.
func segmentOf(text string) string {
	if strings.Contains(text, "TestRealClaude") ||
		strings.Contains(text, "/tmp/claude-") ||
		strings.Contains(text, "/private/tmp/claude-") ||
		strings.Contains(text, "/var/folders/") {
		return "e2e"
	}
	if strings.Contains(text, "worktree") || strings.Contains(text, "/Workspace/") {
		return "prod"
	}
	return "unknown"
}

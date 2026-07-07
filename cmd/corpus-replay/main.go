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
	"os"
	"path/filepath"
	"sort"
	"strings"

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
	anchors map[string]bool // anchor label -> appeared in content
}

func main() {
	dir := flag.String("dir", "", "directory of .cast recordings to replay (required)")
	stride := flag.Int("stride", 1, "run the classifier every Nth output event (1 = every event)")
	perCast := flag.Bool("per-cast", false, "print one line per cast (fires, anchors, segment, tag)")
	only := flag.String("only", "", "only replay casts whose filename contains this substring")
	flag.Parse()

	if *dir == "" {
		fmt.Fprintln(os.Stderr, "corpus-replay: -dir is required")
		flag.Usage()
		os.Exit(2)
	}
	if *stride < 1 {
		*stride = 1
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

	results := make([]castResult, 0, len(paths))
	for _, p := range paths {
		r, err := replayCast(p, *stride)
		if err != nil {
			fmt.Fprintf(os.Stderr, "corpus-replay: skip %s: %v\n", filepath.Base(p), err)
			continue
		}
		results = append(results, r)
	}

	report(os.Stdout, results, *dir, *stride, *perCast)
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
		anchors: map[string]bool{},
	}

	buf := tuidriver.NewBuffer(0) // DefaultBufferCap rolling window
	var full strings.Builder      // full decoded output, for anchor + segment scans

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
			applyDetectors(buf.Snapshot(), res.fired)
		}
	}
	if err := sc.Err(); err != nil {
		return castResult{}, err
	}
	// Always classify the final frame, even if the stride skipped it, so a modal
	// that is up on the last event is not missed.
	applyDetectors(buf.Snapshot(), res.fired)

	text := full.String()
	res.segment = segmentOf(text)
	for _, a := range contentAnchors {
		if strings.Contains(text, a.phrase) {
			res.anchors[a.label] = true
		}
	}
	return res, nil
}

// applyDetectors runs every structural detector on snap and records fires into
// fired. Modal classes are recorded as "modal:<class>".
func applyDetectors(snap []byte, fired map[string]bool) {
	for _, d := range flatDetectors {
		if d.fire(snap) {
			fired[d.name] = true
		}
	}
	if mc := tuidriver.DetectModalClass(snap); mc != tuidriver.ModalClassUnknown {
		fired["modal:"+string(mc)] = true
	}
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

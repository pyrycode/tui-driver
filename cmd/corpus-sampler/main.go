// Command corpus-sampler extracts the distinct stable screens out of the PTY
// recording corpus, with provenance, so the offline model-labeling pass (#257)
// and fixture promotion (#258) work from a low-thousands exemplar set instead of
// the corpus's ~4.5M raw render events.
//
// A stable screen is a grid that sat unchanged through a quiet gap: for two
// consecutive output events i-1 and i whose timestamps differ by at least -gap,
// the frame painted by event i-1 waited out the quiet [ts[i-1], ts[i]] window,
// so it is emitted as one sample. Whole-grid dedupe fails on the raw corpus
// (96.9% of per-event grids are unique) because the rolling buffer window shifts
// on every output event; gating on the inter-event time gap and deduping on a
// normalized hash (digit runs collapsed, spinner glyphs unified, rows
// right-trimmed) pulls the distinct situations out.
//
// Sibling of cmd/corpus-replay (#227): the cast-directory listing, asciinema
// header parse, -ok/-err tag read, and prod/e2e segment cues are copied from it
// (a package main cannot import another's helpers). The divergence is timing:
// corpus-replay discards the event timestamp, the sampler needs it to measure
// inter-event gaps (parseOutputEventTS).
//
// Scope: quiet-gap stable screens ONLY. Random mid-stream slices, detector-fire
// moments, and final frames are #256 ("extra sources"), not built here.
//
// Local audit tool — no CI workflow (org rule); build/run BY HAND like
// corpus-replay.
//
// ⚠️ Self-reference: sample grids quote detection anchors, and displaying them on
// screen mid-run can false-fire live detection (as #152/#154/#155). Grids are
// written to -out ONLY; stdout carries aggregate counts only; test failures
// reference sample hashes and cast names, never grid content.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pyrycode/tui-driver/pkg/tuidriver"
)

// sample is one distinct stable screen with full provenance. Grid is the RAW
// rendered grid (un-normalized); Hash is over the normalized form. Written to
// -out only — never to stdout.
type sample struct {
	Grid    string  `json:"grid"`    // Render() output at the gap moment
	Hash    string  `json:"hash"`    // hashGrid(normalize(Grid))
	Cast    string  `json:"cast"`    // filepath.Base(path)
	Event   int     `json:"event"`   // output-event index of the stable frame (i-1)
	TS      float64 `json:"ts"`      // ts[i-1], seconds
	Cols    int     `json:"cols"`    // from cast header (0 → Render default)
	Rows    int     `json:"rows"`    // from cast header
	Tag     string  `json:"tag"`     // tagFromName: "ok" | "err" | "untagged"
	Segment string  `json:"segment"` // segmentOf: "prod" | "e2e" | "unknown"
	Seen    int     `json:"seen"`    // total occurrences across the run (first = 1)
	// Source is the set of rules that found this screen (e.g. "gap", "final"),
	// always serialized as a sorted, distinct JSON array — one screen can be
	// found by more than one source. Follow-up sources (#273/#274) add values
	// here, not new fields.
	Source []string `json:"source"`
}

// source names. Multi-valued from the start so #273 (-fires) / #274 (-midstream)
// layer on by adding values, not new machinery.
const (
	sourceGap   = "gap"
	sourceFinal = "final"
)

func main() {
	dir := flag.String("dir", "", "directory of .cast recordings to sample (required)")
	out := flag.String("out", "", "output JSONL file for distinct stable screens (required)")
	gap := flag.Duration("gap", 500*time.Millisecond, "minimum inter-event quiet gap that marks a stable screen")
	final := flag.Bool("final", false, "also emit each cast's final rendered frame as a sample (source \"final\")")
	flag.Parse()

	if *dir == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "corpus-sampler: -dir and -out are required")
		flag.Usage()
		os.Exit(2)
	}

	paths, err := castPaths(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "corpus-sampler: %v\n", err)
		os.Exit(1)
	}
	if len(paths) == 0 {
		fmt.Fprintf(os.Stderr, "corpus-sampler: no .cast files in %s\n", *dir)
		os.Exit(1)
	}

	distinct, casts, events, gaps, err := collect(paths, gap.Seconds(), *final)
	if err != nil {
		fmt.Fprintf(os.Stderr, "corpus-sampler: %v\n", err)
		os.Exit(1)
	}

	if err := writeSamples(*out, distinct); err != nil {
		fmt.Fprintf(os.Stderr, "corpus-sampler: %v\n", err)
		os.Exit(1)
	}

	// stdout: aggregate counts ONLY — never any grid content or grid-derived text.
	fmt.Printf("casts=%d events=%d gaps=%d distinct=%d\n", casts, events, gaps, len(distinct))
}

// castPaths lists the .cast files in dir, sorted for deterministic output.
func castPaths(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".cast") {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	sort.Strings(paths)
	return paths, nil
}

// collect walks paths in order, dedupes stable screens across the whole run on
// Hash (first occurrence kept in discovery order, repeats bump Seen), and
// returns the distinct set plus aggregate counts. A cast that fails to open or
// scan is skipped with a stderr note and NOT counted in casts — one bad cast
// never aborts a 1900-cast run. Determinism follows from sorted castPaths +
// in-order events + first-occurrence-wins.
func collect(paths []string, gap float64, final bool) (distinct []sample, casts, events, gaps int, err error) {
	idx := map[string]int{} // hash -> position in distinct
	for _, p := range paths {
		samples, ev, gp, serr := sampleCast(p, gap, final)
		if serr != nil {
			fmt.Fprintf(os.Stderr, "corpus-sampler: skip %s: %v\n", filepath.Base(p), serr)
			continue
		}
		casts++
		events += ev
		gaps += gp
		for _, s := range samples {
			if pos, ok := idx[s.Hash]; ok {
				distinct[pos].Seen++
				// Union the finder into the kept entry: one screen can be found
				// by more than one source. Seen still counts total occurrences.
				distinct[pos].Source = mergeSources(distinct[pos].Source, s.Source)
				continue
			}
			s.Seen = 1
			idx[s.Hash] = len(distinct)
			distinct = append(distinct, s)
		}
	}
	return distinct, casts, events, gaps, nil
}

// mergeSources returns the sorted union of source sets a and b with duplicates
// removed. Source sets are tiny (a handful of values at most), so the linear
// membership scan is fine. Sorting makes the serialized array order independent
// of which source found the screen first, so re-runs stay byte-identical.
func mergeSources(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, s := range b {
		found := false
		for _, v := range out {
			if v == s {
				found = true
				break
			}
		}
		if !found {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// sampleCast walks one cast and returns its stable-screen samples (Tag/Segment
// already stamped; Seen left 0 — collect owns dedupe), plus the cast's
// output-event count and gap count. For consecutive output events i-1 and i with
// ts[i]-ts[i-1] >= gap, it renders the buffer holding events [0 … i-1] — i.e.
// BEFORE appending event i — and emits one gap sample for the frame event i-1
// painted (Event=i-1, TS=ts[i-1], source "gap"). Non-"o" events are skipped for
// both gap timing and the buffer, so "consecutive output events" means
// consecutive "o". When final is set and the cast had >=1 output event, one more
// sample (source "final") is appended for the ending frame — the buffer rendered
// after the last output event. The returned gap count excludes that final sample.
func sampleCast(path string, gap float64, final bool) (samples []sample, events, gaps int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()

	name := filepath.Base(path)
	var cols, rows int
	buf := tuidriver.NewBuffer(0) // DefaultBufferCap rolling window, same as the live consumer
	var full strings.Builder      // whole decoded output, for segmentOf after the walk

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // cast event lines can be large
	header := true
	idx := 0
	var prevTS float64
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
				cols, rows = h.Width, h.Height
			}
			header = false
			continue
		}
		ts, data, ok := parseOutputEventTS(line)
		if !ok {
			continue // non-output event ("i" input, resize, or a parse miss)
		}
		if idx >= 1 && ts-prevTS >= gap {
			// event idx-1's frame sat stable through the quiet [prevTS, ts] window;
			// render the buffer as it stood after idx-1, before appending idx.
			grid := tuidriver.Render(buf.Snapshot(), cols, rows)
			samples = append(samples, sample{
				Grid:   grid,
				Hash:   hashGrid(normalize(grid)),
				Cast:   name,
				Event:  idx - 1,
				TS:     prevTS,
				Cols:   cols,
				Rows:   rows,
				Source: []string{sourceGap},
			})
			gaps++
		}
		buf.Append(data)
		full.Write(data)
		prevTS = ts
		idx++
	}
	if err := sc.Err(); err != nil {
		return nil, 0, 0, err
	}

	// The final frame captures how the run ended, which the quiet-gap rule misses
	// when the last events arrive in a burst. buf now holds every output event, so
	// its render is the ending screen painted by the last output event (idx-1).
	// A cast with zero output events (idx == 0) has no ending frame to emit.
	if final && idx >= 1 {
		grid := tuidriver.Render(buf.Snapshot(), cols, rows)
		samples = append(samples, sample{
			Grid:   grid,
			Hash:   hashGrid(normalize(grid)),
			Cast:   name,
			Event:  idx - 1,
			TS:     prevTS,
			Cols:   cols,
			Rows:   rows,
			Source: []string{sourceFinal},
		})
	}

	tag := tagFromName(name)
	segment := segmentOf(full.String())
	for i := range samples {
		samples[i].Tag = tag
		samples[i].Segment = segment
	}
	return samples, idx, gaps, nil
}

// parseOutputEventTS forks corpus-replay's parseOutputEvent to also return the
// event timestamp ev[0]. ok=false for any non-"o" event or malformed line.
func parseOutputEventTS(line []byte) (ts float64, data []byte, ok bool) {
	var ev []json.RawMessage
	if err := json.Unmarshal(line, &ev); err != nil || len(ev) < 3 {
		return 0, nil, false
	}
	var code string
	if err := json.Unmarshal(ev[1], &code); err != nil || code != "o" {
		return 0, nil, false
	}
	if err := json.Unmarshal(ev[0], &ts); err != nil {
		return 0, nil, false
	}
	var s string
	if err := json.Unmarshal(ev[2], &s); err != nil {
		return 0, nil, false
	}
	return ts, []byte(s), true
}

// spinnerGlyphs are the five sparkle frames of claude's spinner (pkg/tuidriver
// state.go:54-60), re-listed here as byte escapes so the glyph never renders in
// this source (the package's screen-literal discipline). normalize unifies them
// to canonicalSpinner so the same screen sampled on different spinner frames
// folds to one hash. The middle-dot U+00B7 is deliberately excluded — it doubles
// as an in-region separator (state.go:108), the same reason the library keeps it
// out of its set.
var spinnerGlyphs = []string{
	"\xe2\x9c\xbb", // ✻ U+273B
	"\xe2\x9c\xb3", // ✳ U+2733
	"\xe2\x9c\xa2", // ✢ U+2722
	"\xe2\x9c\xb6", // ✶ U+2736
	"\xe2\x9c\xbd", // ✽ U+273D
}

// canonicalSpinner is the single frame every spinnerGlyph unifies to.
const canonicalSpinner = "\xe2\x9c\xbb" // ✻ U+273B

// digitRun matches a maximal run of ASCII digits; normalize collapses each to
// "0" so a token counter or seconds counter ticking does not fork the hash.
var digitRun = regexp.MustCompile(`[0-9]+`)

// normalize maps a raw grid to its dedupe key form: unify the five sparkle
// spinner glyphs to one canonical rune, collapse each run of ASCII digits to a
// single "0", right-trim every row. Order: unify → collapse → trim.
func normalize(grid string) string {
	s := grid
	for _, g := range spinnerGlyphs {
		if g != canonicalSpinner {
			s = strings.ReplaceAll(s, g, canonicalSpinner)
		}
	}
	s = digitRun.ReplaceAllString(s, "0")
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.Join(lines, "\n")
}

// hashGrid returns hex(sha256(normalized)). Deterministic; the JSONL and failure
// messages reference this, never the grid.
func hashGrid(normalized string) string {
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// writeSamples writes each distinct sample as one JSON object + "\n" to path
// (JSONL). Grids go here ONLY; a failed write must not fall back to stdout.
func writeSamples(path string, distinct []sample) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	w := bufio.NewWriter(f)
	for _, s := range distinct {
		line, merr := json.Marshal(s)
		if merr != nil {
			return merr
		}
		if _, werr := w.Write(line); werr != nil {
			return werr
		}
		if werr := w.WriteByte('\n'); werr != nil {
			return werr
		}
	}
	return w.Flush()
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

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sample is one decoded input record from the corpus-sampler output JSONL
// (cmd/corpus-sampler/main.go:53). Only the fields corpus-label consumes are
// declared; a package main cannot import the sampler's type, so the json tags are
// copied. Tolerant decode is free: an absent field stays zero, which is the AC's
// "degrades gracefully" mechanism — a missing source → nil → empty fire tier, a
// missing tag → "" → empty err tier, with no presence checks.
type sample struct {
	Hash   string   `json:"hash"`   // normalized-grid hash; the join + resume key
	Grid   string   `json:"grid"`   // RAW rendered grid; flows only to the child's stdin
	Cast   string   `json:"cast"`   // originating cast filename
	Tag    string   `json:"tag"`    // "ok" | "err" | "untagged"
	Source []string `json:"source"` // sampler rules that found this screen
}

// labelRecord is one persisted label. Grid is deliberately NOT stored — #258 joins
// labels to grids on Hash against the sampler file; duplicating grids would bloat
// -out and widen the self-reference surface.
type labelRecord struct {
	Hash       string  `json:"hash"`
	Label      string  `json:"label"`
	Confidence float64 `json:"confidence"`
	Model      string  `json:"model"`
	Cast       string  `json:"cast"`
	Tag        string  `json:"tag"`
}

// parkRecord is one batch that failed to label twice: its screens' hashes and the
// verbatim last model response, held for manual review. Parked screens are never
// dropped and never written to -out, so a later run re-attempts them.
type parkRecord struct {
	Hashes   []string `json:"hashes"`
	Raw      string   `json:"raw"`
	Attempts int      `json:"attempts"`
	Model    string   `json:"model"`
}

// sourceFires is the sampler source value that marks a detector-fire sample
// (#273's -fires source; unmerged as of this writing). The empty-tier fallback is
// graceful today: no sample carries it, so the fire tier is simply empty until the
// operator re-samples with the richer source.
const sourceFires = "fires"

// batchInRange reports whether n is an acceptable -batch size (AC range 10–20).
func batchInRange(n int) bool { return n >= 10 && n <= 20 }

// readSamples decodes -in line by line into []sample. A blank or malformed line is
// skipped with a stderr note (tolerant — one bad line never aborts the run,
// mirroring corpus-sampler's cast-skip). The scanner buffer is enlarged because a
// sample line embeds a whole rendered grid.
func readSamples(path string) ([]sample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var samples []sample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		var s sample
		if err := json.Unmarshal(b, &s); err != nil {
			fmt.Fprintf(os.Stderr, "corpus-label: skip %s:%d: %v\n", path, line, err)
			continue
		}
		samples = append(samples, s)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return samples, nil
}

// readLabels decodes an existing -out labels JSONL into []labelRecord — used both
// for the resume labeled-set and for re-pass selection. A missing file returns the
// os.Open error verbatim so the caller can distinguish it with os.IsNotExist. A
// malformed line is fatal: a corrupt resume source must not silently re-label
// everything.
func readLabels(path string) ([]labelRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var recs []labelRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		var r labelRecord
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("corrupt labels line %s:%d: %w", path, line, err)
		}
		recs = append(recs, r)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return recs, nil
}

// labeledSet is the set of hashes already labeled in -out — the resume skip-set.
func labeledSet(recs []labelRecord) map[string]bool {
	set := make(map[string]bool, len(recs))
	for _, r := range recs {
		set[r.Hash] = true
	}
	return set
}

// selectUnlabeled returns the input samples not already present in labeled,
// de-duped by hash within the run (first wins) so each hash is queued at most once
// → "exactly one label per hash". Input order is preserved for determinism.
func selectUnlabeled(samples []sample, labeled map[string]bool) []sample {
	seen := map[string]bool{}
	var out []sample
	for _, s := range samples {
		if labeled[s.Hash] || seen[s.Hash] {
			continue
		}
		seen[s.Hash] = true
		out = append(out, s)
	}
	return out
}

// selectRepass returns the samples whose -out label is low-confidence (< minConf)
// or a free-text unusual: label — the re-pass targets. It joins the target hashes
// back to their input samples (which carry the grid needed to re-label); a target
// hash absent from -in is skipped with a stderr note (no grid to send).
func selectRepass(samples []sample, recs []labelRecord, minConf float64) []sample {
	byHash := make(map[string]sample, len(samples))
	for _, s := range samples {
		byHash[s.Hash] = s
	}
	var out []sample
	for _, r := range recs {
		if r.Confidence >= minConf && !strings.HasPrefix(r.Label, unusualPrefix) {
			continue
		}
		s, ok := byHash[r.Hash]
		if !ok {
			fmt.Fprintf(os.Stderr, "corpus-label: re-pass skip %s: not present in -in\n", r.Hash)
			continue
		}
		out = append(out, s)
	}
	return out
}

// prioritize orders the queue into three tiers — detector-fire samples first, then
// error-tagged, then the rest — stable within each tier (input order preserved). A
// screen that qualifies for the fire tier is not also placed in err (highest tier
// wins). Reading provenance tolerantly is the AC's graceful degradation: an absent
// source leaves the fire tier empty, an absent tag leaves the err tier empty.
func prioritize(samples []sample) []sample {
	var fire, errt, rest []sample
	for _, s := range samples {
		switch {
		case hasSource(s.Source, sourceFires):
			fire = append(fire, s)
		case s.Tag == "err":
			errt = append(errt, s)
		default:
			rest = append(rest, s)
		}
	}
	out := make([]sample, 0, len(samples))
	out = append(out, fire...)
	out = append(out, errt...)
	out = append(out, rest...)
	return out
}

func hasSource(src []string, want string) bool {
	for _, s := range src {
		if s == want {
			return true
		}
	}
	return false
}

// batchesOf splits the ordered queue into chunks of size. The final chunk may be
// smaller. size is validated in [10,20] by main before this is reached.
func batchesOf(samples []sample, size int) [][]sample {
	var batches [][]sample
	for i := 0; i < len(samples); i += size {
		end := i + size
		if end > len(samples) {
			end = len(samples)
		}
		batches = append(batches, samples[i:end])
	}
	return batches
}

// appendLabels appends each record as one JSON object + "\n" to path (JSONL),
// creating it if absent, and flushes before returning so a mid-run kill loses at
// most the in-flight batch. Labels carry hashes, never grids.
func appendLabels(path string, recs []labelRecord) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	w := bufio.NewWriter(f)
	for _, r := range recs {
		if err := writeJSONLine(w, r); err != nil {
			return err
		}
	}
	return w.Flush()
}

// appendPark appends one parkRecord as a JSON object + "\n" to path (JSONL).
func appendPark(path string, p parkRecord) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	w := bufio.NewWriter(f)
	if err := writeJSONLine(w, p); err != nil {
		return err
	}
	return w.Flush()
}

// rewriteLabels atomically writes the full record set to path. Re-pass uses it to
// overwrite updated entries every batch; the labels file is low-thousands lines, so a
// full rewrite is fine. It writes to a temp file in the same directory, fsyncs, then
// renames over path — so a kill mid-write leaves the temp file untouched and the
// previous -out intact, keeping re-pass resumable. A plain truncate-then-write would
// leave -out truncated on a mid-write crash, losing labels resume can't recover.
func rewriteLabels(path string, recs []labelRecord) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".labels-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Remove the temp file on any failure before the rename lands; a successful
	// rename consumes it, so err == nil leaves nothing to clean.
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	w := bufio.NewWriter(tmp)
	for _, r := range recs {
		if werr := writeJSONLine(w, r); werr != nil {
			tmp.Close()
			return werr
		}
	}
	if ferr := w.Flush(); ferr != nil {
		tmp.Close()
		return ferr
	}
	if serr := tmp.Sync(); serr != nil {
		tmp.Close()
		return serr
	}
	if cerr := tmp.Close(); cerr != nil {
		return cerr
	}
	return os.Rename(tmpName, path)
}

func writeJSONLine(w *bufio.Writer, v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := w.Write(line); err != nil {
		return err
	}
	return w.WriteByte('\n')
}

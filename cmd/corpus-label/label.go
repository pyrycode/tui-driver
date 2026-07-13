package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/pyrycode/tui-driver/pkg/tuidriver"
)

// Non-modal taxonomy labels — corpus-label's own vocabulary, each mapped to the
// library concept it names. Kept as constants (not derived) because the library
// exposes these as detector functions, not string constants. Referenced by the
// judge, never by calling the detectors (that would defeat the independent-judge
// premise).
const (
	labelIdle           = "idle"            // tuidriver.IsIdle
	labelBusy           = "busy"            // tuidriver.IsThinking (library term "thinking")
	labelMcpFailure     = "mcp-failure"     // tuidriver.HasMcpFailureBanner
	labelNetworkFailure = "network-failure" // tuidriver.HasNetworkFailure
	labelStartup        = "startup"         // tuidriver.Readiness pre-first-prompt family
)

// unusualPrefix is the free-text escape hatch: any label with this prefix is valid
// regardless of the taxonomy. It is the whole point of the independent judge — it
// surfaces situations no enum value covers (the dot-frame spinner gap #243 was
// found exactly this way, by reading frames rather than by any detector).
const unusualPrefix = "unusual:"

// screenHashMarker prefixes each screen block in the prompt so a response object
// can be tied back to its screen; the fake claude in tests reads the batch hashes
// off these markers too.
const screenHashMarker = "SCREEN-HASH:"

// taxonomyLabels returns the sorted set of enum labels the judge may assign. The
// modal half is built from pkg/tuidriver's ModalClass constants so the vocabulary
// stays in sync with the detectors; ModalClassAgents (retired #245 — cannot render
// on the pinned claude) and ModalClassUnknown (empty string) are excluded.
func taxonomyLabels() []string {
	labels := []string{
		labelIdle, labelBusy, labelMcpFailure, labelNetworkFailure, labelStartup,
		string(tuidriver.ModalClassPermission),
		string(tuidriver.ModalClassTrustFolder),
		string(tuidriver.ModalClassMCP),
		string(tuidriver.ModalClassSlashPicker),
		string(tuidriver.ModalClassAskUserQuestion),
		string(tuidriver.ModalClassModelSelect),
		string(tuidriver.ModalClassPermissionsConfig),
	}
	sort.Strings(labels)
	return labels
}

// taxonomySet is taxonomyLabels as a membership set for validation.
func taxonomySet() map[string]bool {
	set := map[string]bool{}
	for _, l := range taxonomyLabels() {
		set[l] = true
	}
	return set
}

// validLabel reports whether label is an allowed taxonomy enum value or a free-text
// unusual: label.
func validLabel(label string, tax map[string]bool) bool {
	return tax[label] || strings.HasPrefix(label, unusualPrefix)
}

// validConfidence reports whether c is in [0,1].
func validConfidence(c float64) bool {
	return c >= 0 && c <= 1
}

// buildPrompt renders the batch into a single prompt for the judge: the allowed
// labels, the unusual: instruction, the JSON-array output contract, then one block
// per screen tagged with its hash and raw grid. Grids appear ONLY here (the judge's
// prompt file) — never on argv, never on this process's stdout, never in a record.
func buildPrompt(batch []sample, labels []string) string {
	var b strings.Builder
	b.WriteString("You are an independent judge classifying terminal screen captures from the claude CLI.\n")
	b.WriteString("Assign each screen exactly one label from this list:\n")
	for _, l := range labels {
		b.WriteString("  - ")
		b.WriteString(l)
		b.WriteByte('\n')
	}
	b.WriteString("If none of those fit, use \"unusual: <short description>\".\n")
	b.WriteString("Give each a confidence in the range [0,1].\n")
	b.WriteString("Respond with ONLY a JSON array, one object per screen, no prose:\n")
	b.WriteString("[{\"hash\":\"...\",\"label\":\"...\",\"confidence\":0.0}, ...]\n\n")
	for _, s := range batch {
		b.WriteString(screenHashMarker)
		b.WriteByte(' ')
		b.WriteString(s.Hash)
		b.WriteByte('\n')
		b.WriteString(s.Grid)
		b.WriteString("\n---\n")
	}
	return b.String()
}

// labeler labels batches through a runner (production: one fresh `pyry agent-run`
// per batch on the subscription login).
type labeler struct {
	runner runner          // the per-batch judge call; the test injection seam
	model  string          // -model; recorded on each label and park record
	labels []string        // sorted taxonomy for the prompt
	tax    map[string]bool // taxonomy membership set for validation
}

// labelBatch labels one batch: build prompt → run the judge → parse+validate.
// Malformed output (bad JSON, hash mismatch, invalid label/confidence) or a
// runner error (non-zero exit, timeout, unparseable result) is retried once;
// still malformed → the batch is parked with the verbatim last response
// (returned as parked, labels nil). Never drops a screen, never crashes the run.
func (l *labeler) labelBatch(ctx context.Context, batch []sample) (labels []labelRecord, parked *parkRecord) {
	prompt := buildPrompt(batch, l.labels)
	var lastRaw string
	for attempt := 1; attempt <= 2; attempt++ {
		raw, err := l.runner.run(ctx, prompt)
		if err != nil {
			// A runner error is treated as malformed for this attempt. raw is
			// empty on error (the runner never returns grid-bearing stdout), so
			// the park record captures only the error text.
			lastRaw = strings.TrimSpace(raw + "\n" + err.Error())
			continue
		}
		lastRaw = raw
		recs, perr := parseResponse(raw, batch, l.tax, l.model)
		if perr != nil {
			continue
		}
		return recs, nil
	}
	return nil, &parkRecord{
		Hashes:   hashesOf(batch),
		Raw:      lastRaw,
		Attempts: 2,
		Model:    l.model,
	}
}

// parseResponse extracts the outermost JSON array from raw (models may wrap it in
// prose), unmarshals it, and validates against the batch: every batch hash present
// exactly once, no extra hashes, each label valid per the taxonomy contract, each
// confidence in [0,1]. Any failure returns an error (the caller treats it as
// malformed). On success it returns one labelRecord per screen, carrying the model
// plus the sample's cast/tag for #258's join. Error messages carry hashes only,
// never grid content.
func parseResponse(raw string, batch []sample, tax map[string]bool, model string) ([]labelRecord, error) {
	arr, ok := extractJSONArray(raw)
	if !ok {
		return nil, fmt.Errorf("no JSON array in response")
	}
	var items []struct {
		Hash       string  `json:"hash"`
		Label      string  `json:"label"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(arr), &items); err != nil {
		return nil, fmt.Errorf("unmarshal response array: %w", err)
	}
	byHash := make(map[string]sample, len(batch))
	for _, s := range batch {
		byHash[s.Hash] = s
	}
	seen := make(map[string]bool, len(items))
	recs := make([]labelRecord, 0, len(items))
	for _, it := range items {
		s, ok := byHash[it.Hash]
		if !ok {
			return nil, fmt.Errorf("response hash %s not in batch", it.Hash)
		}
		if seen[it.Hash] {
			return nil, fmt.Errorf("duplicate hash %s in response", it.Hash)
		}
		seen[it.Hash] = true
		if !validLabel(it.Label, tax) {
			return nil, fmt.Errorf("invalid label for hash %s", it.Hash)
		}
		if !validConfidence(it.Confidence) {
			return nil, fmt.Errorf("confidence out of range for hash %s", it.Hash)
		}
		recs = append(recs, labelRecord{
			Hash:       s.Hash,
			Label:      it.Label,
			Confidence: it.Confidence,
			Model:      model,
			Cast:       s.Cast,
			Tag:        s.Tag,
		})
	}
	if len(seen) != len(batch) {
		return nil, fmt.Errorf("response labeled %d of %d screens", len(seen), len(batch))
	}
	return recs, nil
}

// extractJSONArray returns the substring from the first '[' to the last ']' in s,
// tolerating prose wrapped around the array. ok is false if no bracketed span
// exists. It does not validate JSON — the caller's Unmarshal does.
func extractJSONArray(s string) (string, bool) {
	i := strings.IndexByte(s, '[')
	j := strings.LastIndexByte(s, ']')
	if i < 0 || j < 0 || j < i {
		return "", false
	}
	return s[i : j+1], true
}

func hashesOf(batch []sample) []string {
	hs := make([]string, len(batch))
	for i, s := range batch {
		hs[i] = s.Hash
	}
	return hs
}

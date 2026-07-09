package tuidriver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// corpusManifestEntry mirrors the JSON shape cmd/corpus-sampler's promote mode
// writes to testdata/corpus/manifest.json (#258). It is a DATA contract, not an
// imported Go API — a package main cannot be imported — so these JSON tags must
// stay in lockstep with promote.go's manifestEntry. That is the intended,
// low-risk coupling the ticket's size note calls out.
type corpusManifestEntry struct {
	Fixture        string `json:"fixture"`
	ModalClass     string `json:"modalClass"`
	Busy           bool   `json:"busy"`
	Idle           bool   `json:"idle"`
	McpFailure     bool   `json:"mcpFailure"`
	NetworkFailure bool   `json:"networkFailure"`
	UnknownDialog  bool   `json:"unknownDialog"`
	Cast           string `json:"cast"`
	Event          int    `json:"event"`
	Hash           string `json:"hash"`
	Note           string `json:"note,omitempty"`
}

// TestCorpusFixtureClassification is the single manifest-driven test that pins
// every promoted corpus fixture (#258). It loads testdata/corpus/manifest.json
// and, for each entry, feeds the raw-snapshot fixture through the SAME detector
// path the existing testdata/*.bin fixtures use
// (TestDetectModalClassRealFixtures), asserting every recorded axis. Promoting a
// new screen needs zero new test code — just the tool run + a git add.
//
// Self-reference discipline (AC): failure messages reference the fixture PATH
// and expected-vs-actual class name / bool ONLY. They never print the snapshot,
// the grid, or any byte content — a printed anchor could forge a live detector
// when the suite runs inside the agent pipeline (as #152/#154/#155).
func TestCorpusFixtureClassification(t *testing.T) {
	manifestPath := filepath.Join("testdata", "corpus", "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read corpus manifest %s: %v", manifestPath, err)
	}
	var entries []corpusManifestEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("parse corpus manifest %s: %v", manifestPath, err)
	}
	// The seeds guarantee >= 2 entries; an empty/missing manifest is a real
	// failure, not a skip.
	if len(entries) < 2 {
		t.Fatalf("corpus manifest %s has %d entries, want >= 2 (idle + modal seeds)", manifestPath, len(entries))
	}

	for _, e := range entries {
		t.Run(e.Fixture, func(t *testing.T) {
			snap, err := os.ReadFile(filepath.Join("testdata", "corpus", e.Fixture))
			if err != nil {
				t.Fatalf("read fixture %s: %v", e.Fixture, err)
			}
			if got := DetectModalClass(snap); got != ModalClass(e.ModalClass) {
				t.Errorf("%s: DetectModalClass = %q, want %q", e.Fixture, got, e.ModalClass)
			}
			if got := IsThinking(snap); got != e.Busy {
				t.Errorf("%s: IsThinking = %v, want %v", e.Fixture, got, e.Busy)
			}
			if got := IsIdle(snap); got != e.Idle {
				t.Errorf("%s: IsIdle = %v, want %v", e.Fixture, got, e.Idle)
			}
			if got := HasMcpFailureBanner(snap); got != e.McpFailure {
				t.Errorf("%s: HasMcpFailureBanner = %v, want %v", e.Fixture, got, e.McpFailure)
			}
			if got := HasNetworkFailure(snap); got != e.NetworkFailure {
				t.Errorf("%s: HasNetworkFailure = %v, want %v", e.Fixture, got, e.NetworkFailure)
			}
			if got := HasUnknownDialog(snap); got != e.UnknownDialog {
				t.Errorf("%s: HasUnknownDialog = %v, want %v", e.Fixture, got, e.UnknownDialog)
			}
		})
	}
}

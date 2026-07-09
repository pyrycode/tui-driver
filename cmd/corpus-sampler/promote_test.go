package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSyntheticCast writes a minimal asciinema cast (120x40 header + one output
// event per frame, 0.1s apart) to path. Frames are plain UTF-8 with no ESC
// bytes, so the .cast fixture ESC-escaping landmine (codebase/255 / #274) never
// applies — json.Marshal handles the framing. Bytes accumulate in the rolling
// buffer across frames, exactly as the live consumer sees them.
func writeSyntheticCast(t *testing.T, path string, frames ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"version":2,"width":120,"height":40}` + "\n")
	for i, f := range frames {
		line, err := json.Marshal([]any{float64(i), "o", f})
		if err != nil {
			t.Fatalf("marshal cast event %d: %v", i, err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write cast %s: %v", path, err)
	}
}

// gapCastSample lays down a two-frame cast whose 1.0s inter-event gap makes the
// sampler emit one gap sample for the first frame (a bare ❯ idle prompt),
// collects it in-process to get a real matching hash, writes the sampler JSONL,
// and returns the recordings dir, the JSONL path, and the sole distinct sample.
// The gap source (Event=i-1, snapshot before appending frame 1) exercises the
// mid-cast reconstruction path, not just -final.
func gapCastSample(t *testing.T) (recordingsDir, jsonlPath string, s sample) {
	t.Helper()
	recordingsDir = filepath.Join(t.TempDir(), "casts")
	if err := os.MkdirAll(recordingsDir, 0o755); err != nil {
		t.Fatalf("mkdir casts: %v", err)
	}
	castPath := filepath.Join(recordingsDir, "foo-ok.cast")
	// Frame 0 renders a bare idle prompt; frame 1 is a distinct idle prompt 1.0s
	// later. The gap samples frame 0 (buffer = frame 0 only).
	writeSyntheticCast(t, castPath, "❯ ready one\r\n", "\x1b[2J\x1b[H❯ ready two\r\n")

	distinct, _, _, _, err := collect([]string{castPath}, 0.5, false, false, 0)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(distinct) != 1 {
		t.Fatalf("collect distinct = %d, want 1 (one gap sample)", len(distinct))
	}
	jsonlPath = filepath.Join(t.TempDir(), "screens.jsonl")
	if err := writeSamples(jsonlPath, distinct); err != nil {
		t.Fatalf("writeSamples: %v", err)
	}
	return recordingsDir, jsonlPath, distinct[0]
}

func readManifestFile(t *testing.T, outDir string) []manifestEntry {
	t.Helper()
	entries, err := readManifest(filepath.Join(outDir, manifestName))
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	return entries
}

func TestPromote_WritesFixtureAndManifest(t *testing.T) {
	recordingsDir, jsonlPath, s := gapCastSample(t)
	outDir := t.TempDir()

	if err := promote(jsonlPath, s.Hash, recordingsDir, outDir); err != nil {
		t.Fatalf("promote: %v", err)
	}

	// The fixture is the raw snapshot: the gap sample's buffer held only frame 0,
	// so the bytes are exactly that frame.
	fixtureName := s.Hash + ".bin"
	got, err := os.ReadFile(filepath.Join(outDir, fixtureName))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if want := []byte("❯ ready one\r\n"); !bytes.Equal(got, want) {
		t.Errorf("fixture bytes = %q, want %q", got, want)
	}

	entries := readManifestFile(t, outDir)
	if len(entries) != 1 {
		t.Fatalf("manifest entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Fixture != fixtureName {
		t.Errorf("Fixture = %q, want %q", e.Fixture, fixtureName)
	}
	if e.Hash != s.Hash {
		t.Errorf("Hash = %q, want %q", e.Hash, s.Hash)
	}
	if e.Cast != "foo-ok.cast" {
		t.Errorf("Cast = %q, want foo-ok.cast", e.Cast)
	}
	if e.Event != 0 {
		t.Errorf("Event = %d, want 0", e.Event)
	}
	// The recorded axes must match the detectors on the reconstructed snapshot —
	// a bare ❯ prompt is idle, no modal, no spinner, no banners.
	if e.ModalClass != "" {
		t.Errorf("ModalClass = %q, want \"\"", e.ModalClass)
	}
	if !e.Idle {
		t.Errorf("Idle = false, want true")
	}
	if e.Busy || e.McpFailure || e.NetworkFailure || e.UnknownDialog {
		t.Errorf("unexpected axis fired: %+v", e)
	}
}

func TestPromote_IdempotentReRun(t *testing.T) {
	recordingsDir, jsonlPath, s := gapCastSample(t)
	outDir := t.TempDir()

	if err := promote(jsonlPath, s.Hash, recordingsDir, outDir); err != nil {
		t.Fatalf("promote (first): %v", err)
	}
	fixturePath := filepath.Join(outDir, s.Hash+".bin")
	first, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture after first promote: %v", err)
	}

	if err := promote(jsonlPath, s.Hash, recordingsDir, outDir); err != nil {
		t.Fatalf("promote (second): %v", err)
	}
	second, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture after second promote: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("fixture bytes changed across re-run: %q vs %q", first, second)
	}
	if entries := readManifestFile(t, outDir); len(entries) != 1 {
		t.Fatalf("manifest entries after re-run = %d, want 1 (no duplicate)", len(entries))
	}
}

func TestPromote_PreservesHumanConfirmedValue(t *testing.T) {
	recordingsDir, jsonlPath, s := gapCastSample(t)
	outDir := t.TempDir()

	// Seed the manifest with a human-edited axis that disagrees with the detector
	// (the fixture is plainly idle, but a reviewer recorded idle=false with a
	// note). A re-promote must skip-not-overwrite so the human's call survives.
	seeded := []manifestEntry{{
		Fixture: s.Hash + ".bin",
		Hash:    s.Hash,
		Cast:    "foo-ok.cast",
		Event:   0,
		Idle:    false,
		Note:    "human says not idle",
	}}
	if err := writeManifest(filepath.Join(outDir, manifestName), seeded); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	if err := promote(jsonlPath, s.Hash, recordingsDir, outDir); err != nil {
		t.Fatalf("promote: %v", err)
	}

	entries := readManifestFile(t, outDir)
	if len(entries) != 1 {
		t.Fatalf("manifest entries = %d, want 1", len(entries))
	}
	if entries[0].Idle {
		t.Errorf("Idle = true, want false (human-confirmed value overwritten)")
	}
	if entries[0].Note != "human says not idle" {
		t.Errorf("Note = %q, want %q (human note lost)", entries[0].Note, "human says not idle")
	}
}

func TestPromote_HashMismatchAborts(t *testing.T) {
	recordingsDir, _, s := gapCastSample(t)
	outDir := t.TempDir()

	// Rewrite the JSONL with a tampered hash for the same provenance: findSample
	// matches on the tampered hash, but reconstruction recomputes the real hash
	// from the cast and detects the mismatch — the recordings-out-of-sync case.
	tampered := s
	tampered.Hash = strings.Repeat("0", 64)
	jsonlPath := filepath.Join(t.TempDir(), "tampered.jsonl")
	if err := writeSamples(jsonlPath, []sample{tampered}); err != nil {
		t.Fatalf("writeSamples: %v", err)
	}

	if err := promote(jsonlPath, tampered.Hash, recordingsDir, outDir); err == nil {
		t.Fatal("promote succeeded, want hash-mismatch error")
	}
	// Nothing may be written on a failed promotion.
	if got := listDir(t, outDir); len(got) != 0 {
		t.Errorf("outDir not empty after aborted promote: %v", got)
	}
}

func TestPromote_EventOutOfRangeAborts(t *testing.T) {
	recordingsDir, _, s := gapCastSample(t)
	outDir := t.TempDir()

	// Keep the real hash so findSample matches, but push Event past the cast's
	// output events so reconstruction exhausts before reaching the index.
	oor := s
	oor.Event = 99
	jsonlPath := filepath.Join(t.TempDir(), "oor.jsonl")
	if err := writeSamples(jsonlPath, []sample{oor}); err != nil {
		t.Fatalf("writeSamples: %v", err)
	}

	if err := promote(jsonlPath, oor.Hash, recordingsDir, outDir); err == nil {
		t.Fatal("promote succeeded, want event-out-of-range error")
	}
	if got := listDir(t, outDir); len(got) != 0 {
		t.Errorf("outDir not empty after aborted promote: %v", got)
	}
}

func TestPromote_HashNotFound(t *testing.T) {
	recordingsDir, jsonlPath, _ := gapCastSample(t)
	outDir := t.TempDir()

	err := promote(jsonlPath, strings.Repeat("f", 64), recordingsDir, outDir)
	if err == nil {
		t.Fatal("promote succeeded, want hash-not-found error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want it to mention 'not found'", err)
	}
	if got := listDir(t, outDir); len(got) != 0 {
		t.Errorf("outDir not empty after aborted promote: %v", got)
	}
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

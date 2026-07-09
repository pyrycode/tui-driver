package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/pyrycode/tui-driver/pkg/tuidriver"
)

// manifestEntry is one promoted fixture's committed record: the raw-snapshot
// fixture basename, the per-axis expected classification, and provenance. It is
// consumed as a DATA artifact — pkg/tuidriver's table test declares a separate
// struct with identical JSON tags (a package main cannot be imported), so the
// two tag sets must stay in lockstep. That is the intended, low-risk coupling
// #258's size note calls out (no mid-flight Go-API drift).
//
// The axis values the promoter writes are the detector's CURRENT verdict as a
// convenience; the COMMITTED value is the human's confirmed call (see the
// mandatory review step in README.md). For the seed screens detector and human
// agree, so the suite stays green.
type manifestEntry struct {
	Fixture        string `json:"fixture"`        // basename under testdata/corpus/, "<hash>.bin"
	ModalClass     string `json:"modalClass"`     // DetectModalClass value; "" == ModalClassUnknown
	Busy           bool   `json:"busy"`           // IsThinking
	Idle           bool   `json:"idle"`           // IsIdle
	McpFailure     bool   `json:"mcpFailure"`     // HasMcpFailureBanner
	NetworkFailure bool   `json:"networkFailure"` // HasNetworkFailure
	UnknownDialog  bool   `json:"unknownDialog"`  // HasUnknownDialog
	Cast           string `json:"cast"`           // provenance: source cast filename
	Event          int    `json:"event"`          // provenance: output-event index
	Hash           string `json:"hash"`           // idempotency key + provenance
	Note           string `json:"note,omitempty"` // optional human eyeball label
}

// manifestName is the fixed basename of the committed manifest inside the
// corpus fixture directory.
const manifestName = "manifest.json"

// promote turns one sampled screen into a committed fixture + manifest entry.
// It finds the sample in the sampler JSONL by hash, reconstructs the raw buffer
// snapshot by replaying the source cast (the JSONL stores only the rendered grid
// + hash, never the raw bytes), verifies the reconstruction against the recorded
// hash, writes the fixture, and appends a manifest entry.
//
// Re-running for a hash already in the manifest is a success no-op: the fixture
// is rewritten byte-identical (reconstruction is deterministic) and the manifest
// is left untouched, so a human-confirmed axis value from a prior review is
// preserved. Reconstruction and verification happen BEFORE any write, so a
// failed promotion leaves the tree untouched.
func promote(inJSONL, hash, recordingsDir, outDir string) error {
	s, err := findSample(inJSONL, hash)
	if err != nil {
		return err
	}

	castPath := filepath.Join(recordingsDir, s.Cast)
	snap, err := reconstructSnapshot(castPath, s.Event, s.Cols, s.Rows, s.Hash)
	if err != nil {
		return err
	}

	// Read the manifest before touching the tree so a corrupt manifest aborts
	// without clobbering anything.
	manifestPath := filepath.Join(outDir, manifestName)
	entries, err := readManifest(manifestPath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating corpus dir %s: %w", outDir, err)
	}
	fixtureName := hash + ".bin"
	fixturePath := filepath.Join(outDir, fixtureName)
	if err := os.WriteFile(fixturePath, snap, 0o644); err != nil {
		return fmt.Errorf("writing fixture %s: %w", fixturePath, err)
	}

	// Skip-not-overwrite: an entry for this hash already exists, so the fixture
	// (rewritten identical above) is all that changes — the human-confirmed
	// manifest value stands.
	for _, e := range entries {
		if e.Hash == hash {
			return nil
		}
	}

	entry := computeVerdicts(snap)
	entry.Fixture = fixtureName
	entry.Cast = s.Cast
	entry.Event = s.Event
	entry.Hash = hash
	entries = append(entries, entry)
	return writeManifest(manifestPath, entries)
}

// findSample scans the sampler JSONL for the entry whose normalized-grid hash
// equals hash and returns it. The sampler dedupes on hash, so at most one entry
// matches; first match wins. Errors if the file is unreadable, a line is
// malformed, or no entry matches.
func findSample(inJSONL, hash string) (sample, error) {
	f, err := os.Open(inJSONL)
	if err != nil {
		return sample{}, fmt.Errorf("opening sampler JSONL %s: %w", inJSONL, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // sampler lines carry a full grid
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var s sample
		if err := json.Unmarshal(line, &s); err != nil {
			return sample{}, fmt.Errorf("parsing sampler JSONL %s: %w", inJSONL, err)
		}
		if s.Hash == hash {
			return s, nil
		}
	}
	if err := sc.Err(); err != nil {
		return sample{}, fmt.Errorf("reading sampler JSONL %s: %w", inJSONL, err)
	}
	return sample{}, fmt.Errorf("hash %s not found in %s", hash, inJSONL)
}

// reconstructSnapshot replays castPath, appending each output event's bytes to a
// fresh rolling buffer, and returns the raw snapshot after appending the output
// event at index event. This is the source-independent inverse of sampleCast:
// every sampler source (gap / fire / midstream / final) stamps Event so that the
// events in the buffer are exactly 0…Event inclusive.
//
// It verifies the reconstruction by rendering at the sample's stored cols/rows
// (the dims the sampler hashed at) and comparing hashGrid(normalize(...)) to
// wantHash — a mismatch means the recordings dir is out of sync with the JSONL.
// The error never prints the grid, only the cast path (self-reference
// discipline). Errors if the cast has fewer than event+1 output events.
func reconstructSnapshot(castPath string, event, cols, rows int, wantHash string) ([]byte, error) {
	f, err := os.Open(castPath)
	if err != nil {
		return nil, fmt.Errorf("opening source cast %s: %w", castPath, err)
	}
	defer f.Close()

	buf := tuidriver.NewBuffer(0)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	header := true
	idx := 0
	var snap []byte
	found := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if header {
			header = false // first line is the asciinema header; dims come from the sample
			continue
		}
		_, data, ok := parseOutputEventTS(line)
		if !ok {
			continue // non-output event ("i" input, resize, or a parse miss)
		}
		buf.Append(data)
		if idx == event {
			snap = buf.Snapshot()
			found = true
			break
		}
		idx++
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading source cast %s: %w", castPath, err)
	}
	if !found {
		return nil, fmt.Errorf("cast %s: only %d output events, need index %d", castPath, idx, event)
	}

	got := hashGrid(normalize(tuidriver.Render(snap, cols, rows)))
	if got != wantHash {
		return nil, fmt.Errorf("reconstructed hash mismatch for %s (recordings dir out of sync with the JSONL)", castPath)
	}
	return snap, nil
}

// computeVerdicts pre-fills the six per-axis classifications from the public
// detectors on the raw snapshot. The detectors render at their default dims
// (NewGrid(snap, 0, 0)) — the SAME entry points the manifest-driven table test
// re-runs — so the recorded verdicts and the test agree by construction,
// independent of the sample's stored cols/rows. Provenance fields are left for
// the caller to stamp.
func computeVerdicts(snap []byte) manifestEntry {
	return manifestEntry{
		ModalClass:     string(tuidriver.DetectModalClass(snap)),
		Busy:           tuidriver.IsThinking(snap),
		Idle:           tuidriver.IsIdle(snap),
		McpFailure:     tuidriver.HasMcpFailureBanner(snap),
		NetworkFailure: tuidriver.HasNetworkFailure(snap),
		UnknownDialog:  tuidriver.HasUnknownDialog(snap),
	}
}

// readManifest loads the committed manifest. A missing or empty file is not an
// error — the first promotion creates it — and yields an empty slice.
func readManifest(path string) ([]manifestEntry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading manifest %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var entries []manifestEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parsing manifest %s: %w", path, err)
	}
	return entries, nil
}

// writeManifest writes entries as a pretty-printed JSON array sorted by fixture
// name with a trailing newline, so re-writes are byte-stable and diffs stay
// minimal for the human-review step.
func writeManifest(path string, entries []manifestEntry) error {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Fixture < entries[j].Fixture })
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling manifest: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing manifest %s: %w", path, err)
	}
	return nil
}

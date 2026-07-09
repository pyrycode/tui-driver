package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestMain re-execs the test binary as a fake `claude`: when CORPUS_LABEL_FAKE=1
// the process reads the prompt on stdin, consults scenario env knobs, emits a
// canned response, and exits — so no test ever calls a real model. Tests set
// -claude = os.Args[0] and pass the marker + scenario knobs via t.Setenv, which
// ride through scrubEnv (it strips only ANTHROPIC_API_KEY).
func TestMain(m *testing.M) {
	if os.Getenv(fakeMarkerEnv) == "1" {
		os.Exit(fakeClaude())
	}
	os.Exit(m.Run())
}

const (
	fakeMarkerEnv = "CORPUS_LABEL_FAKE"       // "1" → act as fake claude
	fakeModeEnv   = "CORPUS_LABEL_FAKE_MODE"  // response scenario
	fakeCountEnv  = "CORPUS_LABEL_FAKE_COUNT" // call-count file (retry scenario)
)

// fakeClaude is the fake `claude -p`. It first enforces the billing invariant —
// the API key must be absent from the child env (its presence would flip billing
// to metered) — then reads the batch hashes off stdin and emits a response per the
// scenario knob. Returns the process exit code.
func fakeClaude() int {
	// Billing invariant (AC): the key must have been scrubbed from the child env.
	// Its presence is a hard failure — exit non-zero so the batch parks and the
	// driving test fails loudly.
	if _, ok := os.LookupEnv(apiKeyEnv); ok {
		fmt.Fprintln(os.Stderr, "fake claude: "+apiKeyEnv+" present in child env — scrub failed")
		return 3
	}

	in, _ := io.ReadAll(os.Stdin)
	hashes := hashesFromPrompt(string(in))

	mode := os.Getenv(fakeModeEnv)
	if mode == "" {
		mode = "valid"
	}
	if mode == "retry" {
		// Malformed on call 1, valid on call 2, driven by a call-count file.
		if bumpCount(os.Getenv(fakeCountEnv)) == 1 {
			fmt.Println("sorry — no json this time")
			return 0
		}
		mode = "valid"
	}

	switch mode {
	case "valid":
		fmt.Print(fakeArray(hashes, "idle", 0.9))
	case "lowconf":
		fmt.Print(fakeArray(hashes, "idle", 0.3))
	case "unusual":
		fmt.Print(fakeArray(hashes, "unusual: something the enum misses", 0.9))
	case "badlabel":
		fmt.Print(fakeArray(hashes, "not-a-real-label", 0.9))
	case "badconf":
		fmt.Print(fakeArray(hashes, "idle", 1.5))
	case "missing":
		// Drop the last hash → count mismatch → malformed.
		if len(hashes) > 0 {
			hashes = hashes[:len(hashes)-1]
		}
		fmt.Print(fakeArray(hashes, "idle", 0.9))
	case "malformed":
		fmt.Println("this is prose, not a json array")
	default:
		fmt.Println("unknown fake scenario")
	}
	return 0
}

// hashesFromPrompt reads the batch hashes back out of the prompt's SCREEN-HASH
// markers — the same markers the parse ties responses to.
func hashesFromPrompt(prompt string) []string {
	var hs []string
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, screenHashMarker) {
			if h := strings.TrimSpace(strings.TrimPrefix(line, screenHashMarker)); h != "" {
				hs = append(hs, h)
			}
		}
	}
	return hs
}

// fakeArray renders a valid JSON array wrapped in a line of prose, exercising the
// tolerant array extraction end-to-end.
func fakeArray(hashes []string, label string, conf float64) string {
	var b strings.Builder
	b.WriteString("Here are the labels you asked for:\n[")
	for i, h := range hashes {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"hash":%q,"label":%q,"confidence":%g}`, h, label, conf)
	}
	b.WriteString("]\n")
	return b.String()
}

func bumpCount(path string) int {
	if path == "" {
		return 1
	}
	raw, _ := os.ReadFile(path)
	n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	n++
	_ = os.WriteFile(path, []byte(strconv.Itoa(n)), 0o644)
	return n
}

// --- test helpers ---

func useFakeClaude(t *testing.T, mode string) {
	t.Helper()
	t.Setenv(fakeMarkerEnv, "1")
	t.Setenv(fakeModeEnv, mode)
}

func fakeLabeler(model string) *labeler {
	return &labeler{
		claudePath: os.Args[0],
		model:      model,
		labels:     taxonomyLabels(),
		tax:        taxonomySet(),
	}
}

func mkBatch(n int) []sample {
	b := make([]sample, n)
	for i := range b {
		h := fmt.Sprintf("h%03d", i)
		b[i] = sample{Hash: h, Grid: "synthetic screen " + h + "\n", Cast: "fixture-ok.cast", Tag: "ok"}
	}
	return b
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeSamplesJSONL(t *testing.T, path string, samples []sample) {
	t.Helper()
	var lines []string
	for _, s := range samples {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(b))
	}
	writeLines(t, path, lines...)
}

func readLabelsT(t *testing.T, path string) []labelRecord {
	t.Helper()
	recs, err := readLabels(path)
	if err != nil {
		t.Fatalf("readLabels: %v", err)
	}
	return recs
}

// --- labelBatch scenarios (drive the fake directly) ---

func TestLabelBatch_HappyPath(t *testing.T) {
	useFakeClaude(t, "valid")
	l := fakeLabeler("haiku")
	batch := mkBatch(12)
	recs, park := l.labelBatch(batch)
	if park != nil {
		t.Fatalf("batch parked unexpectedly: %+v", park.Hashes)
	}
	if len(recs) != len(batch) {
		t.Fatalf("labeled %d screens, want %d", len(recs), len(batch))
	}
	for i, r := range recs {
		if r.Hash != batch[i].Hash {
			t.Errorf("rec[%d] hash = %s, want %s", i, r.Hash, batch[i].Hash)
		}
		if r.Label != "idle" || r.Confidence != 0.9 || r.Model != "haiku" {
			t.Errorf("rec %s = %q/%v/%s, want idle/0.9/haiku", r.Hash, r.Label, r.Confidence, r.Model)
		}
		if r.Cast != batch[i].Cast || r.Tag != batch[i].Tag {
			t.Errorf("rec %s cast/tag not carried from sample", r.Hash)
		}
	}
}

func TestLabelBatch_RetryThenSucceed(t *testing.T) {
	useFakeClaude(t, "retry")
	t.Setenv(fakeCountEnv, filepath.Join(t.TempDir(), "count"))
	l := fakeLabeler("haiku")
	batch := mkBatch(11)
	recs, park := l.labelBatch(batch)
	if park != nil {
		t.Fatalf("batch parked, want labeled after one retry")
	}
	if len(recs) != len(batch) {
		t.Fatalf("labeled %d, want %d after retry", len(recs), len(batch))
	}
}

func TestLabelBatch_ParksAfterTwoMalformed(t *testing.T) {
	useFakeClaude(t, "malformed")
	l := fakeLabeler("haiku")
	batch := mkBatch(10)
	recs, park := l.labelBatch(batch)
	if recs != nil {
		t.Fatalf("got labels, want park on twice-malformed")
	}
	if park == nil {
		t.Fatal("batch not parked after two malformed responses")
	}
	if len(park.Hashes) != len(batch) {
		t.Errorf("park hashes = %d, want %d", len(park.Hashes), len(batch))
	}
	if park.Attempts != 2 {
		t.Errorf("park attempts = %d, want 2", park.Attempts)
	}
	if park.Model != "haiku" {
		t.Errorf("park model = %q, want haiku", park.Model)
	}
	if !strings.Contains(park.Raw, "not a json array") {
		t.Errorf("park raw did not capture the verbatim response: %q", park.Raw)
	}
}

func TestLabelBatch_ValidityGate(t *testing.T) {
	batch := mkBatch(10)
	cases := []struct {
		mode     string
		wantPark bool
	}{
		{"unusual", false}, // free-text escape hatch is accepted
		{"badlabel", true}, // outside taxonomy and not unusual: → malformed → park
		{"badconf", true},  // confidence outside [0,1] → malformed → park
		{"missing", true},  // a batch hash unlabeled → count mismatch → park
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			useFakeClaude(t, c.mode)
			l := fakeLabeler("haiku")
			recs, park := l.labelBatch(batch)
			if (park != nil) != c.wantPark {
				t.Errorf("mode %s: parked = %v, want %v", c.mode, park != nil, c.wantPark)
			}
			if !c.wantPark && len(recs) != len(batch) {
				t.Errorf("mode %s: labeled %d, want %d", c.mode, len(recs), len(batch))
			}
		})
	}
}

// TestLabelBatch_APIKeyScrubbed is the billing invariant. The test sets the API
// key in its own env; scrubEnv must remove it from the child, so the fake sees it
// absent and labels normally. If the scrub were broken the fake would exit
// non-zero, the batch would park, and this test would fail loudly.
func TestLabelBatch_APIKeyScrubbed(t *testing.T) {
	useFakeClaude(t, "valid")
	t.Setenv(apiKeyEnv, "sk-should-be-scrubbed")
	l := fakeLabeler("haiku")
	batch := mkBatch(10)
	recs, park := l.labelBatch(batch)
	if park != nil {
		t.Fatalf("batch parked — API key leaked into child env (scrub failed)")
	}
	if len(recs) != len(batch) {
		t.Fatalf("labeled %d, want %d", len(recs), len(batch))
	}
}

func TestScrubEnv_RemovesNotBlanks(t *testing.T) {
	in := []string{"PATH=/bin", apiKeyEnv + "=secret", "HOME=/home"}
	out := scrubEnv(in)
	if len(out) != 2 {
		t.Fatalf("scrubEnv len = %d, want 2 (key removed, not blanked)", len(out))
	}
	for _, kv := range out {
		if strings.HasPrefix(kv, apiKeyEnv+"=") {
			t.Fatalf("scrubEnv left %s present: %q", apiKeyEnv, kv)
		}
	}
}

// --- run() end-to-end scenarios ---

// TestRun_ResumeSkipsLabeledNoDupNoLoss simulates a kill-and-restart: -out is
// pre-populated with the results of an already-completed first slice. A fresh run
// must skip exactly those hashes (no re-label), keep them (no loss), and label the
// remainder once each.
func TestRun_ResumeSkipsLabeledNoDupNoLoss(t *testing.T) {
	useFakeClaude(t, "valid")
	dir := t.TempDir()
	in := filepath.Join(dir, "in.jsonl")
	out := filepath.Join(dir, "out.jsonl")
	park := filepath.Join(dir, "park.jsonl")

	samples := mkBatch(25)
	writeSamplesJSONL(t, in, samples)

	// A completed first slice of 10, already persisted to -out.
	done := make([]labelRecord, 10)
	for i := range done {
		done[i] = labelRecord{Hash: samples[i].Hash, Label: "idle", Confidence: 0.9, Model: "haiku"}
	}
	if err := appendLabels(out, done); err != nil {
		t.Fatal(err)
	}

	if err := run(in, out, park, 15, "haiku", 0.7, os.Args[0]); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := readLabelsT(t, out)
	if len(got) != 25 {
		t.Fatalf("labels = %d, want 25 (10 pre + 15 new, none lost)", len(got))
	}
	seen := map[string]int{}
	for _, r := range got {
		seen[r.Hash]++
	}
	for h, c := range seen {
		if c != 1 {
			t.Errorf("hash %s labeled %d times, want exactly 1", h, c)
		}
	}
	if fileHasLines(t, park) {
		t.Errorf("park file non-empty, want no parks on the happy path")
	}
}

func TestRun_MissingOutIsFullRun(t *testing.T) {
	useFakeClaude(t, "valid")
	dir := t.TempDir()
	in := filepath.Join(dir, "in.jsonl")
	out := filepath.Join(dir, "out.jsonl")
	park := filepath.Join(dir, "park.jsonl")

	writeSamplesJSONL(t, in, mkBatch(12))
	if err := run(in, out, park, 15, "haiku", 0.7, os.Args[0]); err != nil {
		t.Fatalf("run with absent -out: %v", err)
	}
	if got := readLabelsT(t, out); len(got) != 12 {
		t.Fatalf("labels = %d, want 12 (absent -out = full run)", len(got))
	}
}

func TestRun_ParkedBatchLeavesOutUnchanged(t *testing.T) {
	useFakeClaude(t, "malformed")
	dir := t.TempDir()
	in := filepath.Join(dir, "in.jsonl")
	out := filepath.Join(dir, "out.jsonl")
	park := filepath.Join(dir, "park.jsonl")

	writeSamplesJSONL(t, in, mkBatch(12))
	if err := run(in, out, park, 15, "haiku", 0.7, os.Args[0]); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The single batch parks: -out stays empty, -park gets the batch, run does not
	// crash.
	if _, err := readLabels(out); err == nil {
		t.Errorf("out file exists, want no -out writes for a parked batch")
	}
	if !fileHasLines(t, park) {
		t.Errorf("park file empty, want the malformed batch parked")
	}
}

// TestRun_RepassSelectsAndOverwrites drives re-pass mode: a non-default -model
// re-labels only the low-confidence and unusual -out records, overwriting their
// entries; high-confidence records are untouched.
func TestRun_RepassSelectsAndOverwrites(t *testing.T) {
	useFakeClaude(t, "valid") // re-pass re-labels selected screens as idle/0.9/sonnet
	dir := t.TempDir()
	in := filepath.Join(dir, "in.jsonl")
	out := filepath.Join(dir, "out.jsonl")
	park := filepath.Join(dir, "park.jsonl")

	samples := []sample{
		{Hash: "h0", Grid: "synthetic screen h0\n", Cast: "x-ok.cast", Tag: "ok"},
		{Hash: "h1", Grid: "synthetic screen h1\n", Cast: "x-ok.cast", Tag: "ok"},
		{Hash: "h2", Grid: "synthetic screen h2\n", Cast: "x-ok.cast", Tag: "ok"},
	}
	writeSamplesJSONL(t, in, samples)

	pre := []labelRecord{
		{Hash: "h0", Label: "idle", Confidence: 0.9, Model: "haiku"},                  // high conf — untouched
		{Hash: "h1", Label: "busy", Confidence: 0.3, Model: "haiku"},                  // low conf — re-pass
		{Hash: "h2", Label: "unusual: novel thing", Confidence: 0.95, Model: "haiku"}, // unusual — re-pass
	}
	if err := appendLabels(out, pre); err != nil {
		t.Fatal(err)
	}

	if err := run(in, out, park, 15, "sonnet", 0.7, os.Args[0]); err != nil {
		t.Fatalf("run (re-pass): %v", err)
	}

	byHash := map[string]labelRecord{}
	for _, r := range readLabelsT(t, out) {
		byHash[r.Hash] = r
	}
	if len(byHash) != 3 {
		t.Fatalf("labels = %d, want 3 (re-pass overwrites, never grows)", len(byHash))
	}
	if byHash["h0"].Model != "haiku" {
		t.Errorf("h0 (high conf) model = %s, want haiku (untouched)", byHash["h0"].Model)
	}
	if byHash["h1"].Model != "sonnet" || byHash["h1"].Confidence != 0.9 {
		t.Errorf("h1 (low conf) = %s/%v, want sonnet/0.9 (re-passed)", byHash["h1"].Model, byHash["h1"].Confidence)
	}
	if byHash["h2"].Model != "sonnet" {
		t.Errorf("h2 (unusual) model = %s, want sonnet (re-passed)", byHash["h2"].Model)
	}
}

func fileHasLines(t *testing.T, path string) bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(b))) > 0
}

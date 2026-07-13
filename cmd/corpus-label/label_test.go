package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestMain re-execs the test binary as a fake `pyry agent-run`: when
// CORPUS_LABEL_FAKE=1 the process reads the batch prompt from its --prompt-file
// argument, consults scenario env knobs, emits a stream-json result line, and
// exits — so no test ever spawns real pyry or a real model. Tests set
// -pyry = os.Args[0] and pass the marker + scenario knobs via t.Setenv, which
// ride through childEnv (it strips only the two metered-billing variables).
func TestMain(m *testing.M) {
	if os.Getenv(fakeMarkerEnv) == "1" {
		os.Exit(fakePyry())
	}
	os.Exit(m.Run())
}

const (
	fakeMarkerEnv = "CORPUS_LABEL_FAKE"       // "1" → act as fake pyry
	fakeModeEnv   = "CORPUS_LABEL_FAKE_MODE"  // response scenario
	fakeCountEnv  = "CORPUS_LABEL_FAKE_COUNT" // call-count file (retry scenario)
)

// fakePyry is the fake `pyry agent-run`. It first enforces the billing
// invariant — both metered-billing variables must be absent from the child env
// (either would flip billing off the subscription) — then reads the batch
// prompt from --prompt-file and emits a stream-json response per the scenario
// knob. Returns the process exit code.
func fakePyry() int {
	// Billing invariant (AC): both metered vars must have been scrubbed from the
	// child env. Presence is a hard failure — exit non-zero so the batch parks and
	// the driving test fails loudly.
	if _, ok := os.LookupEnv(apiKeyEnv); ok {
		fmt.Fprintln(os.Stderr, "fake pyry: "+apiKeyEnv+" present in child env — billing invariant violated")
		return 3
	}
	if _, ok := os.LookupEnv(useStreamJSONEnv); ok {
		fmt.Fprintln(os.Stderr, "fake pyry: "+useStreamJSONEnv+" present in child env — would route to metered print mode")
		return 4
	}

	prompt := readPromptFileArg()
	hashes := hashesFromPrompt(prompt)

	// Mirror real pyry: echo the delivered user prompt (which carries the grids)
	// as a non-result stream-json line. Every scenario emits this, so the tests
	// prove grids never reach a park record — corpus-label must parse ONLY the
	// trailing result line.
	emitUserEcho(prompt)

	mode := os.Getenv(fakeModeEnv)
	if mode == "" {
		mode = "valid"
	}
	if mode == "retry" {
		// Malformed on call 1, valid on call 2, driven by a call-count file.
		if bumpCount(os.Getenv(fakeCountEnv)) == 1 {
			emitResult("sorry — no json this time")
			return 0
		}
		mode = "valid"
	}

	switch mode {
	case "valid":
		emitResult(fakeArray(hashes, "idle", 0.9))
	case "lowconf":
		emitResult(fakeArray(hashes, "idle", 0.3))
	case "unusual":
		emitResult(fakeArray(hashes, "unusual: something the enum misses", 0.9))
	case "badlabel":
		emitResult(fakeArray(hashes, "not-a-real-label", 0.9))
	case "badconf":
		emitResult(fakeArray(hashes, "idle", 1.5))
	case "missing":
		// Drop the last hash → count mismatch → malformed.
		if len(hashes) > 0 {
			hashes = hashes[:len(hashes)-1]
		}
		emitResult(fakeArray(hashes, "idle", 0.9))
	case "malformed":
		emitResult("this is prose, not a json array")
	case "errortrailer":
		// Exit 0 but an error-typed result line → parseResultText errors → park
		// with a grid-free note.
		emitErrorResult()
	case "exit":
		// Non-zero exit → run() returns an error without touching stdout → park
		// with a grid-free note.
		return 5
	default:
		emitResult("unknown fake scenario")
	}
	return 0
}

// readPromptFileArg finds the --prompt-file value in the re-exec'd argv and
// returns the file's contents (the batch prompt).
func readPromptFileArg() string {
	for i, a := range os.Args {
		if a == "--prompt-file" && i+1 < len(os.Args) {
			b, _ := os.ReadFile(os.Args[i+1])
			return string(b)
		}
	}
	return ""
}

// emitUserEcho writes a `type:"user"` stream-json line carrying the raw prompt
// (grids included), mirroring pyry's verbatim entry re-emit.
func emitUserEcho(prompt string) {
	line, _ := json.Marshal(map[string]any{"type": "user", "raw": prompt})
	fmt.Println(string(line))
}

// emitResult writes a successful `type:"result"` trailer whose `result` field
// is text (the judge's reply). corpus-label reads exactly this field.
func emitResult(text string) {
	line, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "is_error": false,
		"result": text, "terminal_reason": "completed",
	})
	fmt.Println(string(line))
}

// emitErrorResult writes an error-typed `type:"result"` trailer — pyry's wedge
// shape — which corpus-label must treat as a failed attempt.
func emitErrorResult() {
	line, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": "error_during_execution", "is_error": true,
		"result": "", "terminal_reason": "stream_closed",
	})
	fmt.Println(string(line))
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

func useFakePyry(t *testing.T, mode string) {
	t.Helper()
	t.Setenv(fakeMarkerEnv, "1")
	t.Setenv(fakeModeEnv, mode)
}

func fakeLabeler(t *testing.T, model string) *labeler {
	t.Helper()
	sys := filepath.Join(t.TempDir(), "system.txt")
	if err := os.WriteFile(sys, []byte("test system prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &labeler{
		runner: &pyryRunner{
			pyryPath:         os.Args[0],
			model:            model,
			effort:           "low",
			workdir:          ".",
			systemPromptPath: sys,
			maxTurns:         labelMaxTurns,
			timeout:          batchTimeout,
		},
		model:  model,
		labels: taxonomyLabels(),
		tax:    taxonomySet(),
	}
}

// fakeOptions builds a run() options set pointed at the fake pyry.
func fakeOptions(in, out, park string) options {
	return options{
		in: in, out: out, park: park, batch: 15,
		model: "haiku", effort: "low", workdir: ".",
		pyryPath: os.Args[0], minConf: 0.7,
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
	useFakePyry(t, "valid")
	l := fakeLabeler(t, "haiku")
	batch := mkBatch(12)
	recs, park := l.labelBatch(context.Background(), batch)
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
	useFakePyry(t, "retry")
	t.Setenv(fakeCountEnv, filepath.Join(t.TempDir(), "count"))
	l := fakeLabeler(t, "haiku")
	batch := mkBatch(11)
	recs, park := l.labelBatch(context.Background(), batch)
	if park != nil {
		t.Fatalf("batch parked, want labeled after one retry")
	}
	if len(recs) != len(batch) {
		t.Fatalf("labeled %d, want %d after retry", len(recs), len(batch))
	}
}

func TestLabelBatch_ParksAfterTwoMalformed(t *testing.T) {
	useFakePyry(t, "malformed")
	l := fakeLabeler(t, "haiku")
	batch := mkBatch(10)
	recs, park := l.labelBatch(context.Background(), batch)
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
			useFakePyry(t, c.mode)
			l := fakeLabeler(t, "haiku")
			recs, park := l.labelBatch(context.Background(), batch)
			if (park != nil) != c.wantPark {
				t.Errorf("mode %s: parked = %v, want %v", c.mode, park != nil, c.wantPark)
			}
			if !c.wantPark && len(recs) != len(batch) {
				t.Errorf("mode %s: labeled %d, want %d", c.mode, len(recs), len(batch))
			}
		})
	}
}

// TestLabelBatch_PyryErrorParks covers the two runner-error shapes — an
// error-typed result trailer (exit 0) and a non-zero pyry exit. Both retry once
// then park; neither surfaces stdout.
func TestLabelBatch_PyryErrorParks(t *testing.T) {
	for _, mode := range []string{"errortrailer", "exit"} {
		t.Run(mode, func(t *testing.T) {
			useFakePyry(t, mode)
			l := fakeLabeler(t, "haiku")
			recs, park := l.labelBatch(context.Background(), mkBatch(10))
			if recs != nil {
				t.Fatalf("mode %s: got labels, want park", mode)
			}
			if park == nil {
				t.Fatalf("mode %s: batch not parked", mode)
			}
			if park.Attempts != 2 {
				t.Errorf("mode %s: park attempts = %d, want 2", mode, park.Attempts)
			}
		})
	}
}

// TestLabelBatch_ParkRawNeverContainsGrids is the self-reference guarantee at the
// runner seam. The fake pyry echoes the prompt (grids) as a user line in every
// scenario, yet a parked batch's Raw must carry only the model's reply or a
// grid-free error — never grid content.
func TestLabelBatch_ParkRawNeverContainsGrids(t *testing.T) {
	for _, mode := range []string{"malformed", "errortrailer", "exit"} {
		t.Run(mode, func(t *testing.T) {
			useFakePyry(t, mode)
			l := fakeLabeler(t, "haiku")
			_, park := l.labelBatch(context.Background(), mkBatch(10))
			if park == nil {
				t.Fatalf("mode %s: expected park", mode)
			}
			if strings.Contains(park.Raw, "synthetic screen") {
				t.Errorf("mode %s: park raw leaked grid content: %q", mode, park.Raw)
			}
		})
	}
}

// TestLabelBatch_MeteredVarsStrippedFromChild is the billing invariant. Each
// sub-test sets one metered variable in its own env; childEnv must remove it from
// the child, so the fake sees it absent and labels normally. If the scrub were
// broken the fake would exit non-zero, the batch would park, and this fails loudly.
func TestLabelBatch_MeteredVarsStrippedFromChild(t *testing.T) {
	cases := map[string]string{
		apiKeyEnv:        "sk-should-be-stripped",
		useStreamJSONEnv: "1",
	}
	for name, val := range cases {
		t.Run(name, func(t *testing.T) {
			useFakePyry(t, "valid")
			t.Setenv(name, val)
			l := fakeLabeler(t, "haiku")
			batch := mkBatch(10)
			recs, park := l.labelBatch(context.Background(), batch)
			if park != nil {
				t.Fatalf("batch parked — %s leaked into child env (scrub failed)", name)
			}
			if len(recs) != len(batch) {
				t.Fatalf("labeled %d, want %d", len(recs), len(batch))
			}
		})
	}
}

// --- runner unit tests (pure helpers) ---

func TestChildEnv_StripsMeteredVarsKeepsToken(t *testing.T) {
	in := []string{
		"PATH=/bin",
		apiKeyEnv + "=sk-secret",
		useStreamJSONEnv + "=1",
		"CLAUDE_CODE_OAUTH_TOKEN=durable-token",
		"HOME=/home",
	}
	out := childEnv(in)
	has := func(prefix string) bool {
		for _, kv := range out {
			if strings.HasPrefix(kv, prefix) {
				return true
			}
		}
		return false
	}
	if has(apiKeyEnv + "=") {
		t.Errorf("childEnv left %s present", apiKeyEnv)
	}
	if has(useStreamJSONEnv + "=") {
		t.Errorf("childEnv left %s present", useStreamJSONEnv)
	}
	if !has("CLAUDE_CODE_OAUTH_TOKEN=") {
		t.Error("childEnv stripped the durable subscription token")
	}
	if !has("PATH=") || !has("HOME=") {
		t.Error("childEnv stripped unrelated variables")
	}
	if len(out) != 3 {
		t.Errorf("childEnv len = %d, want 3 (two metered vars removed)", len(out))
	}
}

func TestBuildAgentRunArgs_PinsRequiredFlags(t *testing.T) {
	r := &pyryRunner{model: "haiku", effort: "low", workdir: "/w", systemPromptPath: "/sys", maxTurns: 4}
	args := buildAgentRunArgs(r, "/prompt")
	if len(args) == 0 || args[0] != "agent-run" {
		t.Fatalf("args[0] = %v, want agent-run first", args)
	}
	got := flagMap(args)
	want := map[string]string{
		"--prompt-file":        "/prompt",
		"--system-prompt-file": "/sys",
		"--model":              "haiku",
		"--effort":             "low",
		"--workdir":            "/w",
		"--output-format":      "stream-json",
		"--max-turns":          "4",
		"--allowed-tools":      allowedToolsMinimal,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("flag %s = %q, want %q", k, got[k], v)
		}
	}
	if got["--allowed-tools"] == "" {
		t.Error("--allowed-tools is empty; pyry requires a non-empty allow-list")
	}
}

func flagMap(args []string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			m[args[i]] = args[i+1]
			i++
		}
	}
	return m
}

func TestParseResultText_ReadsLastResultLineIgnoringGrids(t *testing.T) {
	// The user echo carries grid content; parseResultText must ignore it and
	// return only the trailing result line's text.
	stdout := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"user","raw":"synthetic screen h000"}`,
		`{"type":"assistant","message":{"id":"m1"}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"[{\"hash\":\"h\"}]","terminal_reason":"completed"}`,
	}, "\n") + "\n"
	got, err := parseResultText(stdout)
	if err != nil {
		t.Fatalf("parseResultText: %v", err)
	}
	if got != `[{"hash":"h"}]` {
		t.Errorf("result = %q, want the array text", got)
	}
	if strings.Contains(got, "synthetic screen") {
		t.Errorf("parseResultText leaked grid content: %q", got)
	}
}

func TestParseResultText_ErrorsOnErrorTrailer(t *testing.T) {
	stdout := `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"","terminal_reason":"stream_closed"}` + "\n"
	if _, err := parseResultText(stdout); err == nil {
		t.Error("parseResultText on an error-typed trailer = nil error, want error")
	}
}

func TestParseResultText_ErrorsWhenAbsent(t *testing.T) {
	stdout := `{"type":"system","subtype":"init"}` + "\n" + `{"type":"user","raw":"x"}` + "\n"
	if _, err := parseResultText(stdout); err == nil {
		t.Error("parseResultText with no result line = nil error, want error")
	}
}

// --- run() end-to-end scenarios ---

// TestRun_ResumeSkipsLabeledNoDupNoLoss simulates a kill-and-restart: -out is
// pre-populated with the results of an already-completed first slice. A fresh run
// must skip exactly those hashes (no re-label), keep them (no loss), and label the
// remainder once each.
func TestRun_ResumeSkipsLabeledNoDupNoLoss(t *testing.T) {
	useFakePyry(t, "valid")
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

	if err := run(context.Background(), fakeOptions(in, out, park)); err != nil {
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
	useFakePyry(t, "valid")
	dir := t.TempDir()
	in := filepath.Join(dir, "in.jsonl")
	out := filepath.Join(dir, "out.jsonl")
	park := filepath.Join(dir, "park.jsonl")

	writeSamplesJSONL(t, in, mkBatch(12))
	if err := run(context.Background(), fakeOptions(in, out, park)); err != nil {
		t.Fatalf("run with absent -out: %v", err)
	}
	if got := readLabelsT(t, out); len(got) != 12 {
		t.Fatalf("labels = %d, want 12 (absent -out = full run)", len(got))
	}
}

func TestRun_ParkedBatchLeavesOutUnchanged(t *testing.T) {
	useFakePyry(t, "malformed")
	dir := t.TempDir()
	in := filepath.Join(dir, "in.jsonl")
	out := filepath.Join(dir, "out.jsonl")
	park := filepath.Join(dir, "park.jsonl")

	writeSamplesJSONL(t, in, mkBatch(12))
	if err := run(context.Background(), fakeOptions(in, out, park)); err != nil {
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
	useFakePyry(t, "valid") // re-pass re-labels selected screens as idle/0.9/sonnet
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

	opt := fakeOptions(in, out, park)
	opt.model = "sonnet"
	if err := run(context.Background(), opt); err != nil {
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

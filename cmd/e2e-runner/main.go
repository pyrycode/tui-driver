// e2e-runner orchestrates the tui-driver end-to-end checks against a real
// `claude` binary. It shells out to the prebuilt spike + probe binaries
// under -bin-dir, runs each as a Check, and emits a single-file JSON report.
//
// Exit code: 0 iff every check passed; 1 otherwise (any fail / timeout /
// report-write error). The report itself always emits when feasible — even
// on partial failure — so CI gets a uniform artifact.
//
// Headless / reproducible MCP: the runner sets TUIDRIVER_STRICT_MCP_CONFIG=1
// on each spawned child. The spike+probe binaries already call
// tuidriver.EnsureClaudeEnv, which transparently appends
// --strict-mcp-config to the claude invocation when that env var is set.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	defaultCheckTimeout = 60 * time.Second
	probeCheckTimeout   = 30 * time.Second
	defaultWallBudget   = 10 * time.Minute
)

// successSuccess matches the four "result" spikes' `SUCCESS: <text>` line.
var successSuccess = regexp.MustCompile(`(?m)^SUCCESS`)

// observedSuccess matches the two "observation" spikes' `OBSERVED: ...` line.
var observedSuccess = regexp.MustCompile(`(?m)^OBSERVED`)

// probeOutDirRe scrapes the probe's "probe outDir=<path>" stderr line.
// The path is whitespace-delimited; matches the logger.Printf format at
// cmd/probe-first-prompt-hang/main.go.
var probeOutDirRe = regexp.MustCompile(`probe outDir=(\S+)`)

// Check is one orchestrated subprocess invocation. Fields are populated at
// startup from the hardcoded check list; runCheck consumes them uniformly.
type Check struct {
	Name          string
	Kind          string         // "spike" | "probe" — informational only
	Binary        string         // path within -bin-dir
	Args          []string
	SuccessMarker *regexp.Regexp // nil = exit-code-only success
	Timeout       time.Duration  // zero falls back to defaultCheckTimeout
	OnFailure     func(stdout, stderr string) map[string]any
}

// CheckResult is the per-check entry surfaced into the report. Extra is
// flattened into the JSON object as siblings of name/status/duration_ms.
type CheckResult struct {
	Name       string
	Status     string // "pass" | "fail" | "timeout"
	DurationMs int64
	Extra      map[string]any
}

// Report is the top-level e2e-report.json shape.
type Report struct {
	ClaudeVersion   string           `json:"claude_version"`
	TotalDurationMs int64            `json:"total_duration_ms"`
	Checks          []map[string]any `json:"checks"`
}

// timeoutOverrides parses repeated -timeout NAME=DUR flags into a map. Names
// not in the check list are rejected at parse time so typos surface early.
type timeoutOverrides struct {
	m     map[string]time.Duration
	known map[string]bool
}

func (t *timeoutOverrides) String() string { return "" }

func (t *timeoutOverrides) Set(raw string) error {
	parts := strings.SplitN(raw, "=", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid -timeout %q (want NAME=DUR)", raw)
	}
	d, err := time.ParseDuration(parts[1])
	if err != nil {
		return fmt.Errorf("invalid -timeout %q: %w", raw, err)
	}
	if !t.known[parts[0]] {
		return fmt.Errorf("unknown check name %q in -timeout (typo?)", parts[0])
	}
	if t.m == nil {
		t.m = make(map[string]time.Duration)
	}
	t.m[parts[0]] = d
	return nil
}

func main() {
	binDir := flag.String("bin-dir", "./bin", "directory containing built spike+probe binaries")
	reportPath := flag.String("report", "./e2e-report.json", "output path for e2e-report.json")
	wall := flag.Duration("wall", defaultWallBudget, "top-level wall budget for the whole run")

	checks := buildChecks()
	tos := &timeoutOverrides{known: map[string]bool{}}
	for _, c := range checks {
		tos.known[c.Name] = true
	}
	flag.Var(tos, "timeout", "per-check timeout override, NAME=DUR (repeatable)")

	flag.Parse()

	for i := range checks {
		if d, ok := tos.m[checks[i].Name]; ok {
			checks[i].Timeout = d
		}
	}

	if err := os.MkdirAll(filepath.Dir(*reportPath), 0755); err != nil && filepath.Dir(*reportPath) != "." {
		fmt.Fprintf(os.Stderr, "mkdir report dir: %v\n", err)
		os.Exit(1)
	}

	parentCtx, cancel := context.WithTimeout(context.Background(), *wall)
	defer cancel()

	runStart := time.Now()
	claudeVersion := captureClaudeVersion(parentCtx)
	fmt.Fprintf(os.Stderr, "e2e-runner: claude_version=%q\n", claudeVersion)

	results := make([]CheckResult, 0, len(checks))
	allPassed := true
	for _, c := range checks {
		if parentCtx.Err() != nil {
			// Top-level wall budget exhausted — report remaining checks as
			// timeout with duration_ms=0 so the array length stays stable.
			results = append(results, CheckResult{
				Name:       c.Name,
				Status:     "timeout",
				DurationMs: 0,
			})
			allPassed = false
			continue
		}
		fmt.Fprintf(os.Stderr, "e2e-runner: running %s\n", c.Name)
		r := runCheck(parentCtx, c, *binDir)
		fmt.Fprintf(os.Stderr, "e2e-runner: %s -> %s (%dms)\n", r.Name, r.Status, r.DurationMs)
		if r.Status != "pass" {
			allPassed = false
		}
		results = append(results, r)
	}

	total := time.Since(runStart)
	report := Report{
		ClaudeVersion:   claudeVersion,
		TotalDurationMs: total.Milliseconds(),
		Checks:          marshalResults(results),
	}

	if err := writeReport(*reportPath, report); err != nil {
		fmt.Fprintf(os.Stderr, "write report: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "e2e-runner: wrote %s\n", *reportPath)

	if !allPassed {
		os.Exit(1)
	}
}

// buildChecks returns the hardcoded list of checks. Order matters: serial
// execution iterates this slice in order, so cheap checks first means
// quicker failure feedback in CI.
func buildChecks() []Check {
	commonArgs := []string{"-trust-folder=accept"}
	return []Check{
		{
			Name:          "spike-one-turn",
			Kind:          "spike",
			Binary:        "spike-one-turn",
			Args:          commonArgs,
			SuccessMarker: successSuccess,
		},
		{
			Name:          "spike-multi-turn",
			Kind:          "spike",
			Binary:        "spike-multi-turn",
			Args:          commonArgs,
			SuccessMarker: successSuccess,
		},
		{
			Name:          "spike-cancel",
			Kind:          "spike",
			Binary:        "spike-cancel",
			Args:          commonArgs,
			SuccessMarker: successSuccess,
		},
		{
			Name:          "spike-permission",
			Kind:          "spike",
			Binary:        "spike-permission",
			Args:          commonArgs,
			SuccessMarker: successSuccess,
		},
		{
			Name:          "spike-multiselect",
			Kind:          "spike",
			Binary:        "spike-multiselect",
			Args:          commonArgs,
			SuccessMarker: observedSuccess,
		},
		{
			Name:          "spike-ask-user",
			Kind:          "spike",
			Binary:        "spike-ask-user",
			Args:          commonArgs,
			SuccessMarker: observedSuccess,
		},
		{
			Name:    "probe-first-prompt-hang",
			Kind:    "probe",
			Binary:  "probe-first-prompt-hang",
			Args:    commonArgs,
			Timeout: probeCheckTimeout,
			OnFailure: func(stdout, stderr string) map[string]any {
				m := probeOutDirRe.FindStringSubmatch(stderr)
				if len(m) < 2 {
					return nil
				}
				return map[string]any{"recording_dir": m[1]}
			},
		},
	}
}

func captureClaudeVersion(ctx context.Context) string {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "claude", "--version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// runCheck runs one Check with its (possibly defaulted) timeout, classifies
// the outcome, and assembles the CheckResult. Stderr is mirrored to the
// host stderr so operators see progress; both stdout and stderr are also
// buffered for marker-regex matching and OnFailure consumption.
func runCheck(parent context.Context, c Check, binDir string) CheckResult {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = defaultCheckTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	binPath := filepath.Join(binDir, c.Binary)
	cmd := exec.CommandContext(ctx, binPath, c.Args...)
	cmd.Env = append(os.Environ(), "TUIDRIVER_STRICT_MCP_CONFIG=1")
	cmd.Stdin = nil

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = io.MultiWriter(&stderrBuf, os.Stderr)

	start := time.Now()
	runErr := cmd.Run()
	dur := time.Since(start)

	status := "pass"
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		status = "timeout"
	case runErr != nil:
		status = "fail"
	case cmd.ProcessState != nil && cmd.ProcessState.ExitCode() != 0:
		status = "fail"
	case c.SuccessMarker != nil && !c.SuccessMarker.Match(stdoutBuf.Bytes()):
		status = "fail"
	}

	result := CheckResult{
		Name:       c.Name,
		Status:     status,
		DurationMs: dur.Milliseconds(),
	}
	if status != "pass" && c.OnFailure != nil {
		if extra := c.OnFailure(stdoutBuf.String(), stderrBuf.String()); len(extra) > 0 {
			result.Extra = extra
		}
	}
	return result
}

// marshalResults flattens each CheckResult into a map[string]any so the
// JSON entry has name/status/duration_ms at the top level alongside any
// check-specific fields (rather than nesting under an "extra" key).
func marshalResults(results []CheckResult) []map[string]any {
	out := make([]map[string]any, 0, len(results))
	for _, r := range results {
		entry := map[string]any{
			"name":        r.Name,
			"status":      r.Status,
			"duration_ms": r.DurationMs,
		}
		for k, v := range r.Extra {
			entry[k] = v
		}
		out = append(out, entry)
	}
	return out
}

func writeReport(path string, r Report) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

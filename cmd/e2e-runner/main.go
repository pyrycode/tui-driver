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
	Kind          string // "spike" | "probe" | "version-lock" — informational only
	Binary        string // path within -bin-dir
	Args          []string
	SuccessMarker *regexp.Regexp // nil = exit-code-only success
	Timeout       time.Duration  // zero falls back to defaultCheckTimeout
	OnFailure     func(stdout, stderr string) map[string]any
	// Run is an in-process check body. When non-nil, runCheck calls Run
	// instead of spawning Binary, and returns a CheckResult directly.
	// OnFailure is NOT invoked for in-process checks — Run returns its
	// extra fields directly. Binary / Args / SuccessMarker are
	// ignored when Run is set.
	Run func(ctx context.Context) (status string, extra map[string]any)
	// NonGating marks an informational check whose non-pass status is recorded
	// in the report but does NOT fail the overall run. Use for a throwaway
	// diagnostic that is worth running for its artifact but should not gate the
	// suite (e.g. probe-first-prompt-hang, #181).
	NonGating bool
}

// gateFailed reports whether a check's result should redden the overall gate.
// A NonGating check never gates: its non-pass status still lands in the report,
// but the run stays green on it (#181).
func gateFailed(c Check, status string) bool {
	return status != "pass" && !c.NonGating
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
	lockPath := flag.String("lock", "./claude-version.lock", "path to claude-version.lock")

	// Captured below via captureClaudeVersion. The version-lock closure
	// reads it lazily so the same variable feeds both Report.ClaudeVersion
	// and the parsed installed_version field in the check's report entry.
	var claudeVersion string
	runVersionLock := func(ctx context.Context) (string, map[string]any) {
		return runClaudeVersionLockCheck(ctx, claudeVersion, *lockPath)
	}
	checks := buildChecks(runVersionLock)
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
	claudeVersion = captureClaudeVersion(parentCtx)
	fmt.Fprintf(os.Stderr, "e2e-runner: claude_version=%q\n", claudeVersion)

	results := make([]CheckResult, 0, len(checks))
	allPassed := true
	skipRest := false
	for _, c := range checks {
		if parentCtx.Err() != nil || skipRest {
			// Either top-level wall budget exhausted, or claude-version-lock
			// failed and short-circuited the run. Report remaining checks as
			// timeout with duration_ms=0 so the array length stays stable.
			results = append(results, CheckResult{
				Name:       c.Name,
				Status:     "timeout",
				DurationMs: 0,
			})
			if gateFailed(c, "timeout") {
				allPassed = false
			}
			continue
		}
		fmt.Fprintf(os.Stderr, "e2e-runner: running %s\n", c.Name)
		r := runCheck(parentCtx, c, *binDir)
		fmt.Fprintf(os.Stderr, "e2e-runner: %s -> %s (%dms)\n", r.Name, r.Status, r.DurationMs)
		if gateFailed(c, r.Status) {
			allPassed = false
		}
		if r.Name == "claude-version-lock" && r.Status != "pass" {
			skipRest = true
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
// quicker failure feedback in CI. The first entry, claude-version-lock,
// is in-process; its Run closure is supplied by main (it needs the
// already-captured claude --version output, which lives in main's scope).
func buildChecks(runVersionLock func(ctx context.Context) (string, map[string]any)) []Check {
	commonArgs := []string{"-trust-folder=accept"}
	return []Check{
		{
			Name: "claude-version-lock",
			Kind: "version-lock",
			Run:  runVersionLock,
		},
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
			// Per-check timeout override. spike-multi-turn drives 5 sequential
			// prompts (incl. a parallel-tool-call probe and a 1+2+...+100
			// reasoning probe) whose wall time is bounded only by claude's
			// streaming speed, not by anything the harness controls. Observed
			// 39-49s on claude 2.1.158, but the distribution is wide (the ticket
			// saw ~40s / ~60s / >420s on 2.1.150), so the 60s default cap sits
			// inside it and yields intermittent timeout verdicts unrelated to the
			// PR under test. 180s clears the observed spread with margin; the
			// spike's own 60s PTY-quiescence watchdog still bounds a true hang.
			// Same pattern as spike-cancel (#69). Ticket #111.
			Timeout: 180 * time.Second,
		},
		{
			Name:          "spike-cancel",
			Kind:          "spike",
			Binary:        "spike-cancel",
			Args:          commonArgs,
			SuccessMarker: successSuccess,
			// Per-check timeout override: the 5-probe shape (3 cancel probes +
			// Probe 4 re-running the 1000-word monad essay as a full recovery
			// turn + Probe 5 follow-up) takes ~60-70s wall time on claude
			// 2.1.148 — Probe 4's essay generation alone is ~30-40s of real
			// model time. The default 60s cut runs short of completion. The
			// spec for #69 estimated 18-25s but undercounted Probe 4's full
			// essay generation. Ticket #69.
			Timeout: 120 * time.Second,
		},
		{
			// Observation rig since #180: ^OBSERVED, not ^SUCCESS. On claude
			// 2.1.199 Probe 2's post-approve turn never reaches the readiness
			// predicate (gotEndTurn ∧ ❯-present ∧ PTY-quiet) that the same
			// approve keystroke reached on 2.1.158, so the spike drains a bounded
			// settle window and records the post-approve state instead of hanging
			// to the 60s cap. Re-gate to successSuccess only once the post-approve
			// turn reaches end_turn again against the pinned claude, and file any
			// warranted library fix as a separate blocked-by follow-up (#206→#207
			// shape) rather than forcing this back to ^SUCCESS.
			Name:          "spike-permission",
			Kind:          "spike",
			Binary:        "spike-permission",
			Args:          commonArgs,
			SuccessMarker: observedSuccess,
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
			Name:          "spike-queued-modals",
			Kind:          "spike",
			Binary:        "spike-queued-modals",
			Args:          commonArgs,
			SuccessMarker: observedSuccess,
			// Two approvals + two fast tool executions + settle windows run
			// ~20-40s; 120s gives ~3-6x margin. The spike's own 120s PTY-quiet
			// watchdog bounds a true hang. Tunable after the first live run
			// (same posture as spike-cancel #69 / spike-multi-turn #111).
			Timeout: 120 * time.Second,
		},
		{
			Name:          "spike-long-prompt",
			Kind:          "spike",
			Binary:        "spike-long-prompt",
			Args:          commonArgs,
			SuccessMarker: successSuccess,
		},
		{
			Name:          "spike-short-prompt",
			Kind:          "spike",
			Binary:        "spike-short-prompt",
			Args:          commonArgs,
			SuccessMarker: successSuccess,
		},
		{
			// De-gated (#181, #177 AC4): a throwaway first-prompt-timing
			// diagnostic, not a library regression check. On claude 2.1.199 it
			// does not reach a clean end_turn — a run 2026-07-07 hit a false idle
			// ~0.25s in, sent its prompt before claude was ready, the session
			// JSONL never appeared, and it exited non-zero at its internal 30s
			// sessionFileWait. It is also environment-sensitive (clean shell vs a
			// nested Claude Code session render differently, #181). So it always
			// reddened the gate for no library signal. NonGating keeps it running
			// for its recording artifact while its status stays informational.
			// Re-gate (drop NonGating, add SuccessMarker) only once it reaches a
			// clean end_turn against the pinned claude; consider removing it
			// entirely if the 30s cost is not worth the artifact.
			Name:      "probe-first-prompt-hang",
			Kind:      "probe",
			Binary:    "probe-first-prompt-hang",
			Args:      commonArgs,
			Timeout:   probeCheckTimeout,
			NonGating: true,
			OnFailure: func(stdout, stderr string) map[string]any {
				m := probeOutDirRe.FindStringSubmatch(stderr)
				if len(m) < 2 {
					return nil
				}
				return map[string]any{"recording_dir": m[1]}
			},
		},
		{
			// De-gated (#251): an observation rig recording how claude encodes a
			// non-ASCII cwd into its projects-dir name (#206; ^OBSERVED gate, ships
			// green when it observes, even when the observation contradicts
			// EncodeCwd). On claude 2.1.199 it now times out BEFORE it can observe —
			// the session's ~/.claude/projects/<encoded-cwd> dir never appears within
			// the probe's internal 30s sessionFileWait glob-poll, so it exits
			// non-zero with no library signal. Diagnosis: first-prompt-readiness, the
			// structural twin of probe-first-prompt-hang (#181). The probe sends its
			// throwaway "hi\r" ONLY to make claude write the deferred session JSONL
			// (which materialises the projects-dir); when that keystroke lands on a
			// false idle (❯ present but claude's input handler not yet wired after
			// startup/trust-accept) it is dropped, no JSONL is written, and the poll
			// times out. An EncodeCwd/path change is excluded: discoverProjectsDir
			// globs by session-id (.../*/<session-id>.jsonl), independent of
			// EncodeCwd, and this same 2.1.199 observed cleanly on 2026-07-06 — the
			// dir now fails to appear at all, not under a changed name. De-gate, not
			// re-anchor: the only idle predicate is IsIdle (the bare ❯ glyph that
			// produces the false idle), no positive ready-signal exists to wait on
			// (#173 Open Q1), and any probe-level re-anchor is a live-claude-gated
			// hypothesis the claude-free make check gate cannot confirm. NonGating
			// keeps it running for its recording artifact while its status stays
			// informational. Timeout stays unset (60s default) so the internal 30s
			// poll fits inside it — do NOT set probeCheckTimeout here. Re-gate (drop
			// NonGating) only once it reaches OBSERVED reliably against the pinned
			// claude, which needs the deferred re-anchor spike (#251 Open Q1).
			Name:          "probe-cwd-encoding",
			Kind:          "probe",
			Binary:        "probe-cwd-encoding",
			Args:          commonArgs,
			SuccessMarker: observedSuccess,
			NonGating:     true,
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

// parseClaudeVersion extracts the leading whitespace-separated token from
// `claude --version` output (e.g. "2.1.144 (Claude Code)" -> "2.1.144").
// Returns "" for empty / whitespace-only input.
func parseClaudeVersion(raw string) string {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// lockFile is the parsed contents of claude-version.lock.
type lockFile struct {
	Version string
	Flags   []string
	Values  []string
}

// parseLockFile reads and parses the claude-version.lock file at path. See
// the format rules in docs/specs/architecture/36-claude-version-lock.md
// (key=value, # comments, exactly one version= line, zero or more flag=
// and value= lines, empty values rejected).
//
// `flag=` and `value=` are independent substring assertions against
// `claude --help`. Use `flag=` for flag names and `value=` for enumerated
// values that appear in choice-lists — this fits claude --help's actual
// format, where flag-and-value pairs are NOT rendered as a literal
// "--flag value" example (the choices appear inline parenthesised).
func parseLockFile(path string) (lockFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return lockFile{}, err
	}
	var lf lockFile
	versionSeen := false
	lines := strings.Split(string(b), "\n")
	for i, raw := range lines {
		lineNo := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return lockFile{}, fmt.Errorf("at line %d: expected key=value, got %q", lineNo, line)
		}
		key := strings.TrimSpace(line[:eq])
		value := strings.TrimSpace(line[eq+1:])
		switch key {
		case "version":
			if value == "" {
				return lockFile{}, fmt.Errorf("at line %d: empty version value", lineNo)
			}
			if versionSeen {
				return lockFile{}, fmt.Errorf("at line %d: duplicate version key", lineNo)
			}
			lf.Version = value
			versionSeen = true
		case "flag":
			if value == "" {
				return lockFile{}, fmt.Errorf("at line %d: empty flag value", lineNo)
			}
			lf.Flags = append(lf.Flags, value)
		case "value":
			if value == "" {
				return lockFile{}, fmt.Errorf("at line %d: empty value value", lineNo)
			}
			lf.Values = append(lf.Values, value)
		default:
			return lockFile{}, fmt.Errorf("at line %d: unknown key %q", lineNo, key)
		}
	}
	if !versionSeen {
		return lockFile{}, fmt.Errorf("missing required version= line")
	}
	return lf, nil
}

// runClaudeVersionLockCheck is the Run callback for the claude-version-lock
// check. It asserts that every flag AND every value the library depends on
// still appears verbatim in `claude --help`. The `version=` field of the
// lock file is informational — patch (or any) drift between the installed
// claude and the lock's version is intentionally tolerated; bumping the
// lock is coupled to a deliberate fixture re-record sweep (#47, #57). The
// extra map always carries installed_version, expected_version,
// missing_flags, and missing_values so the report schema stays uniform
// across pass/fail.
func runClaudeVersionLockCheck(ctx context.Context, capturedVersion, lockPath string) (string, map[string]any) {
	// failExtra builds the report's extra map for early-return failure paths
	// that don't reach evaluateClaudeVersionLock. On the success path the
	// helper produces its own extra; building one here would be overwritten.
	failExtra := func(installed, expected string) map[string]any {
		return map[string]any{
			"installed_version": installed,
			"expected_version":  expected,
			"missing_flags":     []string{},
			"missing_values":    []string{},
		}
	}

	lf, err := parseLockFile(lockPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "e2e-runner: %s not found; required for claude-version-lock check\n", lockPath)
		} else {
			fmt.Fprintf(os.Stderr, "e2e-runner: %s parse error %v\n", lockPath, err)
		}
		return "fail", failExtra(parseClaudeVersion(capturedVersion), "")
	}

	if capturedVersion == "unknown" {
		fmt.Fprintln(os.Stderr, "e2e-runner: claude --version failed at startup; cannot enforce claude-version.lock")
		return "fail", failExtra("", lf.Version)
	}
	installed := parseClaudeVersion(capturedVersion)

	helpOut, err := exec.CommandContext(ctx, "claude", "--help").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e-runner: claude --help failed: %v\n", err)
		return "fail", failExtra(installed, lf.Version)
	}

	status, extra := evaluateClaudeVersionLock(lf, installed, string(helpOut))
	for _, f := range extra["missing_flags"].([]string) {
		fmt.Fprintf(os.Stderr, "e2e-runner: claude --help no longer mentions flag %s; review %s\n", f, lockPath)
	}
	for _, v := range extra["missing_values"].([]string) {
		fmt.Fprintf(os.Stderr, "e2e-runner: claude --help no longer mentions value %s; review %s\n", v, lockPath)
	}
	return status, extra
}

// evaluateClaudeVersionLock applies the lock policy to a parsed lockFile,
// the parsed installed-version token, and a captured `claude --help`
// output. Returns the check's status ("pass" or "fail") and the extra
// map populated with installed_version, expected_version, missing_flags,
// missing_values (always present; empty []string slices on pass).
//
// Side-effecting concerns (reading the lock file, exec'ing claude --help,
// emitting stderr lines for the operator) live in the runClaudeVersionLockCheck
// wrapper. The version field of the lock is *not* compared against the
// installed version: per spec 64, patch drift in either direction is
// informational only — the flag/value substring contract is the
// substantive assertion.
func evaluateClaudeVersionLock(lf lockFile, installedVersion, helpOut string) (string, map[string]any) {
	missingFlags := []string{}
	for _, f := range lf.Flags {
		if !strings.Contains(helpOut, f) {
			missingFlags = append(missingFlags, f)
		}
	}
	missingValues := []string{}
	for _, v := range lf.Values {
		if !strings.Contains(helpOut, v) {
			missingValues = append(missingValues, v)
		}
	}
	extra := map[string]any{
		"installed_version": installedVersion,
		"expected_version":  lf.Version,
		"missing_flags":     missingFlags,
		"missing_values":    missingValues,
	}
	status := "pass"
	if len(missingFlags) > 0 || len(missingValues) > 0 {
		status = "fail"
	}
	return status, extra
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

	if c.Run != nil {
		start := time.Now()
		status, extra := c.Run(ctx)
		return CheckResult{
			Name:       c.Name,
			Status:     status,
			DurationMs: time.Since(start).Milliseconds(),
			Extra:      extra,
		}
	}

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

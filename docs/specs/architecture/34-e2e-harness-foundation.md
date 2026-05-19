# Spec 34 — e2e harness foundation: runner + Makefile + spike & probe checks

Ticket: https://github.com/pyrycode/tui-driver/issues/34
Status: ready for developer

## Files to read first

- `cmd/spike-one-turn/main.go:60-110` — canonical flag surface (`-session-id`, `-trust-folder`) and exit-code shape (`os.Exit(1)` on failure, prints `SUCCESS: <text>` on green). All four "original" spikes mirror this.
- `cmd/spike-one-turn/main.go:255-260` — the `fmt.Printf("SUCCESS: %s\n", assistantText)` line. Anchors the default success-marker regex.
- `cmd/spike-multiselect/main.go:60-80` — flag surface (`-trigger`, `-post-trigger-keys`, `-trust-folder`) and `OBSERVED:` Printf at L384 — does **NOT** print `^SUCCESS`. The runner's success-marker is per-check, not global.
- `cmd/spike-ask-user/main.go:80-100` — flag surface (`-prompt`, `-answer`, `-trust-folder`) and `OBSERVED:` Printf at L301 — same not-SUCCESS shape as multiselect.
- `cmd/spike-cancel/main.go:148-165` — adds `-cancel-keystroke` alongside the base flags; otherwise identical contract.
- `cmd/spike-permission/main.go:213-240` — adds `-approve-keystroke`, `-modal-predicate`; otherwise identical contract.
- `cmd/probe-first-prompt-hang/main.go:73-80` — `outDir` construction: `filepath.Join(os.TempDir(), "probe-first-prompt-hang-"+tsTag)`. Recording dir is created on every run; the runner must scrape its path from probe stderr (`probe outDir=...` log line at L120) and preserve+report on failure.
- `cmd/probe-first-prompt-hang/main.go:45-50` — `wallTimeout = 3 * time.Minute`, `hangThreshold = 10 * time.Second`. The runner's 30s wall budget for the probe check is stricter than `wallTimeout` (which is for the probe's own internal forensics) and looser than `hangThreshold` (which is the probe's internal "fast" verdict).
- `pkg/tuidriver/pty.go:24-40` — `EnsureClaudeEnv(cmd)` — the single helper every spike+probe calls to seed `cmd.Env`. **This is the seam** the harness extends to inject `--strict-mcp-config` without touching any spike (see Design § "Headless MCP plumbing").
- `pkg/tuidriver/tuidriver.go:59-63` — package doc explicitly documents `--strict-mcp-config` semantics ("skips configured MCP servers entirely … for reproducibility scenarios — CI, tests, containers"). Justifies the env-var path.
- `docs/knowledge/architecture/system-overview.md:7-14` — current layout: 6 spike binaries + 1 probe, all single-file `cmd/<name>/main.go`, all currently throwaway. Harness operates orthogonally — does not extend the library.

## Context

PRs #19–#30 have built up the comprehensive pattern matcher / state detector / watchdog set in `pkg/tuidriver/`. The 6 spike binaries plus `probe-first-prompt-hang` empirically exercise every primitive end-to-end against real `claude`. What's missing is a single command that runs them all serially and emits a machine-readable report — so the next consumer (pyry acp integration) inherits a clean baseline and bisecting "claude changed / library broken / consumer wrong" stays cheap.

This ticket ships the **harness foundation**: a `make e2e` target + `cmd/e2e-runner/` orchestrator + `e2e-report.json` schema + two check kinds (spike orchestration, probe runner). Follow-ups (out of scope here) add snapshot-drift, claude-version-lock, operator docs, and CI wiring.

Two non-obvious facts from reading the code that diverge from the ticket body:

1. **Not all spikes print `^SUCCESS`.** The ticket body says all 6 spikes print `^SUCCESS` on green. The four "result" spikes do (`spike-one-turn`, `spike-multi-turn`, `spike-cancel`, `spike-permission`); the two "observation" spikes (`spike-multiselect`, `spike-ask-user`) print `^OBSERVED` instead. Their contract is "exit 0 + observation logged." The success marker must therefore be **per-check**, not a global `^SUCCESS` regex.
2. **Spikes hardcode the `claude` invocation.** Every spike's `exec.Command("claude", "--session-id", sessionID)` is fixed at the call site; there's no per-spike `--strict-mcp-config` plumbing today. Rather than touch all 7 binaries (which would bust the file-count budget and reshape spike internals), the cleanest seam is **`EnsureClaudeEnv`** — a single helper every spike+probe already calls before `Start`. Extending it to also append `--strict-mcp-config` to `cmd.Args` when an env var is set means zero spike modifications.

## Design

### Package layout

```
Makefile                         # NEW: `make e2e` target + supporting targets
cmd/e2e-runner/main.go           # NEW: orchestrator binary
pkg/tuidriver/pty.go             # MODIFIED: EnsureClaudeEnv gains opt-in strict-mcp-config
pkg/tuidriver/pty_test.go        # MODIFIED: covers the new behaviour
.gitignore                       # MODIFIED: add /e2e-report.json
```

No new packages. The runner is a single-file `main` program — same shape as the existing spikes — that depends on `pkg/tuidriver` only indirectly (it never imports it; it just shells out).

### Headless MCP plumbing

Extend `EnsureClaudeEnv` in `pkg/tuidriver/pty.go` with a new exported constant + opt-in behaviour:

```go
// StrictMcpConfigEnv is the env var the e2e harness sets to ask
// EnsureClaudeEnv to additionally append --strict-mcp-config to cmd.Args.
// Set to "1" to enable. Empty / unset / any other value: no-op.
const StrictMcpConfigEnv = "TUIDRIVER_STRICT_MCP_CONFIG"
```

`EnsureClaudeEnv(cmd)` adds: after the existing TERM-handling loop returns, check `os.Getenv(StrictMcpConfigEnv) == "1"`; if true, append `"--strict-mcp-config"` to `cmd.Args` unless it's already present. Args mutation is safe — `EnsureClaudeEnv` is always called before `pty.Start` / `StartPTY`.

**Rationale:** every spike+probe currently calls `tuidriver.EnsureClaudeEnv(cmd)` exactly once between `exec.Command("claude", ...)` and `pty.Start`. The harness sets the env var on the spawned-spike process; the spike's `EnsureClaudeEnv` call then transparently appends the flag to `cmd.Args` before invoking claude. No spike-side changes required.

Doc-comment update: expand `EnsureClaudeEnv` godoc to describe the strict-mcp opt-in. The function's role broadens slightly from "seed cmd.Env" to "prepare cmd for driving claude" — name kept for API stability.

Idempotency: skip the append if `--strict-mcp-config` is already in `cmd.Args` (defensive; spikes don't pass it today but might in future).

### `Check` abstraction

The runner exposes one type with a clean extension point so follow-up tickets (snapshot-drift, version-lock) add one struct each:

```go
type Check struct {
    Name           string         // stable identifier; e.g. "spike-one-turn"
    Kind           string         // "spike" | "probe"; selects runner behaviour
    Binary         string         // path within bin-dir; e.g. "spike-one-turn"
    Args           []string       // CLI args to pass; e.g. ["-trust-folder=accept"]
    SuccessMarker  *regexp.Regexp // stdout regex; nil = exit-code-only success
    Timeout        time.Duration  // per-check wall budget; default applies if zero
    OnFailure      func(stdout, stderr string) map[string]any
                                  // optional; emits check-specific fields into the report
                                  // (e.g. probe scrapes recording_dir on failure)
}

type CheckResult struct {
    Name        string                 `json:"name"`
    Status      string                 `json:"status"`        // "pass" | "fail" | "timeout"
    DurationMs  int64                  `json:"duration_ms"`
    Extra       map[string]any         `json:"-"`             // flattened into the JSON entry
}
```

Marshalling: `CheckResult` is serialised to a `map[string]any` so `Extra` fields are flattened to siblings of `name/status/duration_ms` (rather than nested under "extra"). One small marshaller function does this — keeps the JSON schema flat per the ticket's example.

### Built-in check list

Hardcoded `[]Check{...}` slice in `main.go`. Eight entries total (6 spikes + 1 probe):

| Name | Kind | Binary | Args | SuccessMarker |
|---|---|---|---|---|
| `spike-one-turn` | spike | `spike-one-turn` | `-trust-folder=accept` | `^SUCCESS` |
| `spike-multi-turn` | spike | `spike-multi-turn` | `-trust-folder=accept` | `^SUCCESS` |
| `spike-cancel` | spike | `spike-cancel` | `-trust-folder=accept` | `^SUCCESS` |
| `spike-permission` | spike | `spike-permission` | `-trust-folder=accept` | `^SUCCESS` |
| `spike-multiselect` | spike | `spike-multiselect` | `-trust-folder=accept` | `^OBSERVED` |
| `spike-ask-user` | spike | `spike-ask-user` | `-trust-folder=accept` | `^OBSERVED` |
| `probe-first-prompt-hang` | probe | `probe-first-prompt-hang` | `-trust-folder=accept` | (nil — exit-code only) |

Probe's `OnFailure` scrapes `recording_dir` from probe stderr by regex `probe outDir=(\S+)` (matches the `logger.Printf("probe outDir=%s", outDir)` line at probe/main.go:120).

### `runCheck` semantics

One function — `runCheck(ctx context.Context, c Check, binDir string) CheckResult` — does the same thing for every check kind. The `Kind` field is informational; behaviour differs only in defaults (timeout) and post-processing (`OnFailure`).

Steps:
1. Compute timeout: `c.Timeout` if non-zero, else 60s default. Probe overrides via the runner-level config to 30s (ticket § AC bullet 2 sub-bullet 2: "Passes iff the probe exits 0 within 30s wall time").
2. Build a `context.WithTimeout` from the parent context.
3. `exec.CommandContext(ctx, filepath.Join(binDir, c.Binary), c.Args...)`. Set `cmd.Env = append(os.Environ(), "TUIDRIVER_STRICT_MCP_CONFIG=1")` so the child spike picks up strict-mcp via `EnsureClaudeEnv`.
4. Capture stdout + stderr to `bytes.Buffer`s. Stdin = `nil` (no TTY). Stderr can still be mirrored to the host stderr in addition to the buffer, so an operator running `make e2e` interactively sees progress.
5. `cmd.Run()`. Measure wall time around it.
6. Classify:
   - `ctx.Err() == context.DeadlineExceeded` → `status="timeout"`
   - else `cmd.ProcessState.ExitCode() != 0` → `status="fail"`
   - else if `c.SuccessMarker != nil && !c.SuccessMarker.Match(stdout)` → `status="fail"` (marker missing)
   - else → `status="pass"`
7. If status != "pass" and `c.OnFailure != nil`, call it with stdout+stderr strings; merge returned map into `result.Extra`.

The runner does NOT kill the probe's recording dir on failure — that's the whole point of preserving it. On `pass`, the probe directory is left in place too (it's small, lives under `/tmp`, and isn't policed by the runner). The probe's existing wallTimeout of 3 min stays untouched — the runner's 30s timeout kills the probe earlier, but only on hang.

### Top-level wall budget

10-minute parent context wrapping all checks. If it fires, the in-flight check sees ctx cancellation and reports `status="timeout"`; remaining checks are reported with `status="timeout"` and `duration_ms=0` without being run, so the report's `checks[]` always has exactly 7 entries.

### Report assembly

Top-level struct:

```go
type Report struct {
    ClaudeVersion   string           `json:"claude_version"`
    TotalDurationMs int64            `json:"total_duration_ms"`
    Checks          []map[string]any `json:"checks"`
}
```

- `ClaudeVersion`: capture by running `claude --version` once before any check; store the trimmed stdout. On non-zero exit, set to `"unknown"` and continue (don't abort the whole run — the operator may still want spike pass/fail info).
- `TotalDurationMs`: measured around the whole sequence.
- `Checks`: built via the per-result map marshaller.

Output: write to `./e2e-report.json` (relative to cwd, which is the repo root when invoked via `make e2e`). `os.WriteFile` with `0644`. Atomic-write isn't required — the file is a CI artifact, not a concurrent resource.

### Path scrubbing

The report MUST NOT embed machine-specific paths. Concretely, the runner:
- Does NOT include the binary's own argv0, cwd, `$HOME`, or `bin-dir` value in the report body.
- Does NOT include captured stdout/stderr in the report (those are operator-facing diagnostics, written to the runner's own stderr; the report is for CI).
- Allows the probe's `recording_dir` to be absolute on failure entries only (ticket exception — "directory must be locatable for forensics"). On `pass`, the probe entry has no `recording_dir`.

If a `Check` adds machine-path fields in future, the contract is: only emit them on `fail` / `timeout` and only when the path is essential for forensics.

### Concurrency model

Sequential. The runner is a single goroutine driving each check in turn. No fan-out, no shared state beyond the parent context. Spawned child processes are isolated via their own contexts.

The only goroutine boundary is between `cmd.Run()` (blocks until subprocess exits or ctx fires) and the parent — handled by `exec.CommandContext`'s built-in SIGKILL-on-cancel. No manual signal plumbing required.

### Error handling

| Failure | Behaviour |
|---|---|
| `claude` binary missing | `claude --version` fails → `ClaudeVersion="unknown"`; checks proceed and almost certainly all fail. Report still emits; runner exits 1. |
| Spike binary missing in `bin-dir` | The check's `cmd.Run()` returns an `exec.ErrNotFound`-shaped error; treated as `status="fail"`, runner continues. |
| Per-check timeout | `status="timeout"`. Runner continues with next check. |
| Top-level 10-min wall budget hit | Remaining checks reported as `status="timeout"` without invocation. Report still emits. |
| Report write fails | Print to stderr, exit 1 — but only after the run completes. |
| Probe stderr regex fails to match `recording_dir` | `OnFailure` map omits the field; report entry is still well-formed. |

Exit code contract: `0` iff every check passed (status `pass`); `1` otherwise (any fail / any timeout / report-write error).

### CLI flags on `cmd/e2e-runner`

```
-bin-dir PATH                Path to directory containing built spike+probe binaries.
                             Default: ./bin (Makefile builds here).
-report PATH                 Output path for e2e-report.json.
                             Default: ./e2e-report.json.
-timeout NAME=DUR            Override per-check timeout. Repeatable.
                             Example: -timeout spike-cancel=90s -timeout probe-first-prompt-hang=45s.
                             Unknown NAME → fatal flag-parse error so typos surface immediately.
-wall DUR                    Override top-level wall budget. Default: 10m.
```

The repeatable `-timeout` flag uses Go's `flag.Func` for cleanest implementation. The probe's 30s default is set in the Check slice initializer; the `-timeout` flag overrides it.

### Makefile

Repo-root `Makefile`. Targets:

- `.PHONY: e2e build-bin clean-bin clean-report`
- `build-bin` — builds all 6 spikes + probe + runner to `./bin/<name>`. Single `go build` invocation per binary; failures abort the target.
- `e2e` — depends on `build-bin`; runs `./bin/e2e-runner -bin-dir ./bin -report ./e2e-report.json`; propagates the runner's exit code.
- `clean-bin` — `rm -rf ./bin`.
- `clean-report` — `rm -f ./e2e-report.json`.

Keep it shell-portable (no GNU-specific features). Plain `go build -o $@ ./cmd/$(notdir $@)` style.

### `.gitignore`

Append `/e2e-report.json` next to the existing `/bin/` line. Generated artifact; no value in committing.

## Concurrency model

Single goroutine. The runner drives checks serially. `exec.CommandContext` handles subprocess lifecycle. No channels, no waitgroups, no shared state.

The only non-obvious detail: the spike+probe binaries themselves run goroutines internally (PTY reader, JSONL tailer, watchdog) but that's their problem, not the runner's. The runner only watches the subprocess as a whole via `cmd.Run()`.

## Error handling

See Design § "Error handling" above.

The dominant invariant: **the report always emits**, even on partial failure, so CI gets a single uniform artifact. The only conditions where the report doesn't emit are (a) the report-write itself fails or (b) the runner crashes on a programmer error. Both surface via non-zero exit and a stderr message.

## Testing strategy

Unit-testable surface is small — most of the runner is integration with real `claude`.

1. **`pkg/tuidriver/pty_test.go`** gains coverage for the `EnsureClaudeEnv` strict-mcp behaviour:
   - With env var unset: `cmd.Args` unchanged.
   - With env var `="1"`: `--strict-mcp-config` appended.
   - With env var `="1"` and flag already present: no double-append.
   - With env var set to anything other than `"1"`: no append (defensive).

   Use `t.Setenv` for hermetic env scoping.

2. **`cmd/e2e-runner`** — no unit tests at this stage. The runner IS the integration test. Validation contract:
   - Manual smoke: `make e2e` against an installed `claude` succeeds and produces `e2e-report.json` matching the schema.
   - Forced-failure smoke: temporarily rename one spike binary in `bin-dir` and re-run; verify the report has that check at `status="fail"` and others at `status="pass"`.

   Document these in the PR description, not in source.

3. **No mocked `claude`.** The whole point of the harness is end-to-end against real claude; mocking defeats the purpose.

## Open questions

1. **Should the runner shell out to `go run` instead of pre-built `bin/` paths?** Decided: no. `go run` adds ~1–2s compile latency per invocation, eats into per-check timeouts, and complicates CI artifact reasoning. Makefile builds explicitly to `./bin/` — same shape as if a CI pipeline does it.
2. **Should `e2e-report.json` track per-check stdout snippets for fail diagnostics?** Decided: no. The report is for CI's "did everything pass?" signal. Diagnostics go to the runner's own stderr (already mirrored from each spike's stderr) and to the probe's recording dir on probe failure. Adding stdout snippets bloats the file and risks embedding machine paths from spike log lines.
3. **Should the strict-mcp env var be named with a `CLAUDE_` prefix or `TUIDRIVER_` prefix?** Picked `TUIDRIVER_STRICT_MCP_CONFIG` — it's our knob, not claude's. Claude itself reads `--strict-mcp-config` as a CLI flag; our env var is the harness-to-spike signal. The name matches the project (tuidriver) so a future operator grepping env doesn't confuse it with a real claude env knob.
4. **Probe failure: what if `recording_dir` is never logged (probe crashed before `MkdirAll`)?** The regex match against probe stderr fails; `OnFailure` returns an empty map; the report entry has no `recording_dir`. This is correct — there's no dir to point at.

## Out of scope (per ticket)

- Snapshot-drift check.
- Claude-version-lock check + `claude-version.lock` file.
- Operator documentation at `docs/knowledge/e2e-testing.md`.
- GitHub Actions wiring.

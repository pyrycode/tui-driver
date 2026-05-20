# e2e harness

The single command (`make e2e`) that verifies the library's empirical end-to-end behaviour against a real installed `claude` binary. Runs the `claude-version-lock` check first, then every spike + probe + the snapshot-drift check serially, classifies each as pass / fail / timeout, and emits a single-file JSON report (`e2e-report.json`) suitable for CI artifact collection. Introduced by [#34](../codebase/34.md); extended by [#35](../codebase/35.md) (snapshot-drift), [#36](../codebase/36.md) (claude-version-lock), and [#40](../codebase/40.md) (GitHub Actions push-to-main workflow). Operator runbook lives here; per-ticket build notes live in `codebase/<N>.md`.

## What it does

- Runs the in-process `claude-version-lock` check first (asserts `claude --version` matches `claude-version.lock` and that every flag pinned in the lock file still appears in `claude --help`), then the 7 spike binaries (`spike-one-turn`, `spike-multi-turn`, `spike-cancel`, `spike-permission`, `spike-multiselect`, `spike-ask-user`, `spike-long-prompt`), then `probe-first-prompt-hang`, then the `snapshot-drift` check — all serially against a real `claude` install. `spike-long-prompt` is the `Session.WritePrompt` regression rig — bracketed-paste path, 3.45 KB embedded fixture, three token markers, substring assertion (see [#47](../codebase/47.md)).
- Captures pass/fail/timeout + wall duration per check.
- Emits `e2e-report.json` at the repo root (always — even on partial failure, so CI gets a uniform artifact).
- Exits `0` iff every check passed; `1` otherwise.
- **Short-circuits the slow checks on a `claude-version-lock` failure.** If claude has drifted out from under the library, every subsequent check is appended to the report with `status="timeout"` and `duration_ms=0` rather than burning ~5 minutes of wall time on spikes that are doomed anyway. See [§ How it handles failure](#how-it-handles-failure).

CI integration: a GitHub Actions workflow at `.github/workflows/e2e.yml` runs `make e2e` on every push to `main` and on manual dispatch. See [§ CI integration](#ci-integration) below for the trigger surface, cost-cap rationale, and artifact shape. Introduced by [#40](../codebase/40.md).

## How to run

```sh
make e2e             # build everything to ./bin and run the runner
make build-bin       # build only (no run)
make clean-bin       # rm -rf ./bin
make clean-report    # rm -f ./e2e-report.json
```

The runner can also be invoked directly after `make build-bin`:

```sh
./bin/e2e-runner -bin-dir ./bin -report ./e2e-report.json
```

## CLI flags

| Flag | Default | Purpose |
|---|---|---|
| `-bin-dir PATH` | `./bin` | Directory containing the built spike+probe binaries. The Makefile builds here. |
| `-report PATH` | `./e2e-report.json` | Output path for the report. |
| `-lock PATH` | `./claude-version.lock` | Path to the claude-version lock file consumed by the `claude-version-lock` check. |
| `-wall DUR` | `10m` | Top-level wall budget for the entire run. |
| `-timeout NAME=DUR` | per-check default (60s; 30s for the probe; 180s for snapshot-drift) | Per-check timeout override. Repeatable. Unknown `NAME` is a hard parse error so typos surface immediately. |

Example: `./bin/e2e-runner -timeout spike-cancel=90s -timeout probe-first-prompt-hang=45s -timeout claude-version-lock=10s`.

## Headless / CI plumbing

The runner sets `TUIDRIVER_STRICT_MCP_CONFIG=1` on every child spike+probe process. The spike+probe binaries all call `tuidriver.EnsureClaudeEnv` between constructing the `*exec.Cmd` and starting the PTY; when that env var is `"1"`, `EnsureClaudeEnv` transparently appends `--strict-mcp-config` to `cmd.Args` (idempotent — skipped if already present). The flag tells `claude` to skip configured MCP servers entirely, which is what CI / containers / reproducibility scenarios want. Zero spike-side changes required; the env-var contract is the seam.

No TTY is required on stdin. No MCP servers need to be configured on the host.

The harness uses `--strict-mcp-config` for determinism: without it, `claude` waits on configured MCP servers to register before processing the first prompt. On the GitHub runner that has no MCP servers configured this is mostly a no-op, but on operator machines (where `make e2e` also runs) host-level MCP config produces flaky first-prompt timing. Setting `TUIDRIVER_STRICT_MCP_CONFIG=1` from the runner side makes the harness behave identically regardless of host MCP config. This is consistent with the May 18 walk-back, not a contradiction: that walk-back recommended against making `--strict-mcp-config` the library's user-facing default sidestep for pyry acp — a consumer where MCP is a user-facing feature and stripping it would harm users. The e2e/CI context has no user-facing MCP feature, so the harness using the flag for determinism is exactly the kind of scoped, internal use the walk-back left intact. See `.github/workflows/e2e.yml` for the workflow that drives the runner.

## CI integration

`.github/workflows/e2e.yml` invokes `make e2e` on every push to `main` and on `workflow_dispatch` (manual UI trigger). Explicitly NOT `pull_request` — every run burns metered `ANTHROPIC_API_KEY` credits (CI runners cannot use a Max subscription), so per-PR runs would multiply spend. The push-to-main gate is the cheapest coverage that still catches regressions before downstream consumers hit them. PR-time coverage is a separate concern: the code-review agent runs the harness selectively elsewhere.

**Cost controls.** Two complementary mechanisms:

- Trigger surface narrow to `push` (branch `main` only) + `workflow_dispatch`. No `pull_request`, no `paths:` filters (drift must surface on every push to `main`), no `[skip ci]` opt-out paths.
- Job-level `timeout-minutes: 20` is the hard ceiling. Operator's p99 wall-time target is `< 15min`; the 20-minute cap gives ~5min headroom so a slow-but-healthy run isn't spuriously killed, while firmly bounding cost if the harness hangs. If runs reliably exceed 15min, file a profiling follow-up rather than bumping the cap.

**Cache shape.** `~/.npm-global` is cached with `key: claude-${{ runner.os }}-${{ hashFiles('claude-version.lock') }}`. Any edit to the lock file (version, flags, comments) flips the key; cache miss triggers a fresh `npm install -g "@anthropic-ai/claude-code@${version}"` where `${version}` is parsed from `claude-version.lock`. Lock-file drift is the bug we're catching — missing the cache on every drift event is correct.

**Authentication.** `ANTHROPIC_API_KEY` flows through `${{ secrets.ANTHROPIC_API_KEY }}` at the job-`env` level only (see `.github/workflows/e2e.yml`). The secret must exist in repo secrets before the workflow can succeed. The credential is owned by <the maintainer's Anthropic Console account — Juhana to fill in before merge or in a follow-up doc PR>; rotation requires access to that account.

*Routine rotation* (no compromise suspected):

1. Log in to the owning Anthropic Console account and generate a new API key, labelling it so the GitHub origin is obvious (e.g. `tui-driver-ci-2026-MM-DD`).
2. Update the `ANTHROPIC_API_KEY` secret at `https://github.com/pyrycode/tui-driver/settings/secrets/actions` with the new value.
3. Trigger an `e2e` run — wait for the next push to `main` or dispatch manually via the Actions UI — and confirm it goes green.
4. Revoke the old key in Anthropic Console *only after* that run succeeds. Keeping the old key valid until the new one is validated avoids stranding an in-flight run on a half-rotated secret.

*If a key is leaked while a workflow run is in flight* (stop the bleeding first):

1. Revoke the compromised key in Anthropic Console immediately — do this before anything else, even if a workflow run is mid-execution.
2. Cancel any in-flight `e2e` workflow run via the Actions UI. (`concurrency.cancel-in-progress: false` serialises subsequent pushes but does NOT prevent manual cancellation.)
3. Generate a new API key (label as in routine rotation).
4. Update the `ANTHROPIC_API_KEY` secret at `https://github.com/pyrycode/tui-driver/settings/secrets/actions`.
5. Re-dispatch via `workflow_dispatch` to confirm the new key works without waiting for the next push to `main`.

**`--strict-mcp-config` is NOT set at the CI layer.** The runner already injects `TUIDRIVER_STRICT_MCP_CONFIG=1` onto every spike+probe child, which `EnsureClaudeEnv` translates into `--strict-mcp-config` on argv. CI passes nothing extra; the env-var seam from [§ Headless / CI plumbing](#headless--ci-plumbing) does the work. Future edits MUST NOT also set the env var at the workflow level — the runner-side injection is sufficient and the redundancy would mislead readers.

**Artifacts.** Two uploads, both gated on `if: always()` so they fire even on harness failure or timeout-kill — which is exactly when they're needed:

- `e2e-report` → `e2e-report.json` (`if-no-files-found: warn` — absence is itself an anomaly)
- `probe-recordings` → `/tmp/probe-first-prompt-hang-*` (`if-no-files-found: ignore` — clean runs may have no captures)

Both use 30-day retention (trimmed from the 90-day default; drift events get inspected within hours).

**Exit code & branch protection.** The workflow exits non-zero on any harness failure (the runner's exit-code contract from #34). This is the wiring point for branch protection: add `e2e` as a required check via the GitHub UI to gate merges on the workflow's verdict. The branch-protection toggle itself is out of scope for the workflow file.

**Concurrency.** `concurrency.group: e2e-${{ github.ref }}` with `cancel-in-progress: false` — back-to-back pushes serialise rather than racing or cancelling each other. The push-to-main backstop is meant to land a verdict per SHA, not race itself.

## Check kinds

Four kinds today (`Kind` is informational — every check goes through the same `runCheck` body):

- **`version-lock`** — in-process check that runs first. Parses `claude-version.lock`, parses the leading token out of the captured `claude --version` output, asserts strict string equality against the lock's `version=` value, then runs `claude --help` once and `strings.Contains`-checks every `flag=` entry against the help output. Default 60s timeout (override via `-timeout claude-version-lock=DUR`). Failure short-circuits every subsequent check. Emits `installed_version`, `expected_version`, and `missing_flags` (always present — empty slice on pass) on the report entry. The only in-process check kind: the `Run` field on `Check` is set, `Binary`/`Args`/`SuccessMarker` are ignored. Introduced by [#36](../codebase/36.md).
- **`spike`** — runs a single spike binary with `-trust-folder=accept`. Passes iff the subprocess exits 0 AND its stdout matches the check's `SuccessMarker` regex.
  - Result spikes (`one-turn`, `multi-turn`, `cancel`, `permission`, `long-prompt`) use `^SUCCESS` as their marker (they print `SUCCESS: <text>` on green).
  - Observation spikes (`multiselect`, `ask-user`) use `^OBSERVED` as their marker (they print `OBSERVED: <text>` — their contract is "exit 0 + observation logged").
- **`probe`** — same as spike but with a 30s default timeout (instead of 60s) and an `OnFailure` callback that scrapes the recording-dir path from probe stderr (`probe outDir=<path>`) and emits it as `recording_dir` in the report entry. The probe's own internal `wallTimeout` is 3 min; the runner's 30s timeout kills it earlier (only on hang), and 30s is looser than the probe's 10s "fast" verdict so a healthy 2–3s baseline doesn't trip it.
- **`snapshot`** — drives `cmd/e2e-snapshot-check` (a thin orchestrator that invokes `spike-multiselect` three times via its `-trigger` / `-settle` flags, scrapes the dump path from spike-multiselect's stderr, and byte-compares each capture to the committed `pkg/tuidriver/testdata/{picker,mcp,agents}-snapshot.bin`). 180s default timeout (`-timeout snapshot-drift=DUR` to override). No `SuccessMarker` — exit code is authoritative. An `OnComplete` callback (see below) parses one `SNAPSHOT <name> match|diff` stdout line per fixture into a `snapshots[]` list that appears on both `pass` AND `fail` so the operator can always pinpoint which fixture(s) drifted (or confirm none did). **Read-only** — never writes to the committed fixtures; re-recording is a separate maintainer step (see [#35](../codebase/35.md)). Introduced by [#35](../codebase/35.md). Byte-equality is intentional even though it's fragile — false positives are a feature that force the maintainer to look and re-record deliberately.

Future check kinds will add one entry each to the hardcoded check list; the abstraction is intentionally a leaf, not a framework. The first subprocess-vs-in-process inflection point landed with `version-lock` — when the check's inputs already live in the runner's scope (the captured `claude --version` output, the lock-file path from a `-lock` flag), an in-process `Run` callback is the right shape; when the work is non-trivial orchestration that would clutter `main` (snapshot-drift's 3-spike + byte-compare flow), a dedicated `cmd/<name>/main.go` binary is.

### `Check.Run` (in-process check body)

One optional field on the `Check` struct: `Run func(ctx context.Context) (status string, extra map[string]any)`. When non-nil, `runCheck` calls `Run` instead of spawning `Binary`, and returns a `CheckResult` directly. `OnFailure` / `OnComplete` are NOT invoked for in-process checks — `Run` returns its `extra` map directly. `Binary` / `Args` / `SuccessMarker` are ignored when `Run` is set. Used today only by `claude-version-lock`; the field is parallel to the existing optional callbacks and doesn't introduce an interface, registry, or kind-switch. Decision rule for adding a new check: in-process when its inputs already live in the runner's scope; a separate binary when the work is non-trivial orchestration. The closure for `claude-version-lock` is built in `main` before `buildChecks` is called so it can capture `claudeVersion` by-reference (populated later by `captureClaudeVersion`) and the `-lock` flag value — dereferenced lazily inside `Run`, after `flag.Parse()`.

### `OnComplete` vs `OnFailure`

Two optional post-processing callbacks on the `Check` struct:

- **`OnFailure(stdout, stderr) → map[string]any`** — fires only on non-pass status. Used by the probe to emit `recording_dir` strictly on `fail`/`timeout` (it's a machine-specific `/tmp/...` path and must not appear on `pass` per the path-scrubbing rule).
- **`OnComplete(stdout, stderr) → map[string]any`** — fires regardless of status, after `OnFailure` if both are set. Used by `snapshot-drift` to emit the per-fixture `snapshots[]` list on both pass and fail.

Both callbacks return `map[string]any` that gets flattened into the report entry as siblings of `name/status/duration_ms`. Precedence: `OnComplete` keys would overwrite `OnFailure` keys if any check populated both — no current check does, but the order is part of the contract.

## Report schema

```json
{
  "claude_version": "2.1.144 (Claude Code)",
  "total_duration_ms": 312456,
  "checks": [
    { "name": "claude-version-lock",     "status": "pass",    "duration_ms":    38,
      "installed_version": "2.1.144",
      "expected_version":  "2.1.144",
      "missing_flags": [] },
    { "name": "spike-one-turn",          "status": "pass",    "duration_ms":  9123 },
    { "name": "spike-multi-turn",        "status": "pass",    "duration_ms": 18247 },
    { "name": "spike-cancel",            "status": "pass",    "duration_ms": 13002 },
    { "name": "spike-permission",        "status": "fail",    "duration_ms": 27341 },
    { "name": "spike-multiselect",       "status": "timeout", "duration_ms": 60000 },
    { "name": "spike-ask-user",          "status": "pass",    "duration_ms":  8200 },
    { "name": "probe-first-prompt-hang", "status": "fail",    "duration_ms":  3214,
      "recording_dir": "/tmp/probe-first-prompt-hang-2026-05-19T13-24-11Z/" },
    { "name": "snapshot-drift",          "status": "fail",    "duration_ms": 42118,
      "snapshots": [
        { "file": "pkg/tuidriver/testdata/picker-snapshot.bin", "result": "match" },
        { "file": "pkg/tuidriver/testdata/mcp-snapshot.bin",    "result": "diff"  },
        { "file": "pkg/tuidriver/testdata/agents-snapshot.bin", "result": "match" }
      ] }
  ]
}
```

`claude_version` is the raw `claude --version` output (whitespace-trimmed); the bare version token (e.g. `2.1.144`) is parsed inside the `claude-version-lock` check and surfaced separately as `installed_version`. If `claude --version` itself fails, `claude_version` is the string `"unknown"` and `claude-version-lock` short-circuits the run.

`installed_version`, `expected_version`, and `missing_flags` always ride on the `claude-version-lock` entry — even on pass, the empty slice is emitted so a CI consumer can rely on the schema. On a drift short-circuit, every check below the `claude-version-lock` `fail` entry appears with `status="timeout"` and `duration_ms=0` (the same shape wall-budget exhaustion produces); the surrounding `claude-version-lock` `fail` entry disambiguates the two.

`snapshots[]` appears on both `pass` and `fail` so a CI consumer can always pinpoint which fixture(s) drifted. It's omitted only when the check binary crashed before emitting any `SNAPSHOT` line (extremely rare — the binary is engineered to always exit cleanly with a real exit code and partial output). `file` is always a repo-relative path; the `/tmp/spike-multiselect-bytes-<ns>.bin` dump path is operator-facing diagnostic output (visible in mirrored stderr) and never enters the report.

**Status values:**
- `pass` — subprocess exited 0, success marker matched (if any). For in-process checks (`version-lock`): the `Run` callback returned `"pass"`.
- `fail` — subprocess exited non-zero, or success marker missing, or `OnFailure` callback reported a problem. For in-process checks: the `Run` callback returned `"fail"`.
- `timeout` — subprocess hit the per-check timeout OR the top-level wall budget was exhausted before this check ran OR a `claude-version-lock` failure short-circuited the rest of the run (un-run checks are appended with `duration_ms=0` so the `checks[]` array length stays stable). `status="timeout"` is triply overloaded; the surrounding `claude-version-lock` entry disambiguates "drift short-circuit" from "wall budget exhausted" from "this individual check actually timed out."

**Path scrubbing:** the report MUST NOT embed machine-specific paths. The runner does not include argv0, cwd, `$HOME`, or `-bin-dir` in the report body. Captured stdout/stderr go to the runner's own stderr (mirrored from each spike's stderr for operator visibility), not into the JSON. The probe's `recording_dir` on failure is the only allowed absolute path — the directory must be locatable for forensics. On `pass`, the probe entry has no `recording_dir`.

**Map-key ordering:** each `checks[]` entry is built as a `map[string]any`, so `encoding/json` emits keys alphabetically (`duration_ms`, `name`, `status`, then any check-specific fields). The schema is deterministic — only the human-readable order is alphabetical rather than logical.

## How it handles failure

| Failure | Behaviour |
|---|---|
| `claude` binary missing | `claude --version` fails → `claude_version="unknown"`; `claude-version-lock` emits a `claude --version failed at startup; cannot enforce claude-version.lock` stderr line and returns `fail`; remaining checks short-circuit with `status="timeout"` and `duration_ms=0`. Report still emits; exit 1. |
| `claude-version.lock` missing | `claude-version-lock` emits `claude-version.lock not found; required for claude-version-lock check` on stderr, returns `fail`, short-circuits the run. Report still emits with `installed_version` parsed from the captured raw version and `expected_version: ""`. |
| `claude-version.lock` parse error | Stderr: `claude-version.lock parse error at line N: <reason>`. Same shape as the missing-file path — `fail`, short-circuit, `expected_version: ""`. |
| `claude` version drifted from lock | Stderr: `claude version 2.1.X does not match claude-version.lock (2.1.144); review and update`. The check continues to the `--help` flag check (so both findings appear in `missing_flags` for the operator), then returns `fail` and short-circuits the rest of the run. |
| `claude --help` missing a pinned flag | One stderr line per missing flag: `claude --help no longer mentions <flag>; review claude-version.lock`. Missing flags collected into `missing_flags`. `fail` + short-circuit. |
| Spike binary missing in `-bin-dir` | The check's `cmd.Run()` returns an error; treated as `status="fail"`. Runner continues with the next check. |
| Per-check timeout | `status="timeout"`. Runner continues. |
| Top-level wall budget hit | In-flight check ends with `status="timeout"`. Remaining checks appended with `status="timeout"` and `duration_ms=0` (un-run). |
| Probe failure | `recording_dir` scraped from probe stderr and added to the report entry. The directory is preserved on disk for forensics (not cleaned up by the runner). |
| `recording_dir` not found in probe stderr | Report entry is well-formed without it. There's no directory to point at. |
| Snapshot capture failure (spike-multiselect crashes, dump missing, stderr lacks the expected log line) | The check binary emits `SNAPSHOT <name> diff` for that fixture, logs `drift in <path>: <err>` to stderr, and continues with the next fixture. The aggregate exit code is `1`. `snapshots[]` still reflects every fixture the check got to. |
| Snapshot byte-mismatch (the actual drift case) | `SNAPSHOT <name> diff` + `drift in <path>` on stderr. Maintainer inspects `/tmp/spike-multiselect-bytes-<ns>.bin` (path visible in mirrored stderr) vs the committed fixture by hand — typically `diff <(xxd /tmp/...) <(xxd pkg/tuidriver/testdata/...-snapshot.bin)`. If the change is legitimate, re-record. |
| Report write fails | Print to stderr, exit 1. |

The dominant invariant: **the report always emits when feasible** — even on partial failure — so CI gets a single uniform artifact.

## Concurrency model

Single goroutine. The runner drives checks serially. `exec.CommandContext` handles subprocess lifecycle (SIGKILL on cancel). No fan-out, no channels, no shared state beyond the parent context. The spike+probe binaries run their own goroutines internally (PTY reader, JSONL tailer, watchdog) — that's their problem; the runner only watches each subprocess as a whole via `cmd.Run()`.

## `claude-version.lock`

The pinned-claude contract at the repo root, consumed by the `claude-version-lock` check.

```
# claude-version.lock — pinned claude binary contract for the e2e harness.
# Update deliberately when bumping the installed claude; the docs ticket
# (#37) covers the workflow.

version=2.1.144

flag=--session-id
flag=--permission-mode bypassPermissions
```

**Format rules** (enforced by `parseLockFile` in `cmd/e2e-runner/main.go`):

- Lines beginning with `#` (after optional leading whitespace) are comments. Blank/whitespace-only lines are ignored.
- Non-comment lines must be `key=value` with `key ∈ {"version", "flag"}`. Anything else is a parse error with the line number.
- Exactly one `version=` line is required. Zero or two-or-more → parse error.
- Zero or more `flag=` lines. Empty value (`flag=`) is a parse error.
- Key and value are trimmed of surrounding whitespace; the value portion preserves internal whitespace verbatim (so `flag=--permission-mode bypassPermissions` stores the literal substring that will be searched for in `claude --help` output).

**Updating the lock file** (manual; tooling deliberately not shipped):

1. Bump claude on the maintainer's box. Run `claude --version`; copy the leading token (e.g. `2.1.145`).
2. Run `claude --help` and confirm every existing `flag=` entry still appears verbatim.
3. Edit `claude-version.lock` — change the `version=` line; add/remove `flag=` lines for any new dependencies.
4. Re-run `make e2e`. The `claude-version-lock` check should pass; if it doesn't, the lock file edit was incomplete.
5. Commit the lock file edit in the same commit as any library changes that depend on the new claude.

The format is intentionally NOT JSON / TOML / YAML — hand-edit-friendliness and grep-friendliness matter more than data-model expressiveness for a 6-line contract file edited once per claude upgrade.

## Re-recording snapshot fixtures

`snapshot-drift` is **read-only** — it never overwrites `pkg/tuidriver/testdata/*-snapshot.bin`. When a diff is legitimate (claude's UI genuinely changed), the maintainer re-records the affected fixture(s) manually:

```sh
make build-bin
# Picker (default settle is fine).
TUIDRIVER_STRICT_MCP_CONFIG=1 ./bin/spike-multiselect -trust-folder=accept -trigger='/'
cp /tmp/spike-multiselect-bytes-<latest>.bin pkg/tuidriver/testdata/picker-snapshot.bin

# Mcp (needs longer settle for the MCP-server-connecting state to resolve).
TUIDRIVER_STRICT_MCP_CONFIG=1 ./bin/spike-multiselect -trust-folder=accept -trigger="$(printf '/mcp\r')" -settle=5s
cp /tmp/spike-multiselect-bytes-<latest>.bin pkg/tuidriver/testdata/mcp-snapshot.bin

# Agents.
TUIDRIVER_STRICT_MCP_CONFIG=1 ./bin/spike-multiselect -trust-folder=accept -trigger="$(printf '/agents\r')"
cp /tmp/spike-multiselect-bytes-<latest>.bin pkg/tuidriver/testdata/agents-snapshot.bin
```

The `TUIDRIVER_STRICT_MCP_CONFIG=1` env var matters: the e2e runner sets it on every child, so the captures the check derives are under `--strict-mcp-config`. Re-recording without the env var would produce fixtures that diff against every CI run.

## Files

- `cmd/e2e-runner/main.go` — orchestrator binary; `Check.Run`/`Check.OnComplete`, `claude-version-lock` + `snapshot-drift` entries, `parseClaudeVersion`, `parseLockFile`, `parseSnapshotResults`, `runClaudeVersionLockCheck`, `skipRest` short-circuit branch.
- `cmd/e2e-snapshot-check/main.go` — snapshot-drift orchestrator; serial spike-multiselect invocations + byte-compare.
- `cmd/e2e-runner/main_test.go` — `parseSnapshotResults`, `parseClaudeVersion`, `parseLockFile` table tests.
- `claude-version.lock` — pinned claude-version + flag contract; hand-edited per upgrade.
- `Makefile` — `e2e`, `build-bin`, `clean-bin`, `clean-report` targets; `CHECKERS` variable for non-spike/non-probe check binaries.
- `pkg/tuidriver/pty.go` — `EnsureClaudeEnv` + the `StrictMcpConfigEnv` opt-in.
- `pkg/tuidriver/testdata/{picker,mcp,agents}-snapshot.bin` — committed byte fixtures consumed by both the unit tests in `pkg/tuidriver/` and the snapshot-drift check.
- `.gitignore` — `/e2e-report.json` and `/e2e-runner` (generated artifacts).
- `.github/workflows/e2e.yml` — push-to-main + `workflow_dispatch` GitHub Actions workflow that invokes `make e2e`, caches the claude install on `claude-version.lock`, and uploads `e2e-report.json` + `/tmp/probe-first-prompt-hang-*` artifacts. Introduced by [#40](../codebase/40.md).

## Related

- Per-ticket notes: [#34](../codebase/34.md), [#35](../codebase/35.md), [#36](../codebase/36.md), [#40](../codebase/40.md)
- Specs: [#34](../../specs/architecture/34-e2e-harness-foundation.md), [#35](../../specs/architecture/35-snapshot-drift.md), [#36](../../specs/architecture/36-claude-version-lock.md), [#40](../../specs/architecture/40-ci-e2e-workflow.md)
- ADRs: orthogonal to both [0001](../decisions/0001-hybrid-jsonl-tui.md) and [0002](../decisions/0002-pattern-matching-over-emulation.md) — the harness shells out (and now also runs an in-process structural check); neither extends the library's signal model.
- System overview: [architecture/system-overview.md](../architecture/system-overview.md)

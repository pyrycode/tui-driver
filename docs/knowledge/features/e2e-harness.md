# e2e harness

The single command (`make e2e`) that verifies the library's empirical end-to-end behaviour against a real installed `claude` binary. Runs every spike + probe serially, classifies each as pass / fail / timeout, and emits a single-file JSON report (`e2e-report.json`) suitable for CI artifact collection. Introduced by [#34](../codebase/34.md). Operator runbook lives here; per-ticket build notes live in `codebase/34.md`.

## What it does

- Runs the 6 spike binaries (`spike-one-turn`, `spike-multi-turn`, `spike-cancel`, `spike-permission`, `spike-multiselect`, `spike-ask-user`), `probe-first-prompt-hang`, and the `snapshot-drift` check serially against a real `claude` install.
- Captures pass/fail/timeout + wall duration per check.
- Emits `e2e-report.json` at the repo root (always — even on partial failure, so CI gets a uniform artifact).
- Exits `0` iff every check passed; `1` otherwise.

Out of scope (today): claude-version-lock check + lock file, GitHub Actions wiring — see [#34 § Follow-ups](../codebase/34.md#links) and [#35 § Follow-ups](../codebase/35.md#links).

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
| `-wall DUR` | `10m` | Top-level wall budget for the entire run. |
| `-timeout NAME=DUR` | per-check default (60s; 30s for the probe) | Per-check timeout override. Repeatable. Unknown `NAME` is a hard parse error so typos surface immediately. |

Example: `./bin/e2e-runner -timeout spike-cancel=90s -timeout probe-first-prompt-hang=45s`.

## Headless / CI plumbing

The runner sets `TUIDRIVER_STRICT_MCP_CONFIG=1` on every child spike+probe process. The spike+probe binaries all call `tuidriver.EnsureClaudeEnv` between constructing the `*exec.Cmd` and starting the PTY; when that env var is `"1"`, `EnsureClaudeEnv` transparently appends `--strict-mcp-config` to `cmd.Args` (idempotent — skipped if already present). The flag tells `claude` to skip configured MCP servers entirely, which is what CI / containers / reproducibility scenarios want. Zero spike-side changes required; the env-var contract is the seam.

No TTY is required on stdin. No MCP servers need to be configured on the host.

## Check kinds

Three kinds today (`Kind` is informational — every check goes through the same `runCheck` body):

- **`spike`** — runs a single spike binary with `-trust-folder=accept`. Passes iff the subprocess exits 0 AND its stdout matches the check's `SuccessMarker` regex.
  - Result spikes (`one-turn`, `multi-turn`, `cancel`, `permission`) use `^SUCCESS` as their marker (they print `SUCCESS: <text>` on green).
  - Observation spikes (`multiselect`, `ask-user`) use `^OBSERVED` as their marker (they print `OBSERVED: <text>` — their contract is "exit 0 + observation logged").
- **`probe`** — same as spike but with a 30s default timeout (instead of 60s) and an `OnFailure` callback that scrapes the recording-dir path from probe stderr (`probe outDir=<path>`) and emits it as `recording_dir` in the report entry. The probe's own internal `wallTimeout` is 3 min; the runner's 30s timeout kills it earlier (only on hang), and 30s is looser than the probe's 10s "fast" verdict so a healthy 2–3s baseline doesn't trip it.
- **`snapshot`** — drives `cmd/e2e-snapshot-check` (a thin orchestrator that invokes `spike-multiselect` three times via its `-trigger` / `-settle` flags, scrapes the dump path from spike-multiselect's stderr, and byte-compares each capture to the committed `pkg/tuidriver/testdata/{picker,mcp,agents}-snapshot.bin`). 180s default timeout (`-timeout snapshot-drift=DUR` to override). No `SuccessMarker` — exit code is authoritative. An `OnComplete` callback (see below) parses one `SNAPSHOT <name> match|diff` stdout line per fixture into a `snapshots[]` list that appears on both `pass` AND `fail` so the operator can always pinpoint which fixture(s) drifted (or confirm none did). **Read-only** — never writes to the committed fixtures; re-recording is a separate maintainer step (see [#35](../codebase/35.md)). Introduced by [#35](../codebase/35.md). Byte-equality is intentional even though it's fragile — false positives are a feature that force the maintainer to look and re-record deliberately.

Future check kinds (version-lock) will add one entry each to the hardcoded check list; the abstraction is intentionally a leaf, not a framework.

### `OnComplete` vs `OnFailure`

Two optional post-processing callbacks on the `Check` struct:

- **`OnFailure(stdout, stderr) → map[string]any`** — fires only on non-pass status. Used by the probe to emit `recording_dir` strictly on `fail`/`timeout` (it's a machine-specific `/tmp/...` path and must not appear on `pass` per the path-scrubbing rule).
- **`OnComplete(stdout, stderr) → map[string]any`** — fires regardless of status, after `OnFailure` if both are set. Used by `snapshot-drift` to emit the per-fixture `snapshots[]` list on both pass and fail.

Both callbacks return `map[string]any` that gets flattened into the report entry as siblings of `name/status/duration_ms`. Precedence: `OnComplete` keys would overwrite `OnFailure` keys if any check populated both — no current check does, but the order is part of the contract.

## Report schema

```json
{
  "claude_version": "claude X.Y.Z (or 'unknown' if `claude --version` fails)",
  "total_duration_ms": 312456,
  "checks": [
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

`snapshots[]` appears on both `pass` and `fail` so a CI consumer can always pinpoint which fixture(s) drifted. It's omitted only when the check binary crashed before emitting any `SNAPSHOT` line (extremely rare — the binary is engineered to always exit cleanly with a real exit code and partial output). `file` is always a repo-relative path; the `/tmp/spike-multiselect-bytes-<ns>.bin` dump path is operator-facing diagnostic output (visible in mirrored stderr) and never enters the report.

**Status values:**
- `pass` — subprocess exited 0, success marker matched (if any).
- `fail` — subprocess exited non-zero, or success marker missing, or `OnFailure` callback reported a problem.
- `timeout` — subprocess hit the per-check timeout OR the top-level wall budget was exhausted before this check ran (un-run checks are appended with `duration_ms=0` so the `checks[]` array length stays stable).

**Path scrubbing:** the report MUST NOT embed machine-specific paths. The runner does not include argv0, cwd, `$HOME`, or `-bin-dir` in the report body. Captured stdout/stderr go to the runner's own stderr (mirrored from each spike's stderr for operator visibility), not into the JSON. The probe's `recording_dir` on failure is the only allowed absolute path — the directory must be locatable for forensics. On `pass`, the probe entry has no `recording_dir`.

**Map-key ordering:** each `checks[]` entry is built as a `map[string]any`, so `encoding/json` emits keys alphabetically (`duration_ms`, `name`, `status`, then any check-specific fields). The schema is deterministic — only the human-readable order is alphabetical rather than logical.

## How it handles failure

| Failure | Behaviour |
|---|---|
| `claude` binary missing | `claude --version` fails → `claude_version="unknown"`; checks proceed and almost certainly all fail. Report still emits; exit 1. |
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

- `cmd/e2e-runner/main.go` — orchestrator binary; `Check.OnComplete`, `snapshot-drift` entry, `parseSnapshotResults`.
- `cmd/e2e-snapshot-check/main.go` — snapshot-drift orchestrator; serial spike-multiselect invocations + byte-compare.
- `cmd/e2e-runner/main_test.go` — `parseSnapshotResults` table tests.
- `Makefile` — `e2e`, `build-bin`, `clean-bin`, `clean-report` targets; `CHECKERS` variable for non-spike/non-probe check binaries.
- `pkg/tuidriver/pty.go` — `EnsureClaudeEnv` + the `StrictMcpConfigEnv` opt-in.
- `pkg/tuidriver/testdata/{picker,mcp,agents}-snapshot.bin` — committed byte fixtures consumed by both the unit tests in `pkg/tuidriver/` and the snapshot-drift check.
- `.gitignore` — `/e2e-report.json` and `/e2e-runner` (generated artifacts).

## Related

- Per-ticket notes: [#34](../codebase/34.md), [#35](../codebase/35.md)
- Spec: [docs/specs/architecture/34-e2e-harness-foundation.md](../../specs/architecture/34-e2e-harness-foundation.md)
- ADRs: orthogonal to both [0001](../decisions/0001-hybrid-jsonl-tui.md) and [0002](../decisions/0002-pattern-matching-over-emulation.md) — the harness shells out, it doesn't extend the library's signal model.
- System overview: [architecture/system-overview.md](../architecture/system-overview.md)

# e2e harness

The single command (`make e2e`) that verifies the library's empirical end-to-end behaviour against a real installed `claude` binary. Runs the `claude-version-lock` check first, then every spike + probe + the snapshot-drift check serially, classifies each as pass / fail / timeout, and emits a single-file JSON report (`e2e-report.json`) suitable for CI artifact collection. Introduced by [#34](../codebase/34.md); extended by [#35](../codebase/35.md) (snapshot-drift), [#36](../codebase/36.md) (claude-version-lock), [#40](../codebase/40.md) (GitHub Actions push-to-main workflow), [#64](../codebase/64.md) (`claude-version-lock` policy narrowed — version equality dropped, flag/value presence kept), [#81](../codebase/81.md) (snapshot-drift byte-compare → parsed-shape JSON compare; renderer-byte volatility absorbed by parsers), [#128](../codebase/128.md) (`/mcp` strict-mcp empty-state classifier + re-record), [#129](../codebase/129.md) (snapshot-drift `/` picker case dropped — irreducibly host-dependent), and [#178](../codebase/178.md) (snapshot-drift `/agents` case dropped — modal removed in claude 2.1.199 — leaving `mcp` the sole surviving fixture, and the `claude-version.lock` `version=` bumped `2.1.158`→`2.1.199` as the sweep-completing annotation). Operator runbook lives here; per-ticket build notes live in `codebase/<N>.md`.

## What it does

- Runs the in-process `claude-version-lock` check first (asserts every `flag=` and `value=` pinned in `claude-version.lock` still appears in `claude --help` — claude patch drift between the installed binary and the lock's informational `version=` field is intentionally tolerated, see [#64](../codebase/64.md)), then the 7 spike binaries (`spike-one-turn`, `spike-multi-turn`, `spike-cancel`, `spike-permission`, `spike-multiselect`, `spike-ask-user`, `spike-long-prompt`), then the two probes (`probe-first-prompt-hang`, then `probe-cwd-encoding`), then the `snapshot-drift` check — all serially against a real `claude` install. `spike-long-prompt` is the `Session.WritePrompt` regression rig — bracketed-paste path, 3.45 KB embedded fixture, three token markers, substring assertion (see [#47](../codebase/47.md)). `probe-cwd-encoding` is a **recording rig** — it observes byte-for-byte how `claude` encodes a non-ASCII cwd leaf (`Työ😀`) into its `~/.claude/projects/<name>` dir, discovered *independently of `EncodeCwd`* (glob `~/.claude/projects/*/<session-id>.jsonl`), and ships green on any observation; first live run (claude 2.1.199) found **per-UTF-16-code-unit** encoding, contradicting the library's per-byte `EncodeCwd` (see [#206](../codebase/206.md), unblocks the [#207](https://github.com/pyrycode/tui-driver/issues/207) fix).
- Captures pass/fail/timeout + wall duration per check.
- Emits `e2e-report.json` at the repo root (always — even on partial failure, so CI gets a uniform artifact).
- Exits `0` iff every check passed; `1` otherwise.
- **Short-circuits the slow checks on a `claude-version-lock` failure.** If claude has drifted out from under the library, every subsequent check is appended to the report with `status="timeout"` and `duration_ms=0` rather than burning ~5 minutes of wall time on spikes that are doomed anyway. See [§ How it handles failure](#how-it-handles-failure).

CI integration: a GitHub Actions workflow at `.github/workflows/e2e.yml` runs `make e2e` on every push to `main` and on manual dispatch. See [§ CI integration](#ci-integration) below for the trigger surface, cost-cap rationale, and artifact shape. Introduced by [#40](../codebase/40.md).

## How to run

```sh
make e2e                              # build everything to ./bin and run the runner (inherits operator's Claude config)
make e2e MODEL=haiku EFFORT=low       # CI default: pin --model haiku --effort low (caps metered-API spend)
make build-bin                        # build only (no run)
make clean-bin                        # rm -rf ./bin
make clean-report                     # rm -f ./e2e-report.json
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
| `-timeout NAME=DUR` | per-check default (60s; 30s for `probe-first-prompt-hang`; 120s for spike-cancel; 180s for snapshot-drift) | Per-check timeout override. Repeatable. Unknown `NAME` is a hard parse error so typos surface immediately. `spike-cancel`'s 120s built-in is structural — Probe 4 re-runs the 1000-word monad essay as a real recovery turn (~30–40s of model time); see [#69](../codebase/69.md). `probe-cwd-encoding` deliberately keeps the 60s **default** (not the 30s `probeCheckTimeout`), because its internal 30s glob-poll must fit inside the runner budget; see [#206](../codebase/206.md). |

Example: `./bin/e2e-runner -timeout spike-cancel=90s -timeout probe-first-prompt-hang=45s -timeout claude-version-lock=10s`.

## Headless / CI plumbing

The runner sets `TUIDRIVER_STRICT_MCP_CONFIG=1` on every child spike+probe process. The spike+probe binaries all call `tuidriver.EnsureClaudeEnv` between constructing the `*exec.Cmd` and starting the PTY; when that env var is `"1"`, `EnsureClaudeEnv` transparently appends `--strict-mcp-config` to `cmd.Args` (idempotent — skipped if already present). The flag tells `claude` to skip configured MCP servers entirely, which is what CI / containers / reproducibility scenarios want. Zero spike-side changes required; the env-var contract is the seam.

No TTY is required on stdin. No MCP servers need to be configured on the host.

The harness uses `--strict-mcp-config` for determinism: without it, `claude` waits on configured MCP servers to register before processing the first prompt. On the GitHub runner that has no MCP servers configured this is mostly a no-op, but on operator machines (where `make e2e` also runs) host-level MCP config produces flaky first-prompt timing. Setting `TUIDRIVER_STRICT_MCP_CONFIG=1` from the runner side makes the harness behave identically regardless of host MCP config. This is consistent with the May 18 walk-back, not a contradiction: that walk-back recommended against making `--strict-mcp-config` the library's user-facing default sidestep for pyry acp — a consumer where MCP is a user-facing feature and stripping it would harm users. The e2e/CI context has no user-facing MCP feature, so the harness using the flag for determinism is exactly the kind of scoped, internal use the walk-back left intact. See `.github/workflows/e2e.yml` for the workflow that drives the runner.

The same env-var seam carries `TUIDRIVER_CLAUDE_MODEL` and `TUIDRIVER_CLAUDE_EFFORT`. When either is set to a non-empty value, `EnsureClaudeEnv` appends `--model <value>` and/or `--effort <value>` to each spike+probe's `cmd.Args` (idempotent — skipped if already present). The Makefile surfaces them as `make e2e MODEL=haiku EFFORT=low`, inlined per-recipe so the vars only ride the runner invocation (never `build-bin`'s `go build`). The runner itself is unchanged: `cmd.Env = append(os.Environ(), …)` already inherits whatever Make exports, so model/effort flow shell → Make → runner → spike child without per-binary plumbing. **Unset is the load-bearing default**: it preserves the operator's interactive Claude config (currently Opus 4.7 + high), which is required for Max-subscription local development. CI sets `MODEL=haiku EFFORT=low` to pin a cheap model — see [§ CI integration](#ci-integration).

## CI integration

`.github/workflows/e2e.yml` invokes `make e2e` on every push to `main` and on `workflow_dispatch` (manual UI trigger). Explicitly NOT `pull_request` — every run burns metered `ANTHROPIC_API_KEY` credits (CI runners cannot use a Max subscription), so per-PR runs would multiply spend. The push-to-main gate is the cheapest coverage that still catches regressions before downstream consumers hit them. PR-time coverage is a separate concern: the code-review agent runs the harness selectively elsewhere.

**Cost controls.** Two complementary mechanisms:

- Trigger surface narrow to `push` (branch `main` only) + `workflow_dispatch`. No `pull_request`, no `paths:` filters (drift must surface on every push to `main`), no `[skip ci]` opt-out paths.
- Job-level `timeout-minutes: 20` is the hard ceiling. Operator's p99 wall-time target is `< 15min`; the 20-minute cap gives ~5min headroom so a slow-but-healthy run isn't spuriously killed, while firmly bounding cost if the harness hangs. If runs reliably exceed 15min, file a profiling follow-up rather than bumping the cap.
- **Model / effort pinned to the cheap path.** CI invokes `make e2e MODEL=haiku EFFORT=low` (per #48). The default Opus + high pairing the operator's Claude config uses would cost ~$0.50–$2 per spike × 7 spikes + probe ≈ $3–$15 per CI run; Haiku low runs ~$0.20–$0.70 per run — roughly 18–90× cheaper at parity coverage (spikes test PTY/JSONL/modal behaviour, not reasoning quality). The seam is the `TUIDRIVER_CLAUDE_MODEL` / `TUIDRIVER_CLAUDE_EFFORT` env vars; see [§ Headless / CI plumbing](#headless--ci-plumbing) for the propagation chain. Local `make e2e` with no overrides keeps inheriting the operator's interactive config (Max-subscription path).

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
- `probe-recordings` → `/tmp/probe-first-prompt-hang-*` (`if-no-files-found: ignore` — clean runs may have no captures). **Note:** this glob does not yet cover `probe-cwd-encoding`'s recording dir (`<tmp>/probe-cwd-encoding-*`); #206 added the probe but did not touch the workflow, so on a red `probe-cwd-encoding` run the `observation.log` is written locally but not uploaded as a CI artifact — a follow-up (see [#206](../codebase/206.md) § Follow-ups) should widen the glob.

Both use 30-day retention (trimmed from the 90-day default; drift events get inspected within hours).

**Exit code & branch protection.** The workflow exits non-zero on any harness failure (the runner's exit-code contract from #34). This is the wiring point for branch protection: add `e2e` as a required check via the GitHub UI to gate merges on the workflow's verdict. The branch-protection toggle itself is out of scope for the workflow file.

**Concurrency.** `concurrency.group: e2e-${{ github.ref }}` with `cancel-in-progress: false` — back-to-back pushes serialise rather than racing or cancelling each other. The push-to-main backstop is meant to land a verdict per SHA, not race itself.

## Check kinds

Four kinds today (`Kind` is informational — every check goes through the same `runCheck` body):

- **`version-lock`** — in-process check that runs first. Parses `claude-version.lock`, parses the leading token out of the captured `claude --version` output (informational only — populates `installed_version` for the report; the lock's `version=` populates `expected_version`), then runs `claude --help` once and `strings.Contains`-checks every `flag=` and `value=` entry against the help output. The substantive contract is the flag/value presence — claude patch drift between the lock's `version=` and the installed binary is intentionally tolerated (see [#64](../codebase/64.md) for the rationale: the version-equality assertion produced spurious FAILs every time the reviewer-side claude auto-updated, while adding no signal the flag/value check didn't already provide). Default 60s timeout (override via `-timeout claude-version-lock=DUR`). Failure short-circuits every subsequent check. Emits `installed_version`, `expected_version`, `missing_flags`, and `missing_values` (always present — empty slices on pass) on the report entry. The only in-process check kind: the `Run` field on `Check` is set, `Binary`/`Args`/`SuccessMarker` are ignored. Introduced by [#36](../codebase/36.md); policy narrowed by [#64](../codebase/64.md).
- **`spike`** — runs a single spike binary with `-trust-folder=accept`. Passes iff the subprocess exits 0 AND its stdout matches the check's `SuccessMarker` regex.
  - Result spikes (`one-turn`, `multi-turn`, `cancel`, `permission`, `long-prompt`) use `^SUCCESS` as their marker (they print `SUCCESS: <text>` on green).
  - Observation spikes (`multiselect`, `ask-user`) use `^OBSERVED` as their marker (they print `OBSERVED: <text>` — their contract is "exit 0 + observation logged").
- **`probe`** — same as spike, plus an `OnFailure` callback that scrapes the recording-dir path from probe stderr (`probe outDir=<path>`) and emits it as `recording_dir` in the report entry. **Timeout is per-check, not per-Kind:** `probe-first-prompt-hang` sets `Timeout: probeCheckTimeout` (30s) — its own internal `wallTimeout` is 3 min but the runner kills it earlier on hang, and 30s is looser than the probe's 10s "fast" verdict so a healthy 2–3s baseline doesn't trip it. `probe-cwd-encoding` (since [#206](../codebase/206.md)) leaves `Timeout` **unset** → the 60s `defaultCheckTimeout`, because its internal 30s glob-poll (waiting on `claude` to write the session JSONL, deferred until first input) would not fit inside a 30s check budget. So do not assume "probe ⇒ 30s"; read the check's `Timeout` field. Both probes use `SuccessMarker: ^OBSERVED` (recording rigs: green = exit 0 + observation logged).
  - **`NonGating` — de-gating a check without removing it.** An optional `bool` field on `Check`. `gateFailed(c, status)` returns `false` for any `NonGating` check regardless of `status` — its result (status, `recording_dir`, etc.) still lands in the report, but a non-`pass` outcome never reddens the overall `make e2e` verdict. Use for an environment/timing-sensitive rig that is worth running for its artifact but has no in-library fix available (a re-anchor would be an unverified live-claude-gated hypothesis; see the spike-from-fix split rule). **Both probes are `NonGating` today:** `probe-first-prompt-hang` since [#181](https://github.com/pyrycode/tui-driver/issues/181) (false idle → dropped first prompt → session JSONL never appears within its own 30s wait), and `probe-cwd-encoding` since [#251](../codebase/251.md) for the identical root cause (structural twin — same false-idle-before-first-prompt mechanism, same 30s `sessionFileWait` timeout, diagnosed as unrelated to `EncodeCwd` since `discoverProjectsDir` discovers by session-id, not by the encoded path). Re-gating either check is deferred, out-of-band work: a spike-from-fix pair where child A re-anchors the probe on a real readiness signal and observes reliability across N live `make e2e` runs, and child B drops `NonGating` only if A proves it out — no in-library positive readiness signal exists yet beyond `IsIdle` ([#173](https://github.com/pyrycode/tui-driver/issues/173) Open Q1).
- **`snapshot`** — drives `cmd/e2e-snapshot-check` (a thin orchestrator that invokes `spike-multiselect` once per fixture — a single `mcp` fixture today — via its `-trigger` / `-settle` flags, scrapes the dump path from spike-multiselect's stderr, reads the `<dump>.parsed.json` sidecar the spike writes after running `ParseMcpStatus`, and `reflect.DeepEqual`-compares the JSON tree to the committed `pkg/tuidriver/testdata/mcp-snapshot.json`). 180s default timeout (`-timeout snapshot-drift=DUR` to override). No `SuccessMarker` — exit code is authoritative. An `OnComplete` callback (see below) parses one `SNAPSHOT <name> match|diff` stdout line per fixture into a `snapshots[]` list that appears on both `pass` AND `fail` so the operator can always pinpoint which fixture(s) drifted (or confirm none did). Default mode is **read-only** — re-recording rides on an opt-in `-record` flag (operator-driven via `make rerecord-snapshots`); `make e2e` never sets it. Introduced by [#35](../codebase/35.md) (byte-compare); switched to parsed-shape JSON compare by [#81](../codebase/81.md) after #72 evidence that every byte-level mitigation surfaces the next layer of renderer volatility (Try-line rotation, bimodal whitespace-clear pass). The parsers absorb the volatile bytes; the parsed shape is stable. Comparison is exact-equal across the entire parsed struct — no field whitelist, no subset matching. The check binary unconditionally sets `TUIDRIVER_STRICT_MCP_CONFIG=1` on every spike-multiselect child so re-recorded fixtures don't leak operator MCP-config paths into `mcp-snapshot.json`. The `/` **picker** case was dropped by [#129](../codebase/129.md): its render embeds the operator's plugin/skill commands (`/code-review`, `/figma-use`, `/walkthrough`, …) which are not claude built-ins, and no host-portable built-in-vs-plugin discriminator exists (confirmed live against claude 2.1.158 — resolving spec #81's picker Open Question), so any committed picker fixture re-commits operator-specific state and stays red on `main` and on a clean CI runner. The `/agents` case was then dropped by [#178](../codebase/178.md): the tabbed running-subagents modal was **removed in claude 2.1.199** (`/agents` now prints a one-line "wizard has been removed" notice, which `DetectModalClass` classifies `Unknown`), so the committed `agents-snapshot.json` described a modal that no longer exists and could not be re-recorded — a cleaner drop than picker's, since there is no render to capture at all. **The check now drives a single `mcp` fixture.** Both dropped cases keep their *parser/classifier* coverage in `pkg/tuidriver/` unit tests against retained host-independent `.bin` byte fixtures — `picker-snapshot.bin` / `picker-truecolor-snapshot.bin` for the picker (#129), `agents-snapshot.bin` for agents (#178) — raw bytes, not a live capture.

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
      "installed_version": "2.1.146",
      "expected_version":  "2.1.144",
      "missing_flags": [],
      "missing_values": [] },
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
        { "file": "pkg/tuidriver/testdata/mcp-snapshot.json",    "result": "diff"  }
      ] }
  ]
}
```

`claude_version` is the raw `claude --version` output (whitespace-trimmed); the bare version token (e.g. `2.1.146`) is parsed inside the `claude-version-lock` check and surfaced separately as `installed_version`. If `claude --version` itself fails, `claude_version` is the string `"unknown"` and `claude-version-lock` short-circuits the run.

`installed_version`, `expected_version`, `missing_flags`, and `missing_values` always ride on the `claude-version-lock` entry — even on pass, the empty slices are emitted so a CI consumer can rely on the schema. `installed_version` ≠ `expected_version` is **not** a gate condition (per [#64](../codebase/64.md), patch drift is informational only); the fields exist so the operator can see drift in the JSON report and so re-record sweeps have a single grep target for "what claude was this lock last reviewed against." Consumers that want a soft drift indicator can compare the two fields themselves; the harness no longer fails on the inequality. On a drift short-circuit (lock-file missing, parse error, captured-version `"unknown"`, `claude --help` exec failure, or any `missing_flags` / `missing_values` entries), every check below the `claude-version-lock` `fail` entry appears with `status="timeout"` and `duration_ms=0` (the same shape wall-budget exhaustion produces); the surrounding `claude-version-lock` `fail` entry disambiguates the two.

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
| `claude` version drifted from lock | Tolerated by design (since [#64](../codebase/64.md)). `installed_version` ≠ `expected_version` shows up in the report but does not produce a stderr line and does not gate the check. The substantive contract is the flag/value presence check below. |
| `claude --help` missing a pinned flag | One stderr line per missing flag: `claude --help no longer mentions flag <flag>; review claude-version.lock`. Missing flags collected into `missing_flags`. `fail` + short-circuit. |
| `claude --help` missing a pinned value | One stderr line per missing value: `claude --help no longer mentions value <value>; review claude-version.lock`. Missing values collected into `missing_values`. `fail` + short-circuit. |
| Spike binary missing in `-bin-dir` | The check's `cmd.Run()` returns an error; treated as `status="fail"`. Runner continues with the next check. |
| Per-check timeout | `status="timeout"`. Runner continues. |
| Top-level wall budget hit | In-flight check ends with `status="timeout"`. Remaining checks appended with `status="timeout"` and `duration_ms=0` (un-run). |
| Probe failure | `recording_dir` scraped from probe stderr and added to the report entry. The directory is preserved on disk for forensics (not cleaned up by the runner). |
| `recording_dir` not found in probe stderr | Report entry is well-formed without it. There's no directory to point at. |
| Snapshot capture failure (spike-multiselect crashes, dump missing, stderr lacks the expected log line) | The check binary emits `SNAPSHOT <name> diff` for that fixture, logs `drift in <path>: <err>` to stderr, and continues with the next fixture. The aggregate exit code is `1`. `snapshots[]` still reflects every fixture the check got to. **Missing-sidecar case:** when the `.parsed.json` is absent because `DetectModalClass` didn't recognise the captured UI (spike's `default` branch wrote nothing), the `<err>` is enriched with the class spike-multiselect logged — `… no such file (spike classified modal as "X" and wrote no sidecar — classifier drift, not a content diff)` — so the failure reads as classifier drift, not a content diff. `ModalClassUnknown` (empty string) renders as `Unknown`; if no `modal-class detected=` line is present, the bare error is returned unchanged (see [#128](../codebase/128.md)). |
| Snapshot parsed-shape mismatch (the actual drift case) | `SNAPSHOT <name> diff` + `drift in <path>` on stderr, followed by `  captured at <parsed.json>` on the next line. Maintainer inspects via `diff <(jq . pkg/tuidriver/testdata/<name>-snapshot.json) <(jq . /tmp/spike-multiselect-bytes-<ns>.parsed.json)`. If the change is legitimate, `make rerecord-snapshots` to refresh the JSON fixtures, bump `claude-version.lock` `version=`, commit both together. |
| Report write fails | Print to stderr, exit 1. |

The dominant invariant: **the report always emits when feasible** — even on partial failure — so CI gets a single uniform artifact.

## Concurrency model

Single goroutine. The runner drives checks serially. `exec.CommandContext` handles subprocess lifecycle (SIGKILL on cancel). No fan-out, no channels, no shared state beyond the parent context. The spike+probe binaries run their own goroutines internally (PTY reader, JSONL tailer, watchdog) — that's their problem; the runner only watches each subprocess as a whole via `cmd.Run()`.

## `claude-version.lock`

The pinned API surface at the repo root, consumed by the `claude-version-lock` check. As of [#64](../codebase/64.md): `version=` is informational metadata; `flag=` and `value=` lines are the enforced contract.

```
# claude-version.lock — pinned API surface for the e2e harness.
#
# `version=` is informational: the claude version this lock was last
# reviewed against. It is NOT enforced at runtime — claude patch drift
# above this version is intentionally tolerated. Bump deliberately when
# re-recording snapshot fixtures (see #47, #57).
#
# `flag=` and `value=` lines ARE enforced — each MUST appear as a
# substring of `claude --help` for the e2e gate to pass.

version=2.1.144

flag=--session-id
flag=--permission-mode
value=bypassPermissions
flag=--model
flag=--effort
```

**Format rules** (enforced by `parseLockFile` in `cmd/e2e-runner/main.go`):

- Lines beginning with `#` (after optional leading whitespace) are comments. Blank/whitespace-only lines are ignored.
- Non-comment lines must be `key=value` with `key ∈ {"version", "flag", "value"}`. Anything else is a parse error with the line number.
- Exactly one `version=` line is required. Zero or two-or-more → parse error. The field is required at parse time even though it is no longer enforced at runtime — keeping it required preserves a single grep target for "what claude version was this lock last reviewed against?", which matters for the snapshot-drift re-record discipline (see [#47](../codebase/47.md), [#57](../codebase/57.md)).
- Zero or more `flag=` lines. Empty value (`flag=`) is a parse error.
- Zero or more `value=` lines. Empty value (`value=`) is a parse error. Conventionally used for argv values that appear as standalone tokens in `claude --help` output (e.g. `bypassPermissions` for `--permission-mode bypassPermissions`) — splitting flag and value into separate lines lets the substring check survive cosmetic re-flowing of the `--help` text.
- Key and value are trimmed of surrounding whitespace; the value portion preserves internal whitespace verbatim.

**Updating the lock file** (manual; tooling deliberately not shipped):

The semantics of an edit shifted with [#64](../codebase/64.md). Two distinct triggers, with different urgency:

- **Legitimate / scheduled — re-recording fixtures.** When `snapshot-drift` or one of the byte-stream-sensitive spikes (`spike-multi-turn`, `spike-cancel`, `spike-permission`, `spike-ask-user`) starts failing because claude's TUI output has genuinely changed, the fixture re-record sweep AND the lock bump travel together. Bumping `version=` is the maintainer's annotation that "the fixtures committed alongside this edit were captured against this claude." See [#47](../codebase/47.md) and [#57](../codebase/57.md) on why these two changes must coincide.
- **No-op / informational — patch drift.** When the operator's claude has auto-updated past `version=` but every pinned flag and value still appears in `claude --help`, the harness no longer cares. The operator MAY bump `version=` to reflect the latest reviewed-against value, but the bump has no functional effect on the gate. Bumping mid-feature-ticket purely to silence the gate is what [#64](../codebase/64.md) exists to make unnecessary; the anti-pattern (`#47` and `#57` lessons) still applies if a bump cascades into sibling-fixture failures.

Workflow when re-recording fixtures (the legitimate path):

1. Bump claude on the maintainer's box. Run `claude --version`; note the leading token (e.g. `2.1.146`).
2. Run `claude --help` and confirm every existing `flag=` and `value=` entry still appears verbatim. Add new lines for any new dependencies; remove lines for retired ones.
3. Re-record snapshot fixtures per § [Re-recording snapshot fixtures](#re-recording-snapshot-fixtures) below and any other byte-stream-coupled fixtures the cascade names.
4. Edit `claude-version.lock` — change the `version=` line to match what you re-recorded against; update `flag=` / `value=` lines if step 2 surfaced changes.
5. Re-run `make e2e`. The `claude-version-lock` check should pass under the new claude (it would have passed before the bump too — that's the [#64](../codebase/64.md) change); the byte-stream-coupled checks should also pass once their fixtures have been refreshed.
6. Commit the lock-file edit and the fixture refresh in the same commit, alongside any library changes that depend on the new claude.

The format is intentionally NOT JSON / TOML / YAML — hand-edit-friendliness and grep-friendliness matter more than data-model expressiveness for a small contract file edited once per re-record sweep.

## Re-recording snapshot fixtures

`snapshot-drift` is **read-only by default** — `make e2e` never writes to `pkg/tuidriver/testdata/*-snapshot.json`. When a diff is legitimate (claude's UI genuinely changed) the maintainer regenerates the affected JSON fixtures via the dedicated `-record` mode (introduced by [#81](../codebase/81.md)):

```sh
make rerecord-snapshots          # build-bin + bin/e2e-snapshot-check -record -bin-dir ./bin
git diff pkg/tuidriver/testdata/ # review the parsed-shape changes
# bump claude-version.lock version= to match `claude --version`
git add pkg/tuidriver/testdata/*-snapshot.json claude-version.lock
git commit -m "fixtures: re-record snapshot-drift fixtures for claude X.Y.Z"
make e2e                         # validate the re-record produced a green run
```

The check binary unconditionally sets `TUIDRIVER_STRICT_MCP_CONFIG=1` on every spike-multiselect child it spawns (regardless of `-record`), so re-recorded JSON is reproducible across hosts — when an MCP modal *is* rendered, operator MCP config does not leak into `mcp-snapshot.json`'s `categories[].path` field. **As of claude 2.1.158 (still confirmed on 2.1.199, [#178](../codebase/178.md)), `/mcp` under `--strict-mcp-config` renders no modal at all** — it prints an inline `No MCP servers configured` empty-state, which `DetectModalClass` classifies as MCP (since [#128](../codebase/128.md)) and `ParseMcpStatus` reduces to `{"total_servers":0,"categories":null}`. So the committed `mcp-snapshot.json` is currently that empty shape; the strict-mcp seam still does the reproducibility work (no host-specific config can appear in a `{0, null}` fixture). (`/agents` also changed across this window — its tabbed modal was **removed** in 2.1.199, so its snapshot-drift case was dropped by #178; the `agents-snapshot.bin` unit fixture is retained. A `modal-class=Unknown` red is not always renderer drift to re-record — first confirm the modal still exists; the `/agents` red was a *removed* modal, not a stale one.) The `.bin` fixtures in the same directory are unit-test fixtures (consumed by `pkg/tuidriver/{picker,mcp,agents,grid,modal}_test.go`) and are NOT touched by `-record`; if claude's TUI byte stream changes structurally and breaks unit tests, the `.bin` files are refreshed manually via the legacy `spike-multiselect` invocation pattern (see [#35](../codebase/35.md) history). The full-modal `mcp-snapshot.bin` (10 servers / 3 categories, non-strict) and the new strict-mcp empty-state `mcp-empty-snapshot.bin` are distinct captures — see [#128](../codebase/128.md).

**Don't relax the comparison policy on intermittent drift.** If a fixture diffs across two consecutive `make e2e` runs without any code change, the parser has a non-deterministic input filter — file a separate parser-stabilisation ticket, don't whitelist fields. The exact-equal policy is load-bearing; subset matching is the failure mode that #72 evidence already rejected.

## Files

- `cmd/e2e-runner/main.go` — orchestrator binary; `Check.Run`/`Check.OnComplete`, `claude-version-lock` + `snapshot-drift` entries, `parseClaudeVersion`, `parseLockFile`, `parseSnapshotResults`, `runClaudeVersionLockCheck`, `skipRest` short-circuit branch.
- `cmd/e2e-snapshot-check/main.go` — snapshot-drift orchestrator; serial spike-multiselect invocations + parsed-shape JSON compare via `reflect.DeepEqual`; `-record` flag for fixture refresh; unconditional `TUIDRIVER_STRICT_MCP_CONFIG=1` on each child. See [#81](../codebase/81.md).
- `cmd/e2e-runner/main_test.go` — `parseSnapshotResults`, `parseClaudeVersion`, `parseLockFile` table tests.
- `claude-version.lock` — pinned claude-version + flag contract; hand-edited per upgrade.
- `Makefile` — `e2e`, `build-bin`, `clean-bin`, `clean-report`, `rerecord-snapshots` targets; `CHECKERS` variable for non-spike/non-probe check binaries; `MODEL` / `EFFORT` overrides inlined per-recipe on `e2e`. The per-binary pattern rule (`$(BIN_DIR)/%: FORCE`) lists a recipe-less `FORCE:` target as a prereq so `go build` runs every invocation and its cache handles incremental rebuilds across `cmd/` and `pkg/` — without this, stale binaries from a previous build would silently shadow source edits (see [#79](../codebase/79.md)). Steady-state no-op `make build-bin` ≈ 480 ms across all 10 binaries.
- `pkg/tuidriver/pty.go` — `EnsureClaudeEnv` + the `StrictMcpConfigEnv` opt-in + the `ClaudeModelEnv` / `ClaudeEffortEnv` `--model`/`--effort` passthrough seam.
- `pkg/tuidriver/testdata/mcp-snapshot.json` — the single committed parsed-shape fixture consumed by the snapshot-drift check. Output of `ParseMcpStatus` marshalled via `json.MarshalIndent(_, "", "  ")`. Hand-editable; `git diff`-friendly. Regenerated via `make rerecord-snapshots`. Two sibling `.json` fixtures were deleted as their e2e cases were dropped: `picker-snapshot.json` by [#129](../codebase/129.md) (picker irreducibly host-dependent) and `agents-snapshot.json` by [#178](../codebase/178.md) (`/agents` modal removed in claude 2.1.199) — see § "Check kinds". See [#81](../codebase/81.md).
- `pkg/tuidriver/testdata/{picker,mcp,agents}-snapshot.bin` — raw PTY byte fixtures consumed by unit tests in `pkg/tuidriver/` (picker_test.go, mcp_test.go, agents_test.go, grid_test.go, modal_test.go). NOT read by the snapshot-drift check since [#81](../codebase/81.md); kept because the unit tests feed raw bytes into parsers and detectors (input ≠ output for those tests). The `picker-*` and `agents` `.bin` fixtures are **retained** even though their `.json` snapshot-drift siblings were dropped (#129, #178) — the `.bin` is what keeps the picker/agents parser + classifier coverage host-independent across those drops.
- `.gitignore` — `/e2e-report.json` and `/e2e-runner` (generated artifacts).
- `.github/workflows/e2e.yml` — push-to-main + `workflow_dispatch` GitHub Actions workflow that invokes `make e2e`, caches the claude install on `claude-version.lock`, and uploads `e2e-report.json` + `/tmp/probe-first-prompt-hang-*` artifacts. Introduced by [#40](../codebase/40.md).

## Related

- Per-ticket notes: [#34](../codebase/34.md), [#35](../codebase/35.md), [#36](../codebase/36.md), [#40](../codebase/40.md), [#64](../codebase/64.md), [#81](../codebase/81.md), [#128](../codebase/128.md), [#129](../codebase/129.md), [#178](../codebase/178.md)
- Specs: [#34](../../specs/architecture/34-e2e-harness-foundation.md), [#35](../../specs/architecture/35-snapshot-drift.md) (superseded), [#36](../../specs/architecture/36-claude-version-lock.md), [#40](../../specs/architecture/40-ci-e2e-workflow.md), [#64](../../specs/architecture/64-claude-version-lock-policy.md), [#81](../../specs/architecture/81-snapshot-drift-parsed-shape.md) (current snapshot-drift design), [#128](../../specs/architecture/128-mcp-empty-state-classifier.md), [#129](../../specs/architecture/129-drop-picker-snapshot-case.md), [#178](../../specs/architecture/178-drop-agents-snapshot-case.md)
- ADRs: orthogonal to both [0001](../decisions/0001-hybrid-jsonl-tui.md) and [0002](../decisions/0002-pattern-matching-over-emulation.md) — the harness shells out (and now also runs an in-process structural check); neither extends the library's signal model.
- System overview: [architecture/system-overview.md](../architecture/system-overview.md)

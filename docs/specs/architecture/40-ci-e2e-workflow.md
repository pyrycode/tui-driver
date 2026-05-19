# Spec 40 — e2e: GitHub Actions workflow runs `make e2e` on push-to-main

Ticket: https://github.com/pyrycode/tui-driver/issues/40
Status: ready for developer
Label: `security-sensitive` (see § Security review at end)

## Files to read first

- `Makefile` — the headline target is `make e2e`; it depends on `build-bin` which compiles every spike + probe + checker + the runner into `./bin/`. The workflow's "build all spike + probe binaries" step (AC #2 bullet 4) and the "run `make e2e`" step (AC #2 bullet 5) collapse into a single `make e2e` invocation — `build-bin` is a phony prerequisite, so `make e2e` already builds everything.
- `claude-version.lock` (repo root) — the file whose content keys the CI cache. Current `version=2.1.144` + two `flag=` lines. The install step parses out `version=`; the in-process `claude-version-lock` check (run by the runner, not by CI) re-validates both the version and the flag presence inside `claude --help`. Belt-and-suspenders: CI installs the lock-pinned version, harness asserts at run time.
- `cmd/e2e-runner/main.go:113-184` — the runner sets `TUIDRIVER_STRICT_MCP_CONFIG=1` on every spawned child (see step 3 in the spec-34 `runCheck` semantics: `cmd.Env = append(os.Environ(), "TUIDRIVER_STRICT_MCP_CONFIG=1")`). **AC #2's last bullet is therefore automatic** — the CI workflow MUST NOT add this env var at the job/step level; doing so is harmless redundancy that future readers will misinterpret as load-bearing.
- `pkg/tuidriver/pty.go:24-40` — `EnsureClaudeEnv` consumes `TUIDRIVER_STRICT_MCP_CONFIG` and appends `--strict-mcp-config` to `cmd.Args` if set. Confirms the runner-side wiring is sufficient.
- `cmd/probe-first-prompt-hang/main.go:73-80` — `outDir := filepath.Join(os.TempDir(), "probe-first-prompt-hang-"+tsTag)`. On Linux (`ubuntu-latest`), `os.TempDir()` resolves to `/tmp`, so the path glob `/tmp/probe-first-prompt-hang-*` is the artifact target. The probe creates the dir on every run, not just on failure; uploading on `if: always()` therefore captures both pass-with-recording (rare; the probe writes captures on its way to pass too) and fail-with-recording.
- `docs/knowledge/features/e2e-harness.md` § "Headless / CI plumbing" — the operator-facing summary of how `TUIDRIVER_STRICT_MCP_CONFIG` flows from runner → spike → claude. Documents that "no TTY is required on stdin. No MCP servers need to be configured on the host" — i.e. ubuntu-latest is a supported execution environment by design.
- `docs/specs/architecture/34-e2e-harness-foundation.md` § "Headless MCP plumbing" + § "runCheck semantics" — the original decision record for the env-var seam. Reading § "Path scrubbing" too: the report deliberately excludes machine-specific paths, so `e2e-report.json` is safe to upload as a CI artifact without further scrubbing.
- `go.mod` (top line: `go 1.26.2`) — the version `actions/setup-go@v5` reads via `go-version-file: go.mod`. No hardcoded version string in the workflow (AC #2 bullet 2).
- Existing workflow precedent: **none.** `ls .github/workflows/` returns no files; this is the repo's first GitHub Actions workflow. There is no in-repo convention to copy from; pick the conservative shape documented below.

## Context

The e2e harness (`make e2e`) is the empirical contract test for the library against real `claude`. It was built up over #34 (foundation), #35 (snapshot-drift), #36 (claude-version-lock), and now needs a deterministic backstop on the release branch: a CI job that runs it on every push to `main`, fails the build on red, and archives the report + probe recordings for forensics.

PR-time coverage is a separate concern. Per the ticket body, the code-review agent runs the harness selectively elsewhere; this workflow is the cheap, predictable, all-the-time gate at the merge point. The trigger surface is deliberately narrow because **every run burns Anthropic API credits** (CI runners can't use a Max subscription — `ANTHROPIC_API_KEY` is metered billing). The two cost controls are the AC: (a) `push` + `workflow_dispatch` only, no `pull_request`; (b) finite `timeout-minutes` `<= 20` so a hang is killed before the bill compounds.

Two non-obvious facts from reading the code that this spec leans on:

1. **`make e2e` already does the right thing on `ubuntu-latest`.** The runner is sequential, single-goroutine, and shells out to claude via PTY. The library's whole point is that this works on Linux with no extra setup. No special CI flag, no test-mode toggle, no headless harness mode. The workflow is one `make e2e` call after the binaries are installed and the API key is in env.
2. **`--strict-mcp-config` plumbing is already in the runner.** AC #2 last bullet asks for it; the runner already injects it via `TUIDRIVER_STRICT_MCP_CONFIG=1` on every child. The CI workflow doesn't need to set this env var or pass any flag — `make e2e` invokes the runner, which does the rest. The spec calls this out explicitly because the AC's surface phrasing ("such that each spike receives `--strict-mcp-config` (or the harness's equivalent CI flag)") could lead a developer to wire it again at the workflow level. **Don't.** The "harness's equivalent" already fires unconditionally; CI is a no-op caller from this angle.

## Design

### File touched

```
.github/workflows/e2e.yml        # NEW: single workflow file, ~80 lines of YAML
```

No other production changes. No Makefile edit (it already produces what CI needs). No documentation edit (the documentation phase appends to `docs/knowledge/features/e2e-harness.md` after merge). No `.gitignore` edit (`/e2e-report.json` is already listed from #34).

Production source files modified: **0** (YAML is config, not source per the file-count self-check). Confirmed S size.

### Workflow shape

```yaml
name: e2e
on:
  push:
    branches: [main]
  workflow_dispatch:
permissions:
  contents: read
concurrency:
  group: e2e-${{ github.ref }}
  cancel-in-progress: false
jobs:
  e2e:
    runs-on: ubuntu-latest
    timeout-minutes: 20
    env:
      ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
    steps:
      # see § Steps below
```

**Per-key rationale:**

- `on.push.branches: [main]` — AC: push-to-main only. Explicitly NOT `pull_request`. Don't add path filters; the ticket forbids `[skip ci]`-style opt-outs, and path filters would let a "docs-only" push silently bypass drift detection that a hypothetical doc change might in fact affect (the operator runbook references the workflow).
- `on.workflow_dispatch:` — AC: manual trigger via the Actions UI. No inputs; the workflow is parameter-free.
- `permissions: contents: read` — minimum required (checkout reads the repo, nothing else needs write). The default token permissions in GitHub Actions are read/write across the board; this explicit `read` shrinks the blast radius if a downstream step were ever compromised. No `id-token: write` (no OIDC needed), no `actions: write`, no `pull-requests: write`.
- `concurrency.group: e2e-${{ github.ref }}` with `cancel-in-progress: false` — serialise runs on `main`. If two pushes land back-to-back, the second waits for the first rather than racing or cancelling it. `cancel-in-progress: false` preserves the first run's artifact + signal (a cancelled run muddies the "did the gate pass on this SHA?" question; for a release-branch backstop we want each main SHA's run to complete). For `workflow_dispatch`, the same group means a manual re-run during a push-triggered run queues correctly. This is mild defensive shaping — there's no observed contention today; it costs ~zero lines of YAML and prevents a future "two pushes, two parallel API-paid runs, one cancelled" failure mode.
- `runs-on: ubuntu-latest` — AC.
- `timeout-minutes: 20` — AC mandates `<= 20`; the operator's wall-time target is `< 15min` p99. 20m gives ~5min headroom over p99 so a slow-but-healthy run isn't spuriously killed, while still firmly bounding cost. If runs reliably exceed 15min, follow the technical-note instruction (file a profiling follow-up, do NOT bump this cap without thought).
- `env.ANTHROPIC_API_KEY` set at the **job** level (not step level): the runner subprocess and any descendant inherit it. GitHub auto-masks the secret in logs as long as the workflow references `${{ secrets.* }}` (which we do — never copy it into an intermediate env var or shell variable).

### Steps (in order)

```yaml
- name: Checkout
  uses: actions/checkout@v4

- name: Set up Go
  uses: actions/setup-go@v5
  with:
    go-version-file: go.mod
    cache: true

- name: Cache claude CLI
  id: cache-claude
  uses: actions/cache@v4
  with:
    path: ~/.npm-global
    key: claude-${{ runner.os }}-${{ hashFiles('claude-version.lock') }}

- name: Install claude CLI (cache miss only)
  if: steps.cache-claude.outputs.cache-hit != 'true'
  run: |
    set -euo pipefail
    mkdir -p "$HOME/.npm-global"
    npm config set prefix "$HOME/.npm-global"
    version="$(awk -F= '/^version=/ {print $2; exit}' claude-version.lock | tr -d '[:space:]')"
    test -n "$version" || { echo "claude-version.lock: missing version=" >&2; exit 1; }
    npm install -g "@anthropic-ai/claude-code@${version}"

- name: Put claude on PATH
  run: echo "$HOME/.npm-global/bin" >> "$GITHUB_PATH"

- name: Run make e2e
  run: make e2e

- name: Upload e2e-report.json
  if: always()
  uses: actions/upload-artifact@v4
  with:
    name: e2e-report
    path: e2e-report.json
    if-no-files-found: warn
    retention-days: 30

- name: Upload probe recordings
  if: always()
  uses: actions/upload-artifact@v4
  with:
    name: probe-recordings
    path: /tmp/probe-first-prompt-hang-*
    if-no-files-found: ignore
    retention-days: 30
```

**Per-step rationale:**

- **Checkout.** `actions/checkout@v4` — the current major (as of the architect's pin window). Default `fetch-depth: 1` is fine; the workflow needs the worktree, not git history.
- **Set up Go.** `actions/setup-go@v5` with `go-version-file: go.mod` reads the directive (`go 1.26.2`) and installs that toolchain. AC #2 bullet 2: "no hardcoded version string." `cache: true` is the default in v5 but written explicitly for self-documentation; it caches `~/go/pkg/mod` keyed on `go.sum`.
- **Cache claude CLI.** Separate `cache` step (rather than relying on `actions/setup-node`'s built-in cache, which doesn't fit our global-install model). `path: ~/.npm-global` matches the install step's `npm config set prefix`. `key: claude-${{ runner.os }}-${{ hashFiles('claude-version.lock') }}` — AC #2 bullet 3: a different lock value invalidates the cache. `hashFiles` hashes the file content, so any edit (version, flags, even a comment) changes the key. Lock-file version drift is the bug we're catching, so missing the cache on every drift event is correct.
- **Install claude CLI (cache miss only).** Guarded by `steps.cache-claude.outputs.cache-hit != 'true'` so it only runs when the cache key didn't match. The script:
  - `set -euo pipefail` — fail fast on any error; treat unset vars as errors; propagate failures through pipes.
  - Configure npm to install globally under `~/.npm-global` so the cache path is stable and user-owned (avoids `sudo` for `/usr/local`).
  - Parse the version from `claude-version.lock` with `awk -F= '/^version=/ {print $2; exit}'` (matches the lock's `key=value` shape; the `exit` defends against accidental duplicate `version=` lines by taking the first — though `parseLockFile` in the runner would catch the dup as a parse error on the next step anyway). `tr -d '[:space:]'` defends against trailing whitespace.
  - Sanity check: error out if the parsed version is empty.
  - `npm install -g "@anthropic-ai/claude-code@${version}"` — exact version, never `latest`, never a range. The lock file is the source of truth.
- **Put claude on PATH.** `$GITHUB_PATH` is the canonical mechanism for cross-step PATH mutation. This step runs on both cache hit and miss (no `if:` guard) so the cached binary directory is reliably available to `make e2e`. The install step's PATH mutation would only fire on cache miss, so this separate step is the cache-hit-safe equivalent.
- **Run make e2e.** Just `make e2e`. No env vars set here (the job-level `ANTHROPIC_API_KEY` is already inherited; `TUIDRIVER_STRICT_MCP_CONFIG` is injected by the runner onto its children — not the CI's job). Exit code propagates naturally: `make e2e` returns the runner's exit code, which is `0` iff every check passed (#34 spec § "Exit code contract"). AC #4 (non-zero on harness failure) falls out for free.
- **Upload e2e-report.json.** `if: always()` per AC #3. `if-no-files-found: warn` — the runner emits the report on essentially every failure mode (#34 § "the report always emits" invariant), so a missing file would indicate a deeper crash worth noticing in the log. `retention-days: 30` — drift events are useful for ~weeks; a year of history would just inflate storage with no review value. Default is 90 days; 30 is a deliberate trim.
- **Upload probe recordings.** Separate artifact (not bundled into the report) because the recording dir can be many small files and is only emitted on probe runs that produced output. `if-no-files-found: ignore` — on a healthy run, the probe may not have anything to ship (the probe always creates `outDir`, but the artifact action ignoring "no files found" is conservative against a future probe variant that only writes-on-failure). Path glob `/tmp/probe-first-prompt-hang-*` is an absolute path — `actions/upload-artifact@v4` accepts absolute paths; only the file basenames are preserved inside the artifact archive (so the resulting artifact tree is `probe-recordings/probe-first-prompt-hang-<tsTag>/...`).

### What the workflow does NOT do

These are intentional non-features; calling them out so a well-intentioned developer doesn't add them:

- **No `setup-node` step.** `ubuntu-latest` ships with a recent node + npm pre-installed; no per-project node version is pinned and none is needed (claude-code is installed globally, not run from a `node_modules`). Adding `setup-node` would be code volume without value.
- **No matrix.** Single combination: `ubuntu-latest` × the `go.mod` toolchain. Library is Linux-only for CI by ticket constraint.
- **No `if: github.event_name == 'push'`-style step gating.** Both triggers (`push` to main, `workflow_dispatch`) should run the full job identically. No diverging step set.
- **No `--strict-mcp-config` wiring at the CI level.** The runner does it via `TUIDRIVER_STRICT_MCP_CONFIG=1`; CI is the wrong layer.
- **No notifications / Slack hooks / status comments.** Out of scope. Branch protection on the `e2e` check name is how this gates merges (AC #4 implies but doesn't include the protection toggle; the workflow exits non-zero, which is sufficient).
- **No `paths:` or `paths-ignore:`.** Ticket constraint forbids opt-outs; every push to main runs.
- **No `[skip ci]` skip-pattern.** Same constraint.
- **No `secrets.GITHUB_TOKEN` references.** The workflow doesn't touch the repo, doesn't comment on PRs, doesn't post statuses (beyond the inherent check-run that GitHub creates for every workflow). The job-level `permissions: contents: read` gates the default `GITHUB_TOKEN` to read-only for the workflow.
- **No SHA-pinning of actions.** Major-version pins (`@v4`, `@v5`) for first-party `actions/*` actions. Trust model: `actions/checkout`, `actions/setup-go`, `actions/cache`, `actions/upload-artifact` are all GitHub-stewarded; major-version pinning is the documented best practice for that namespace. SHA pinning would be stronger but adds renovation burden; if a future ticket adds third-party actions (`@some-org/*`), revisit then.

### Concurrency model

Workflow-level: single job, sequential steps. No matrix, no parallel jobs. Job-level concurrency group serialises across runs of this workflow on the same ref. No fan-out anywhere.

### Error handling

| Failure | Behaviour |
|---|---|
| `actions/checkout` fails | Job fails before any API spend. Surface via workflow status. |
| `actions/setup-go` fails (toolchain unavailable) | Job fails before any API spend. |
| Cache restore fails (transient GHA backend issue) | Cache step reports `cache-hit != 'true'`; the install step runs as a fresh install. Functionally a slow run, not a failure. |
| `npm install` fails (registry outage, removed version, network blip) | Install step exits non-zero; workflow fails before `make e2e`. No API spend. |
| `claude-version.lock` malformed (missing `version=`) | Install step's parser exits with `claude-version.lock: missing version=`; workflow fails. |
| `make e2e` fails (any check status `fail`/`timeout`) | Runner exits non-zero (#34 invariant); the step fails; the workflow fails. The report and probe-recordings artifacts still upload via `if: always()`. |
| `make e2e` hangs past the 20-min budget | GitHub kills the job at `timeout-minutes: 20`. Artifact uploads still attempted (`if: always()` fires on cancellation too). |
| Report file missing post-run | Artifact upload step logs a `warn` (per `if-no-files-found: warn`) but does not fail the workflow on top of the already-failed `make e2e`. The workflow's overall conclusion is already `failure` from the prior step. |
| Probe recordings absent | Artifact upload step ignores per `if-no-files-found: ignore`. |
| GitHub Actions runner image regression breaks pre-installed tools | Outside the workflow's control. Surface via the workflow's regular failure; investigate via the runner image changelog. Not a code change in this repo. |

### Testing strategy

The workflow is exercised by being merged. There is no unit-test surface for a YAML file.

Concrete validation the developer does before flipping `done:developer`:

1. **Syntax: `actionlint` or equivalent.** Run `actionlint .github/workflows/e2e.yml` locally (`brew install actionlint`) — catches typos in keys, invalid `uses:` references, and most expression-language mistakes. Not a hard merge gate (the project has no CI for the workflow file itself), but a 5-second sanity check.
2. **Dispatch test on a feature branch.** Push the feature branch with the new workflow, then in the Actions UI, choose "Run workflow" with the `feature/40` ref. This exercises `workflow_dispatch` against the actual branch contents without first merging to main. Validate:
   - All steps execute in order; no syntax errors.
   - Cache step runs (cache miss on first dispatch; cache hit on second).
   - claude installs to `~/.npm-global/bin/claude`; PATH propagation works.
   - `make e2e` runs to completion and emits `e2e-report.json` (or fails, depending on the current main vs branch state).
   - Both artifact uploads succeed.
3. **Post-merge observation, first run.** After the PR merges, the push-to-main triggers the workflow. Confirm green; confirm wall time fits the `<15min` target. If not, file the profiling follow-up per the technical note.

The dispatch-test approach (#2 above) requires `ANTHROPIC_API_KEY` to already exist in repo secrets. Per AC #5, that prerequisite is on the maintainer; #41 (out of scope here) documents the rotation procedure.

### Open questions / decisions

- **Pre-decided: 20-minute job timeout.** AC ceiling is 20. The 15-minute wall-time target is a p99 budget; setting `timeout-minutes` to 15 would invert the relationship (the cap would frequently kill healthy long-tail runs). 20m gives the p99 budget 5m of slack before the hard kill — enough to absorb noise without inviting unbounded burn.
- **Pre-decided: cache the entire npm-global tree, not just the binary.** `npm install -g @anthropic-ai/claude-code` installs the package itself plus its dependency tree under `~/.npm-global/lib/node_modules/`. Caching only `~/.npm-global/bin/claude` would restore the symlink target but not the JS files it points to. Cache the whole prefix.
- **Pre-decided: 30-day artifact retention.** Default is 90 days. Drift events are usually inspected within hours of failing; >30 days of history is rarely consulted. Conservative trim, easy to revisit.
- **Pre-decided: `concurrency` group with `cancel-in-progress: false`.** The push-to-main backstop should land a verdict per SHA, not race or cancel itself. Documented above.
- **Pre-decided: no `setup-node` step.** Pre-installed node is sufficient; npm versioning isn't load-bearing for claude-code installation.
- **Pre-decided: major-version action pins.** Trade-off documented above.
- **Out of scope: branch protection toggle.** Ticket scope. The maintainer adds `e2e` as a required check via GitHub UI after the workflow lands.
- **Out of scope: `ANTHROPIC_API_KEY` rotation procedure.** Tracked in #41 per AC #5.
- **Out of scope: pre-PR / per-PR CI coverage.** Cost cap rationale (ticket context). The code-review agent runs the harness selectively elsewhere.
- **Out of scope: notification channels on red.** No Slack hook, no email rule. GitHub's built-in workflow status surfaces enough; downstream consumers (operators monitoring main) can subscribe to repo activity if they want push notifications.

## Security review

**Verdict:** PASS

This is a CI workflow that consumes a secret (`ANTHROPIC_API_KEY`), installs a third-party CLI (`@anthropic-ai/claude-code`), runs it against the public Anthropic API, and uploads two artifact bundles. The threat surface is small and the design's defaults are aligned with current GitHub Actions security guidance.

**Findings:**

- [Trust boundaries] No findings. The single secret crosses one boundary: GitHub Secrets → workflow `env` → `make e2e` subprocess tree. Reference is via `${{ secrets.ANTHROPIC_API_KEY }}` only — never copied into a shell variable, never echoed, never written to a file the workflow controls. GitHub's automatic log masking applies to any string that flows from a `secrets.*` reference; the workflow does nothing that would defeat masking.
- [Tokens, secrets, credentials] No MUST FIX. One SHOULD FIX **for code review**: confirm the developer did not introduce an intermediate shell-variable assignment of the secret. The shape of the workflow as specified above sets `ANTHROPIC_API_KEY` exactly once at the job-`env` level and references it only by inheritance into `make e2e`; any deviation (e.g. `run: API_KEY=$ANTHROPIC_API_KEY claude ...`) breaks the masking guarantee. Lifecycle (rotation/revocation) is owned by #41, not this spec.
- [File operations] No findings. The workflow's `run:` script writes nothing user-controlled to the filesystem. `~/.npm-global` is created with default-umask permissions (sufficient for an ephemeral runner). No `os.Stat`-then-`os.Open` pattern, no symlink handling. `e2e-report.json` is written by the runner (already audited under #34) and read-only consumed by the upload step.
- [Subprocess / external command execution] No findings. The only user-input-like value in a subprocess argument is the version string parsed from `claude-version.lock`. `awk` + `tr` + `test -n` reject empty values; the lock file's repo-root location and `parseLockFile` co-validation (the runner re-parses it strictly and fails on any malformed input) cap the blast radius. `npm install -g "@anthropic-ai/claude-code@${version}"` is double-quoted; a malicious lock-file value (e.g. ` && rm -rf /`) would arrive at npm as a literal version string, which npm rejects as a parse error. The runner subprocess (`make e2e`) inherits only the explicitly-set job-env vars plus the runner's defaults — no `sh -c "$user_input"` pattern anywhere. `set -euo pipefail` on the install script catches partial failures.
- [Cryptographic primitives] N/A — the workflow does no crypto itself. TLS to the npm registry and to `api.anthropic.com` is handled by the respective clients (npm, claude). The `ANTHROPIC_API_KEY` is opaque to this workflow.
- [Network & I/O] No findings. The runner makes outbound calls to the npm registry (install) and to `api.anthropic.com` (via claude). No inbound network surface — the workflow doesn't expose a listener. No HTTP server in scope, so `ReadTimeout`/`WriteTimeout` aren't applicable. Outbound runtime is bounded by the job `timeout-minutes: 20`.
- [Error messages, logs, telemetry] SHOULD FIX **for code review** (not a spec change): if a future workflow change adds `ACTIONS_STEP_DEBUG: true` or copies env into the log output, the secret's masking still holds **for direct prints** but `printenv` / `env` would emit the value verbatim. The current spec doesn't introduce any such call; flag for code-review vigilance only. Recording-dir contents in the `probe-recordings` artifact are PTY captures of claude's TUI output — claude does not echo its own API key, and the harness does not write env into the recording. Residual risk: a future claude version that reflected its env into its TUI display would surface the key in recordings; not a present-day vulnerability, and the artifact is private to repo collaborators by GHA default.
- [Concurrency] N/A — the workflow has one job, sequential steps. The job-level `concurrency` group serialises across workflow runs (not security-relevant on its own; documented for run-state hygiene).
- [Threat model alignment] No project-wide threat-model doc exists in the repo. The threats touched here are: (a) API key exfiltration via logs or artifacts — addressed above; (b) supply-chain compromise of `@anthropic-ai/claude-code` — partially mitigated by exact-version pinning in `claude-version.lock`; npm package republish attacks on an exact version are detected by npm's tampering guarantees (registry-side), not by anything in this workflow. SHA-pinning the actions would harden against `actions/*` supply-chain compromise; deferred per § "no SHA-pinning of actions" rationale above. If a future ticket re-evaluates that trade-off, the relevant SHAs are pinned at the major-version tag's HEAD at the time of pinning.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-19

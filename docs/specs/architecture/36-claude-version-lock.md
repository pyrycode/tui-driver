# Spec 36 — e2e: claude-version-lock check + claude-version.lock file

Ticket: https://github.com/pyrycode/tui-driver/issues/36
Status: ready for developer

## Files to read first

- `cmd/e2e-runner/main.go:31-36` — runner constants (`defaultCheckTimeout = 60s`, etc.). The new check uses the default-timeout fallback, no explicit `Timeout`.
- `cmd/e2e-runner/main.go:53-67` — `Check` struct. This spec adds **one optional field**: `Run func(ctx context.Context) (status string, extra map[string]any)`. Parallel to existing optional `OnFailure` / `OnComplete` callbacks — the "intentionally a leaf, not a framework" extension shape from #34.
- `cmd/e2e-runner/main.go:113-184` — `main()`. Two surgical changes here: (1) consume `claudeVersion` (already captured) inside the new check, (2) add a short-circuit branch after `runCheck` returns so a version-lock failure skips remaining checks.
- `cmd/e2e-runner/main.go:147-166` — the existing for-loop over `checks`. The wall-budget-exhaustion arm at L148 (`if parentCtx.Err() != nil { append timeout, duration_ms=0 }`) is the precedent for how short-circuited checks are recorded; the new short-circuit reuses the same shape.
- `cmd/e2e-runner/main.go:186-257` — `buildChecks()`. The new entry slots at the **top** of the returned slice (AC bullet 5: runs first).
- `cmd/e2e-runner/main.go:280-288` — `captureClaudeVersion`. Unchanged. Its returned value (`"2.1.144 (Claude Code)"` today) feeds both `Report.ClaudeVersion` and the new check, satisfying "do not introduce a third call site."
- `cmd/e2e-runner/main.go:290-348` — `runCheck`. Add one branch at the top: if `c.Run != nil`, call it and return a `CheckResult` directly (skipping the subprocess path). `OnFailure` / `OnComplete` semantics are not exercised by in-process checks — the `Run` callback returns its `extra` map directly.
- `cmd/e2e-runner/main.go:350-367` — `marshalResults`. No change — the new check's `installed_version` / `expected_version` / `missing_flags` ride through the existing `Extra → siblings` flatten.
- `cmd/e2e-runner/main_test.go:1-50` — existing table-test conventions. The new spec adds analogous table tests for two pure helpers (`parseClaudeVersion`, `parseLockFile`).
- `docs/knowledge/features/e2e-harness.md:46-57` — operator-facing reference for the existing `spike`/`probe`/`snapshot` check kinds. This spec does **not** require a doc edit (architect doesn't own the feature doc); the documentation phase updates it after merge.
- `docs/knowledge/codebase/34.md` § "Patterns established" — the "Flat `Check` struct, hardcoded slice, single `runCheck` function … snapshot-drift and version-lock checks will each add one struct entry, not a new abstraction" seed for this spec. Compliance: one new entry in the slice, one new optional field on `Check`, no new package, no registry, no plugin shape.
- `docs/specs/architecture/35-snapshot-drift.md` § "The `e2e-snapshot-check` binary" — the alternative integration shape (helper binary). Read for contrast; this spec deliberately picks in-process over the helper-binary precedent. § "Open questions" justifies why.
- Bash one-liner to verify the canonical `claude --version` shape on the maintainer's box: `claude --version` → `2.1.144 (Claude Code)`. The parser must accept the trailing parenthetical and extract the leading `2.1.144` token.

## Context

The library's empirical contract with claude is two flags: `--session-id` and `--permission-mode bypassPermissions`. Both are subject to upstream churn — rename, removal, semantic change — that can land silently when the maintainer upgrades claude. The e2e harness today catches behavioural drift (spikes fail), but only AFTER running the slow spikes; and it doesn't distinguish "library is broken" from "claude moved underneath us." A version + flag lockfile turns the second case into an immediate, actionable error: *"claude 2.1.144 → 2.1.145; review claude-version.lock."*

This check is in-process, fast (<1s typical), and a structural drift signal — it must run **first** so spike failures attributable to claude drift never accumulate 5 minutes of wall time before the maintainer learns why.

Two non-obvious facts from reading the existing code:

1. **`claude --version` output isn't a bare version token.** `claude --version` prints `2.1.144 (Claude Code)\n`. `captureClaudeVersion` today trims whitespace but does NOT extract the version token; `Report.ClaudeVersion` therefore contains `"2.1.144 (Claude Code)"`. The lock file's expected value is the bare version token `2.1.144`. The new check must parse out the leading token (first whitespace-separated word) before comparing. **Report.ClaudeVersion stays as-is** for schema stability; parsing happens inside the new check and the parsed value lands in `installed_version` (a check-specific field).

2. **The runner doesn't currently short-circuit.** Every check runs serially regardless of prior failures. AC bullet 5's "drift failure short-circuits the slower spike + probe checks below it" is a new behaviour. The cleanest shape: a small flag (`skipRest`) set when version-lock fails non-pass; the existing wall-budget-exhausted arm at `main.go:148` is the model — un-run checks are appended with `status="timeout"` and `duration_ms=0`. Operator can disambiguate: a `claude-version-lock` failure at the top of `checks[]` followed by every other check showing `duration_ms=0` is the unambiguous "drift short-circuited the run" pattern.

## Design

### Files touched

```
claude-version.lock              # NEW: repo-root data file (~6 lines incl. comments)
cmd/e2e-runner/main.go           # MODIFIED: add Check.Run field, version-lock check, short-circuit branch
cmd/e2e-runner/main_test.go      # MODIFIED: table tests for parseClaudeVersion + parseLockFile
```

No new packages. No new binaries. No Makefile change. No library import.

Production source files modified: **1** (`cmd/e2e-runner/main.go`). Within the S budget (≤3 files, ≤150 lines).

### Lock file: `claude-version.lock`

Plain text, line-based, key=value. Lives at repo root (sibling of `Makefile`, `e2e-report.json`, `go.mod`). Initial content:

```
# claude-version.lock — pinned claude binary contract for the e2e harness.
# Update deliberately when bumping the installed claude; the docs ticket
# (#37) covers the workflow.

version=2.1.144

flag=--session-id
flag=--permission-mode bypassPermissions
```

**Format rules (the parser enforces all of these — table tests cover each rule):**

- Lines beginning with `#` (after optional leading whitespace) are comments.
- Blank lines (or whitespace-only lines) are ignored.
- Non-comment lines must be `key=value` with `key ∈ {"version", "flag"}`. Anything else is a parse error with the line number.
- Exactly one `version=` line is required. Two or more → parse error. Zero → parse error.
- Zero or more `flag=` lines. Empty value (`flag=`) is a parse error.
- Both key and value are trimmed of surrounding whitespace before storage.
- The `value` portion is taken verbatim including internal whitespace, e.g. `flag=--permission-mode bypassPermissions` stores `--permission-mode bypassPermissions` (the literal substring the help-output search will look for).

The format is intentionally NOT JSON / TOML / YAML — the file is hand-edited once per claude upgrade and grep-friendliness matters more than data-model expressiveness.

### `Check.Run` field (in-process integration)

One new optional field on the existing struct:

```go
type Check struct {
    Name          string
    Kind          string
    Binary        string
    Args          []string
    SuccessMarker *regexp.Regexp
    Timeout       time.Duration
    OnFailure     func(stdout, stderr string) map[string]any
    OnComplete    func(stdout, stderr string) map[string]any
    // Run is an in-process check body. When non-nil, runCheck calls Run
    // instead of spawning Binary, and returns a CheckResult directly.
    // OnFailure / OnComplete are NOT invoked for in-process checks — Run
    // returns its extra fields directly. Binary / Args / SuccessMarker
    // are ignored when Run is set.
    Run func(ctx context.Context) (status string, extra map[string]any)
}
```

**Rationale:** in-process keeps the work where the inputs already live (captured claude version) and avoids a fourth orchestrated binary in `./bin/`. The extension is one optional field, parallel to existing optional callbacks — it does NOT introduce an interface, a registry, or a kind-switch. `Kind` stays informational (the new check sets `Kind: "version-lock"`). This honors #34's "intentionally a leaf, not a framework" constraint.

**Why not a helper binary** (the snapshot-drift precedent):

- A helper would need to either (a) re-run `claude --version` itself — violates "no third call site" — or (b) accept the captured version via flag/env, paying a round-trip for a single string the runner already has in scope.
- Snapshot-drift's helper exists because it orchestrates THREE spike-multiselect subprocess invocations with per-fixture flags + byte-comparison — non-trivial flow the runner shouldn't own. The version-lock check is two string comparisons; the in-process body is ~30 LOC.
- One new binary means a new Makefile entry, a new exec round-trip per run, a new `go build` target. Net cost > the cost of one optional field.

### `runCheck` change

Add one branch at the top of `runCheck` (before the subprocess path):

- Signature unchanged: `runCheck(parent context.Context, c Check, binDir string) CheckResult`.
- If `c.Run != nil`: build `ctx` with timeout same as today (`c.Timeout` or `defaultCheckTimeout`), record `start`, call `c.Run(ctx)`, return `CheckResult{Name, Status, DurationMs, Extra}` directly. `binDir` is unused on this path.
- Else: the existing subprocess body runs unchanged.

That is the entirety of the runCheck change.

### `claude-version-lock` check entry

Inserted as the **first** entry in `buildChecks()`:

- `Name: "claude-version-lock"`
- `Kind: "version-lock"` (new informational kind; documents the entry's role in the slice)
- `Binary`, `Args`, `SuccessMarker`, `OnFailure`, `OnComplete`: zero values
- `Timeout`: zero (`runCheck` falls back to `defaultCheckTimeout = 60s`; the runtime `-timeout claude-version-lock=DUR` override is wired automatically via the existing `tos.known` registration loop at main.go:120-122 — AC bullet 4 free)
- `Run`: a closure (built in `main` before the `buildChecks()` call) that captures the parsed `claudeVersion` and the lock-file path; signature `func(ctx) (status, extra)`. See § "Run callback body" below.

### Short-circuit on version-lock failure

In `main`'s for-loop over `checks`, after `r := runCheck(...)`:

- If `r.Name == "claude-version-lock"` AND `r.Status != "pass"`: set a local `skipRest := true`.
- At the top of the loop, the existing wall-budget arm at L148 widens by one clause: `if parentCtx.Err() != nil || skipRest { ... }` — un-run checks get `status="timeout"`, `duration_ms=0`, `allPassed = false`. The `checks[]` array length stays stable per the established invariant.

No new status value. No new top-level Report field. Operator disambiguates `timeout` causes from the surrounding entries (a `claude-version-lock` `fail` immediately above means short-circuit, not wall budget).

### Run callback body

Behavior (NOT a full function body — the developer writes it in the project's style):

1. Read `claude-version.lock` from cwd (repo root when `make e2e` runs from there).
   - Missing file → `status="fail"`, stderr line: `e2e-runner: claude-version.lock not found; required for claude-version-lock check`. `extra` carries `installed_version` (parsed from the captured raw version), `expected_version: ""`, `missing_flags: []`.
   - Parse error → `status="fail"`, stderr line: `e2e-runner: claude-version.lock parse error at line N: <reason>`. Same `extra` shape with `expected_version: ""`.
2. Parse the captured raw `claude --version` output via `parseClaudeVersion`.
   - If captured value is `"unknown"` (i.e. `captureClaudeVersion` itself failed): `status="fail"`, stderr line: `e2e-runner: claude --version failed at startup; cannot enforce claude-version.lock`. `installed_version: ""`, `expected_version: <lock>`, `missing_flags: []`.
3. Compare parsed `installed_version` to `expected_version` (lock's `version=`). Strict string equality.
   - Mismatch → `status="fail"`, stderr line: `e2e-runner: claude version <installed> does not match claude-version.lock (<expected>); review and update`. Continue to flag check (so both findings appear in `missing_flags` for the operator).
4. Run `claude --help` once via `exec.CommandContext(ctx, "claude", "--help").CombinedOutput()` (claude prints help on stdout; CombinedOutput is robust to either-channel emission). Honor `ctx` for cancellation/timeout.
   - claude not on PATH / timeout / exit non-zero → `status="fail"`, stderr line: `e2e-runner: claude --help failed: <err>`. `missing_flags: []` (we can't enumerate; the operator sees the help-call failure directly in stderr).
5. For each `flag=X` in the lock file: `strings.Contains(helpOutput, X)`. Collect misses into `missing_flags []string`.
   - Any miss → `status="fail"`, one stderr line per missing flag: `e2e-runner: claude --help no longer mentions <flag>; review claude-version.lock`.
6. If versions match AND no flags missing: `status="pass"`. (Still emit `missing_flags: []` so the schema is uniform across pass/fail.)

`extra` map keys returned by `Run` on every code path:

| key | value | always emitted? |
|---|---|---|
| `installed_version` | parsed version string (e.g. `"2.1.144"`), or `""` if claude --version failed | yes |
| `expected_version` | lock's `version=` value, or `""` if lock file was unreadable | yes |
| `missing_flags` | `[]string` of flag values from the lock file that didn't appear in `claude --help`. Empty `[]` on pass. | yes (empty on pass per AC bullet 3) |

These ride through the existing `marshalResults` flatten — they appear as siblings of `name/status/duration_ms` in the JSON entry. Map-key ordering remains alphabetical (`duration_ms`, `expected_version`, `installed_version`, `missing_flags`, `name`, `status`).

### Helper functions (both pure, both unit-tested)

**`parseClaudeVersion(raw string) string`** — extracts the leading whitespace-separated token from the raw `claude --version` output. `""` in → `""` out. `"2.1.144 (Claude Code)"` → `"2.1.144"`. `"  2.1.144  "` → `"2.1.144"`. Behaviour is "first non-whitespace run"; the developer picks the most idiomatic Go shape (`strings.Fields(...)[0]` with a length guard is one obvious option).

**`parseLockFile(path string) (lockFile, error)`** — opens and parses the lock file. Internal type:

```go
type lockFile struct {
    Version string
    Flags   []string
}
```

Error wraps line number on parse failure. Returns a typed error or `fmt.Errorf` with `at line N: <reason>` — the developer picks the shape. The error message must include the line number so the run-callback can surface it verbatim.

### Test surface

`cmd/e2e-runner/main_test.go` gains two tables following the existing `TestParseSnapshotResults` style:

- **`TestParseClaudeVersion`**: scenarios
  - canonical: `"2.1.144 (Claude Code)"` → `"2.1.144"`
  - whitespace padding: `"  2.1.144  "` → `"2.1.144"`
  - bare token (defensive — future claude might drop the parenthetical): `"2.1.144"` → `"2.1.144"`
  - empty: `""` → `""`
  - whitespace only: `"   "` → `""`

- **`TestParseLockFile`** (uses `t.TempDir()` + `os.WriteFile` per test): scenarios
  - canonical (matches the file shipped with the spec) — `version=2.1.144` + two flags → `{Version:"2.1.144", Flags:["--session-id","--permission-mode bypassPermissions"]}`
  - comments and blank lines tolerated
  - leading/trailing whitespace on keys and values trimmed
  - missing `version=` → parse error
  - two `version=` lines → parse error mentioning line 2
  - unknown key (`color=red`) → parse error mentioning the line
  - empty flag value (`flag=`) → parse error
  - no flags at all (only `version=`) → valid; `Flags == nil` or `Flags == []string{}` (developer picks; test should pin which)

End-to-end coverage of the `Run` callback itself is implicit — the e2e harness exercises it on every `make e2e` invocation against the actual installed claude, and CI will fail on day-zero drift.

### Concurrency model

Unchanged. The new in-process check runs in the same single goroutine as the for-loop body. `exec.CommandContext` for `claude --help` honors the per-check timeout via the parent context. No new goroutines, no channels, no shared state.

### Error handling

Already enumerated under "Run callback body." The dominant invariant from #34 holds: the report ALWAYS emits when feasible — every failure mode here resolves to a `CheckResult{Status: "fail", Extra: {installed_version, expected_version, missing_flags}}` that the existing report machinery serializes correctly.

### Testing strategy

| Layer | What's tested | How |
|---|---|---|
| `parseClaudeVersion` | string extraction across canonical / padded / bare / empty inputs | Table tests |
| `parseLockFile` | file shape, error cases, whitespace handling, line-number error context | Table tests with `t.TempDir()` fixtures |
| `Check.Run` integration | The version-lock check runs first, emits the right fields on pass, short-circuits subsequent checks on fail | Implicit via `make e2e` against the real installed claude (no unit test for the closure body — its branches are dominated by I/O against `claude` and the filesystem; the helpers cover the pure parts) |
| Backwards compat | Subprocess checks still run unchanged (`Check.Run == nil` path in `runCheck`) | Existing TestParseSnapshotResults + a `make e2e` run on a maintainer box |

### Operator workflow (informational; the docs ticket #37 owns the runbook)

```
$ make e2e
e2e-runner: claude_version="2.1.144 (Claude Code)"
e2e-runner: running claude-version-lock
e2e-runner: claude-version-lock -> pass (38ms)
e2e-runner: running spike-one-turn
...
```

On drift:

```
$ make e2e
e2e-runner: claude_version="2.1.145 (Claude Code)"
e2e-runner: running claude-version-lock
e2e-runner: claude version 2.1.145 does not match claude-version.lock (2.1.144); review and update
e2e-runner: claude-version-lock -> fail (42ms)
e2e-runner: wrote ./e2e-report.json
$ echo $?
1
```

Maintainer inspects the diff in claude's release notes, updates `claude-version.lock`, re-runs.

## Concurrency model

Single goroutine. The version-lock `Run` callback executes inline in the runner's main loop. `claude --help` is a normal `exec.CommandContext` subprocess; SIGKILL on context cancel. No fan-out.

## Error handling

All error paths produce `status="fail"` with as-complete-as-possible `extra` fields plus a one-line stderr message keyed off the failure category (missing lock file, parse error, claude --version failed, version mismatch, --help failed, flag missing). See § "Run callback body" for the exhaustive mapping.

## Testing strategy

Table tests for the two pure helpers; e2e harness itself for integration. The check's branches that depend on the filesystem and the `claude` binary are not unit-tested in isolation — they're exercised on every `make e2e` against the real installation, which is the only environment where their failure modes have any meaning.

## Open questions

- **Pre-decided: lock file location.** Repo root, sibling of `Makefile`. Alternative was `cmd/e2e-runner/claude-version.lock` (coloc with the consumer); rejected — the lock file is a top-level repo contract (the maintainer edits it during claude upgrades, not when poking at the runner). Top-level placement makes it visible in `git status` after an upgrade and in `ls` for first-time contributors.

- **Pre-decided: lock file format.** Plain key=value with `#` comments. Alternatives considered: JSON (rejected — hand-edit-hostile, schema-bracket noise), TOML (rejected — second-system effect for a 6-line file), bare version-then-flags-listed (rejected — no key/value disambiguation, no extensibility if a future field is needed). The chosen format absorbs future keys (e.g. a hypothetical `min-version=` or `tested-on=`) by extending the allowed key set in `parseLockFile` — one switch arm.

- **Pre-decided: in-process over helper binary.** See § "Why not a helper binary" above. The helper-binary precedent (#35 snapshot-drift) doesn't generalize — that check has structural reasons to be its own binary (3 spike orchestration, byte-comparison) that this one lacks.

- **Pre-decided: short-circuit semantics.** A `skipRest` local in `main` widens the existing wall-budget arm by one clause; un-run checks are reported as `status="timeout"`, `duration_ms=0`. No new status value, no new Report field. The trade-off is documented at #34 lessons (`status="timeout"` already overloads "timed out" vs "wall budget"); adding a third overload ("short-circuited by drift") is acceptable — the surrounding `claude-version-lock` `fail` entry disambiguates.

- **Pre-decided: parseClaudeVersion is the first whitespace token.** Alternatives: regex over `^[0-9]+\.[0-9]+\.[0-9]+`, or full semver parsing. Rejected — both are overengineering for a check that already does exact string equality. If claude ever prints `claude 2.1.144 (Claude Code)` (verb-prefixed), this rule fails and we add a second `strings.Fields` index lookup. Cross that bridge when claude does.

- **Out of scope: lock-file generation tooling.** The follow-up docs ticket (#37) owns "how the maintainer updates the lock file." This spec does not ship a `make update-claude-lock` target or similar. Manual edit + re-run is the workflow.

- **Out of scope: feature-doc update.** The architect doesn't edit `docs/knowledge/features/e2e-harness.md`. The documentation phase appends a "Check kinds" entry for `version-lock` and a new top section on the lock file after this ticket merges. The existing `# Out of scope (today)` line in that doc explicitly anticipates this ticket and will need updating to remove the reference.

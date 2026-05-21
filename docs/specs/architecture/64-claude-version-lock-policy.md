# Spec 64 — e2e: drop version pin from `claude-version-lock`; keep flag + value presence checks

Ticket: https://github.com/pyrycode/tui-driver/issues/64
Status: ready for developer
Predecessor spec: [`36-claude-version-lock.md`](./36-claude-version-lock.md) — this spec narrows that one's pin semantics; the in-process plumbing, report fields, lock-file format, and short-circuit behaviour all carry forward unchanged.

## Files to read first

- `cmd/e2e-runner/main.go:401-470` — `runClaudeVersionLockCheck`. The version-equality block (the `installed != lf.Version` branch at lines 435-439 and the stderr it emits) is removed by this spec; everything else in the function stays.
- `cmd/e2e-runner/main.go:334-399` — `lockFile` struct + `parseLockFile`. **Unchanged.** The `version=` line stays required at parse time (one-source-of-truth for "what version was the lock last verified against"); only its runtime *meaning* shifts from "must equal installed" to "informational annotation."
- `cmd/e2e-runner/main.go:323-332` — `parseClaudeVersion`. **Unchanged.** Still parses the leading whitespace-separated token.
- `cmd/e2e-runner/main_test.go:114-244` — existing `TestParseLockFile` + `TestParseLockFile_MissingFile`. The new `TestEvaluateClaudeVersionLock` block follows the same `t.TempDir()` + table-driven shape; no edits to the existing tables.
- `cmd/e2e-runner/main_test.go:92-112` — `TestParseClaudeVersion`. **Unchanged.**
- `docs/knowledge/features/e2e-harness.md:99` — the `version-lock` bullet under "Check kinds". The narrative description currently says "asserts strict string equality against the lock's `version=` value"; the documentation phase will rewrite it. The architect does NOT touch this file (see § Out of scope).
- `docs/knowledge/features/e2e-harness.md:165-183` — "How it handles failure" table. The "claude version drifted from lock" row will be removed by the documentation phase; preserved here for context so the spec's narrative matches the failure-mode set that survives.
- `docs/knowledge/codebase/47.md` § "Lessons learned", third bullet — the "lock bumps cascade into sibling-fixture failures" lesson. After this spec lands, the lesson's *force* is unchanged: `snapshot-drift` (the canonical TUI-byte-stream check) still cascades when claude's render shape moves under it. The fixture-coupling guarantee was always owned by `snapshot-drift`, not by `claude-version-lock`'s equality check; dropping the equality assertion does not weaken that guarantee, only removes a spurious failure surface that was masquerading as one.
- `docs/knowledge/codebase/57.md` § "Lessons learned", second bullet — the most recent victim. The convenience-bump anti-pattern still applies: `version=` edits remain deliberate (CI cache key, fixture re-record coupling), even though they no longer block unrelated tickets at the e2e gate.
- `claude-version.lock` — current contents. Plain key=value with one `version=` line, four `flag=` lines, one `value=` line. No edits required by this spec.
- `.github/workflows/ci.yml` — confirms `make e2e` does NOT run in GitHub-hosted CI today (decision recorded 2026-05-19; pure-go checks only). The only consumer of the `claude-version-lock` check is `make e2e` invoked locally by the operator and by the code-review agent in the dispatcher pipeline. This rules out a "CI install pin" coupling that earlier reading of the feature doc (lines 53-93) suggests — those lines describe a retired workflow and are stale (a separate ticket).

## Context

`claude-version-lock` today does two distinct things:

1. **Equality assertion** — `parseClaudeVersion(claude --version)` MUST string-equal `lockFile.Version`.
2. **Flag + value presence** — every `flag=X` and `value=X` in the lock file MUST appear as a substring of `claude --help`.

Both must pass for the check to be green. Per the dispatcher's quality-gate contract, a red e2e gate FAILs the ticket regardless of per-diff verdict.

The first assertion has two failure modes:

- **Legitimate FAIL:** the operator bumped the lock without re-recording snapshot fixtures (#47, #57 lessons). The bump is the discipline-violation; the lock-version mismatch is a proxy for it.
- **Spurious FAIL:** claude auto-updated on the reviewer-side machine. The reviewer can't NOT update (Anthropic ships fixes the reviewer wants on the human-side claude they drive interactively). The bytes-on-disk contract between tui-driver and claude is unchanged; the lock is just stale by one patch.

Today's check cannot distinguish these. Both produce the same `claude version 2.1.X does not match claude-version.lock (2.1.Y)` stderr line and the same `status="fail"`. Concrete recent cost: #57 (EncodeCwd `realpath` canonicalisation) burned 4 code-review cycles, every per-diff verdict PASS, every FAIL was the spurious case. Resolution required operator override.

The second assertion (flag + value presence) is the *substantive* contract: tui-driver's empirical dependency on claude is "this binary still accepts `--session-id`, `--permission-mode`, `--model`, `--effort`, and the value `bypassPermissions`." That assertion cleanly captures the API-surface contract; it FAILs only when a real upstream rename/removal happens.

The version-equality assertion adds **no signal** beyond the flag-presence check — it's a proxy for "the flags are where we expect," but the flags themselves are already checked directly. Dropping it closes the spurious-FAIL class entirely without weakening the substantive check.

The ticket lays out three options (A / B / C). This spec picks **Option A** (drop the version-equality assertion entirely; keep flag + value presence). The rationale is in § Design § "Trade-offs vs Options B and C".

## Design

### Behavioural change

Inside `runClaudeVersionLockCheck`:

- The version-equality comparison (`if installed != lf.Version { status = "fail"; emit stderr line }`) is **removed**.
- The `installed_version` and `expected_version` fields continue to appear in the report's `extra` map. Their semantic shifts from "if these differ, that's the FAIL cause" to "if these differ, that's metadata — the operator sees the drift but isn't blocked by it." This preserves visibility into "what version was last verified against" without making it a gate.
- The flag-presence loop and value-presence loop are **unchanged**. Any missing flag or value still FAILs the check with the same per-line stderr message and the same `missing_flags` / `missing_values` extra fields.
- All other paths (`lockFile missing`, `parse error`, `capturedVersion == "unknown"`, `claude --help` exec failure) are **unchanged**.

The check still runs first and still short-circuits subsequent checks on a non-pass status — that machinery lives in `main()` (the `skipRest := r.Status != "pass"` clause at main.go:181-183) and is independent of the equality assertion.

### Lock file format

**Unchanged.** `claude-version.lock` keeps its current shape:

```
# claude-version.lock — pinned claude binary contract for the e2e harness.
# Update deliberately when bumping the installed claude; the docs ticket
# (#37) covers the workflow.

version=2.1.144

flag=--session-id
flag=--permission-mode
value=bypassPermissions
flag=--model
flag=--effort
```

`parseLockFile` still requires exactly one `version=` line. The field's role becomes purely informational ("this lock was last reviewed against claude 2.1.144"). Keeping `version=` required (rather than making it optional or dropping it) is the smallest delta to the parser; it also preserves a single grep target for "what version was this lock last verified against?", which matters for the snapshot-drift re-record discipline (#47, #57): when a maintainer re-records fixtures, they'll want a record of which claude version those fixtures were captured against. The lock's `version=` line continues to be that record.

The architect MAY update the lock file's leading comment block to clarify the new semantics. The developer SHOULD update the leading comment to read approximately:

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
```

The exact wording is the developer's call; the discriminating content is "version= is informational; flag=/value= are enforced."

### `evaluateClaudeVersionLock` helper (new, pure, unit-tested)

The AC requires a mechanical test for "patch drift above lock + flags present → PASS". Today's `runClaudeVersionLockCheck` is partly testable already (lock-file I/O is faked via `t.TempDir()`), but the `claude --help` exec is not. The cleanest way to make the new AC mechanically testable is to extract a pure helper that takes the help output as a string:

```go
// evaluateClaudeVersionLock applies the lock policy to a parsed lockFile,
// the parsed installed-version token, and a captured `claude --help`
// output. Returns the check's status ("pass" or "fail") and the extra
// map populated with installed_version, expected_version, missing_flags,
// missing_values (always present; empty slices on pass).
//
// Side-effecting concerns (reading the lock file, exec'ing claude --help,
// emitting stderr lines for the operator) live in the outer
// runClaudeVersionLockCheck wrapper and are NOT exercised by this helper.
func evaluateClaudeVersionLock(lf lockFile, installedVersion, helpOut string) (status string, extra map[string]any)
```

**Behaviour:**

- Initialise `extra` with `installed_version = installedVersion`, `expected_version = lf.Version`, `missing_flags = []string{}`, `missing_values = []string{}`.
- For each `f` in `lf.Flags`: if `!strings.Contains(helpOut, f)`, append to `missing_flags`.
- For each `v` in `lf.Values`: if `!strings.Contains(helpOut, v)`, append to `missing_values`.
- If either slice is non-empty → `status = "fail"`. Otherwise → `status = "pass"`.
- The version comparison is **not** performed here.

**`runClaudeVersionLockCheck` after the change** (sketch — developer writes the actual body in the project's idiom):

1. `parseLockFile(lockPath)` — same as today, including error/exists branches that emit the existing stderr lines and return early with `status="fail"`.
2. If `capturedVersion == "unknown"`: same stderr line, same early `fail` return (unchanged).
3. `installed := parseClaudeVersion(capturedVersion)`.
4. `helpOut, err := exec.CommandContext(ctx, "claude", "--help").CombinedOutput()` — same as today. On error, same stderr line and early `fail` return (unchanged).
5. `status, extra := evaluateClaudeVersionLock(lf, installed, string(helpOut))`.
6. For each missing flag / missing value, emit the same per-line stderr message as today (`claude --help no longer mentions flag <flag>; review <lockPath>` / analogous for values). The stderr emission stays in the outer wrapper because the helper is pure; the helper returns the lists, the wrapper iterates them and emits.
7. Return `(status, extra)`.

The wrapper's signature is **unchanged** (`func(ctx context.Context, capturedVersion, lockPath string) (string, map[string]any)`); the closure built in `main` still calls it the same way.

### Stderr surface changes

Today's `claude version 2.1.X does not match claude-version.lock (2.1.Y); review and update` line is **removed** — it has no place when the policy no longer treats patch drift as a failure. No replacement message: the operator can still see `installed_version` ≠ `expected_version` in the JSON report if curiosity strikes; emitting a noisy "drift detected, but it's fine" line on every patch-newer run would just train operators to ignore stderr from this check.

All other stderr lines (`lockPath not found`, `lockPath parse error %v`, `claude --version failed at startup`, `claude --help failed: %v`, the per-missing-flag and per-missing-value lines) are **unchanged**.

### Report schema changes

**None.** Every field listed in the existing `extra` map (`installed_version`, `expected_version`, `missing_flags`, `missing_values`) continues to appear, on every code path, with the same types. CI consumers that depend on the schema are unaffected.

The pass condition has narrowed (no longer requires version equality), so a previously-`fail` run with mismatched versions but stable flags is now `pass`. Consumers of `checks[*].status` who treated `fail` as "something needs attention" will see one fewer cause for that signal; they MAY now check `installed_version != expected_version` themselves if they want a softer drift indicator (this is the spec's recommended path for a future "patch drift annotation" reporter, but is out of scope here).

### Trade-offs vs Options B and C

**Option B (version-floor).** Requires a real version comparator: lexicographic comparison fails on `2.1.9` vs `2.1.10` and on cross-major rollovers (`2.1.999` vs `2.2.0`). A correct implementation needs either semver parsing (vendor a library — overkill) or per-component `strconv.Atoi` plus a comparator (~25 LOC + tests). This trades simplicity for a marginally tighter check that the flag-presence path already covers in the cases that matter (the only way a `>= 2.1.144` check FAILs but the flag-presence check passes is when claude has been downgraded — a scenario no reviewer encounters in practice, since `npm install -g @anthropic-ai/claude-code` only moves forward). The two-failure-mode confusion that bit #57 (legitimate vs spurious) is not resolved by Option B — a downgrade below the floor would still produce a "spurious-looking" FAIL on a green-diff PR. Option A closes the class entirely; Option B narrows it.

**Option C (auto-bump-and-rerecord).** Largest blast radius: new Makefile target, new rerecord workflow, decision rule for "which machine's claude is authoritative" (the spec for that is a separate ticket). The ticket body warns explicitly that Option C "may push past S" and instructs the architect to route back to PO with a split proposal if scoped at >100 LoC. Without a forcing function (a known regression that needed atomic rerecord and got blocked because the bump+rerecord wasn't atomic), this is over-engineering. The version-pin pain that triggered #64 is solved completely by Option A; Option C would solve a related-but-different problem (rerecord ergonomics) that hasn't been observed as a bottleneck.

**Option A.** Smallest delta. Eliminates the spurious-FAIL class completely. Preserves the substantive contract (flag + value presence). Costs zero new code paths and one removed branch. Picked.

### Files touched

```
cmd/e2e-runner/main.go           # MODIFIED: drop version-equality branch; extract evaluateClaudeVersionLock helper
cmd/e2e-runner/main_test.go      # MODIFIED: add TestEvaluateClaudeVersionLock table
claude-version.lock              # MODIFIED: update leading comment block (developer-discretion wording per § Lock file format)
```

Production source files (per the architect-stage self-check rule): **1** (`cmd/e2e-runner/main.go`). Plus 1 test file, plus the data file. Well under the 5-file split threshold; well under 100 LOC production-code budget.

### Test surface

`cmd/e2e-runner/main_test.go` gains one new table test:

**`TestEvaluateClaudeVersionLock`** — table-driven. Each row supplies a `lockFile` literal, an `installedVersion` string, and a `helpOut` string; asserts `(status, extra)`. Scenarios:

| Scenario | Lock version | Installed | Help contains | Expected status | Notes |
|---|---|---|---|---|---|
| canonical match | `2.1.144` | `2.1.144` | every flag + value | `pass` | baseline; ensures the refactor didn't regress |
| **patch drift above lock — AC bullet 1** | `2.1.144` | `2.1.145` | every flag + value | `pass` | the new behaviour |
| patch drift below lock | `2.1.144` | `2.1.143` | every flag + value | `pass` | symmetric; below-lock is no longer a FAIL either (drift in either direction is informational only) |
| minor drift above lock | `2.1.144` | `2.2.0` | every flag + value | `pass` | same policy applies across minor / major drift |
| **missing flag — AC bullet 2** | `2.1.144` | `2.1.144` | help with `--session-id` removed | `fail` | `missing_flags == ["--session-id"]`; `missing_values == []` |
| **missing value — AC bullet 2** | `2.1.144` | `2.1.144` | help with `bypassPermissions` removed | `fail` | `missing_values == ["bypassPermissions"]`; `missing_flags == []` |
| both missing | `2.1.144` | `2.1.144` | help missing one flag AND one value | `fail` | both lists non-empty; status still single `fail` |
| empty flags + empty values | `2.1.144` | `2.1.144` | any string | `pass` | defensive — a lock with `version=` only must still pass; mirrors `TestParseLockFile`'s "version only, no flags" row |
| drift above + missing flag | `2.1.144` | `2.1.145` | help with `--session-id` removed | `fail` | flag-presence is the dominant signal; the drift alone does not cause fail, but it does not mask a real flag-presence failure either |

For each row, the test additionally asserts that `extra["installed_version"]`, `extra["expected_version"]`, `extra["missing_flags"]`, `extra["missing_values"]` are all present with the expected types — preserving the report-schema invariant.

The existing `TestParseLockFile`, `TestParseLockFile_MissingFile`, `TestParseClaudeVersion`, and `TestParseSnapshotResults` tables are **unchanged**. No behaviour they exercise is modified by this spec.

End-to-end coverage of the `Run` callback's I/O paths (lockfile missing, parse error, captured version "unknown", `claude --help` failure) is implicit via `make e2e` against the real installed claude — same as today.

### Concurrency model

**Unchanged.** Same single-goroutine path through `main`'s for-loop; the `Run` callback runs inline; `exec.CommandContext` for `claude --help` honors the parent context.

### Error handling

**Unchanged for every path EXCEPT the version-equality path** (which is removed). The full surviving failure-mode set:

| Cause | Status | Stderr | Extra fields |
|---|---|---|---|
| lock file missing | `fail` | `<lockPath> not found; required for claude-version-lock check` | `installed_version` populated, `expected_version=""`, `missing_flags=[]`, `missing_values=[]` |
| lock file parse error | `fail` | `<lockPath> parse error <err>` | same as above |
| `claude --version` failed | `fail` | `claude --version failed at startup; cannot enforce claude-version.lock` | `installed_version=""`, `expected_version=lf.Version`, `missing_flags=[]`, `missing_values=[]` |
| `claude --help` failed | `fail` | `claude --help failed: <err>` | populated `expected_version` + `installed_version`, empty `missing_flags` / `missing_values` |
| any flag in `lf.Flags` missing from help | `fail` | one line per missing flag: `claude --help no longer mentions flag <flag>; review <lockPath>` | `missing_flags` non-empty |
| any value in `lf.Values` missing from help | `fail` | one line per missing value: `claude --help no longer mentions value <value>; review <lockPath>` | `missing_values` non-empty |
| flags + values all present | `pass` | (none) | `missing_flags=[]`, `missing_values=[]` |

The dominant invariant from #34 holds: report ALWAYS emits when feasible.

### Operator workflow

On a green run with patch drift (the new behaviour the ticket exists to ship):

```
$ make e2e
e2e-runner: claude_version="2.1.146 (Claude Code)"
e2e-runner: running claude-version-lock
e2e-runner: claude-version-lock -> pass (43ms)
e2e-runner: running spike-one-turn
...
```

The drift is silently tolerated. The operator who runs `jq '.checks[0]' e2e-report.json` sees `"installed_version": "2.1.146", "expected_version": "2.1.144"` and can choose, on their own schedule, whether to bump the lock and re-record fixtures.

On a real flag-drift FAIL (the substantive contract still firing):

```
$ make e2e
e2e-runner: claude_version="2.2.0 (Claude Code)"
e2e-runner: running claude-version-lock
e2e-runner: claude --help no longer mentions flag --session-id; review ./claude-version.lock
e2e-runner: claude-version-lock -> fail (78ms)
e2e-runner: wrote ./e2e-report.json
$ echo $?
1
```

Same shape as today's flag-drift FAIL. Subsequent checks short-circuit with `status="timeout"`, `duration_ms=0` — unchanged.

## Open questions

- **Pre-decided: keep `version=` required in the lock file format.** Alternative: relax `parseLockFile` to make `version=` optional (or remove it from the file entirely). Rejected — preserving the field as a required annotation costs zero LOC in the parser (`parseLockFile` is unchanged) and gives the snapshot-drift rerecord discipline a single grep target for "what claude was this lock last reviewed against." If a future ticket wants the field gone, it's a one-line `parseLockFile` change at that point.
- **Pre-decided: `expected_version` continues to appear in the report.** Alternative: drop it from `extra` since it no longer gates anything. Rejected — schema stability for CI consumers (and operator visibility into drift, which is real signal even when not gating) outweighs the trivial saving. The field's role shifts from "the value that must match" to "the value the maintainer last reviewed against." Same value, different role.
- **Pre-decided: no `info:` stderr line on patch drift.** Alternative: emit `claude version 2.1.X exceeds lock (2.1.144); no action required` on every patch-newer run. Rejected — operators will tune that out within two runs (same fate as any "warning that's normally fine" log line), and the JSON report already carries the drift information for anyone who cares.
- **Pre-decided: no version-floor check (Option B).** Rationale: § "Trade-offs vs Options B and C". If a future regression shows downgrades happening in the wild, revisit; that scenario hasn't been observed.
- **Pre-decided: no rerecord tooling in this ticket (Option C).** Rationale: § "Trade-offs vs Options B and C". The ticket body explicitly carves out Option C as "may push past S" — out of scope here.
- **Out of scope: feature-doc update.** The architect doesn't edit `docs/knowledge/features/e2e-harness.md`. The documentation phase updates the `version-lock` bullet under "Check kinds" (currently line 99 — drop "asserts strict string equality against the lock's `version=` value"; replace with "ignores `version=` at runtime — that field is informational only"), the "How it handles failure" table (lines 165-183 — remove the "claude version drifted from lock" row), the report-schema commentary around `expected_version`'s role, and the `claude-version.lock` § "Format rules" / "Updating the lock file" section. The developer's job is to make the code match this spec; the documentation phase encodes the policy in the feature doc afterwards.
- **Out of scope: re-evaluate the stale CI section** in `docs/knowledge/features/e2e-harness.md` (lines 53-93). Those lines describe a retired GitHub Actions workflow (`make e2e` no longer runs in GitHub-hosted CI per `.github/workflows/ci.yml`; pure-go checks only). The staleness pre-dates this ticket and is a separate documentation cleanup.
- **Out of scope: lock-bump tooling, fixture rerecord tooling, atomic bump+rerecord workflow.** Option C territory.
- **Out of scope: `snapshot-drift` behaviour.** AC bullet 3 is satisfied trivially — this spec touches no snapshot-drift code, no committed fixture, no `cmd/e2e-snapshot-check/` file. The TUI-byte-stream fixture-coupling guarantee was always owned by `snapshot-drift`, and remains so.

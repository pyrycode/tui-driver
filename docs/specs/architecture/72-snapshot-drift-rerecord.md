# Spec: e2e check `snapshot-drift` — re-record fixtures under runner env

Ticket: [#72](https://github.com/pyrycode/tui-driver/issues/72). Branch: `feature/72`.

This spec accompanies the corrected-premise ticket body. The issue body is authoritative for the *what*; this spec is the *how* the developer should sequence and verify the change. Read the issue body first.

## Files to read first

The work is mechanical — the developer's reading budget should be small and targeted.

- **Issue body of #72** — authoritative call-chain reconstruction and root-cause assignment. Don't re-derive.
- `docs/specs/architecture/35-snapshot-drift.md:258-273` — Open Question 1's resolution block. **Re-record commands live here verbatim**; do not duplicate them. The deferred follow-up is exactly this ticket.
- `cmd/e2e-runner/main.go:281-288, 518` — confirms `TUIDRIVER_STRICT_MCP_CONFIG=1` is set on every child env when `snapshot-drift` runs. The re-record must match.
- `pkg/tuidriver/pty.go:62-117` — `EnsureClaudeEnv`. Shows the exact env-var → flag mapping (`TUIDRIVER_STRICT_MCP_CONFIG`/`_CLAUDE_MODEL`/`_CLAUDE_EFFORT` → `--strict-mcp-config`/`--model`/`--effort`). This is what the re-record env must reproduce.
- `cmd/e2e-snapshot-check/main.go:50-66, 86-115` — the check's fixture table (trigger keystrokes + settle windows for picker / mcp / agents) and the byte-compare. Re-record uses the **same** triggers and settle.
- `cmd/spike-multiselect/main.go:99-100, 228-235` — `exec.Command("claude")` + `EnsureClaudeEnv`, and the `/tmp/spike-multiselect-bytes-<ns>.bin` dump emission. This is the capture path.
- `claude-version.lock` — six effective lines; `version=` is the only line that changes.
- `Makefile` (top ~40 lines) — for the optional `rerecord-snapshots` recipe.

You do not need to read `pkg/tuidriver/{parsers,modal,strip}.go` or any spike binary other than `spike-multiselect`. The check is renderer-driven, not parser-driven (see issue body's premise correction).

## Context

`make e2e`'s `snapshot-drift` check has failed every run on the dispatcher host since #64 removed the `claude-version-lock` short-circuit. The check captures live `claude` PTY bytes via `spike-multiselect` and byte-compares them against three committed fixtures last touched in `9fae70d` (#21, 2026-04-25). Three intentional upstream changes have shifted what `claude` renders into those bytes:

1. `TUIDRIVER_STRICT_MCP_CONFIG=1` plumbing landed in #34 (`e9d338a`) — adds `--strict-mcp-config`, mutes MCP server lines in `/mcp`. Spec 35 OQ1 explicitly predicted this exact drift and deferred re-record.
2. `TUIDRIVER_CLAUDE_MODEL` / `_EFFORT` env-var seam landed in #48 (`c91259b`) — when the host runs `make e2e MODEL=… EFFORT=…`, the picker header shifts.
3. Claude `2.1.144` → `2.1.148` on the host. The lockfile (`claude-version.lock:5-6`) documents version-bump-coupled fixture re-record as the prescribed workflow.

The check is doing exactly what it was designed to do: report drift. The fixtures must be re-recorded under the runner's actual env, and `claude-version.lock`'s informational `version=` bumped in lockstep.

## Design

The work is procedural, not architectural. The design is the sequence of steps and the safety net.

### Step 1 — Establish the re-record env

Before any capture, determine what env the dispatcher host's `make e2e` invocation actually sets. Inspect: is `MODEL=…`/`EFFORT=…` being passed? (Check the dispatcher config or the most recent `make e2e` invocation's environment if accessible. If not accessible, capture under the no-MODEL/no-EFFORT baseline — see Open Questions.) The re-record env MUST be a superset of what the runner exports onto child processes:

| Runner-side                          | Required in re-record env when…                                  |
|--------------------------------------|------------------------------------------------------------------|
| `TUIDRIVER_STRICT_MCP_CONFIG=1`      | Always (set unconditionally in `main.go:518`).                   |
| `TUIDRIVER_CLAUDE_MODEL=<v>`         | Only if host runs `make e2e MODEL=<v>` (Makefile conditional).   |
| `TUIDRIVER_CLAUDE_EFFORT=<v>`        | Only if host runs `make e2e EFFORT=<v>`.                         |

`EnsureClaudeEnv` reads these from the child process env directly (`os.Getenv`), so `export VAR=…` before invoking `spike-multiselect` is sufficient — no flag-passing required.

### Step 2 — Capture one fixture, byte-diff against the committed one

Do **not** re-record all three at once. Capture exactly one (`picker` is the simplest — short settle, simple trigger) using the procedure in spec 35 OQ1 (`docs/specs/architecture/35-snapshot-drift.md:262-267`). Then:

- Run `xxd <new-dump-path> > /tmp/new.xxd` and `xxd pkg/tuidriver/testdata/picker-snapshot.bin > /tmp/old.xxd`.
- `diff /tmp/old.xxd /tmp/new.xxd | head -100` (the full diff is large; first ~100 lines tell the story).
- Read the diff. The drift signature **looks like renderer drift** when changes are:
  - ANSI escape-sequence shifts (`\x1b[38;5;…m` color codes, cursor moves) — claude's rendering library changed colors / layout.
  - String literal swaps that match a known cause: model-name strings (`opus` ↔ `sonnet` ↔ `haiku`), absence of MCP server names (strict-mcp suppression), version strings in the header.
  - Box-drawing reshuffles (`╭╮╰╯│─`) within an otherwise-identical structure.
- The drift signature **looks like a parser regression** if (these are the red flags):
  - The committed bytes appear *inside* the new dump, but extra junk is prepended/appended that shouldn't be there (suggests `rb.Snapshot()` is returning more than the picker frame).
  - The new dump is structurally different — missing the picker frame entirely, or showing an error message in claude's output, or showing the trust-folder modal.
  - Length is wildly off (committed ~1–10 KB; new dump < 500 B or > 100 KB).

**If the diff looks like a parser regression** (any red flag above, or you can't tell with confidence): STOP. Do not re-record the other two. Add a comment to #72 with the xxd diff excerpt and route the ticket back to PO (`needs-rework:po`) with the framing: *"Drift on `picker` is not consistent with renderer drift — see diff excerpt below. A parser fix becomes its own ticket; this ticket becomes 're-record under unchanged parser' once that lands."* This is the safety net required by AC #3 and the ticket's "STOP and route back" instruction in § Size Estimate.

**If the diff looks like renderer drift**: capture the diff signature in 1–2 lines for the PR description (e.g. *"Header line shifted from `Model: claude-3-7-sonnet-20250219` to `Model: claude-opus-4-7`; MCP server lines absent (strict-mcp suppression); colour codes unchanged."*), then proceed to step 3.

### Step 3 — Re-record all three fixtures

Run the three captures in spec 35 OQ1's recipe (`docs/specs/architecture/35-snapshot-drift.md:262-267`), with the re-record env from step 1 exported. Copy each dump into place:

```
pkg/tuidriver/testdata/picker-snapshot.bin
pkg/tuidriver/testdata/mcp-snapshot.bin
pkg/tuidriver/testdata/agents-snapshot.bin
```

Do not edit the bytes. Do not strip or normalize. The check is a literal `bytes.Equal` (`cmd/e2e-snapshot-check/main.go:110`); whatever `rb.Snapshot()` produced is what gets committed.

### Step 4 — Bump `claude-version.lock`

Read `claude --version` on the host (the actual command, not a memory). Strip the leading version token (e.g. `2.1.148 (Claude Code)` → `2.1.148`) and replace the single line at `claude-version.lock:11`:

```
version=2.1.144
```
becomes
```
version=<observed-version>
```

Do **not** touch `flag=` or `value=` lines. Those are independent assertions against `claude --help`; the `claude-version-lock` check (spec 36 / spec 64) enforces them by substring. They only change if `claude --help` actually changed shape — which is a separate ticket if so. Confirm post-edit by running just the `claude-version-lock` check:

```
go build -o bin/e2e-runner ./cmd/e2e-runner
./bin/e2e-runner -wall=30s    # runs claude-version-lock first; will short-circuit the rest, which is fine
```

The runner short-circuits on `version-lock` pass since #64 removed the short-circuit-on-fail behaviour? — re-check: `cmd/e2e-runner/main.go:181-183` short-circuits when `version-lock` *fails*, not when it passes. So `version-lock` pass means the run continues. That's fine; you can `Ctrl-C` once you see `claude-version-lock -> pass` on stderr.

### Step 5 — Full e2e

Run `make e2e` end-to-end. Expected: all three `SNAPSHOT <name> match` lines, plus every other check pass (no regressions). If any other check fails, that is out-of-scope for this ticket — file a follow-up; do **not** absorb it.

### Step 6 (optional) — `make rerecord-snapshots` recipe

The architect's call on the Makefile recipe: **add it**. The cost is ~10 lines of Makefile, the value is that the next renderer-drift cycle (claude `2.1.149` or beyond) will reuse the recipe verbatim instead of re-deriving the env wiring from spec 35. Sketch (developer adapts to the existing Makefile style):

```
rerecord-snapshots: build-bin
	@echo "Re-recording fixtures under runner env (STRICT_MCP_CONFIG=1, plus MODEL/EFFORT if set)."
	@echo "Captures will overwrite pkg/tuidriver/testdata/{picker,mcp,agents}-snapshot.bin."
	# Three captures per spec 35 OQ1. Each invocation writes /tmp/spike-multiselect-bytes-<ns>.bin
	# and stderr-logs the path; the recipe grabs the latest by mtime and moves it into place.
	# … (developer fills in based on Makefile idioms — set STRICT_MCP_CONFIG=1, propagate
	#    MODEL/EFFORT the same way `e2e:` does, run three spike-multiselect invocations,
	#    extract dump path from stderr, copy into pkg/tuidriver/testdata/.)
```

Constraint: **do not** generalize. No `FIXTURE=picker rerecord-one` parametrisation, no `-record` mode on `e2e-snapshot-check`, no shell script extracted to `scripts/`. One recipe in the Makefile, three captures, done. If the recipe ends up >30 lines or grows conditional branches, cut it — leave the procedure in spec 35 OQ1 as the source of truth and ship without the recipe.

If the recipe is added, it must NOT bypass step 2's byte-diff sanity check. The recipe is for the *steady-state* re-record after a maintainer has already inspected one fixture; do not promote it into the developer's first invocation on this ticket.

## Concurrency model

N/A. This is a sequential maintainer workflow: capture one, diff, capture remaining two, edit lockfile, run e2e. No goroutines added.

## Error handling

- **`claude --version` fails on the host**: cannot proceed. The dispatcher host must have a working `claude` for any e2e work. Surface this as a hard error in the PR; do not guess a version.
- **`spike-multiselect` capture times out or returns no `picker-snapshot path=…` line**: that's an upstream spike failure, not a snapshot-drift problem. Out of scope. Stop and file a follow-up.
- **The byte-diff in step 2 is ambiguous** (neither clearly renderer drift nor clearly a parser regression): default to safety — route back to PO per AC #3. The cost of one rework cycle is far less than silently re-recording over a parser bug.

## Testing strategy

The "test" here is the e2e run itself.

- **Acceptance verification**: after step 4, run `make e2e` end-to-end. Required outputs:
  - `e2e-runner: snapshot-drift -> pass`
  - In `e2e-report.json`, the `snapshot-drift` entry has `snapshots: [...]` with all three `result: "match"`.
  - `claude-version-lock -> pass` (regression check for AC #5).
- **No new Go test files.** The check's unit tests (spec 35 § Testing strategy) cover the wire format; they do not cover the fixture content and shouldn't.
- **No `t.Skip` anywhere.** The ticket's first constraint is explicit. Don't add one to mask transient flakes — if a capture is flaky, that's a `spike-multiselect` bug for a separate ticket.

## PR description requirements

AC #7 names two things the PR description must include. Concretely:

1. **Contributors absorbed.** From `git log 9fae70d..HEAD -- cmd/e2e-runner/main.go pkg/tuidriver/pty.go cmd/spike-multiselect/main.go cmd/e2e-snapshot-check/main.go` (run this to confirm; do not rely on this spec's enumeration): the upstream commits that caused the drift to surface. Expected set as of 2026-05-22: `e9d338a` (#34, strict-mcp plumbing), `c91259b` (#48, MODEL/EFFORT seam), plus the implicit `claude 2.1.144 → 2.1.148` host upgrade. Anything else `git log` surfaces — include it.
2. **Re-record env.** Verbatim: which `TUIDRIVER_*` vars were exported during capture, and on which `claude --version`. So the next maintainer can reproduce bit-for-bit.

Also include the byte-diff signature characterisation from step 2 (1–2 lines).

## Open questions

1. **Is the dispatcher host running `make e2e` with `MODEL=…`/`EFFORT=…` set?** If yes, the re-record must use the same values, or the picker fixture will drift again on the next run. The developer should inspect the dispatcher's e2e invocation (or ask the operator) before capturing. If the answer is "no, the host runs bare `make e2e`", capture under the bare env and document that in the PR description — this becomes load-bearing for the next maintainer.

2. **Add the `rerecord-snapshots` Makefile target — yes/no?** The architect recommends yes (see § Step 6 rationale). The developer may decide otherwise if step 6's recipe ends up awkward against the existing Makefile idioms; in that case skip it without further justification needed. Either choice is fine; the AC list does not require the recipe.

3. **Should the lockfile gain a `flag=--strict-mcp-config` line?** Currently `claude-version.lock` does not assert `--strict-mcp-config` is present in `claude --help`, even though the runner depends on it (`pty.go:78-89`). Out of scope for this ticket — flag the gap in the PR description if surfaced, but do not bundle the lockfile expansion. That's a separate ticket if anyone wants it.

## Out of scope

Anything not in the AC list. Specifically:

- Bumping `flag=`/`value=` lines in `claude-version.lock` absent observed change in `claude --help` shape.
- Adding `-record` mode to `e2e-snapshot-check` (spec 35 § 283 explicitly rules this out).
- A general claude-version pinning policy.
- Touching the parsers in `pkg/tuidriver/{picker,mcp,agents}.go`.
- The other live-claude spike checks #69 and #70.
- Fixing the architectural debt around the library JSONL API (#58–#62).

If any of these become *necessary* to make the e2e green (e.g. you discover `claude 2.1.148` changed `--help` shape and broke `flag=`/`value=` matching), STOP and re-scope via PO. Do not bundle.

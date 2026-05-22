# Spec 35 — e2e: snapshot-drift check vs pkg/tuidriver/testdata/*-snapshot.bin

> **Superseded by [Spec 81](81-snapshot-drift-parsed-shape.md)** (#81, 2026-05-22). The
> byte-compare model designed below was retired after #72's evidence that every
> byte-level mitigation surfaces the next layer of renderer volatility. Spec 81
> replaces the byte-compare with a parsed-shape (`ParsePicker` / `ParseMcpStatus` /
> `ParseAgentList`) JSON comparison. Read spec 81 for the canonical design; this
> spec is retained for historical context only.

Ticket: https://github.com/pyrycode/tui-driver/issues/35
Status: superseded by spec 81

## Files to read first

- `cmd/e2e-runner/main.go:48-58` — `Check` struct. Snapshot-drift adds one entry to `buildChecks()` and (this spec) one optional field `OnComplete` to support per-snapshot fields on `pass` as well as `fail`.
- `cmd/e2e-runner/main.go:177-240` — `buildChecks()` slice and the existing `OnFailure` callback pattern used by the probe check. The snapshot-drift entry mirrors this shape.
- `cmd/e2e-runner/main.go:256-300` — `runCheck`. The OnComplete merge point lives here; it must run regardless of status, after the current OnFailure block.
- `cmd/e2e-runner/main.go:305-319` — `marshalResults`. `Extra` is flattened into the per-check JSON object — nothing changes here; the new `snapshots` array rides through the existing flatten.
- `cmd/spike-multiselect/main.go:60-69` — flag surface: `-trigger`, `-trust-folder`, `-settle`. The new check binary invokes spike-multiselect three times via these flags; **no spike-multiselect changes are required**.
- `cmd/spike-multiselect/main.go:226-235` — the `picker-snapshot path=<file>` stderr log line at L235 and the `/tmp/spike-multiselect-bytes-<ns>.bin` dump path at L230. The new check binary scrapes this exact line to find the dump.
- `cmd/spike-multiselect/main.go:381-385` — the `OBSERVED: picker snapshot at <path>` stdout completion line and successful-exit semantics; useful background but the new check binary keys off exit code, not this marker.
- `pkg/tuidriver/testdata/{picker,mcp,agents}-snapshot.bin` — the three committed byte fixtures (raw `rb.Snapshot()` output). Sizes today: picker 4096 B, mcp 4096 B, agents 2236 B. The check reads them via `os.ReadFile` and compares with `bytes.Equal`.
- `pkg/tuidriver/picker_test.go:40`, `pkg/tuidriver/mcp_test.go:20`, `pkg/tuidriver/agents_test.go:30` — confirm the canonical fixture path is `testdata/<name>-snapshot.bin` (relative to the package). The check binary uses `pkg/tuidriver/testdata/` from repo-root cwd.
- `pkg/tuidriver/pty.go` — `EnsureClaudeEnv` and `StrictMcpConfigEnv` (`TUIDRIVER_STRICT_MCP_CONFIG`). Set on the runner side; flows transparently through `e2e-snapshot-check → spike-multiselect → claude`. See § "Open questions" for the fixture-recording implication.
- `Makefile` — `SPIKES`, `PROBES`, `RUNNER`, `ALL_BINS`. The new binary slots in via a new variable so its classification is explicit.
- `docs/specs/architecture/34-e2e-harness-foundation.md` § "Check abstraction" / "runCheck semantics" / "Path scrubbing" — the existing contracts the spec extends. The "snapshot-drift … will each add one struct entry, not a new abstraction" note in #34's "Patterns established" is the design seed for this spec.

## Context

`pkg/tuidriver/testdata/{picker,mcp,agents}-snapshot.bin` are raw byte snapshots of claude's TUI output, consumed by unit tests in `pkg/tuidriver/` (`ParsePicker`, `ParseMcpStatus`, `ParseAgentList`). When claude's UI changes — verb spacing, box-drawing chars, layout — these fixtures stop reflecting reality. Unit tests keep passing against stale bytes, masking real regressions in the library's pattern matchers.

This ticket adds a check to the e2e harness that re-derives each fixture against the currently-installed `claude` and byte-compares to the committed file. The check fails loudly on diff, forcing the maintainer to look — confirm the change is legitimate — and re-record. False positives are a feature: the cost of accidental re-records is much lower than the cost of silently passing tests against stale fixtures.

Two non-obvious facts from reading the existing code that shape the design:

1. **Per-snapshot results must appear on `pass` AND `fail`.** AC bullet 3 explicitly: *"The per-snapshot list appears on both `pass` and `fail` so a CI consumer can always pinpoint which fixture(s) drifted (or confirm none did)."* The existing `Check.OnFailure` callback fires only on non-pass status (`runCheck` L294: `if status != "pass" && c.OnFailure != nil`), so we need a sibling hook that runs unconditionally. We add `OnComplete` — a one-field extension — rather than reshape OnFailure (the probe check at L231-238 relies on OnFailure being fail-only so its `recording_dir` doesn't leak into pass-entries).

2. **Re-derivation does not need a new PTY/claude code path.** `spike-multiselect` already does exactly the capture we need (idle → trust-folder → trigger → settle → snapshot → write `/tmp/spike-multiselect-bytes-<ns>.bin`) and accepts `-trigger` and `-settle` flags to vary per fixture. The new check binary is a thin orchestrator that invokes spike-multiselect three times, scrapes the dump path from its stderr log, reads both the dump and the committed fixture, and `bytes.Equal`s them. No new PTY code, no library import in the runner, no spike-multiselect modifications.

## Design

### Package layout

```
cmd/e2e-snapshot-check/main.go   # NEW: orchestrator binary; ~100 LOC
cmd/e2e-runner/main.go           # MODIFIED: add Check.OnComplete, snapshot-drift entry, stdout parser; ~30 LOC added
Makefile                         # MODIFIED: add `CHECKERS := e2e-snapshot-check`, append to ALL_BINS; ~3 lines
```

No new packages, no new exported library types, no library changes. The runner still does not import `pkg/tuidriver`.

### The `e2e-snapshot-check` binary

Single-file `cmd/e2e-snapshot-check/main.go`. Flags:

```
-bin-dir PATH        Directory containing the built spike-multiselect binary.
                     Default: ./bin (matches e2e-runner's default; runner passes -bin-dir through).
-testdata-dir PATH   Directory containing committed *-snapshot.bin fixtures.
                     Default: ./pkg/tuidriver/testdata.
-spike-timeout DUR   Per-fixture spike-multiselect wall budget. Default: 60s.
```

The runner invokes it with `-bin-dir <inherited>` so the check finds `spike-multiselect` next to itself. `-testdata-dir` defaults to the canonical path; overridable for ad-hoc invocation. `-spike-timeout` lets the maintainer give /mcp's settle window more headroom on slow machines without touching the check timeout.

Fixture table (hardcoded in main.go):

| name | -trigger | -settle | fixture path |
|---|---|---|---|
| `picker` | `/` | (default — omitted from args) | `pkg/tuidriver/testdata/picker-snapshot.bin` |
| `mcp`    | `/mcp\r` (byte 0x0d, not the literal `\r` string) | `5s` | `pkg/tuidriver/testdata/mcp-snapshot.bin` |
| `agents` | `/agents\r` (byte 0x0d) | (default) | `pkg/tuidriver/testdata/agents-snapshot.bin` |

Flow per fixture (sequential — no fan-out):

1. Build args: `["-trust-folder=accept", "-trigger=" + trigger]`. If `settle > 0`, append `-settle=<dur>`.
2. `exec.CommandContext(ctx, filepath.Join(binDir, "spike-multiselect"), args...)`. Capture stderr to a buffer **and** mirror to host stderr via `io.MultiWriter` (operators see spike progress live). Discard stdout (`io.Discard`) — the `OBSERVED:` line carries no information the check needs.
3. `cmd.Run()`. Treat any error (non-zero exit, deadline) as a capture failure → emit `SNAPSHOT <name> diff` and a `drift in <fixture path>: capture failed: <err>` stderr line, set the aggregate-fail flag, continue.
4. Scrape the dump path from buffered stderr with regex `picker-snapshot path=(\S+)` (matches spike-multiselect's L235 log format unchanged across all three trigger variants — the log line says "picker-snapshot" verbatim regardless of which modal was captured). On no match: emit a `diff` for this fixture, log "no dump-path log line in spike-multiselect stderr".
5. `os.ReadFile(dumpPath)` and `os.ReadFile(fixturePath)`. Either error → `diff` + error logged.
6. `bytes.Equal(captured, committed)` →
   - `true`: print `SNAPSHOT <name> match\n` to stdout.
   - `false`: print `SNAPSHOT <name> diff\n` to stdout AND `drift in <fixture path>\n` to stderr.

Exit: `0` iff every fixture matched; `1` if any fixture diffed OR any capture failed.

**Read-only invariant** (AC bullet 2): the binary opens `pkg/tuidriver/testdata/*` with `os.ReadFile` only — no `os.WriteFile`, no `os.OpenFile(..., O_WRONLY|O_TRUNC, ...)` anywhere in the package. The committed fixtures are never side-effected by running e2e. Re-recording remains a manual maintainer step (see § "Open questions").

**Stdout protocol summary**:

```
SNAPSHOT picker match|diff
SNAPSHOT mcp match|diff
SNAPSHOT agents match|diff
```

One line per fixture, in fixture-table order. Parseable by the runner with one regex. No `SUCCESS:` / `OBSERVED:` marker — the binary's success is purely exit-code-driven.

### Runner changes (`cmd/e2e-runner/main.go`)

**(a) Add `OnComplete` to `Check`** — single new field, optional:

```go
// OnComplete fires regardless of status, after OnFailure if both are set.
// Use for fields that must appear on pass entries (e.g. snapshot-drift's
// per-fixture result list).
OnComplete func(stdout, stderr string) map[string]any
```

In `runCheck`, after computing `status` and the existing OnFailure block, append:

```go
if c.OnComplete != nil {
    if extra := c.OnComplete(stdoutBuf.String(), stderrBuf.String()); len(extra) > 0 {
        if result.Extra == nil {
            result.Extra = map[string]any{}
        }
        for k, v := range extra {
            result.Extra[k] = v
        }
    }
}
```

Order: OnFailure first (so per-failure keys can be overwritten by OnComplete if both populate the same key — won't happen in practice, but the precedence is unambiguous). The probe check is unaffected; its OnFailure still fires only on non-pass status as before.

**(b) Add the snapshot-drift entry** to `buildChecks()`. Slot it after the probe entry — it's the slowest check (~30-60s) and goes last so quick fails surface first:

```go
{
    Name:       "snapshot-drift",
    Kind:       "snapshot",                       // informational, mirrors "spike"/"probe"
    Binary:     "e2e-snapshot-check",
    Args:       []string{},                       // bin-dir flows via the binary's own default
    Timeout:    snapshotDriftTimeout,             // new const, 180 * time.Second
    OnComplete: parseSnapshotResults,
    // SuccessMarker: nil — exit code is authoritative (same shape as the probe).
    // OnFailure: nil — `OnComplete` already emits the per-snapshot list on fail.
},
```

Add `snapshotDriftTimeout = 180 * time.Second` next to the existing timeout constants (L31-35). 180s gives headroom for 3 × claude spawns (each ~10-20s) plus /mcp's 5s settle plus margin. Overridable via `-timeout snapshot-drift=DUR` (already wired — `tos.known` populates from `buildChecks()` automatically).

**(c) Add `parseSnapshotResults`** — the OnComplete callback. ~15 LOC:

```go
var snapshotResultRe = regexp.MustCompile(`(?m)^SNAPSHOT (picker|mcp|agents) (match|diff)$`)

func parseSnapshotResults(stdout, _ string) map[string]any {
    matches := snapshotResultRe.FindAllStringSubmatch(stdout, -1)
    if len(matches) == 0 {
        return nil
    }
    snapshots := make([]map[string]any, 0, len(matches))
    for _, m := range matches {
        snapshots = append(snapshots, map[string]any{
            "file":   "pkg/tuidriver/testdata/" + m[1] + "-snapshot.bin",
            "result": m[2],
        })
    }
    return map[string]any{"snapshots": snapshots}
}
```

Output shape in `e2e-report.json`:

```json
{
  "name": "snapshot-drift",
  "status": "pass",
  "duration_ms": 28432,
  "snapshots": [
    {"file": "pkg/tuidriver/testdata/picker-snapshot.bin", "result": "match"},
    {"file": "pkg/tuidriver/testdata/mcp-snapshot.bin",    "result": "match"},
    {"file": "pkg/tuidriver/testdata/agents-snapshot.bin", "result": "match"}
  ]
}
```

On `fail`, same shape; the relevant `result` flips to `"diff"`. The list is always 3 entries when the check produces any output. On a hard runtime failure (e.g. the check binary crashes before printing any `SNAPSHOT` line), `snapshots` is omitted from the report entry (the OnComplete returns nil) — the status is `fail`/`timeout` and the operator inspects host stderr for diagnosis.

**Path-scrubbing compliance** (per #34's invariant): the `snapshots[].file` field uses a fixed, repo-relative path (`pkg/tuidriver/testdata/...`) constructed from the fixture name. No `$HOME`, no `/tmp/spike-multiselect-bytes-<ns>.bin` paths in the report. The `/tmp` dump path is operator-facing diagnostic output (visible via mirrored stderr) and never enters the report.

### Makefile

Add a new variable so the binary's classification is explicit (not a spike, not a probe — it's a check helper):

```makefile
CHECKERS   := e2e-snapshot-check
# ...
ALL_BINS   := $(SPIKES) $(PROBES) $(CHECKERS) $(RUNNER)
```

No other Makefile changes; the existing pattern rule `$(BIN_DIR)/%: ; go build -o $@ ./cmd/$*` handles the new binary uniformly.

### Concurrency model

Sequential everywhere. The runner runs `e2e-snapshot-check` as one subprocess (no change to the runner's concurrency model). Inside `e2e-snapshot-check`, the three spike-multiselect invocations run **serially**, one at a time. Three reasons:

1. **Trust-folder state.** Each spike-multiselect call may race claude's `.claude.json` trust-folder write. Serial runs preserve the `-trust-folder=accept` idempotency assumption.
2. **PTY allocation.** Each spike grabs a PTY master. Serial avoids any platform-specific PTY-table limits.
3. **stderr scraping.** Serial output is trivially attributable per-fixture — no interleaved log lines to demultiplex.

No goroutines in `e2e-snapshot-check` proper; the per-invocation `exec.CommandContext` handles subprocess lifecycle. The spike-multiselect children of course run their own goroutines (PTY reader, watchdog) but that's their problem.

### Error handling

| Failure mode | Behaviour |
|---|---|
| `spike-multiselect` binary missing under `-bin-dir` | `cmd.Run()` returns `exec.ErrNotFound`-shaped error → `diff` for that fixture + stderr log → continue to next fixture → exit 1. |
| `spike-multiselect` exits non-zero (claude crash, idle timeout, etc.) | Same as above. |
| `-spike-timeout` exceeded for one fixture | `ctx.Err() == context.DeadlineExceeded` → `diff` for that fixture → continue. The check's own `-timeout snapshot-drift=DUR` still governs the whole-check budget; per-fixture timeout is a finer-grained safety net inside it. |
| Stderr lacks `picker-snapshot path=...` line | Spike completed but didn't dump. `diff` + log "no dump-path log line in spike-multiselect stderr". |
| Dump file missing or unreadable (e.g. `/tmp` rotation between write and read — extremely unlikely) | `diff` + log read error. |
| Committed fixture missing | `diff` + log read error. (This is an actual misconfiguration — the maintainer deleted a fixture without updating the check.) |
| Whole check timeout via runner's `-timeout snapshot-drift=...` | `runCheck` returns `status="timeout"`; OnComplete still runs and parses whatever `SNAPSHOT` lines were already written to stdout, so the report shows partial results (e.g. picker=match, mcp=match, agents=missing). This is correct: a partial result still beats a black box. |

The dominant invariant: **the check binary always exits cleanly with a real exit code**. No `panic`, no `os.Exit(2)` for unexpected errors — capture failures are degraded-to-diff per fixture, and the aggregate exit code summarizes.

### CLI flags

The check binary's flag surface is intentionally narrow — three flags, no subcommands. Adding `-fixture <name>` to filter to one fixture is tempting for re-recording workflows, but re-recording is a maintainer concern handled via spike-multiselect directly (see § "Open questions"). The check is for e2e drift; mission creep here would erode the AC.

The runner's `-timeout snapshot-drift=DUR` is wired via the existing `tos.known` map in `cmd/e2e-runner/main.go:110-113` — populated from `buildChecks()` names, so adding `snapshot-drift` to the slice auto-registers the flag override. Zero new flag wiring on the runner side.

### Trade-offs considered

- **New `cmd/e2e-snapshot-check/` binary vs. extending `spike-multiselect` with a `-compare=<fixture>` flag.** Picked the new binary. spike-multiselect's contract today is "observe and dump"; bolting on byte-compare semantics would conflate observation with assertion and require touching spike-multiselect's flow control (early-exit on diff, exit-code shape change). The wrapper is ~100 LOC and keeps each binary's role single-purpose.
- **New binary vs. embedding the loop in `cmd/e2e-runner/main.go` directly.** Picked the binary. Embedding would (a) make the runner shell out to spike-multiselect from within a check, which breaks the "one subprocess per Check" pattern #34 codified, (b) couple the runner's concurrency boundaries to per-fixture failure semantics, and (c) bloat `main.go` past the LOC threshold where a single-file orchestrator stops being readable. Subprocess isolation also means a panic in the snapshot-check binary doesn't take down the runner.
- **`OnComplete` callback vs. converting `OnFailure` to fire on both statuses.** Picked the additive `OnComplete`. The probe check (`cmd/e2e-runner/main.go:231-238`) relies on `recording_dir` appearing only on `fail` — converting OnFailure to fire on pass would leak that path into pass entries and violate #34's path-scrubbing rule. Additive extension is safer and the abstraction stays one struct field wider, not deeper.
- **Hex-diff snippet in the report on `diff`.** Considered, rejected. Per #34's path-scrubbing rule the report avoids embedding raw content; a hex snippet would also bloat `e2e-report.json` from ~400 B to several KB on diff. Forensics: the operator inspects `/tmp/spike-multiselect-bytes-<ns>.bin` directly (path visible in mirrored stderr) and `diff <(xxd /tmp/...) <(xxd pkg/tuidriver/testdata/...-snapshot.bin)` by hand.

## Concurrency model

See Design § "Concurrency model". One sentence: the runner spawns one subprocess, that subprocess serially spawns three more, no goroutines anywhere in the check binary, `exec.CommandContext` carries the cancellation chain end-to-end.

## Error handling

See Design § "Error handling". Invariant: **the check binary always exits with a real exit code**, and the runner's OnComplete callback parses whatever `SNAPSHOT` lines appeared in stdout regardless of status — so partial results survive timeouts.

## Testing strategy

1. **Manual integration test** — `make e2e` against an installed `claude` succeeds, and `e2e-report.json` includes the `snapshot-drift` entry with 3 matching `snapshots[]`. Document in the PR description.

2. **Forced-failure smoke** — temporarily corrupt one fixture (`printf 'X' >> pkg/tuidriver/testdata/picker-snapshot.bin`), run `make e2e`, verify:
   - Runner exit code is 1.
   - `snapshot-drift` entry has `status: "fail"`.
   - `snapshots[]` shows picker as `"diff"` and the other two as `"match"`.
   - Host stderr contains `drift in pkg/tuidriver/testdata/picker-snapshot.bin`.
   Restore the fixture afterwards (or revert the local edit).

3. **Unit test for `parseSnapshotResults`** — `cmd/e2e-runner/main_test.go` (new file, ~30 LOC). Test cases as bullets:
   - All three fixtures `match` → returns 3-entry slice with `result: "match"`.
   - Mixed: picker match, mcp diff, agents match → 3-entry slice with the right `result` values, files prefixed `pkg/tuidriver/testdata/`.
   - No `SNAPSHOT` lines in stdout (e.g. check binary crashed early) → returns nil.
   - Extra lines around the `SNAPSHOT` lines (timestamps, log noise) → still parses correctly.
   The function is small and pure; a unit test costs little and pins the wire format that the report consumer relies on.

4. **No unit test for the check binary itself** — it's an integration shim by design. The integration tests above cover its behaviour.

5. **No mocked `claude`.** Same rationale as #34: the whole point of the harness is end-to-end against the real claude.

## Open questions

1. **First-run drift expected on `mcp` fixture.** The committed `pkg/tuidriver/testdata/mcp-snapshot.bin` was captured before #34 landed and therefore without `--strict-mcp-config`. The e2e check sets `TUIDRIVER_STRICT_MCP_CONFIG=1` (inherited via #34's plumbing through `EnsureClaudeEnv`), so claude runs with `--strict-mcp-config` — which suppresses MCP servers entirely. The `/mcp` modal will look different (no servers listed), and the byte-compare will diff on the first run after this PR merges.

   **Resolution:** the maintainer re-records `mcp-snapshot.bin` (and possibly the other two for consistency) with the strict-mcp env set:

   ```bash
   make build-bin
   TUIDRIVER_STRICT_MCP_CONFIG=1 ./bin/spike-multiselect -trust-folder=accept -trigger="$(printf '/mcp\r')" -settle=5s
   cp /tmp/spike-multiselect-bytes-<latest>.bin pkg/tuidriver/testdata/mcp-snapshot.bin
   # Repeat for picker (-trigger='/') and agents (-trigger="$(printf '/agents\r')").
   ```

   This re-record is **out of scope for #35** — the developer ships the check; the maintainer re-records the fixtures in a follow-up PR or as part of merging #35. If we want #35 to ship green on first CI run, the developer can re-record as part of this PR; if we want #35 to demonstrate that the check actually catches drift, the developer leaves the existing fixtures and the failing `snapshot-drift` entry is the proof-of-concept. **Defer to the developer + maintainer**; spec doesn't dictate.

2. **`/mcp` settle window of 5 s.** Picked empirically from the ticket body ("longer `-settle` — `/mcp`'s MCP-server-connecting state resolves over time"). With `--strict-mcp-config` set, the connecting state collapses to "no servers" near-instantly, so 5 s is conservative. If re-recording shows the modal stabilises in <1 s under strict-mcp, the spec can be tightened to `2s` or removed entirely (default 1.5 s). Defer to first-run empirical data.

3. **`-fixture <name>` filter on `e2e-snapshot-check`.** Not added. Re-recording is a maintainer workflow that runs `spike-multiselect` directly (per Open Question 1 above), not the check binary. Adding the flag would invite using the check binary as a recording tool, which conflates the assertion role with the capture role. If the operator workflow grows to need this, add it in a follow-up — easy bolt-on.

4. **Concurrency: should the three spike-multiselect invocations parallelize?** No (see Design § "Concurrency model"). The serial cost is ~30-60 s total wall time, well within the 180 s check budget. Parallelizing would buy maybe 30 s in the green-path case at the cost of (a) PTY races, (b) stderr demuxing, (c) trust-folder write races. Not worth it.

## Out of scope (per ticket)

- Re-recording fixtures with `TUIDRIVER_STRICT_MCP_CONFIG=1` set (see Open Question 1; this is a maintainer follow-up).
- Claude-version-lock check + `claude-version.lock` file.
- Operator documentation in `docs/knowledge/e2e-testing.md` or `docs/knowledge/features/e2e-harness.md`.
- GitHub Actions wiring.
- A `-record` mode on `e2e-snapshot-check` (re-recording stays a maintainer command via spike-multiselect; conflating check + record would erode the read-only AC).

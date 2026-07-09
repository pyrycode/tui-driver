# Spec #252 — Retire the obsolete `/mcp` snapshot-drift case (last surviving fixture)

**Ticket:** [#252](https://github.com/pyrycode/tui-driver/issues/252) · **Size:** S (mechanically XS — deletion-dominated, net-negative LOC, zero new production code) · **Not security-sensitive** (removes an e2e case; does not touch the classifier or any grant/answer path).

**Supersedes** the prior `feature/252` spec `252-mcp-snapshot-reconcile-builtin-computer-use.md` (the re-record path), which this commit deletes. Split from #250; the diagnosis is settled and re-litigated in the ticket body — this spec does **not** re-open it.

**Precedent:** #129 (dropped `/picker`) and #178 (dropped `/agents`) already eroded this harness case-by-case; `mcp` is the sole fixture left, so retiring it retires the whole `snapshot-drift` check. Both prior drops kept full `.bin` unit coverage; this one does too.

---

## Files to read first

- `cmd/e2e-snapshot-check/main.go` (whole file, 199 lines) — the single-fixture check binary; the `fixtures` table at `:71-73` has exactly one entry (`mcp`). **Deleted in full** (with its `main_test.go`).
- `cmd/e2e-runner/main.go:395-402` — the `snapshot-drift` `Check{}` entry (`Binary: "e2e-snapshot-check"`, `OnComplete: parseSnapshotResults`). The removal anchor.
- `cmd/e2e-runner/main.go:49-51` (`snapshotResultRe`), `:406-425` (`parseSnapshotResults`), `:34` (`snapshotDriftTimeout` const) — snapshot-drift-only symbols; all become dead on removal.
- `cmd/e2e-runner/main.go:62-72` (the `OnComplete` **and** `OnFailure` field doc-comments) and `:665-674` (`runCheck`'s `OnComplete` invocation) — **read both to tell them apart.** `OnComplete` has exactly one consumer (snapshot-drift, `:401`) → remove. `OnFailure` is used by the two probes (`:347`, `:387`) → **keep**. See § "Landmine" below.
- `cmd/e2e-runner/main.go:75-78` (`NonGating` field) + `:341-346` / `:381-394` (the two probe entries that set it) — **keep untouched.** The PO note calls this "NonGatingField plumbing that only snapshot-drift uses" — that is a misnomer; `NonGating` is load-bearing for both probes.
- `cmd/e2e-runner/main_test.go:11-68` (`TestParseSnapshotResults`) — the only test of the deleted function; removed with it. The rest of this file (`parseClaudeVersion` / `parseLockFile` / `evaluateClaudeVersionLock` / `gateFailed` tests) is untouched.
- `Makefile:11-17` (`CHECKERS`, `ALL_BINS`), `:33` (`.PHONY`), `:50-54` (`rerecord-snapshots` target) — the build-surface removals.
- `pkg/tuidriver/testdata/mcp-snapshot.json` — the drifted fixture; **deleted.** (It is read **only** by the e2e check via `-record`/compare; no unit test reads it — confirmed: `grep -rn mcp-snapshot.json` hits only `cmd/e2e-runner/main_test.go` string literals and docs.)
- **Retained `.bin` coverage — do not touch:** `pkg/tuidriver/mcp_test.go:19` (`ParseMcpStatus(mcp-snapshot.bin)` → `TotalServers == 10`), `pkg/tuidriver/anchor_forgery_test.go:330` (`DetectModalClass(mcp-snapshot.bin) == ModalClassMCP`, #221 forgery positive control), `pkg/tuidriver/modal_test.go:383` (`TestDetectModalClassMcpEmptyIsIdleNotModal`), `pkg/tuidriver/state_test.go:329` (`IsIdle(mcp-empty-snapshot.bin)`), `pkg/tuidriver/ready_test.go:45` (mcp-empty ready test).

---

## Context

`make e2e`'s `snapshot-drift` check re-derives the `/mcp` render (`spike-multiselect -trigger=/mcp\r` → `ParseMcpStatus` → `.parsed.json` sidecar) and `reflect.DeepEqual`s the parsed shape against the committed `mcp-snapshot.json`. Since #223 (`2411344`) removed `anchorMCPEmptySpaced`, the strict-mcp empty-state (`⎿ No MCP servers configured…`) classifies `ModalClassUnknown` — **deliberately**, and pinned by three retained tests (AC #2). Consequently `spike-multiselect` writes **no** sidecar, so `e2e-snapshot-check` degrades to a missing-sidecar `diff` **regardless of the committed fixture value**. The check has been un-satisfiable on a clean runner since #223.

Two remediations are ruled out by the ticket (do **not** attempt either): a **re-record** (the populated `computer-use` picker is host-dependent and still produces no sidecar in the empty-state), and a **classifier fix** (re-adding empty-state detection reverts #223's correct decision and breaks the three retained tests). `modal.go` **must not be touched.** The only correct remediation is retirement, matching the #129/#178 precedent.

## Design — retire the check and its now-dead scaffolding

Deletion-dominated. No net new production code. The design is a removal manifest; the developer executes it and confirms the two gates (§ Testing strategy). Order the edits so `go build ./...` never sees a dangling reference (delete the runner entry and its helpers together, in one edit pass, before or with the binary deletion).

### Removal manifest

| Surface | Action | Notes |
|---|---|---|
| `pkg/tuidriver/testdata/mcp-snapshot.json` | `git rm` | Sole snapshot-drift fixture. No unit test reads it. |
| `cmd/e2e-snapshot-check/` (dir: `main.go` + `main_test.go`) | `git rm -r` | Single-fixture binary; nothing imports it (a `main` package). |
| `cmd/e2e-runner/main.go` — `snapshot-drift` `Check{}` entry (`:395-402`) | delete | The check itself. |
| `cmd/e2e-runner/main.go` — `snapshotResultRe` (`:49-51`), `parseSnapshotResults` (`:406-425`), `snapshotDriftTimeout` const (`:34`) | delete | Snapshot-drift-only; dead after the entry is gone. |
| `cmd/e2e-runner/main.go` — `OnComplete` field (`:62-66`) + its `runCheck` block (`:665-674`) | delete | **Sole consumer was snapshot-drift.** Vacuous once removed. |
| `cmd/e2e-runner/main.go` — `Kind` field comment (`:57`) | edit | Drop `"snapshot"` from the `"spike" | "probe" | "snapshot" | "version-lock"` enum comment. Cosmetic; keeps the doc honest. |
| `cmd/e2e-runner/main_test.go` — `TestParseSnapshotResults` (`:11-68`) | delete | Tests the deleted function. |
| `Makefile` — `CHECKERS := e2e-snapshot-check` (`:13`) + `$(CHECKERS)` in `ALL_BINS` (`:17`) | delete | Stops building the retired binary. |
| `Makefile` — `rerecord-snapshots` target (`:50-54`) + its `.PHONY` entry (`:33`) | delete | Re-record command is meaningless with no fixture. |

**Keep (do not remove):** `OnFailure` (used by both probes, `:347`/`:387`), `NonGating` + `gateFailed` (used by both probes), `spike-multiselect` and its `.parsed.json` sidecar write (a retained regression harness per CLAUDE.md — an orphaned sidecar is harmless retained telemetry, cf. `ParseSpinner` no-op in #164), `regexp` import (still used by `successSuccess`/`observedSuccess`/`probeOutDirRe`), `time` import (used throughout).

### Landmine — `OnComplete` vs `NonGating`

The ticket's Technical Notes say "the `snapshots[]` / **NonGatingField** plumbing that only snapshot-drift uses." **This is a PO misnomer.** `NonGating` is used by `probe-first-prompt-hang` (#181) and `probe-cwd-encoding` (#251) — removing it re-gates two de-gated informational probes and reddens `make e2e` for no library signal. The field that is *actually* snapshot-drift-only is **`OnComplete`**. Remove `OnComplete`; leave `NonGating` and `OnFailure` alone.

## `claude-version.lock` — no change

No `version=` bump. There is no re-record, and `claude --version` still reports `2.1.199` (the string #178 pinned). The `version=` bump convention (#47/#57) applies only to deliberate fixture re-records; this is a deletion. The five enforced `flag=`/`value=` lines are untouched. **Do not edit `claude-version.lock`.**

## Concurrency model

None. No goroutines, channels, or lifecycle changes — this is a static removal of a serial check entry from `buildChecks`.

## Error handling / failure modes

- **Dangling reference after partial edit.** If the runner entry is removed but `parseSnapshotResults`/`snapshotResultRe`/`snapshotDriftTimeout` are left, `go vet` reports "declared and not used" (const/var) — caught by `make check`. Delete them in the same pass.
- **Empty directory left behind.** `git rm -r cmd/e2e-snapshot-check/` removes both files; confirm the directory is gone (empty dirs aren't tracked by git but shouldn't linger on disk).
- **No runtime failure surface** — the change removes code paths; it does not add any.

## Testing strategy (claude-free — this is fully verifiable in `make check`)

The whole change is confirmable without a live `claude` capture. Two gates:

1. **`make check` green** (`go vet ./...` + `go test -race ./...`):
   - Compiles cleanly — no dangling `snapshot-drift` references (the vet pass proves the removals are complete).
   - Retained `.bin` unit tests pass: `TestParseMcpStatusRealFixture` (`TotalServers == 10`), the `anchor_forgery_test.go:330` `ModalClassMCP` positive control, `TestDetectModalClassMcpEmptyIsIdleNotModal`, `IsIdle(mcp-empty-snapshot.bin)`, the mcp-empty ready test — all untouched, so all stay green.
   - `cmd/e2e-runner/main_test.go` still compiles and its remaining tests pass after `TestParseSnapshotResults` is removed.

2. **The check is gone from the runner's list, deterministically.** AC #4 wants claude-free confirmation that `make e2e` emits no `snapshot-drift` entry. Confirm two ways:
   - **Source:** `buildChecks` (`cmd/e2e-runner/main.go:224-404`) no longer contains a check named `snapshot-drift` / `Binary: "e2e-snapshot-check"`; `grep -rn 'snapshot-drift\|e2e-snapshot-check\|rerecord-snapshots\|parseSnapshotResults\|snapshotResultRe\|snapshotDriftTimeout' cmd/ Makefile` returns nothing.
   - **Recommended guard test (small, mechanical, ~10 lines):** add `TestBuildChecksExcludesSnapshotDrift` to `cmd/e2e-runner/main_test.go`. Call `buildChecks(func(context.Context) (string, map[string]any) { return "pass", nil })` and assert no returned `Check` has `Name == "snapshot-drift"` or `Binary == "e2e-snapshot-check"`. This turns AC #4's "verifiable from the runner's check list and `cmd/e2e-runner/main_test.go`" into a green, deterministic assertion and guards against an accidental re-add (deterministic-code safety net for the removal, per belt-and-suspenders). It is the one net-new test; everything else is deletion.

3. **`make build-bin` succeeds** — confirms the `Makefile` no longer references the deleted `e2e-snapshot-check` binary (`ALL_BINS` no longer expands `$(CHECKERS)`), and `rerecord-snapshots` is gone.

Live `make e2e` is **not** required for merge; if an operator runs it, the report simply has no `snapshot-drift` entry. Do not gate the PR on it.

## Scope-out (leave alone)

- **`modal.go` / the classifier** — #223's decision stands; touching it reverts the correct fix and drags the forgery-hardened classifier (#221/#223) back into scope for negative value.
- **`spike-multiselect` and its sidecar write** — retained regression harness; the now-orphaned sidecar is harmless.
- **`docs/knowledge/features/e2e-harness.md` and `docs/knowledge/INDEX.md`** — they describe the retired check but are documentation-phase-owned. PO/architect/developer must **not** edit `INDEX.md`; the doc reconciliation happens post-merge in the documentation phase.

## Open questions

- **Guard test — keep or drop?** The `TestBuildChecksExcludesSnapshotDrift` guard is a judgment call. It directly satisfies AC #4's claude-free-verification clause and is cheap, but it asserts the *absence* of a thing (mildly unusual). Recommended in; if the developer prefers, source-inspection + the passing existing suite also satisfy AC #4. Either is acceptable — the AC is about *confirmability*, not a mandated new test.
- **Re-introduction path (not for this ticket).** This retires an empty, un-satisfiable harness; it does not forbid the pattern. If a future claude build ships a host-portable, classifiable `/mcp` (or other) render, the snapshot-drift harness can be re-introduced with a fresh fixture and its scaffolding restored. Note for a future PO ticket, not scope here.

## Acceptance criteria

- [ ] The `/mcp` snapshot-drift case is **retired, not re-recorded**: `pkg/tuidriver/testdata/mcp-snapshot.json` is deleted; and because it is the sole surviving fixture, the dead scaffolding is removed rather than left as vacuous always-pass code — the `cmd/e2e-snapshot-check` binary (dir), the `snapshot-drift` entry + its `snapshotResultRe`/`parseSnapshotResults`/`snapshotDriftTimeout`/`OnComplete` plumbing in `cmd/e2e-runner`, and the `rerecord-snapshots` Makefile target (+ `CHECKERS`) are all removed. No `snapshot-drift` check exists.
- [ ] `pkg/tuidriver/modal.go` is **not** modified. The three tests pinning #223's decision stay green: `TestDetectModalClassMcpEmptyIsIdleNotModal`, `IsIdle(mcp-empty-snapshot.bin)`, and the mcp-empty ready test.
- [ ] `.bin` unit coverage is retained: `mcp-snapshot.bin` (`ParseMcpStatus` → `TotalServers == 10`, `DetectModalClass == ModalClassMCP`, the #221 forgery positive control) and `mcp-empty-snapshot.bin` (Unknown / idle) still pass under `make check`.
- [ ] `NonGating` and `OnFailure` are **untouched** (both used by `probe-first-prompt-hang` and `probe-cwd-encoding`); only `OnComplete` is removed.
- [ ] `make check` is green (vet + race tests), `make build-bin` succeeds, and the runner's `buildChecks` list contains no `snapshot-drift` entry — confirmed claude-free by source inspection and (recommended) `TestBuildChecksExcludesSnapshotDrift`. No live `claude` capture required.
- [ ] `claude-version.lock` is **not** edited (no re-record; `claude --version` still `2.1.199`).

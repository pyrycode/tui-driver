# Spec #178 — Drop the `/agents` case from snapshot-drift + bump lock `version=`

**Ticket:** [#178](https://github.com/pyrycode/tui-driver/issues/178) — snapshot-drift: drop the `/agents` case (removed in claude 2.1.199), confirm `/mcp`, bump `claude-version.lock` `version=`. Split from #176; classifier reconcile carved to #182. Size **S** (`size:s`) — deletion-dominated; this is the exact #129 shape that shipped at **XS**.

**One line:** Remove the `agents` fixture from the snapshot-drift e2e check and delete its committed `.json`, confirm the untouched `mcp` fixture still matches on a clean claude **2.1.199** capture, and bump `claude-version.lock` `version=` `2.1.158`→`2.1.199`. **This is a case drop, not a re-record** (supersedes the earlier "re-record both fixtures" spec `178-snapshot-drift-rerecord-2.1.199.md`, now removed). **No `pkg/tuidriver/` parser/classifier change** — that is #182's concern.

---

## Files to read first

Everything the developer needs on turn 1. This mirrors #129 (`129-drop-picker-snapshot-case.md`) almost exactly — read that spec first; this ticket is the same operation on the `agents` case plus a one-line lock bump.

- `docs/specs/architecture/129-drop-picker-snapshot-case.md` — **the direct precedent. Read in full.** Same shape: drop an e2e-snapshot case, delete its `.json`, retain the `.bin` unit fixtures, narrow the runner regex, update `TestParseSnapshotResults`, fix the Makefile comment. Every edit below has a `129` twin.
- `cmd/e2e-snapshot-check/main.go:20-24` — the stdout-protocol doc comment listing `SNAPSHOT mcp …` / `SNAPSHOT agents …`. The `agents` line (line 23) is one of the removals.
- `cmd/e2e-snapshot-check/main.go:59-63` — the `fixture` struct; the `name` field comment `// "mcp" | "agents"` (line 60) narrows to `// "mcp"`.
- `cmd/e2e-snapshot-check/main.go:72-75` — the `fixtures := []fixture{…}` table. The `{name: "agents", trigger: "/agents\r", settle: 0}` entry (line 74) is the central removal. `mcp` (line 73) stays **verbatim**.
- `cmd/e2e-snapshot-check/main.go:47-57` — `dumpPathRe` (`picker-snapshot path=…`) and `modalClassRe`. **LEAVE ALONE** — trigger-agnostic labels reused by *every* fixture (mcp too), not the agents case. Read only so you don't remove them by name-association (the exact #129 §1 warning).
- `cmd/e2e-runner/main.go:49-51` — `snapshotResultRe` = `^SNAPSHOT (mcp|agents) (match|diff)$`. The `agents` alternation is the second reference to narrow — see Design § 2 (mirrors #129 §2).
- `cmd/e2e-runner/main.go:318-337` — `parseSnapshotResults`. **Read to confirm it needs no body change**: it derives the fixture path generically from the captured `m[1]` name, so dropping `agents` from the table + regex is sufficient. The capture-group parens in `snapshotResultRe` are load-bearing (`m[1]` = name, `m[2]` = result) — narrowing must keep them.
- `cmd/e2e-runner/main_test.go:11-83` — `TestParseSnapshotResults`. Four sub-cases feed `SNAPSHOT agents match` lines and expect `agents-snapshot.json` entries; these are the test updates (Design § 3). Also holds the version-lock tests (`TestEvaluateClaudeVersionLock` etc.) — **do not touch those**; the lock bump is informational and cannot fail them (§ Lock bump).
- `pkg/tuidriver/testdata/agents-snapshot.json` — the `.json` to `git rm` (Design § 4). `agents-snapshot.bin` next to it is **retained**.
- `pkg/tuidriver/agents_test.go:30` and `pkg/tuidriver/modal_test.go:113` — the two consumers of the retained `agents-snapshot.bin` (`ParseAgentList` real-fixture test + the `DetectModalClass` → `ModalClassAgents` table row). **Read to confirm parser/classifier coverage survives the `.json` deletion** — these prove the "retain the `.bin`" AC. Do not edit them; they must stay green untouched.
- `Makefile:44-48` — the `rerecord-snapshots` comment says "the **two** snapshot-drift JSON fixtures" and instructs bumping the lock `version=` in the same commit. Count goes stale to one (Design § 5); the lock-bump instruction is why the bump rides here.
- `claude-version.lock:11` — the `version=2.1.158` line to bump. The header documents the split: `version=` is informational (not enforced against the installed binary — #64); `flag=`/`value=` lines ARE substring-enforced against `claude --help` (§ Lock bump).
- `cmd/e2e-runner/main.go` `runClaudeVersionLockCheck` / `evaluateClaudeVersionLock` (grep — around the version-lock check) — read only to confirm the `version=` bump cannot fail `claude-version-lock`; it compares the five `flag=`/`value=` substrings against `claude --help`, never the `version=` value against the binary.
- `docs/specs/architecture/81-snapshot-drift-parsed-shape.md` — the parsed-shape `reflect.DeepEqual` comparison model this check implements. **Read-only** context for why the surviving `mcp` case compares the way it does.
- `docs/specs/architecture/128-mcp-empty-state-classifier.md` §Context — the strict-mcp `/mcp` empty-state model (`total_servers: 0`) that the retained `mcp` fixture describes, and why `TUIDRIVER_STRICT_MCP_CONFIG=1` is forced onto every spike child.

---

## Context

`snapshot-drift` (spec #81) re-derives each committed `*-snapshot.json` fixture by running `spike-multiselect` with the fixture's `-trigger`, then `reflect.DeepEqual`-compares the freshly-parsed shape against the committed sidecar. It has two cases today: `mcp` and `agents` (the `picker` case was dropped in #129). It reds (~8.8 s) on every PR — a permanent false signal — baseline-confirmed pre-existing (identical on the PR that surfaced it and on merge-base). This is the **parsed-shape** drift child, separate from the first-prompt-deadline hang that reds sibling #177 (split from the same #176); **no ordering dependency** between them.

This ticket was **re-scoped** after the developer ran the check read-only against installed claude **2.1.199**. The earlier premise — "both fixtures stale → Option-C re-record both" — does **not** hold. The two surviving cases resolve differently:

- **`agents` — the modal was removed in claude 2.1.199.** `/agents` no longer opens the tabbed running-subagents modal; it prints a one-line "The /agents wizard has been removed…" notice. `DetectModalClass` returns `Unknown` for that render, so `spike-multiselect` writes no sidecar. The committed `agents-snapshot.json` (`tabs:[Running,Library]`, `empty_text:"No subagents are currently running."`) describes a modal **that no longer exists**. It cannot be re-recorded — the case must be **dropped**, exactly as `/picker` was dropped in #129.
- **`mcp` — already matches. No re-record, no fixture edit.** The committed `mcp-snapshot.json` (`{"total_servers":0,"categories":null}`) is byte-identical to a clean 2.1.199 strict-mcp capture. The initial red was a **dev-host-only artifact**: on a host with unapproved user-scope MCP servers, claude's first-run "N new MCP servers found — approve?" startup picker renders before idle and swallows the `/mcp\r` trigger (the trailing `\r` confirms the approval picker instead of opening `/mcp`), leaving `modal-class=Unknown`. A clean CI runner (no operator MCP servers) never sees the approval picker, so `mcp` matches there with zero change (`--strict-mcp-config` yields `total_servers=0` regardless).

After the drop, the surviving committed fixture set (`mcp` only) matches 2.1.199, so this ticket **completes the calibration sweep**. Per the #47/#57 lesson (and the `Makefile` `rerecord-snapshots` comment), the `claude-version.lock` `version=` bump must coincide with the sweep — so it lives here, and **only** here. Child #182 (retire/reconcile the now-dormant agents classifier) does **not** touch the lock.

## Chosen approach

**Drop the `agents` case from the snapshot-drift check, delete its `.json` fixture, leave `mcp` untouched, and bump the lock `version=`.** No re-record (the agents modal no longer exists to capture; the mcp fixture is already correct), no relaxation of the exact-equal parsed-shape policy (#72/#81). Agents *parser/classifier* coverage is unchanged: `ParseAgentList` and the `DetectModalClass` → `ModalClassAgents` mapping stay exercised by the retained host-independent `agents-snapshot.bin` unit fixture. The snapshot-drift check keeps end-to-end coverage via its surviving `mcp` case.

---

## Design

The whole change is one table-row removal + three consistency edits + one `.json` delete + one lock line. No new code, no new type, no behaviour added. `parseSnapshotResults` derives each fixture path generically from the `SNAPSHOT <name>` line, so once the table entry is gone the runner needs nothing structural — only the regex whitelist and the test expectations follow. Every section below has a #129 twin; deviations from #129 are the lock bump (§ 6) and the mcp-confirm gate (§ 7).

### 1. Remove the `agents` fixture entry and its doc-comment references — `cmd/e2e-snapshot-check/main.go`

- Delete the table row `{name: "agents", trigger: "/agents\r", settle: 0}` (line 74). The table becomes the single remaining row (`mcp`).
- In the stdout-protocol doc comment (lines 20-24), drop the `//\tSNAPSHOT agents match|diff|recorded` line (line 23). The comment now documents one line (`mcp`).
- In the `fixture` struct, narrow the `name` field comment (line 60) from `// "mcp" | "agents"` to `// "mcp"`.
- **Do not touch** `dumpPathRe`, `modalClassRe`, or the `picker-snapshot path=` string literal (lines 47-57) — those labels are trigger-agnostic and reused by the surviving `mcp` fixture. (Exact #129 §1 warning, transposed to agents.)

**Contract preserved:** the binary still emits exactly one `SNAPSHOT <name> match|diff` line per surviving fixture, in table order, and exits 0 iff all matched. No change to `runFixture`, `emitDiff`, `enrichMissingSidecar`, or the `-record` path.

### 2. Narrow the result regex to the emitted set — `cmd/e2e-runner/main.go:51`

Remove `agents` from the alternation, **keeping the capture-group parens** (`m[1]` feeds the fixture path in `parseSnapshotResults`):

```
^SNAPSHOT (mcp|agents) (match|diff)$   →   ^SNAPSHOT (mcp) (match|diff)$
```

**Why, despite the ticket AC list not naming this file.** This is the #129 §2 edit exactly. After § 1 the check binary can never emit `SNAPSHOT agents …`, so the `agents|` alternation is dead — functionally inert but a stale, misleading enumeration a future reader would take as evidence an agents fixture still exists. Removing it keeps `snapshotResultRe` an honest description of the live protocol; it is a 7-character deletion entailed by the same logical change, not adjacent refactoring. The ticket says "Mirror #129" — this is part of the mirror. `parseSnapshotResults`'s **body** is genuinely unchanged (path-derivation is generic over the captured name).

### 3. Update `TestParseSnapshotResults` — `cmd/e2e-runner/main_test.go`

Reduce every sub-case to the single-fixture (`mcp`-only) shape: drop all `SNAPSHOT agents …` stdout lines and every `agents-snapshot.json` `want` entry. As bullet scenarios (developer writes them in the existing table idiom; exact names are the developer's call):

- **"single match"** (from "all match") — stdout: `SNAPSHOT mcp match`; want: `[mcp match]`.
- **"single diff"** (from "mixed match and diff") — stdout: `SNAPSHOT mcp diff`; want: `[mcp diff]`. The two-fixture "mixed" premise no longer applies with one fixture; re-model as a plain diff.
- **"mcp line amid noise"** (from "noisy stdout with SNAPSHOT lines interspersed") — keep the surrounding noise lines, drop the `SNAPSHOT agents match` line; stdout has one `SNAPSHOT mcp diff` interspersed; want: `[mcp diff]`.
- **"empty stdout"** and **"no SNAPSHOT lines amid noise"** — unchanged (no agents reference; both still assert `nil`).
- **"partial output (timeout mid-run)"** — **delete this sub-case.** Its distinct value was truncation *before a later fixture* (`mcp` emitted, `agents` cut off). With one fixture there is no "mid-run" to truncate — a run that never emits the `mcp` line collapses into the already-covered "empty stdout" / "no SNAPSHOT lines" nil path. Removing it is correct, not a coverage loss; note the reason in the PR.

No assertion-helper or signature changes; same `reflect.DeepEqual` comparison. The version-lock tests in the same file (`TestParseClaudeVersion`, `TestParseLockFile`, `TestEvaluateClaudeVersionLock`) are **untouched** — the `version=` bump is informational and does not flow through them.

### 4. Delete the JSON fixture

- `git rm pkg/tuidriver/testdata/agents-snapshot.json`.
- **Retain** `pkg/tuidriver/testdata/agents-snapshot.bin` — it is a host-independent unit-test byte fixture, consumed by `pkg/tuidriver/agents_test.go` (`ParseAgentList` real-fixture test) and `pkg/tuidriver/modal_test.go` (`DetectModalClass` → `ModalClassAgents` table row). Deleting the `.json` must not touch it. (Exact #129 §4 pattern — the `.bin` retention is what keeps parser/classifier coverage intact.)

### 5. Makefile comment — `Makefile:44`

The `rerecord-snapshots` recipe comment reads "Re-record the **two** snapshot-drift JSON fixtures under pkg/tuidriver/testdata/." Change to reflect the **one surviving `mcp`** fixture (e.g. "Re-record the `mcp` snapshot-drift JSON fixture…"). This is the only Makefile edit; the recipe body (`e2e-snapshot-check -record`) is unchanged — `-record` iterates whatever the fixture table now holds (just `mcp`). Leave the "bump `claude-version.lock` `version=` … in the same commit" line as-is; it is the standing instruction this ticket honours.

### 6. Bump `claude-version.lock` `version=` — `claude-version.lock:11`

- Change the single line `version=2.1.158` → `version=2.1.199`.
- Re-verify the five enforced entries against `claude --help` on 2.1.199 — `--session-id`, `--permission-mode`, `bypassPermissions`, `--model`, `--effort`. **Pre-checked during refinement: all five still appear**, so **no `flag=`/`value=` edit is expected.** Confirm independently (`claude --help | grep -E -- '--session-id|--permission-mode|bypassPermissions|--model|--effort'`); edit a `flag=`/`value=` line only if a listed flag/value actually changed. Because `version=` is informational (not compared against the installed binary — #64), this line cannot itself fail the `claude-version-lock` check.

### 7. Confirm `mcp` still matches — no edit (verification, AC gate)

`mcp` is **not** re-recorded and **not** hand-edited. Confirm it still matches on a clean 2.1.199 capture and that the host-portability guard holds:

- Run the check read-only against installed claude (`make e2e`, or `bin/e2e-snapshot-check -bin-dir ./bin` after `make build-bin`, or `go run ./cmd/e2e-snapshot-check`). Expect exactly one line: `SNAPSHOT mcp match`.
- The guard: `mcp-snapshot.json` stays `{"total_servers": 0, "categories": null}` (strict-mcp empty state; `TUIDRIVER_STRICT_MCP_CONFIG=1` is forced onto every spike child, so no operator MCP-config path leaks in). **No hand edit** to `mcp-snapshot.json` under any circumstance.

See § Verification gate for what to do if `mcp` does **not** print `match`.

---

## Verification gate — the two escapes the developer must not rationalize past

**Escape A — a `pkg/tuidriver/` parser/classifier edit becomes tempting.** Dropping the `agents` case is a fixture-table + runner-test + `.json`-delete operation; it must **not** touch `pkg/tuidriver/` parser/classifier code (`agents.go`, `modal.go`, `permission.go`, or their tests). Retiring the now-dormant agents classifier (`ModalClassAgents` / `ParseAgentList` / `DetectModalClass`'s agents branch) is **out of scope → child #182** (blocked-by this ticket). If any step here seems to *require* a `pkg/tuidriver/` edit, **STOP** — that is the signal the work belongs in #182, not that you should force it. This is the belt-and-suspenders deterministic scope guard; #129 dropped the picker e2e case while leaving the picker parser + `.bin` fixtures fully intact, and this ticket does the identical thing for agents.

**Escape B — `mcp` does not print `match`.** Keyed on *why*:

| Observation on the `mcp` case | Meaning | Action |
|---|---|---|
| `SNAPSHOT mcp match` | Fixture is correct against clean 2.1.199 (expected) | ✅ Proceed. No mcp edit. |
| `diff` with `modal-class=Unknown` / missing-sidecar (`classifier drift, not a content diff`) | **Dev-host artifact** — the first-run MCP-approval picker swallowed `/mcp\r` (see Context) | **Not a fixture edit.** Approve your user-scope MCP servers once, or use a clean claude profile, then re-run. A clean CI runner never hits this. |
| `diff` with a written sidecar, guards intact (`total_servers:0`, only wording/field shifts) | Genuine 2.1.199 render drift — contradicts the ticket premise | Unexpected. Refresh via `make rerecord-snapshots` (now iterates `mcp` only; **never hand-edit**), commit the refreshed `mcp` fixture, and flag the surprise in the PR. |
| `diff` with `total_servers > 0` or `categories` populated | Host MCP content leaked into the capture | **Do not commit.** strict-mcp wasn't applied or an operator MCP server was present — re-run cleanly; if it persists, escalate (this is an isolation bug, not a fixture to bake in — the #129 host-taint lesson). |

Evidence points to the first row (the ticket confirms mcp matches on clean 2.1.199), but the gate is mandatory.

---

## Error handling / failure modes

- **`agents` drop forces a `pkg/tuidriver/` edit** → it doesn't (dropping a table row + deleting a `.json` + editing a runner test touches no parser code). If it appears to, that's Escape A → the work is #182, not this ticket.
- **`mcp` reds** → Escape B table above. The dev-host approval-picker artifact is the overwhelmingly likely cause; it is never fixed by editing the fixture.
- **A `flag=`/`value=` genuinely dropped from `claude --help` on 2.1.199** → `claude-version-lock` would fail on the missing substring. Pre-checked as not the case; if it were, a *removed* enforced CLI flag is a compatibility break bigger than a case drop — surface it to PO rather than silently deleting the enforced line.

---

## Testing strategy

- **Unit (changed):** `go test ./cmd/e2e-runner/` — the updated `TestParseSnapshotResults` asserts the `mcp`-only shape and that no `agents-snapshot.json` entry appears in any sub-case.
- **Unit (regression, must stay green untouched):** `go test ./pkg/tuidriver/` — `pkg/tuidriver/agents_test.go` (`ParseAgentList` real-fixture) and `pkg/tuidriver/modal_test.go` (`DetectModalClass` → `ModalClassAgents`) read the **retained** `agents-snapshot.bin` (not the deleted `.json`), proving agents parser/classifier coverage survives the deletion. This is the evidence for the "retain the `.bin`, coverage preserved" AC.
- **Build:** `make check` (`go vet ./...` + `go test -race ./...`) — confirms the regex narrowing and table removal compile with no dangling reference to the removed name.
- **Live gate (operator, needs real claude 2.1.199):** `make e2e` — expect `snapshot-drift` **PASS** with exactly one `SNAPSHOT mcp match` line and no `agents` line; `claude-version-lock` **PASS** (five flag/value substrings present; `version=` informational); `spike-multiselect` continues to **PASS** (unchanged). Re-running `make rerecord-snapshots` should regenerate only the `mcp` JSON.
  - *#177 independence:* if unrelated first-prompt-hang spikes are red in the same `make e2e` run, that is sibling #177's surface — out of scope here; scope verification to the three checks named above.

---

## Out of scope / do not touch

- **`pkg/tuidriver/` agents classifier** — `agents.go`, `modal.go` (agents branch), `permission.go`, `ModalClassAgents`, `ParseAgentList`, the spike-multiselect agents dispatch, `agents-snapshot.bin`. Retiring/reconciling them is **child #182** (blocked-by this ticket). This ticket does **not** touch them (Escape A).
- **`docs/knowledge/features/e2e-harness.md`** and **`docs/knowledge/codebase/178.md`** — owned by the documentation phase; it rewrites the "two fixtures" counts and the report-schema example post-merge from this spec + the merged diff. The developer's worktree mutates only the two code files, the runner test, the deleted `.json`, `Makefile`, `claude-version.lock`, and this spec file. Do **not** edit the knowledge docs in the feature branch.
- **Historical specs** `81-…`, `128-…`, `129-…` reference `agents-snapshot.json` as frozen records — immutable history, do not edit.
- **`cmd/e2e-snapshot-check/main_test.go`** (if present) — any `enrichMissingSidecar` / `modalClassRe` test is independent of the fixture table; keep it verbatim.
- **No picker fixture** — do not re-add or re-record a `/picker` (#129) or `/agents` case.
- **`dumpPathRe` / `modalClassRe` / `picker-snapshot path=` label** — generic, reused by the surviving `mcp` fixture. Leave them.

---

## Open questions

None blocking. The one design judgment — narrowing `snapshotResultRe` to `(mcp)` rather than leaving `agents|` inert (§ 2) — is resolved in favour of an honest protocol whitelist, exactly as #129 §2 resolved it for picker; the fallback (leave it, functionally identical) is noted there if the developer prefers a more minimal diff, but removal is the recommended path and part of the #129 mirror.

---

## Acceptance criteria (developer deliverables)

1. `cmd/e2e-snapshot-check/main.go`: the `agents` fixture-table entry (line 74) is removed; the `SNAPSHOT agents …` protocol-doc line and the `name`-field comment are narrowed to the single `mcp` case; `dumpPathRe` / `modalClassRe` are unchanged.
2. `cmd/e2e-runner/main.go:51`: `snapshotResultRe` no longer includes `agents` in its name alternation; the capture-group parens are preserved (`^SNAPSHOT (mcp) (match|diff)$`).
3. `pkg/tuidriver/testdata/agents-snapshot.json` is deleted (`git rm`); `agents-snapshot.bin` is **retained**.
4. `cmd/e2e-runner/main_test.go`: `TestParseSnapshotResults` updated to the `mcp`-only shape (no `SNAPSHOT agents …` lines, no `agents-snapshot.json` expectation in any sub-case; the two-fixture "partial output" sub-case removed); all sub-cases pass. The version-lock tests in the same file are untouched.
5. `Makefile:44`: the `rerecord-snapshots` comment count is corrected from "two … fixtures" to the one surviving `mcp` fixture.
6. `claude-version.lock:11`: `version=` is bumped `2.1.158` → `2.1.199`. The five enforced `flag=`/`value=` lines are re-verified against `claude --help` on 2.1.199 and edited only if one actually changed (pre-checked: no edit expected).
7. `mcp` is confirmed to still match on a clean 2.1.199 capture (guard intact: `total_servers: 0`, no operator MCP-config path leaked in); **no hand edit to `mcp-snapshot.json`**.
8. `go test ./cmd/e2e-runner/ ./pkg/tuidriver/` stays green — including the untouched `agents_test.go` / `modal_test.go` that read the retained `agents-snapshot.bin`, proving agents parser/classifier coverage is preserved.
9. `make e2e` reports `snapshot-drift` **PASS** with exactly one `SNAPSHOT mcp match` line and no `agents` line; `claude-version-lock` and `spike-multiselect` continue to pass.
10. No `pkg/tuidriver/` parser/classifier code is touched (that is #182's concern — Escape A).

# Spec 81 — snapshot-drift: parsed-shape comparison (supersedes spec 35)

Ticket: https://github.com/pyrycode/tui-driver/issues/81
Status: ready for developer

## Files to read first

- `cmd/e2e-snapshot-check/main.go:1-128` — the current byte-compare implementation in full. This is the file the developer rewrites. Stdout protocol (`SNAPSHOT <name> match|diff`), per-fixture loop, dump-path scraping regex, `emitDiff` helper — all carry over verbatim; only the comparison step changes.
- `cmd/spike-multiselect/main.go:226-235` — the `picker-snapshot path=<file>` stderr log line (kept; still used to find the spike's `/tmp` output) AND the dump-path convention `/tmp/spike-multiselect-bytes-<ns>.bin`.
- `cmd/spike-multiselect/main.go:268-344` — the parsed-JSON dispatch by `DetectModalClass`. **Key fact for this spec:** spike-multiselect already calls `ParsePicker` / `ParseMcpStatus` / `ParseAgentList` and writes the marshalled result via `json.MarshalIndent(_, "", "  ")` to `strings.TrimSuffix(dumpPath, ".bin") + ".parsed.json"` (L274), and logs `parsed-json path=<path>` at L299/L323/L340. The new check binary reads that file directly — no need to invoke the parser, no need to import `pkg/tuidriver/`.
- `pkg/tuidriver/picker.go:13-18` — `PickerItem` JSON shape (`command`, `category` omitempty, `description`, `highlighted`). Defines the picker fixture format.
- `pkg/tuidriver/mcp.go:11-39` — `McpStatus` / `McpGroup` / `McpServer` JSON shape (`total_servers`, `categories[].{name, path?, servers[].{name, status, tool_count?, highlighted}}`). Defines the mcp fixture format.
- `pkg/tuidriver/agents.go:11-22` — `AgentList` / `Agent` JSON shape (`tabs`, `current_tab`, `items[].{name, highlighted}`, `empty_text?`). Defines the agents fixture format.
- `pkg/tuidriver/picker_test.go:30-90`, `pkg/tuidriver/mcp_test.go:9-50`, `pkg/tuidriver/agents_test.go:10-50`, `pkg/tuidriver/grid_test.go:96`, `pkg/tuidriver/modal_test.go:90-100` — five unit-test sites that `os.ReadFile("testdata/<name>-snapshot.bin")`. **Load-bearing reading**: these tests block the AC's "delete the .bin files" instruction. See § "Architect decision: keep the .bin files" below.
- `cmd/e2e-runner/main.go:50-51` — `snapshotResultRe` (no change needed) and `parseSnapshotResults` at L300-319 (one-line edit: change `.bin` to `.json` in the `file:` field).
- `cmd/e2e-runner/main_test.go:24,26,38,40,64,66,75` — test cases that assert `pkg/tuidriver/testdata/<name>-snapshot.bin` in the `parseSnapshotResults` output. Update the suffix to `.json`.
- `docs/specs/architecture/35-snapshot-drift.md` — superseded by this spec; the developer adds a one-paragraph "Superseded by spec 81" header (see § Supersession).
- `docs/specs/architecture/36-claude-version-lock.md` and `docs/specs/architecture/64-claude-version-lock-policy.md` — the rules for the `claude-version.lock` `version=` bump. The fixture-record event triggers a `version=` line bump; spec 64 § "When to bump version=" applies verbatim.
- `claude-version.lock` (repo root) — the file the developer bumps after capturing fresh fixtures. One line: `version=<claude --version output>`.
- `Makefile` — `CHECKERS` already includes `e2e-snapshot-check`. No Makefile changes are required for the basic check. **Optional**: a new `rerecord-snapshots` PHONY target (see § Re-record flow).

## Context

Spec 35 designed `cmd/e2e-snapshot-check` as a byte-compare against three committed `*-snapshot.bin` fixtures. Ticket #72 attempted to harden that approach (Try-line masking, whitespace-clear normaliser) and discovered the byte-compare model has unbounded latent volatility — every byte-level mitigation surfaces the next layer (rotating starter-prompt lines → bimodal whitespace-clear passes → …). This ticket retires the byte-compare and replaces it with a comparison of the **parsed shape** (`ParsePicker` / `ParseMcpStatus` / `ParseAgentList` output) marshalled to JSON.

Two facts shape the design:

1. **`spike-multiselect` already writes `.parsed.json`.** At `cmd/spike-multiselect/main.go:274`, after capturing the snapshot, the binary calls the appropriate parser and writes the result to `<dump-path-without-.bin>.parsed.json`. The new check binary reads that file directly — no import of `pkg/tuidriver/`, no parser re-invocation. This matches spec 35's "runner does not import `pkg/tuidriver/`" property (#34 invariant) and keeps `e2e-snapshot-check` a thin orchestrator.

2. **The parsed shape is stable where the byte stream is not** (#72 finding, re-validated by spike-multiselect's structural log lines: five back-to-back captures yielded identical `item_count=14` / identical command + category + description across captures, regardless of whether the bimodal whitespace-clear pass fired). The parsed-shape comparison directly observes what the screen *means*, not how it's painted.

## Architect decision: keep the .bin files

The AC says "the old `.bin` fixtures are deleted." The implementation cannot follow that verbatim without breaking five unrelated unit tests in `pkg/tuidriver/`:

- `pkg/tuidriver/picker_test.go:40` (`TestParsePickerRealFixture`)
- `pkg/tuidriver/mcp_test.go:20` (`TestParseMcpStatusRealFixture`)
- `pkg/tuidriver/agents_test.go:30` (`TestParseAgentListRealFixture`)
- `pkg/tuidriver/grid_test.go:96` (`TestRenderMcpFixture`)
- `pkg/tuidriver/modal_test.go:93-95` (`TestDetectModalClass`)

These tests pass **raw PTY bytes** into the parser/detector and assert on the parsed output — they cannot consume the `.json` fixtures (the JSON is the parser's *output*, not its *input*). The ticket's "Out of Scope" line "consume [the parsers] as-is" implies we must leave their unit-test surface intact.

**Decision: keep the `*-snapshot.bin` files in `pkg/tuidriver/testdata/`.** They are now unit-test-only fixtures; the e2e check no longer reads them. Both `.bin` and `.json` coexist in `testdata/` for the same three modal classes. This contradicts the AC's "deleted" wording but preserves unit-test coverage at zero cost (the files already exist; no new commits beyond the architect-stage spec). The developer should ship this without re-routing through PO — the architect's call is to override AC scope downward when verbatim compliance would break unrelated tests.

If the maintainer wants the `.bin` files deleted later, that's a follow-up that includes either (a) deleting the unit tests or (b) rewriting them to consume hand-crafted byte fixtures embedded in `_test.go`. Out of scope here.

## Design

### Package layout

```
cmd/e2e-snapshot-check/main.go      # MODIFIED: byte-compare → JSON-compare; +-record flag
cmd/e2e-runner/main.go              # MODIFIED: parseSnapshotResults file ext .bin → .json (1 line)
cmd/e2e-runner/main_test.go         # MODIFIED: test expectations file ext .bin → .json
pkg/tuidriver/testdata/picker-snapshot.json   # NEW: committed parsed-shape fixture
pkg/tuidriver/testdata/mcp-snapshot.json      # NEW
pkg/tuidriver/testdata/agents-snapshot.json   # NEW
pkg/tuidriver/testdata/{picker,mcp,agents}-snapshot.bin   # KEPT (unit-test fixtures)
claude-version.lock                 # MODIFIED: version= bump (1 line)
docs/specs/architecture/35-snapshot-drift.md  # MODIFIED: superseded-by header
Makefile                            # OPTIONAL: PHONY rerecord-snapshots target (see § Re-record flow)
```

No new packages, no new library exports, no parser changes, no Makefile binary-classification changes (`CHECKERS` already lists `e2e-snapshot-check`).

### The check binary — comparison flow

The new `cmd/e2e-snapshot-check/main.go` keeps the existing scaffolding from the current implementation:

- Flag surface stays: `-bin-dir`, `-testdata-dir`, `-spike-timeout`. **Add** `-record` (default false).
- Fixture table stays: `picker` / `mcp` / `agents` with the same `-trigger` and `-settle` values.
- Per-fixture serial loop stays.
- `spike-multiselect` invocation stays: same args (`-trust-folder=accept`, `-trigger=…`, optional `-settle=…`), same stderr mirroring, same `dumpPathRe` scrape.
- `emitDiff` helper stays (same stdout `SNAPSHOT <name> diff` + stderr `drift in <path>` lines).
- Exit code semantics stay: 0 iff every fixture matched (or every re-record copied), 1 otherwise.

The diff is contained:

1. **Path derivation.** After scraping `dumpPath` from spike-multiselect's stderr, compute `parsedPath := strings.TrimSuffix(dumpPath, ".bin") + ".parsed.json"`. This mirrors spike-multiselect L274 exactly. The fixture path becomes `<testdataDir>/<name>-snapshot.json` instead of `.bin`.

2. **Comparison step.** Instead of `bytes.Equal(captured, committed)`:

   ```
   capturedJSON  ← os.ReadFile(parsedPath)
   committedJSON ← os.ReadFile(fixturePath)
   normalised(captured) == normalised(committed) → match
   ```

   Where `normalised(b []byte) []byte` is the result of `json.Unmarshal` into `any` and `json.Marshal` back out. This collapses incidental whitespace / key-order differences between spike-multiselect's `MarshalIndent(_, "", "  ")` and any future re-encoding. The committed file is human-readable indented JSON; the captured file is too (spike-multiselect uses the same encoder); normalisation is belt-and-suspenders against future format changes on either side.

   Use `reflect.DeepEqual(any1, any2)` after `json.Unmarshal` — that's the canonical Go shape-equal predicate and survives map key reordering, slice element identity, etc. Do NOT compare normalised bytes (`bytes.Equal` after re-marshal) — Go's encoding/json is deterministic for struct outputs but the round-trip-through-`any` order is map-keyed, which is not guaranteed identical across runs. `reflect.DeepEqual` over the decoded `any` trees is the load-bearing equality.

3. **On capture / read failure.** `emitDiff` is reused: stdout `SNAPSHOT <name> diff`, stderr `drift in <fixturePath>: <err>`, increment the aggregate-fail flag. Same shape as today.

4. **On mismatch.** `emitDiff(f, fixturePath, nil)` — stdout `SNAPSHOT <name> diff`, stderr `drift in <fixturePath>`. **In addition**, write a single-line operator hint to stderr naming the captured parsed file: `  captured at <parsedPath>` (two-space indent, on the line after `drift in …`). This is the forensic anchor — `diff <(jq . <committed>) <(jq . <captured>)` is the maintainer's diagnosis command.

5. **On match.** stdout `SNAPSHOT <name> match`. No stderr beyond what spike-multiselect already mirrored.

The stdout protocol — three `SNAPSHOT <name> match|diff` lines — is unchanged. `cmd/e2e-runner/main.go:51`'s `snapshotResultRe` continues to match. The only runner-side change is the `.bin` → `.json` literal in `parseSnapshotResults`.

### The check binary — record flow (`-record`)

When `-record` is set, the per-fixture loop:

1. Invokes spike-multiselect identically (same args, same `-bin-dir`).
2. Scrapes `dumpPath` from stderr as today.
3. Derives `parsedPath := strings.TrimSuffix(dumpPath, ".bin") + ".parsed.json"`.
4. **Copies** `parsedPath` → `fixturePath` (write-mode `0o644`). Use `os.ReadFile` + `os.WriteFile` (the file is small — a few KB) rather than `io.Copy` plumbing; readability over performance.
5. Prints `SNAPSHOT <name> recorded` to stdout (a third verb in the stdout protocol — distinct from `match` / `diff` so any future report-consumer can pattern-match cleanly).
6. On any error (capture failed, parsedPath read failed, fixturePath write failed): same `emitDiff` shape with a record-specific error message; exit 1 at the end.

Exit code 0 iff every fixture was recorded successfully.

When `-record` is set, the check makes NO comparison — it overwrites the committed JSON unconditionally. The maintainer reviews the resulting diff via `git diff pkg/tuidriver/testdata/*.json` before committing.

**Strict-mcp invariant for re-recording.** The check binary unconditionally sets `TUIDRIVER_STRICT_MCP_CONFIG=1` in the env passed to every spike-multiselect child (both in the comparison flow and the record flow). When `make e2e` runs the check, the runner has already set this in the process env; setting it again is idempotent. When an operator runs `bin/e2e-snapshot-check -record` directly, the check still sets it — this keeps re-recorded fixtures reproducible across hosts (no operator-MCP-config leaking into the `mcp` fixture's `categories[].path` field).

### JSON fixture shapes

The shape is defined by Go's stdlib `encoding/json` over the parser structs. Reference shapes (representative; actual values come from spike-multiselect's first re-record):

**`picker-snapshot.json`** — array of `PickerItem`. Order is render order (top-to-bottom in the picker). Highlighted == true on exactly one item (the first row in unfiltered mode). Example:

```json
[
  {
    "command": "/figma-use",
    "category": "figma",
    "description": "MANDATORY prerequisite — …",
    "highlighted": true
  },
  {
    "command": "/help",
    "description": "Get help with using Claude Code",
    "highlighted": false
  }
]
```

**`mcp-snapshot.json`** — object `McpStatus`. With `TUIDRIVER_STRICT_MCP_CONFIG=1` set by the check binary (see § "Strict-mcp invariant"), MCP servers are suppressed; the modal renders as "0 servers" with no categories. Expected committed value:

```json
{
  "total_servers": 0,
  "categories": null
}
```

(If empirical capture reveals that `Categories` materialises as `[]` rather than `null`, the committed fixture should reflect that. Developer captures via `-record` and commits what they observe.)

**`agents-snapshot.json`** — object `AgentList`. The Running tab is empty by default; `items` is `null` (since `ParseAgentList` never appends in the empty case):

```json
{
  "tabs": ["Running", "Library"],
  "current_tab": "Running",
  "items": null,
  "empty_text": "No subagents are currently running."
}
```

### Comparison policy: exact-equal of the entire parsed struct

The parsed shape comparison is **exact-equal** across the full `PickerItem` slice / full `McpStatus` struct / full `AgentList` struct. No fields excluded, no subset matching, no field-by-field whitelist.

Rationale:

- **Picker items.** All fields (`command`, `category`, `description`, `highlighted`) are load-bearing. A change in `description` (e.g. a plugin renamed) is a real signal worth surfacing — that's a legitimate drift event. A change in `highlighted` (the default selection moved) is structural. `category` and `command` are obviously identity-bearing. There's no field a maintainer would say "ignore that, it doesn't matter."
- **MCP status.** Under strict-mcp the modal is trivially small (`total_servers: 0`, no categories). Every field reaching the JSON is structural.
- **Agents list.** `tabs` is hard-coded by the parser to `["Running", "Library"]` — stable by construction. `current_tab` / `items` / `empty_text` are all structural. None are noisy.

The volatility this spec fights — Try-line rotation, bimodal whitespace-clear pass — is fully absorbed by the parser: those bytes never reach a `PickerItem` / `McpServer` / `Agent` field (they're outside the SGR-row regex; they're outside the tab-bar / empty-text predicates). Exact-equal is therefore safe; field-subset would just be premature concession.

**If a fixture starts failing intermittently** on two consecutive runs (AC bullet 3), the right escalation is NOT to relax the comparison policy. It's to investigate the parser — a non-deterministic parsed shape means the parser has a non-deterministic input filter, and that's a regression worth surfacing. File a separate ticket; do NOT bundle parser changes into this one (ticket constraint).

### Re-record flow

Two acceptable surfaces (PO asked the architect to pick):

1. **`bin/e2e-snapshot-check -record`** — primary. One command, one tool, same fixture table as the check itself. Idempotent (re-running overwrites the same three files). Operator reviews `git diff` and commits.
2. **`make rerecord-snapshots`** (optional convenience) — a PHONY target that shells out to `bin/e2e-snapshot-check -record -bin-dir $(BIN_DIR)` after `build-bin`. Lower friction for the common "claude minor version bump → re-record" loop. **Spec recommendation:** add it. Three Makefile lines:

   ```makefile
   .PHONY: rerecord-snapshots

   rerecord-snapshots: build-bin
   	$(BIN_DIR)/e2e-snapshot-check -record -bin-dir $(BIN_DIR)
   ```

   This is additive — does not change any existing target. The default `make e2e` workflow is unaffected.

The maintainer workflow after a claude minor-version bump:

```
make rerecord-snapshots          # ← regenerates the three JSON fixtures
git diff pkg/tuidriver/testdata/ # ← review the parsed-shape changes
# bump claude-version.lock version= line to match the current `claude --version`
git add pkg/tuidriver/testdata/*.json claude-version.lock
git commit -m "fixtures: re-record snapshot-drift fixtures for claude X.Y.Z"
make e2e                         # ← validate the re-record produced a green run
```

### Runner changes (`cmd/e2e-runner/main.go`)

One line. In `parseSnapshotResults` (L312-318):

```go
"file":   "pkg/tuidriver/testdata/" + m[1] + "-snapshot.bin",
```

becomes:

```go
"file":   "pkg/tuidriver/testdata/" + m[1] + "-snapshot.json",
```

Nothing else changes on the runner side. `snapshotResultRe` already matches the stdout protocol (it doesn't care about the verb — match or diff or recorded — because `(match|diff)` would need to broaden to `(match|diff|recorded)`).

**Wait — the regex DOES need broadening if we want re-record to populate the report.** Actually no: the runner never invokes the check with `-record` (only the maintainer does, via `make rerecord-snapshots` or direct invocation). Inside `make e2e`, only `match`/`diff` lines appear. The `recorded` verb only appears in operator-driven invocations, where the report isn't produced. Leave `snapshotResultRe` alone.

### Test changes (`cmd/e2e-runner/main_test.go`)

Replace `.bin` with `.json` in the expected `file:` values across the existing test cases. The test structure stays identical. Suggested edit shape: `sed -i 's|-snapshot\.bin|-snapshot.json|g' cmd/e2e-runner/main_test.go` (manually verify the seven occurrences).

The existing test cases (`all match`, `mixed match and diff`, `empty stdout`, `no SNAPSHOT lines amid noise`, `noisy stdout with SNAPSHOT lines interspersed`, `partial output (timeout mid-run)`) are all still correct — they test `parseSnapshotResults`, which still works the same way.

No new test cases needed.

### Supersession of spec 35

Edit `docs/specs/architecture/35-snapshot-drift.md` in place. Prepend a single block at the top of the file (immediately after the `# Spec 35 — …` header line):

```markdown
> **Superseded by [Spec 81](81-snapshot-drift-parsed-shape.md)** (#81, 2026-05-22). The
> byte-compare model designed below was retired after #72's evidence that every
> byte-level mitigation surfaces the next layer of renderer volatility. Spec 81
> replaces the byte-compare with a parsed-shape (`ParsePicker` / `ParseMcpStatus` /
> `ParseAgentList`) JSON comparison. Read spec 81 for the canonical design; this
> spec is retained for historical context only.
```

Leave the rest of spec 35 untouched. Future readers see the supersession marker at the top and follow the link.

### claude-version.lock

After capturing fresh fixtures (`make rerecord-snapshots` or equivalent), the developer reads `claude --version` and updates the `version=` line in `claude-version.lock` to match. Per spec 64 § "When to bump version=", a fixture re-record event IS a bump trigger. The `flag=` and `value=` lines are not touched by this ticket.

The developer captures the current value at fixture-capture time — the spec cannot specify it because the architect doesn't run `claude --version`.

### Concurrency model

Unchanged from spec 35. The check binary's three spike-multiselect invocations run serially (same three reasons spec 35 codified: trust-folder write race, PTY allocation, stderr scraping demux). The runner invokes the check as one subprocess. No goroutines inside the check binary.

The `-record` flow is also serial — same three reasons apply, and operator-facing wall time is dominated by claude spawn cost (~10-20s per fixture, ~30-60s total), well within the 180s timeout.

### Error handling

Inherits spec 35's table verbatim:

| Failure mode | Behaviour |
|---|---|
| `spike-multiselect` binary missing under `-bin-dir` | `cmd.Run()` returns `exec.ErrNotFound`-shaped error → `diff` for that fixture + stderr log → continue → exit 1. |
| `spike-multiselect` exits non-zero | Same as above. |
| `-spike-timeout` exceeded for one fixture | `ctx.Err() == context.DeadlineExceeded` → `diff` for that fixture → continue. |
| Stderr lacks `picker-snapshot path=...` line | `diff` + log "no dump-path log line in spike-multiselect stderr". |
| `<dump>.parsed.json` missing (spike-multiselect detected an unknown modal class and didn't write JSON, e.g. parser change broke detection) | `diff` + log `read parsed-json <path>: <err>`. **This is a legitimate drift signal** — if a `/mcp` invocation doesn't produce parsed JSON, the modal-detector or the parser has changed; the maintainer should investigate. |
| `<dump>.parsed.json` malformed (JSON unmarshal fails on captured) | `diff` + log `parse captured json: <err>`. Same logic as previous row. |
| Committed fixture missing (`pkg/tuidriver/testdata/<name>-snapshot.json` doesn't exist) | `diff` + log `read fixture <path>: <err>`. The maintainer hasn't run `-record` yet, or deleted the file by mistake. |
| Committed fixture malformed | `diff` + log `parse fixture json: <err>`. Hand-edit gone wrong. |
| Whole check timeout via runner's `-timeout snapshot-drift=…` | `runCheck` returns `status="timeout"`; `parseSnapshotResults` runs on partial stdout, so the report shows partial results (e.g. `picker=match`, `mcp=match`, `agents` missing). |

Invariant unchanged: **the check binary always exits with a real exit code**. No panics, no `os.Exit(2)` for unexpected errors. Failures are degraded-to-diff per fixture; aggregate exit code summarises.

## Testing strategy

1. **Manual integration test (green path).** Developer runs `make build-bin && bin/e2e-snapshot-check -record` to populate the three JSON fixtures, then `make e2e`. Verify `e2e-report.json` has `snapshot-drift` with `status: pass` and three `snapshots[]` entries with `result: match`. PR description must paste a representative parser-output JSON for each of the three fixtures (per ticket Technical Notes bullet "PR description should name").

2. **Determinism check (AC bullet 3).** `bin/e2e-snapshot-check` passes on two back-to-back runs from a clean state:
   ```bash
   bin/e2e-snapshot-check && bin/e2e-snapshot-check
   ```
   Both invocations must exit 0. A single passing run is not sufficient. If the second run diffs, the parser has a non-deterministic input filter — escalate to a separate ticket; do NOT relax the comparison policy here.

3. **Forced-failure smoke.** Temporarily hand-edit one fixture (e.g. `jq '.[0].command = "/oops"' pkg/tuidriver/testdata/picker-snapshot.json > /tmp/picker-snapshot.json && mv /tmp/picker-snapshot.json pkg/tuidriver/testdata/picker-snapshot.json`), run `make e2e`, verify:
   - Runner exit code is 1.
   - `snapshot-drift` entry has `status: "fail"`.
   - `snapshots[]` shows `picker` as `"diff"` and the other two as `"match"`.
   - Host stderr contains `drift in pkg/tuidriver/testdata/picker-snapshot.json`.
   - Host stderr also contains the `  captured at /tmp/spike-multiselect-bytes-<ns>.parsed.json` operator hint.
   Revert the local edit.

4. **`parseSnapshotResults` unit test (existing).** The six existing cases in `cmd/e2e-runner/main_test.go` cover the runner-side parser. Update the `.bin` → `.json` suffix in the expected `file:` values; no new cases.

5. **No new unit test for the check binary itself.** It's an integration shim; the manual integration test + forced-failure smoke cover it. Same rationale as spec 35.

6. **No mocked `claude`.** Same as spec 35.

## Open questions

1. **`Categories: null` vs `[]` in `mcp-snapshot.json`.** Go's `encoding/json` marshals a nil slice as `null` (not `[]`). The current `ParseMcpStatus` returns `&McpStatus{}` with `Categories` left nil when no categories are found — so the marshalled output is `"categories": null`. With `--strict-mcp-config`, no categories are matched, so `null` is the expected shape. **But** if the maintainer's claude version ever renders the modal with a category header even under strict-mcp (e.g. "No Project MCPs found" header with empty `Servers`), the marshalled shape flips to `[]` or `[{"name":"Project MCPs", "servers": null}]`. Resolution: the developer commits whatever the first `-record` capture produces; spec doesn't predict the value. The exact-equal comparison is correct either way.

2. **`items: null` vs `[]` in `agents-snapshot.json`.** Same shape concern as above — `ParseAgentList` returns `Items: nil` when no items match. Commit whatever first capture shows.

3. **Do we need to scrub `McpGroup.Path` from committed fixtures?** With `--strict-mcp-config` set unconditionally by the check binary, no MCP categories should appear, so no paths leak into the fixture. The strict-mcp invariant is the structural safeguard. If a future claude version surfaces category headers even under strict-mcp (with `Path` populated from `/Users/<operator>/...`), this becomes a real concern — but the failure mode would be the e2e diffing across hosts, not a security issue. Defer to first-mismatch evidence.

4. **`-record` flag overwrite ergonomics.** The flag overwrites unconditionally. Confirm-before-overwrite (interactive prompt) is rejected — the binary is non-interactive by design (it runs under `make e2e` and CI). `git diff` is the review gate. Aligns with the dispatcher pipeline conventions.

5. **Determinism risk on `picker`.** Picker item order depends on claude's plugin-discovery order. Empirical evidence from #72: five back-to-back captures all produced identical 14-item lists. If a future claude version randomises plugin-discovery order across processes, the fixture will diff intermittently — the right response is to file a parser-stabilisation ticket (e.g. sort items by `command` before marshalling in `ParsePicker`), not to relax the comparison. Out of scope here.

## Out of scope (per ticket)

- Parser-internals changes in `pkg/tuidriver/`.
- A general "switch all comparisons to parsed-shape" sweep.
- Library extraction work (#58–#62).
- Migrating spike binaries to a different invocation pattern.
- Deleting the `*-snapshot.bin` files (architect override — see § "Architect decision: keep the .bin files").
- A claude-version-aware fixture variant scheme (different fixtures per claude version).
- Adding a `-fixture <name>` filter to scope the check or re-record to one modal class.

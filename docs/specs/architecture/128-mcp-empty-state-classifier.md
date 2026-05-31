# Spec 128 — snapshot-drift: reconcile `/mcp` with claude 2.1.158 strict-mcp empty-state

Ticket: https://github.com/pyrycode/tui-driver/issues/128 (split from #127)
Status: ready for developer
Size: S — additive classifier change (one production file) + optional diagnostics (one more) + a re-record + a version bump.

## Files to read first

- `pkg/tuidriver/modal.go:59-121` — **the file to edit.** The package-private anchor block (L59-75) and `DetectModalClass`'s switch (L90-121). Extract: (a) the established **dual-form anchor convention** — `anchorAskUserStripped`/`anchorAskUserSpaced`, `anchorPermissionStripped`/`anchorPermissionSpaced`, `anchorModelSelectStripped`/`anchorModelSelectSpaced` — CSI cursor-forward stripping eats inter-word spaces in some renders, so anchors come in no-space + spaced pairs; (b) the MCP `case` is **first** in the switch (highest priority among modal classes).
- `pkg/tuidriver/modal.go:90-93` — the `findPickerRows` pre-check that runs **before** the switch. If it returns >0 rows the snapshot is classified `slash-picker` and the MCP case never runs. The empty-state must NOT trip this (see § "Slash-picker precedence — verify, don't assume").
- `pkg/tuidriver/picker.go:71-121` — `pickerRowStartRe = ^[ \t\r]*/[a-zA-Z]` and `findPickerRows`. Confirms the `❯ /mcp` command-echo line does not match (line starts with the `❯` glyph, not `/`).
- `pkg/tuidriver/mcp.go:41-179` — `ParseMcpStatus`. **No change.** Extract: `mcpTotalServersRe = ^(\d+)\s*servers?$` (anchored — the empty-state prose `No MCP servers configured…` does not match) and `mcpCategoryHeaderRe` (no `Project|User|Built-in MCPs` header in the empty-state) → parser returns `&McpStatus{TotalServers:0, Categories:nil}` → marshals to `{"total_servers":0,"categories":null}`.
- `pkg/tuidriver/modal_test.go:18-124` — `TestDetectModalClassSyntheticAnchors` (add spaced + empty-state synthetic cases) and `TestDetectModalClassRealFixtures` (add the new `mcp-empty-snapshot.bin` case).
- `pkg/tuidriver/mcp_test.go:19-108` — `TestParseMcpStatusRealFixture` reads `mcp-snapshot.bin` and asserts **10 servers / 3 categories**. This `.bin` is the full-modal unit fixture; it is **untouched** by this ticket. (The e2e `mcp-snapshot.json` is a separate, strict-mcp capture — see § Context.)
- `cmd/spike-multiselect/main.go:253-306` — the `DetectModalClass` dispatch and the `ModalClassMCP` branch. **Key:** the MCP branch writes the `.parsed.json` sidecar **unconditionally** (L301-306), unlike the picker branch (gated on `len(items) > 0`). So once `DetectModalClass` returns MCP for the empty-state, the sidecar appears with **no spike-multiselect change**.
- `cmd/spike-multiselect/main.go:208-217` — the unconditional raw `.bin` dump + the `picker-snapshot path=<path>` log line. This is how the developer captures `mcp-empty-snapshot.bin` (the dump happens regardless of modal class).
- `cmd/e2e-snapshot-check/main.go:65-69` — fixture table: `mcp` uses `-trigger=/mcp\r` (CR commits the command), `-settle=5s`. Under the binary's unconditional `TUIDRIVER_STRICT_MCP_CONFIG=1` (L96-99) this yields the empty-state.
- `cmd/e2e-snapshot-check/main.go:119-123` — the `read parsed-json … no such file` error path = the exact failure in the ticket. Site of the optional diagnosability enrichment (§ Optional).
- `cmd/e2e-snapshot-check/main.go:48-50` — `dumpPathRe` stderr-scrape pattern; the optional enrichment mirrors this with a `modal-class detected=` scrape.
- `pkg/tuidriver/testdata/mcp-snapshot.json` — the current 1-server (`computer-use`, built-in, disabled) e2e fixture. Re-recorded to the empty-state shape by this ticket.
- `docs/specs/architecture/81-snapshot-drift-parsed-shape.md` § "Open question 3" + § "JSON fixture shapes" — the strict-mcp shape fragility this ticket resolves; spec 81's `mcp-snapshot.json` empty-shape prediction is now the realised case.
- `docs/knowledge/features/e2e-harness.md` § "Re-recording snapshot fixtures" + § "Headless / CI plumbing" — `make rerecord-snapshots`, the `TUIDRIVER_STRICT_MCP_CONFIG=1` env-var seam, and the `claude-version.lock` `version=` bump discipline. (**Read-only — do not edit this doc.**)

## Context

The e2e `snapshot-drift` check (spec 81) re-derives `pkg/tuidriver/testdata/mcp-snapshot.json` by running `spike-multiselect -trigger=/mcp\r` under `--strict-mcp-config`, reading the `.parsed.json` sidecar the spike writes, and `reflect.DeepEqual`-comparing it to the committed fixture.

On **claude 2.1.158** (currently installed) the `/mcp` command under `--strict-mcp-config` no longer renders a "Manage MCP servers" modal. It prints an inline empty-state:

```
❯ /mcp
  ⎿  No MCP servers configured. Please run /doctor if this is unexpected. Otherwise, run claude mcp --help or visit
     https://code.claude.com/docs/en/mcp to learn more.
```

`DetectModalClass` has no anchor for this text → returns `ModalClassUnknown` → spike-multiselect's `default` branch writes **no** `.parsed.json` → `e2e-snapshot-check` reads the missing path and degrades to `diff` with `read parsed-json …: no such file`. The committed `mcp-snapshot.json` (1 server, built-in `computer-use`) is **unreproducible** under the gate — a blind re-record reads the same missing path and fails identically. Spec 81's "built-in survives strict-mcp" premise is now empirically false on 2.1.158.

A **second, independent** bug: even the *full* (non-strict) `/mcp` modal classifies as `Unknown` on 2.1.158, because the title now strips to `Manage MCP servers` (spaced) but `anchorMCP` is the no-space `ManageMCPservers`. The committed `mcp-snapshot.bin` unit fixture was captured at an older claude where the title stripped to no-space, so `TestDetectModalClassRealFixtures` still passes against it — but a live full-modal capture no longer classifies.

Two fixtures, two captures — do not conflate them:
- `mcp-snapshot.bin` — **full modal**, 10 servers / 3 categories, non-strict, captured 2026-05-18. Unit-test only (`mcp_test.go`, `modal_test.go`, `grid_test.go`). **Untouched here.**
- `mcp-snapshot.json` — **strict-mcp** capture, consumed only by the e2e snapshot-drift check. **Re-recorded here** to the empty-state shape.

## Design

Three localised changes; the classifier change is the load-bearing one.

### 1. Classifier: recognise the strict-mcp empty-state (AC1) — `pkg/tuidriver/modal.go`

Add empty-state anchor(s) and fold them into the existing MCP `case`. Per the dual-form convention, provide both a spaced and a space-stripped form (the developer keeps whichever the real capture contains; keeping both is cheap belt-and-suspenders):

```go
anchorMCPEmptySpaced   = []byte("No MCP servers configured")
anchorMCPEmptyStripped = []byte("NoMCPserversconfigured")
```

MCP `case` becomes an OR over the title anchors + the empty-state anchors. **Contract:** `DetectModalClass(<strict-mcp empty-state capture>) == ModalClassMCP`. No signature change; the `findPickerRows` pre-check, switch order, and every other `case` are unchanged.

`ParseMcpStatus` needs **no change** — on the empty-state it returns `{TotalServers:0, Categories:nil}` (the total/category regexes don't match the prose; see "Files to read first"). spike-multiselect's MCP branch then writes the sidecar unconditionally. The fix is genuinely localised to the classifier.

### 2. Classifier: fix the spaced full-modal anchor (AC2) — `pkg/tuidriver/modal.go`

Add the spaced title form alongside the existing no-space anchor:

```go
anchorMCPSpaced = []byte("Manage MCP servers")
```

Fold into the same MCP `case` (OR term). The old no-space `anchorMCP` stays — `mcp-snapshot.bin` (no-space title) still matches it, so `TestDetectModalClassRealFixtures` keeps passing. Both forms in one boolean-OR case; no double-counting.

Final MCP case shape (contract sketch, not the literal body):

```go
case bytes.Contains(stripped, anchorMCP) ||
    bytes.Contains(stripped, anchorMCPSpaced) ||
    bytes.Contains(stripped, anchorMCPEmptySpaced) ||
    bytes.Contains(stripped, anchorMCPEmptyStripped):
    return ModalClassMCP
```

Update the anchor-documentation comment block (modal.go:42-49) to list the new MCP forms.

### 3. Re-record the e2e fixture (AC1, AC3) — `pkg/tuidriver/testdata/mcp-snapshot.json`

After the classifier fix, `make rerecord-snapshots` captures the empty-state and overwrites the committed JSON with:

```json
{
  "total_servers": 0,
  "categories": null
}
```

(Commit whatever `-record` actually produces — spec 81 Open Question 1 notes `null` vs `[]` is decided by the capture, and the exact-equal comparison is correct either way.) Because the check binary forces `TUIDRIVER_STRICT_MCP_CONFIG=1`, this shape is host-portable: a clean CI runner and the operator box both render the empty-state, and no operator MCP-config path leaks into the fixture (AC3).

### 4. Bump `claude-version.lock` (re-record discipline)

Bump the `version=` line to match `claude --version` (currently `2.1.158`). Per e2e-harness.md, a fixture re-record event is a `version=` bump trigger. The `flag=`/`value=` lines are not touched (confirm they still appear in `claude --help`; they should — this is a renderer change, not a CLI-surface change).

### Optional (architect's discretion — included): diagnosability enrichment — `cmd/e2e-snapshot-check/main.go`

The cryptic `read parsed-json …: no such file` has masked a classifier regression three times. This is an **observed-recurring** failure (evidence-based) and the fix is **deterministic code on the same error path** (correct belt-and-suspenders fabric), directly serving AC5's "next recurrence is diagnosable." It stays within size (one extra production file, ~8 lines). **Include it.**

At the `os.ReadFile(parsedPath)` error path (L119-123), scrape spike-multiselect's `modal-class detected=<X>` line (logged at spike-multiselect/main.go:254) from the already-captured `stderrBuf`, and enrich the diff message so a missing sidecar reads as classifier drift, e.g.:

> `drift in …/mcp-snapshot.json: read parsed-json …: no such file (spike classified modal as "" / Unknown and wrote no sidecar — classifier drift, not a content diff)`

Add a scrape regex mirroring `dumpPathRe`:

```go
var modalClassRe = regexp.MustCompile(`modal-class detected=(\S*)`)
```

Use `(\S*)` (not `\S+`) — `ModalClassUnknown` is the empty string, so the line is `modal-class detected=` with nothing after; render that as `Unknown` in the message. **Constraints:** change only the error *message*; the stdout protocol (`SNAPSHOT mcp diff`), control flow, and exit codes are unchanged (AC4). When the scrape finds no line (spike crashed before logging), fall back to the current bare message.

### Files touched

| File | Change | Kind |
|---|---|---|
| `pkg/tuidriver/modal.go` | + empty-state + spaced anchors; extend MCP case; update comment | production |
| `cmd/e2e-snapshot-check/main.go` | enrich missing-sidecar diff message | production |
| `pkg/tuidriver/modal_test.go` | + synthetic cases; + real-fixture case | test |
| `pkg/tuidriver/testdata/mcp-empty-snapshot.bin` | NEW — captured strict-mcp empty-state bytes | fixture |
| `pkg/tuidriver/testdata/mcp-snapshot.json` | re-record to empty-state shape | fixture |
| `claude-version.lock` | `version=` → 2.1.158 | config |

**Production source count: 2** (`modal.go`, `e2e-snapshot-check/main.go`) — well under the size-5 split threshold. No new exported types, no signature changes, no consumer call-site cascade (`DetectModalClass`'s impact set is `classify` + tests, all additive-compatible).

## Slash-picker precedence — verify, don't assume

`DetectModalClass` runs `findPickerRows` **before** the switch. If the empty-state capture trips it, the snapshot classifies `slash-picker` and the MCP fix is defeated.

**Evidence it won't trip:** the ticket reports the empty-state currently returns `Unknown` — which means `findPickerRows` already returns nil on it today (otherwise it would report `slash-picker`, not `Unknown`). Line analysis agrees: `❯ /mcp` starts with the `❯` glyph (not `/`), and the `⎿ No MCP…` / `https://…` lines don't start with `/`.

**Still verify on the real capture.** The developer must assert `DetectModalClass(mcp-empty-snapshot.bin) == ModalClassMCP` (the new `TestDetectModalClassRealFixtures` case is exactly this gate). If it ever returns `slash-picker`, do **not** reorder the pre-check — route back via `needs-rework:po` (the empty-state would be structurally ambiguous with the picker, the ticket's stated route-back condition).

## False-positive risk (route-back assessment)

The ticket's route-back trigger: route back if the empty-state can't be classified MCP without unacceptable false-positive risk.

- `No MCP servers configured` is highly specific to the `/mcp` empty-state — not present at idle, in other modals, or in normal scrollback.
- The residual risk is identical in kind to every existing text anchor (`Do you want to proceed`, `Manage MCP servers`, etc.): if claude's *assistant text* ever quotes the phrase verbatim, a transient `mcp` classification fires in the `Events()` modal axis. This is the accepted-risk baseline the whole anchor design already lives with; the new anchor is no worse and is more specific than most.

**Verdict: classify it. No route-back.** Risk is acceptable and consistent with precedent.

## Concurrency model

Unchanged. `DetectModalClass` is a pure synchronous predicate; spike-multiselect and `e2e-snapshot-check` keep their existing serial flows. No goroutines added or altered.

## Error handling

- Empty-state capture parses cleanly → `{0, nil}` sidecar → `match` against the re-recorded fixture.
- All spec-81 snapshot-check failure modes are unchanged; the only delta is a richer *message* on the missing-sidecar path (optional change above), not new control flow.
- If the developer's `-record` capture yields a non-empty MCP shape (e.g. a future claude reintroduces a category header under strict-mcp), commit what it produces — exact-equal comparison handles either shape, and that would itself be a legitimate signal worth a fresh look.

## Testing strategy

Unit (`pkg/tuidriver/`, fast suite — the deterministic gate against anchor-form drift):

- `TestDetectModalClassSyntheticAnchors`: add
  - `{"mcp spaced title", []byte("...Manage MCP servers..."), ModalClassMCP}` — covers AC2.
  - `{"mcp empty-state spaced", []byte("...No MCP servers configured..."), ModalClassMCP}` — covers AC1's classification.
  - (if the real capture shows space-stripping) `{"mcp empty-state stripped", []byte("...NoMCPserversconfigured..."), ModalClassMCP}`.
- `TestDetectModalClassRealFixtures`: add `{"mcp-empty-snapshot.bin", ModalClassMCP}`. **This is the load-bearing regression test** — it asserts the committed anchor matches *real captured bytes*, not a hand-typed string. If the anchor form is wrong, this fails immediately in `go test`, before any e2e run.
- `TestParseMcpStatusRealFixture` (mcp_test.go) and the existing `.bin`-based tests must still pass unchanged (`mcp-snapshot.bin` is untouched) — this is the AC4 regression guard inside the unit suite.

Capturing `mcp-empty-snapshot.bin` (developer recipe):
1. `make build-bin`.
2. `TUIDRIVER_STRICT_MCP_CONFIG=1 ./bin/spike-multiselect -trust-folder=accept -trigger='/mcp\r' -settle=5s` (use `printf` for the `\r`). It always writes a raw `.bin` dump and logs `picker-snapshot path=/tmp/spike-multiselect-bytes-<ns>.bin` regardless of classification.
3. Copy that dump → `pkg/tuidriver/testdata/mcp-empty-snapshot.bin`. Inspect `StripOSC(StripANSI(bytes))` to confirm which empty-state anchor form is present; set the anchor(s) accordingly.

e2e (slow, real claude — the live drift gate):
- `make rerecord-snapshots` → review `git diff pkg/tuidriver/testdata/mcp-snapshot.json` (expect 1-server → empty-state shape).
- `make e2e` → `snapshot-drift` `status: pass`, `snapshots[]` mcp entry `result: match` (AC1), other checks still pass (AC4).
- Determinism (spec 81): two back-to-back `bin/e2e-snapshot-check` runs both exit 0.

PR description (AC5): state the root cause — **claude 2.1.158's `--strict-mcp-config` replaced the `/mcp` modal with an inline `No MCP servers configured` empty-state, and separately the modal title now strips to the spaced `Manage MCP servers`** — and the reconciliation (classifier anchors + empty-state re-record), so the next recurrence is diagnosable rather than re-triaged. Paste the re-recorded `mcp-snapshot.json`.

## Open questions

1. **`categories: null` vs `[]`** — `ParseMcpStatus` leaves `Categories` nil on the empty-state → `null`. Commit whatever `-record` produces; exact-equal handles either. (Spec 81 OQ1.)
2. **Empty-state anchor form** — resolved empirically by the developer against `mcp-empty-snapshot.bin`; the real-fixture test is the gate. Keep both forms if uncertain.
3. **Sibling #129 (picker case, split from #127) coordination** — at spec time no `feature/129` branch exists, so the architect-time branch-overlap check found nothing to block on. If #129 runs concurrently and also re-records fixtures, the only plausible shared file is `claude-version.lock` (both bump `version=` → `2.1.158`, an *identical* edit that git merges cleanly). #129 is about `picker-snapshot.json` / `picker.go`, which this ticket doesn't touch. No structural conflict expected; whichever child's developer merges second rebases on the first. Flagged for the reviewer's awareness, not a blocker.

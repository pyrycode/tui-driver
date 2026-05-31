# Spec 127 — snapshot-drift: missing parsed-json sidecar (post-#81) + picker content drift

Ticket: https://github.com/pyrycode/tui-driver/issues/127
Status: ready for developer
Size: S (no split — see § Sizing)

## Files to read first

- `cmd/e2e-snapshot-check/main.go:83-160` — `runFixture`: the scrape→derive→read→compare flow. **L111-117**: scrapes `picker-snapshot path=<dump>.bin` via `dumpPathRe` (L50), then derives `parsedPath := TrimSuffix(dumpPath,".bin")+".parsed.json"`. **L119-123**: reads `parsedPath` *unconditionally* and emits the exact `read parsed-json <path>: no such file` error from the failure signature. This is the file you enrich for diagnosability (§ Deliverable 2).
- `cmd/spike-multiselect/main.go:208-326` — snapshot + dispatch. **L253-254**: logs `modal-class detected=<class>`. **L256-326**: the `switch modalClass` that *conditionally* writes `<dump>.parsed.json`. Extract the asymmetry: `ModalClassMCP`/`ModalClassAgents` always write (L301-306, L318-323); `ModalClassSlashPicker` writes only `if len(items) > 0` (L276-283); `default` logs `no-parser-for-modal-class class=<class>` and writes nothing (L324-325). The unconditional path-log is L216-217.
- `pkg/tuidriver/modal.go:59-121` — `DetectModalClass` + anchors. **L60**: `anchorMCP = "ManageMCPservers"` (no-space form, assumes CSI-cursor-forward ate the inter-word spaces). **L90-93**: `findPickerRows` runs *first* — a `/mcp` modal containing any line that strips to `/<letter>` would misclassify as slash-picker before the MCP check is even reached. **L96**: the MCP branch matches on the naive `StripOSC(StripANSI(snap))` path. Note the precedent at L64-70: ask-user/permission/model-select each carry **both** a stripped and a spaced anchor variant — the model the MCP fix likely follows.
- `pkg/tuidriver/mcp.go:71-179` — `ParseMcpStatus` uses `Render` (vt10x full emulation), a **different** text-extraction path than the classifier's naive strip. Extract the key asymmetry: the parser is robust to rendering quirks; the *classifier anchor* is the fragile seam. The parser is proven sound (unit test passes) — do not touch it.
- `pkg/tuidriver/picker.go:78-121, 170-237` — `findPickerRows` (row anchor `pickerRowStartRe = ^[ \t\r]*/[a-zA-Z]`, L78) and `ParsePicker` (returns `nil` when no rows → spike writes no sidecar only when `len(items)==0`). Extract: what makes a live `/mcp` capture accidentally match a picker row.
- `pkg/tuidriver/modal_test.go:103-124` — `TestDetectModalClassRealFixtures` pins `mcp-snapshot.bin → ModalClassMCP`. If you re-capture `mcp-snapshot.bin`, this test must still pass; if you add a spaced MCP anchor, add a synthetic case to `TestDetectModalClassSyntheticAnchors` (L18-81).
- `pkg/tuidriver/testdata/mcp-snapshot.json`, `pkg/tuidriver/testdata/picker-snapshot.json` — the two committed re-record targets. (`agents-snapshot.json` passes — do not touch. `picker-truecolor-snapshot.bin` has no `.json` sibling; it is a unit-test-only fixture, out of scope.)
- `docs/specs/architecture/81-snapshot-drift-parsed-shape.md:163-175` (exact-equal policy; "don't relax comparison on drift") and **L259-273** (the error-handling table — the row `<dump>.parsed.json missing … This is a legitimate drift signal` is the design intent this spec *refines*: the signal is correct, the message is too cryptic).
- `docs/knowledge/features/e2e-harness.md` § "Re-recording snapshot fixtures" + § "How it handles failure" (last two rows) + § "`claude-version.lock`" — the re-record runbook, the version-lock bump discipline, and the `make rerecord-snapshots` command.
- `claude-version.lock` (repo root) — `version=2.1.150` line; bump on re-record per #64 policy. `flag=`/`value=` lines are untouched by this ticket.

## Context

`snapshot-drift` is red on `main` (baseline `317c210`) and has been since before PR #126. QA proved via baseline-comparison that #126 did not introduce it (fails identically on merge-base). The failure is environmental to `main` — claude's live TUI output has drifted away from the committed fixtures. The signature:

```
drift in pkg/tuidriver/testdata/mcp-snapshot.json: read parsed-json /tmp/spike-multiselect-bytes-<ns>.parsed.json: open ...: no such file or directory
drift in pkg/tuidriver/testdata/picker-snapshot.json
e2e-runner: snapshot-drift -> fail (10956ms)
```

This is a **new signature**, distinct from the pre-#81 byte-compare drift (#75/#83/#92, retired by #81/#90). It is the post-#81 parsed-shape check's first failure: a *missing sidecar* on the `mcp` fixture plus a plain *parsed-shape diff* on the `picker` fixture.

### Static diagnosis already completed by the architect

Run before writing this spec, so the developer does not repeat it:

1. **All classifier + parser unit tests pass** against the committed `.bin` fixtures (`go test ./pkg/tuidriver/ -run 'TestDetectModalClass|TestParsePicker|TestParseMcp|TestParseAgent'`). In particular `TestDetectModalClassRealFixtures/mcp-snapshot.bin → ModalClassMCP` **passes**. The classifier and parsers are sound *against the bytes recorded when the fixtures were last captured*.
2. Therefore the failure is **pure live drift**: claude's *current* output differs from the recorded `.bin`. The code is not broken against historical input; the input has moved.

This bounds the problem precisely: the fix is about reconciling the code + fixtures with the *current* claude, not repairing a logic regression on old data.

## The producer↔check contract (the seam)

`e2e-snapshot-check` is a thin orchestrator (#81). Per fixture it: runs `spike-multiselect -trigger=… [-settle=…]` → scrapes `picker-snapshot path=<dump>.bin` from stderr → derives `<dump>.parsed.json` → reads it → `reflect.DeepEqual`-compares the unmarshalled tree against the committed `<name>-snapshot.json`.

The fragility: `spike-multiselect` logs the `.bin` path **unconditionally** (L216-217) but writes the `.parsed.json` sidecar **conditionally** (only on `MCP`/`Agents`/`SlashPicker-with-items`). The check derives the sidecar path from the unconditional log line and reads it **unconditionally**. So any producer run that classifies into a no-write branch surfaces in the check as `no such file or directory` — a message that conflates "modal misclassified / produced no parse" (the real signal) with "file I/O failed."

**Consequence for the fix order (load-bearing):** `make rerecord-snapshots` (`e2e-snapshot-check -record`) reads the *same* derived sidecar path (main.go L119, shared by record and compare flows). When the sidecar is missing, `-record` fails identically — **you cannot re-record the mcp fixture until the producer writes a sidecar again.** Re-record is therefore not a valid first move for the mcp symptom; the classifier must be reconciled first. This is the deterministic refutation of the "reflexive re-record" anti-pattern the ticket warns about.

## The two symptoms, decoupled

| fixture | signature | mechanism | fix family |
|---|---|---|---|
| `mcp` | `no such file` | live `/mcp` no longer classifies as `ModalClassMCP` → no-write branch → no sidecar | reconcile classifier, **then** re-record |
| `picker` | plain `diff` | sidecar written, parsed shape ≠ committed → claude's slash-command list drifted | re-record (parser proven sound) |
| `agents` | (passes) | — | do not touch |

The `mcp` symptom is the diagnostic one and must be resolved before its fixture can be re-recorded. The `picker` symptom is an ordinary content drift.

## Design

The fix is **diagnosis-gated**: the architect cannot run live `claude`. Deliverable 1 is a capture-and-branch procedure the developer runs at turn 1; Deliverable 2 is a small deterministic hardening so the *next* recurrence is self-diagnosing. Deliverable 3 is the re-record + verify.

### Deliverable 1 — diagnose the mcp missing-sidecar, then fix the one confirmed cause

**Capture step (honours the e2e-log hygiene rule — redirect to a file, grep only ASCII log lines; never tail raw PTY bytes into the terminal):**

- `make build-bin`
- Run the producer once for `/mcp`, capturing stderr to a file:
  `bin/spike-multiselect -trust-folder=accept -trigger=$'/mcp\r' -settle=5s 2>/tmp/mcp-diag.log`
  (the check uses `TUIDRIVER_STRICT_MCP_CONFIG=1`; export it too so the capture matches the gate).
- Grep only the structural lines: `grep -E 'modal-class detected=|no-parser-for-modal-class|parsed-json path=|picker-snapshot path=' /tmp/mcp-diag.log`.
- Confirm whether a `.parsed.json` was written (`parsed-json path=` line present?) and read the `.bin` with the existing unit-test path if you need to run `DetectModalClass` on the live bytes (a throwaway `go test`-style harness or a one-shot `go run` over the dumped `.bin`).

**Decision tree** — apply ONLY the branch the capture confirms. Each branch is a small, localised change; do not pre-emptively apply more than one.

- **A. `modal-class detected=` is empty / `no-parser-for-modal-class class=` present (Unknown, the most likely).** The naive `StripOSC(StripANSI)` text no longer contains `ManageMCPservers`. Inspect the stripped title region and compare to the anchor. **Fix:** in `modal.go`, add a spaced-form MCP anchor and OR it into the MCP case — mirroring the existing dual-anchor pattern at L64-70 (ask-user / permission / model-select all carry stripped + spaced variants). Contract: `DetectModalClass` returns `ModalClassMCP` for both the space-stripped and space-preserved renderings of the modal title. If the title text itself changed (not just spacing), update the anchor literal(s) to the current text. Keep the anchor a substring that cannot appear at idle or in other modals.
- **B. `modal-class detected=slash-picker`.** `findPickerRows` matched a `/<letter>` line inside the `/mcp` modal (e.g. a footer/URL/hint that strips to `/…`), and `ParsePicker` returned 0 items → no sidecar. **Fix:** tighten the picker row predicate or detection precedence so the `/mcp` modal's content does not satisfy `pickerRowStartRe`. Prefer the narrowest change that keeps all `TestDetectModalClassSyntheticAnchors` picker cases green.
- **C. `modal-class detected=mcp` (sidecar WAS written).** Then the symptom is not a missing sidecar but a content diff — re-record the mcp fixture (Deliverable 3) and confirm `ParseMcpStatus` output is sane. (Low probability given the observed `no such file`, but the capture decides.)
- **D. The `/mcp` modal does not render** (auth screen, different first screen, needs more settle). The capture's `modal-class` and the dumped `.bin` reveal this. If claude changed the `/mcp` UX structurally, this exceeds an S bug — **route back via `needs-rework:po`** with the captured evidence rather than expanding scope here.

After the confirmed fix, re-run the capture and confirm `parsed-json path=` now appears for `/mcp`.

**Constraint:** touch the classifier (`modal.go`) and, only if branch B, the picker predicate (`picker.go`). Do **not** modify `ParseMcpStatus` — it is proven sound and #81 forbids bundling parser-internals changes here. If diagnosis points at a parser regression (not a classifier/anchor issue), that is a separate ticket — route back.

### Deliverable 2 — make the missing-sidecar self-diagnosing (deterministic, evidence-based)

The cryptic `no such file` has now masked a classifier drift across three recurrences (#75/#83/#92 lineage → this). This is observed, not speculative, so a deterministic hardening is warranted (belt-and-suspenders with different fabric than the stochastic claude-drift it guards). Scope it tightly:

- In `cmd/e2e-snapshot-check/main.go`, add a regex sibling to `dumpPathRe` that scrapes `modal-class detected=(\S+)` from the captured `stderrBuf` (the buffer already exists, L100-102). Optionally also scrape `no-parser-for-modal-class`.
- When `os.ReadFile(parsedPath)` fails with a not-exist error (L119-121), enrich the `emitDiff` message to name the detected class, e.g.:
  `read parsed-json <path>: no such file — spike-multiselect classified the modal as %q and wrote no sidecar (classifier/modal drift, NOT a content diff; re-record will not fix this)`.
- Behaviour contract: exit code and stdout protocol unchanged (`SNAPSHOT <name> diff`); only the stderr `drift in …` line gains the detected-class suffix. When `modal-class detected=` is absent from stderr (producer crashed earlier), fall back to today's message verbatim. No new flags, no new exit codes, no report-schema change.

This is additive and ~8 lines. It does not change the read-then-compare logic, the record flow, or `cmd/e2e-runner`. It directly serves AC #2 ("the next recurrence is diagnosable, not re-triaged from scratch").

### Deliverable 3 — re-record the drifted fixtures + bump the version lock

Once Deliverable 1 makes both `/mcp` and `/` produce sidecars whose parsed shape is sane:

- `make rerecord-snapshots` regenerates all three `*-snapshot.json` (agents will round-trip unchanged; mcp + picker update).
- `git diff pkg/tuidriver/testdata/*.json` — review the parsed-shape deltas. For `picker`, confirm the delta is an honest reflection of claude's current command list (added/removed/reordered commands), not a parser artifact. For `mcp`, confirm the shape matches what `ParseMcpStatus` should produce under `TUIDRIVER_STRICT_MCP_CONFIG=1`.
- Bump `claude-version.lock` `version=` to the current `claude --version` token (per #64 policy: a fixture re-record IS a bump trigger). Do not touch `flag=`/`value=` lines.
- Commit the fixture refresh + lock bump together (the e2e-harness re-record discipline).
- If Deliverable 1 re-captured `mcp-snapshot.bin` (e.g. branch A required reconciling the unit-test regression fixture to the new rendering), commit it in the same change and confirm `TestDetectModalClassRealFixtures` + `TestParseMcpStatusRealFixture` stay green.

### Package layout

```
pkg/tuidriver/modal.go              # MODIFIED (likely): MCP anchor reconciliation (branch A) — a few lines
pkg/tuidriver/picker.go             # MODIFIED (only if branch B): tighten picker row predicate
cmd/e2e-snapshot-check/main.go      # MODIFIED: enrich missing-sidecar error with detected modal class (~8 lines)
pkg/tuidriver/modal_test.go         # MODIFIED: synthetic anchor case for the new MCP form (branch A)
pkg/tuidriver/testdata/mcp-snapshot.json    # RE-RECORDED (generated)
pkg/tuidriver/testdata/picker-snapshot.json # RE-RECORDED (generated)
pkg/tuidriver/testdata/mcp-snapshot.bin     # RE-CAPTURED only if branch A reconciles the unit-test fixture
claude-version.lock                 # MODIFIED: version= bump (1 line)
```

No new packages, no new exported types, no signature changes, no `cmd/e2e-runner` change, no Makefile change (`rerecord-snapshots` already exists).

### Concurrency model

Unchanged. `e2e-snapshot-check` runs its three `spike-multiselect` invocations serially; `spike-multiselect` runs its own reader/watchdog goroutines internally. No new goroutines, channels, or shared state introduced by any deliverable.

### Error handling

Deliverable 2 *improves* one row of the #81 error table (the `<dump>.parsed.json missing` row) — same `diff` verdict, same exit code, richer message. All other rows of that table (capture failed, stderr lacks the dump-path line, fixture missing/malformed, mismatch) are unchanged. The invariant holds: the check always exits with a real exit code; failures degrade to per-fixture `diff`; the aggregate exit code summarises.

## Testing strategy

1. **Unit (mandatory, fast, no claude):** after a branch-A anchor change, `go test ./pkg/tuidriver/ -run 'TestDetectModalClass'` — both the synthetic-anchor case for the new MCP form and `TestDetectModalClassRealFixtures` must pass. After any `picker.go` change (branch B), the picker synthetic cases and false-positive guards (`hint-bar text alone`, `slash mid-line`) must stay green.
2. **Producer capture (mandatory):** the Deliverable-1 capture, re-run post-fix, shows `parsed-json path=` for `/mcp` and `/`.
3. **Integration green path:** `make e2e` reports `snapshot-drift -> pass` with all three `snapshots[]` entries `result: match` in `e2e-report.json`. The PR description states the confirmed root cause for each of the two drifted fixtures (AC #2) and pastes the re-recorded parsed JSON for mcp + picker.
4. **Determinism (AC #3):** `bin/e2e-snapshot-check && bin/e2e-snapshot-check` — both exit 0 from a clean state. A single pass is insufficient. If the second run diffs, the parser has a non-deterministic input filter → separate ticket, do not relax the comparison (#81 policy).
5. **Forced-failure smoke for Deliverable 2:** temporarily point the mcp trigger at a class with no parser (or hand-break the anchor) and confirm the enriched stderr names the detected class instead of the bare `no such file`. Revert.
6. **No regression in the other nine checks (AC #3):** the full `make e2e` run is the witness — confirm the eight spikes/probe + claude-version-lock still pass.

Tests are written by the developer in the project idiom; scenarios above are the behaviours to assert, not code to paste.

## Open questions

1. **CI-vs-local picker portability (flag, do not solve here).** The committed `picker-snapshot.json` contains operator/plugin-specific commands (`/figma-use [figma]`, `/walkthrough`, `/logo-creator`, `/dependency-graph`, `/advisor`, `/chrome`, …). A fresh CI runner (`npm i -g @anthropic-ai/claude-code`, no operator plugins) would render a *different* `/` picker, so a fixture re-recorded on the operator box may still diff on CI. The baseline-comparison evidence shows the failure on the operator's own box too, so re-recording there restores the *local* gate regardless. Whether the picker fixture is fundamentally host-portable is a pre-existing tension in #81's design (parsed shape is stable against renderer bytes, but the *content* is environment-dependent). If the developer finds the gate must pass on both CI and local with divergent command sets, that is a #81 follow-up (e.g. scope the picker fixture to built-in-only commands, or make it CI-host-recorded) — **route back via PO**, do not expand this ticket.
2. **`mcp` fixture shape under strict-mcp.** The committed fixture is `total_servers: 1` with a built-in `computer-use` (disabled, highlighted) — strict-mcp suppresses configured servers but not the built-in. The re-record should reflect whatever the current claude renders under `TUIDRIVER_STRICT_MCP_CONFIG=1`; commit what `-record` produces, do not predict it. Confirm no operator-specific path leaks into `categories[].path` (AC #4).
3. **Anchor form precision (branch A).** If the live `/mcp` title is, e.g., `Manage MCP servers` with real spaces, prefer adding the spaced variant `Manage MCP servers` alongside the existing `ManageMCPservers` rather than replacing it — keeps both renderings classified and avoids re-reddening if a future claude version flips back to the space-stripped form.

## Sizing

S, no split. Two co-located symptoms in one domain; production source files touched ≤ 3 (`modal.go`, `e2e-snapshot-check/main.go`, and `picker.go` only in the unlikely branch B); fixtures and lock are generated/config, not counted. No new files, no new exported types, zero consumer call sites, 4 ACs. A split would produce two sub-tickets that both edit the fixtures and the check — guaranteed merge conflict — for no benefit. The only route-back triggers are branch D (claude restructured the `/mcp` UX) or a confirmed parser-internals regression (#81 forbids bundling it); both surface only after the live capture, and the developer should route back via `needs-rework:po` if either fires.

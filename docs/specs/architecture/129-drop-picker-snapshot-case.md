# Spec #129 — Drop the `/` picker case from snapshot-drift

**Ticket:** [#129](https://github.com/pyrycode/tui-driver/issues/129) — snapshot-drift: make `/picker` case host-portable (scope to built-in commands). Split from #127. Size **XS** (`size:xs`, deletion-dominated).

**One line:** Remove the `picker` fixture from the snapshot-drift e2e check and delete its committed parsed-shape JSON, so `make e2e` is green on `main` and on a clean CI runner. No re-record, no new logic.

---

## Files to read first

The developer needs all five of these on turn 1; everything in the Design section references them.

- `cmd/e2e-snapshot-check/main.go:20-29` — the stdout-protocol doc comment listing `SNAPSHOT picker|mcp|agents`. One of the lines to drop.
- `cmd/e2e-snapshot-check/main.go:60-77` — the `fixture` struct (`name` field comment) and the `fixtures := []fixture{…}` table. The `{name: "picker", trigger: "/", settle: 0}` entry (line 74) is the central removal.
- `cmd/e2e-snapshot-check/main.go:48-50` — `dumpPathRe` / the literal `picker-snapshot path=…` label. **LEAVE ALONE** — it's the spike's generic dump-path label reused by *every* trigger (mcp/agents too), not the picker fixture. Read it only so you don't remove it by name-association.
- `cmd/e2e-runner/main.go:49-51` — `snapshotResultRe` regex (`^SNAPSHOT (picker|mcp|agents) (match|diff)$`). The `picker|` alternation is the second, un-flagged-by-the-ticket reference — see Design § 2.
- `cmd/e2e-runner/main_test.go:11-90` — `TestParseSnapshotResults` table. Four of its sub-cases feed `SNAPSHOT picker match` lines and expect a `picker-snapshot.json` entry; these are the test updates.

Supporting / do-not-touch (read only enough to confirm they're unaffected):
- `cmd/e2e-snapshot-check/main_test.go:9-64` — `TestEnrichMissingSidecar`. The `"slash-picker class scraped"` sub-case (line 27) tests `enrichMissingSidecar` / `modalClassRe`, independent of the fixture table. **Keep it; do not edit.**
- `pkg/tuidriver/picker_test.go:40-53` — `TestParsePickerRealFixture` reads `picker-snapshot.bin` (the byte fixture, **retained**), not the `.json`. Confirms parser coverage survives the `.json` deletion.
- `pkg/tuidriver/modal_test.go:111-116` — `DetectModalClass` table reads `picker-snapshot.bin` + `picker-truecolor-snapshot.bin` (both **retained**). Confirms modal-class coverage survives.
- `Makefile:31` — the `rerecord-snapshots` comment says "three snapshot-drift JSON fixtures"; goes stale to two (Design § 5).

---

## Context

snapshot-drift's `picker` case is **red on `main`** as a plain parsed-shape `diff` — no error; the sidecar is written and the parser is sound. The committed `picker-snapshot.json` embeds **operator plugin/skill** commands (`/code-review`, `/figma-use`, `/walkthrough`, …) that are not claude built-ins, so it diffs whenever the operator's plugin set changes (the `main` failure) and would diff on a clean CI runner (no plugins). This is spec [#81](81-snapshot-drift-parsed-shape.md)'s host-portability tension surfacing as a hard failure.

The "scope to built-ins only" approach was investigated live against claude 2.1.158 and ruled out — no host-portable built-in-vs-plugin discriminator exists (`category` is empty on most operator skills; no flag yields a built-ins-only picker; a name allowlist breaks on the scroll window). Full detail is in the ticket body's Context section; the conclusion is the chosen approach below.

This change rests on a precondition already satisfied: **#128 has merged** (`main` includes the merge of `feature/128`, PR #130), so the `mcp` case is now `match` (strict-mcp empty-state) and `agents` is `match`. `picker` is the sole remaining red. Removing it makes the aggregate `snapshot-drift` check pass on `main`.

## Chosen approach

**Drop the `picker` case from the snapshot-drift check and delete its JSON fixture.** No re-record (there is no host-portable scoped set to record — confirmed live), no relaxation of the exact-equal parsed-shape policy (#72/#81). Picker *parser* coverage is unchanged: it stays exercised by `TestParsePickerRealFixture` and the `DetectModalClass` modal-class table against the retained host-independent `.bin` fixtures. The snapshot-drift check keeps end-to-end coverage via its `mcp` and `agents` cases.

---

## Design

The whole change is removal + two consistency edits. There is no new code, no new type, no behaviour added. `parseSnapshotResults` (`cmd/e2e-runner/main.go:306`) derives each fixture path generically from the `SNAPSHOT <name>` line, so once the table entry is gone the runner needs nothing structural — only the regex whitelist and the test expectations follow.

### 1. Remove the `picker` fixture entry and its doc-comment references — `cmd/e2e-snapshot-check/main.go`

- Delete the table row `{name: "picker", trigger: "/", settle: 0}` (line 74). The table becomes the two remaining rows (`mcp`, `agents`), in that order.
- In the stdout-protocol doc comment (lines 20-25), drop the `//\tSNAPSHOT picker match|diff|recorded` line. The comment now documents two lines (`mcp`, `agents`).
- In the `fixture` struct, update the `name` field comment (line 61) from `// "picker" | "mcp" | "agents"` to `// "mcp" | "agents"`.
- **Do not touch** `dumpPathRe` or the `picker-snapshot path=` string literal (lines 48-50) — that label is trigger-agnostic and reused by the surviving fixtures.

**Contract preserved:** the binary still emits exactly one `SNAPSHOT <name> match|diff` line per surviving fixture, in table order, and exits 0 iff all matched. No change to `runFixture`, `emitDiff`, `enrichMissingSidecar`, or the `-record` path.

### 2. Narrow the result regex to the emitted set — `cmd/e2e-runner/main.go:51`

Remove `picker` from the alternation:

```
^SNAPSHOT (picker|mcp|agents) (match|diff)$   →   ^SNAPSHOT (mcp|agents) (match|diff)$
```

**Why this, despite the ticket's "no change to `parseSnapshotResults`" note.** That note is about the *function body*, which genuinely needs no change — it's path-derivation that's generic over the captured name. `snapshotResultRe` is a separate package-level var: a whitelist of the fixture names the protocol can emit. After § 1 the check binary can never emit `SNAPSHOT picker …`, so the `picker|` alternation is dead. Leaving it is functionally inert (it would only ever match a line that's no longer produced), but it is a stale, misleading enumeration — a future reader would infer a picker fixture still exists. Removing it keeps the regex an honest description of the live protocol and is a 7-character deletion entailed by the same logical change, not adjacent refactoring. This is the one place a literal reading of the Technical Notes would leave a loose end; resolve it.

### 3. Update `TestParseSnapshotResults` — `cmd/e2e-runner/main_test.go`

Drop the picker line from each sub-case's `stdout` and the corresponding `picker-snapshot.json` entry from each `want`. As bullet scenarios (developer writes them in the existing table idiom):

- **"all match"** — stdout: `mcp match` + `agents match`; want: `[mcp match, agents match]`.
- **"mixed match and diff"** — stdout: `mcp diff` + `agents match`; want: `[mcp diff, agents match]`.
- **"noisy stdout with SNAPSHOT lines interspersed"** — drop the `SNAPSHOT picker match` line from the noisy stdout; want: `[mcp diff, agents match]` (keep the surrounding noise lines).
- **"partial output (timeout mid-run)"** — re-model as a genuine partial over the two-fixture set: stdout = `SNAPSHOT mcp match` only; want: `[mcp match]` (run cut off before `agents`). The scenario's point is that truncated output still parses — preserve that, don't delete the case.
- **"empty stdout"** and **"no SNAPSHOT lines amid noise"** — unchanged (no picker reference).

No assertion-helper or signature changes; same `reflect.DeepEqual` comparison.

### 4. Delete the JSON fixture

- `git rm pkg/tuidriver/testdata/picker-snapshot.json`.
- **Retain** `pkg/tuidriver/testdata/picker-snapshot.bin` and `pkg/tuidriver/testdata/picker-truecolor-snapshot.bin` — they're unit-test byte fixtures (host-independent), consumed by the picker-parser and modal-class tests in § 6. Deleting the `.json` must not touch them.

### 5. Makefile comment — `Makefile:31`

The `rerecord-snapshots` recipe comment reads "Re-record the **three** snapshot-drift JSON fixtures…". Change `three` → `two`. This is the only Makefile edit — a stale-count comment in a build file directly falsified by § 4. Recipe body (`e2e-snapshot-check -record`) is unchanged: `-record` iterates whatever the fixture table now holds.

---

## Out of scope / do not touch

- **`docs/knowledge/features/e2e-harness.md`** — this feature doc says "three fixtures" (§ "Check kinds", "Files") and shows the picker entry in its report-schema example (line 143). It **is** stale after this change, but updating it is a **documentation-phase deliverable, not a developer AC** — per the pipeline rule that the developer's worktree mutates only code, tests, and this spec file. The documentation agent rewrites it post-merge from this spec + the merged diff. See the Documentation handoff note below; do not edit it in the feature branch.
- **Historical specs** `81-…`, `109-…`, `128-…` reference `picker-snapshot.json` as frozen records (format examples, coordination notes). They are immutable history — do not edit.
- **`cmd/e2e-snapshot-check/main_test.go`** — the `"slash-picker class scraped"` case tests `enrichMissingSidecar`, not the fixture table. Keep it verbatim.
- **`claude-version.lock`** — this ticket does **not** re-record any fixture, so there is no lock bump (the lock bump travels only with a re-record sweep; #128 already handled the 2.1.158 context). Leave it.
- **`dumpPathRe` / `picker-snapshot path=` label** — generic, reused by mcp/agents. Leave it.

---

## Error handling

No new failure modes. The snapshot-drift exit-code contract is unchanged: exit 0 iff every (now two) fixture matched, else 1. Per-fixture capture failures still degrade to `SNAPSHOT <name> diff` + a stderr `drift in <path>` line via the untouched `emitDiff` / `enrichMissingSidecar` paths. The runner's `parseSnapshotResults` still emits a `snapshots[]` entry per emitted line on both pass and fail; the list is simply two entries instead of three.

## Testing strategy

- **Unit (changed):** `go test ./cmd/e2e-runner/` — the updated `TestParseSnapshotResults` asserts the two-fixture shape and that the picker entry no longer appears.
- **Unit (regression, must stay green untouched):** `go test ./pkg/tuidriver/` — `TestParsePickerRealFixture` and the `DetectModalClass` modal-class table prove picker-parser + modal-class coverage survive the `.json` deletion (they read the retained `.bin` fixtures). These are the evidence for AC "coverage preserved".
- **Build:** `go vet ./...` / `go build ./...` — confirms the regex edit and table removal compile (no dangling references to the removed name).
- **Manual smoke (operator, optional — needs a real claude):** `make e2e` on this branch. Expect the report's `snapshot-drift` entry to list exactly `mcp` + `agents`, no `picker` line on stdout, and the aggregate check green (given #128 merged). Re-running `make rerecord-snapshots` should regenerate only `mcp` + `agents` JSON.

## Documentation handoff (post-merge, documentation phase — NOT a developer task)

For the documentation agent's reference, the `e2e-harness.md` edits this change implies:
- "three fixtures" → "two" wherever the count appears (§ "Check kinds" `snapshot` bullet; § "Files" `{picker,mcp,agents}` listings → `{mcp,agents}`).
- Remove the `picker-snapshot.json` row from the report-schema example (line 143) and from the § "Re-recording snapshot fixtures" listing.
- Note that the picker case was dropped because the picker render is irreducibly host-dependent (operator plugin/skill commands leak in; no host-portable built-in-vs-plugin discriminator exists — confirmed live against claude 2.1.158, resolving spec #81's picker Open Question), and that picker *parser* coverage is retained in `pkg/tuidriver/` unit tests against the `.bin` fixtures.

## Open questions

None blocking. The one design judgment — removing `picker|` from `snapshotResultRe` rather than leaving it inert (§ 2) — is resolved in favour of an honest protocol whitelist; the fallback (leave it, functionally identical) is documented there if the developer prefers a more minimal diff, but the recommended path is removal for the reasons given.

---

## Acceptance criteria (developer deliverables)

1. `cmd/e2e-snapshot-check/main.go`: the `picker` fixture-table entry is removed; the `SNAPSHOT picker …` protocol-doc line and the `name`-field comment are updated to the two-fixture set; `dumpPathRe` is unchanged.
2. `cmd/e2e-runner/main.go`: `snapshotResultRe` no longer includes `picker` in its name alternation.
3. `pkg/tuidriver/testdata/picker-snapshot.json` is deleted; `picker-snapshot.bin` and `picker-truecolor-snapshot.bin` are retained.
4. `cmd/e2e-runner/main_test.go`: `TestParseSnapshotResults` updated to the two-fixture shape (no `picker-snapshot.json` expectation in any sub-case); all sub-cases pass.
5. `Makefile`: the `rerecord-snapshots` comment count is `two`.
6. `go test ./cmd/e2e-runner/ ./pkg/tuidriver/` passes — including the untouched `TestParsePickerRealFixture` and `DetectModalClass` modal-class regression tests, proving picker-parser/modal-class coverage is preserved.
7. The PR description states the root cause (operator plugin/skill commands leak into the picker render; no host-portable built-in-vs-plugin discriminator exists — confirmed live against claude 2.1.158, resolving spec #81's picker Open Question) and notes parser coverage is retained in unit tests.

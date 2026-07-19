# #296 — slash-picker block co-signal: reject single-row bottom-region forgeries

**Ticket:** [#296](https://github.com/pyrycode/tui-driver/issues/296) · **Size:** S · **Labels:** `bug`, `security-sensitive`
**Predecessor:** #237 (region-scoped the `/`-row signal) · **Sibling shape:** #242 / #244 (shape-over-literal co-signals)

## Files to read first

- `pkg/tuidriver/picker.go:288-333` — `gridHasPickerRow`, `isSlashPicker`, `isSlashPickerWithGrid`. This is the entire production surface to change. Extract: the current `gridHasPickerRow(g) && snapHasPickerHighlight(snap)` shape and the `pickerRegionRows`/`g.LastRows` region-scope idiom (`:296-303`).
- `pkg/tuidriver/picker.go:78` — `pickerRowStartRe = ^[ \t\r]*/[a-zA-Z]`. The row anchor the new count reuses verbatim. Do not add a second regex.
- `pkg/tuidriver/picker.go:159-190` — `snapHasPickerHighlight`. The chrome half. It stays **unchanged** (see Design § "What does NOT change").
- `pkg/tuidriver/modal.go:250-314` — `DetectModalClass` / `detectModalClassWithGrid`. Extract: the slash-picker arm is last-resort (`:310`), and the doc comment at `:257-273` describes each class's co-signal; the picker paragraph gets a one-line doc touch.
- `pkg/tuidriver/anchor_forgery_test.go:340-397` — `TestSlashPickerRejectsBodyForgery`. The non-vacuity idiom to mirror (`:374` asserts the weak signal IS present), and the **single-row positive control at `:390` that must migrate to a multi-row block**.
- `pkg/tuidriver/anchor_forgery_test.go:274-338` — `TestPermissionsConfigTabForgeryRejectedByRowBoundColor`. The #244 non-vacuity template: assert both pre-fix weak signals present, then assert the new structural co-signal rejects. Copy this shape.
- `pkg/tuidriver/picker_test.go:172-195` — `TestIsSlashPicker` truth table. Case `"row + chrome"` (`:182`) is a **single `/`-row expecting `true`** → must migrate to a 2-row block.
- `pkg/tuidriver/modal_test.go:62-73` and `:262-273` — four single-row synthetic pickers asserting `SlashPicker` that must migrate to blocks (see Testing § "Existing assertions that flip").
- `pkg/tuidriver/modal_test.go:283-296` — the `ParsePicker` single-item sub-test. It stays **green and untouched** (AC4: `ParsePicker` behaviour unchanged; classification-gated, not count-gated).
- `docs/knowledge/codebase/244.md` — the sibling precedent's "Patterns established" and "Lessons learned": reuse the *shape* not the function; check every committed positive control's row layout before picking the binding granularity; non-vacuity idiom.
- **Measured fixture facts** (rendered this run, `NewGrid(snap,0,0).LastRows(pickerRegionRows=6)`):
  - `picker-snapshot.bin`: **5** bottom-region `/`-rows (rows 0–4; row 5 is a wrapped description continuation, not a `/`-row).
  - `picker-truecolor-snapshot.bin`: **6** bottom-region `/`-rows (all 6).
  - Both: `snapHasPickerHighlight == true`. Both classify with margin under a ≥2 threshold.

## Context

#237 region-scoped the slash-picker `/`-row signal to the bottom input window (`pickerRegionRows = 6`), killing transcript-body forgeries (a `/`-line scrolled up in the conversation). A residual class survives **inside** that region, so region-scoping cannot reach it:

- A long shell-command output line wrapping so its **continuation** begins `/Users`, `/Workspace`, `/dev`, `/gradlew`, … lands in the bottom region.
- claude's own footer tip wrapping to a line beginning `/btw` (`Tip: Use /btw to ask a quick side question…`).

Each takes the exact shape the current detector accepts: **one** bottom-region rendered row beginning `/<word>` (`gridHasPickerRow`, ≥1) plus a picker-highlight shade anywhere on the frame (`snapHasPickerHighlight`). Those two ANDed are `isSlashPicker`'s whole test. The highlight shade (xterm-256 index 153 and its truecolor twin) is a general light blue claude paints on paths, links and markers — present on most frames — so the chrome half is nearly free; the `/`-row is the only real discriminator, and a **single** wrapped continuation line forges it.

Agent runs are headless — no human types `/` — so every `slash-picker` fire in an agent run is a false positive. corpus-replay (#227) confirms it at HEAD: `slash-picker` fires in 4/4 production-`ok` runs, each with a flapping signature (~4 transition edges), corpus-replay's own spurious-modal marker (a real dialog fires once, stays at 1). Surfaced by the #257 labeling exercise: the model judge labeled these screens `busy` while the detector said `slash-picker`.

**Key structural observation** (measured): row [2] of `picker-snapshot.bin` is literally `/btw   Ask a quick side question…` — the real picker *contains* the very command the footer-tip forgery quotes. So the discriminator cannot be a literal any line can carry; it must be the *shape*: a real picker is a **list** of command rows; a forgery is a **single** stray `/`-line.

## Design

Add a **structural co-signal on top of** #237's region scope (it does not replace it): the region scope bounds *where* the `/`-row may match; the new co-signal bounds its *shape* — a real picker paints a **block** of command rows, not one line. This is the #242/#244 shape-over-literal posture applied to the slash-picker's row half.

### The change (one production file: `pkg/tuidriver/picker.go`)

**New constant** — the block threshold, with the rationale in its doc comment:

```
const pickerRowBlockMin = 2
```

Doc comment must record: a real picker's command list is a block (the two committed captures carry 5 and 6 rows in the bottom `pickerRegionRows` window); a residual forgery is a single stray `/`-row (a wrapped `/Users…`/`/gradlew…` continuation, or the `/btw…` footer tip); requiring ≥2 rejects the single-row forgery while both real captures classify with margin. It **consciously drops** single-match filtered-picker detection (one command row) — see § "Dropped: single-match detection".

**New helper `gridPickerRowCount(g *Grid) int`** — counts rendered rows within the bottom `pickerRegionRows` window matching `pickerRowStartRe`. Region-scoped exactly like today's `gridHasPickerRow` (reads #150's `Grid` via `g.LastRows(pickerRegionRows)`, so off-screen history never counts). Reuses `pickerRowStartRe` — no new regex.

**Retain `gridHasPickerRow`, re-expressed as `gridPickerRowCount(g) >= 1`.** It is no longer the production gate, but the non-vacuity regression asserts it (the weak "there IS a bottom-region `/`-row" signal). Keeping it means the negative tests prove the *block* requirement — not a missing row or missing colour — is what rejects the forgery.

**New helper `gridHasPickerRowBlock(g *Grid) bool`** = `gridPickerRowCount(g) >= pickerRowBlockMin`. This is the new structural gate.

**Rewire `isSlashPickerWithGrid`** — swap the row-half gate from `gridHasPickerRow(g)` to `gridHasPickerRowBlock(g)`; the chrome half (`snapHasPickerHighlight(snap)`) is unchanged. Signature is byte-for-byte identical → **zero caller fan-out** (`isSlashPicker` and `detectModalClassWithGrid:310` call it unchanged).

Resulting contract: `isSlashPickerWithGrid(g, snap) == gridHasPickerRowBlock(g) && snapHasPickerHighlight(snap)`.

### The doc touch (`pkg/tuidriver/modal.go`)

One-line update to the slash-picker paragraph of `detectModalClassWithGrid`'s doc comment (`:257-273` / `:305-309`): the picker now requires a bottom-region `/`-row **block** (`gridHasPickerRowBlock`, ≥`pickerRowBlockMin`) plus chrome, so a single stray `/`-continuation in the input region no longer forges it (#296). Symbol reference only — no literal command words in prose (the arc's self-reference discipline, #244).

### What does NOT change

- `snapHasPickerHighlight` — the chrome half stays as-is. Swapping it to `snapHasRowOpeningHighlight` (the #244 full-panel move) would **not** help here: a real path *is* painted in the highlight shade, so a wrapped `/Users…` continuation can genuinely *open* its line in the shade — the row-opening check wouldn't reject it. The block count is what does the work; touching chrome adds risk for no gain, and would widen scope past one concern.
- `findPickerRows` / `ParsePicker` — untouched. `ParsePicker` is deliberately permissive and classification-gated (it is only called after `DetectModalClass` confirms a picker), so its single-item behaviour is unchanged (AC4). No new dependency (AC4).
- `pickerRegionRows`, `pickerRowStartRe`, `pickerRowOpenColor` — untouched (reuse the shape, not modify the function — #244 lesson).

### Dropped: single-match detection (documented rationale)

A filtered picker narrowed to exactly **one** command row is structurally identical to a single-row forgery — there is no signal that separates "one real command row" from "one wrapped path." The ≥2 threshold therefore cannot classify it. This is the ticket's flagged positive-control tension, resolved by **consciously dropping** single-match detection, because:

1. The consumer (`pyry agent-run`) is headless — no human types `/`, so a real picker (single-match or otherwise) **never** appears in an agent run; the dropped case has no consumer.
2. The observed residual is exactly a single `/`-row; ≥2 is the minimal separation from it (evidence-based: no wider defense than the observed failure needs).
3. The firm positive controls survive: both real multi-row `.bin` captures (5 and 6 rows) and any genuine filtered picker with ≥2 matches still classify `slash-picker`.

Threshold is **2, not 3**: 2 is the direct encoding of "a list has more than one item; the forgery has one." Both real captures (5, 6) clear 3 as well, but 3 would defend an *unobserved* two-row coincidence at the cost of also dropping genuine two-match pickers — over-engineering past the evidence.

Count is **non-contiguous** (any 2 `/`-rows in the window, not 2 adjacent). Contiguity was considered and rejected: `picker-snapshot.bin` already has a wrapped description (row 5) breaking its block, and at narrower PTY widths the long descriptions wrap and interleave with command rows — a contiguity requirement would grow fragile against real pickers. A non-contiguous count survives description wrapping; the observed forgeries are single-row and rejected regardless.

## Concurrency model

None. Pure synchronous classification over an in-memory byte slice + rendered `Grid`. No goroutines, channels, or shared state introduced. `DetectModalClass`/`isSlashPicker` remain re-entrant and allocation-light (one linear pass over the bottom region).

## Error handling

No new failure modes. `gridPickerRowCount` returns 0 on an empty/zero `Grid` (`g.LastRows` over an empty grid yields no rows), so `isSlashPicker(nil) == false` (already covered by `TestIsSlashPicker`'s empty case). The change only narrows the set of inputs that return `true`; it cannot panic or introduce a new error path.

## Testing strategy

Deterministic, claude-free (`make check` / `go test -race ./...`). Fixtures are hand-built in Go via the existing `gridRows` helper (the `anchor_forgery_test.go` idiom), never as `.bin` bytes with raw ESC — Go `json` rejects `0x1b` (the arc's ESC-escaping landmine).

### New negative-guard fixtures (AC1 + AC2) — add to `anchor_forgery_test.go`

A new test (mirror `TestPermissionsConfigTabForgeryRejectedByRowBoundColor`'s two-part shape) covering the residual trigger tokens as **single** bottom-region `/`-rows with chrome present:

- Path forms: continuation rows beginning `/Users`, `/Workspace`, `/dev`, `/gradlew`.
- Footer-tip form: a row beginning `/btw …` (the wrapped-tip continuation).
- Each fixture: exactly **one** `/`-row placed **inside** the bottom region (e.g. as the last row, with a few non-`/` rows around it — the opposite of #237's fixtures, which pushed the `/`-line *above* the region with 20 filler rows), and a picker-highlight escape (`\x1b[38;5;153m`) somewhere on the frame.

Per fixture, assert (scenarios, not code):
- **Non-vacuity — both pre-fix weak signals ARE present:** `gridHasPickerRow(NewGrid(snap,0,0)) == true` (a bottom-region `/`-row exists) **and** `snapHasPickerHighlight(snap) == true` (chrome exists). If either is false the forgery contrast is void → `t.Fatal` (mirrors `:374`). This proves the new *block* co-signal — not a missing colour or an out-of-region row — is the rejecter.
- **Sanity — count is exactly the single-row shape:** `gridPickerRowCount(...) == 1` (in-region but not a block), so the block gate is not passing vacuously on zero rows.
- **Rejected:** `isSlashPicker(snap) == false` **and** `DetectModalClass(snap) != ModalClassSlashPicker`.

### Positive controls (AC3) — the fix must not silence a real picker

- **Untouched, firm control:** `picker-snapshot.bin` and `picker-truecolor-snapshot.bin` (5 and 6 rows) stay `ModalClassSlashPicker` in `TestParsePickerRealFixture` / `TestDetectModalClassRealFixtures` / `TestPickerAbsolutePathForgeryNotClassified`. Do not edit these.
- **`TestSlashPickerRejectsBodyForgery` stays green:** its three body-forgery forms (pushed above the region) still reject via region scope. Its **single-row positive control at `:390`** (`hl+"/figma-use …"`) must migrate to a **two-row block** (two bottom-region `/`-command rows) so it still asserts `SlashPicker` — proving the region scope + block together admit a real in-region picker.

### Existing assertions that flip (the positive-control tension) — must migrate to blocks

Under ≥2, six single-`/`-row synthetic positives change verdict. Each migrates to a ≥2-row block to keep asserting `SlashPicker` (a real picker is multi-row):

1. `anchor_forgery_test.go:390` — `live` control → 2-row block (covers `:391` `isSlashPicker` and `:394` `DetectModalClass`).
2. `picker_test.go:182` — `TestIsSlashPicker` `"row + chrome"` case → 2-row block input (keeps `want: true`).
3. `modal_test.go:65-67` — `"slash-picker filtered (multiple colors mid-row)"` → add a second `/`-row.
4. `modal_test.go:69-72` — `"slash-picker truecolor highlighted row"` → add a second `/`-row.
5. `modal_test.go:262-268` — `"single-match picker with truecolor chrome is a picker"` → 2-row block (or restate; see below).
6. `modal_test.go:269-273` — `"single-match picker with indexed chrome is a picker"` → 2-row block (or restate).

**Also add one explicit pin of the conscious drop** (so the behaviour change is documented, distinct from the path/tip forgeries): a case asserting that a lone `/<cmd>`-row + chrome (single-match shape) is **NOT** a picker under #296. Cases 5/6 above are the natural home — rename to "single-match picker is dropped (#296)" and flip `want` to `ModalClassUnknown`, keeping the `ParsePicker`-returns-1-item sub-test at `:283-296` unchanged (ParsePicker is unaffected — AC4).

### AC4 / AC5

- `TestParsePickerRealFixture`, `TestParsePickerHypotheticalThirdEncoding`, `TestSnapHasPickerHighlight`, and the `ParsePicker` single-item sub-test stay green untouched — `findPickerRows`/`ParsePicker` behaviour is unchanged, no new dependency.
- `go test -race ./...` and `go vet ./...` pass.

## Open questions

- **Threshold 2 vs 3 and non-contiguity** are decided in Design (evidence-based: 2, non-contiguous). If code-review or a future corpus-replay sweep surfaces a *two-row* in-region forgery (not observed today), revisit — but that is a new ticket, not a pre-emptive widening here.
- **Naming:** `gridHasPickerRowBlock` / `gridPickerRowCount` / `pickerRowBlockMin` are proposed; the developer may pick equivalent names that read like the surrounding `gridHas*` helpers. The contract (region-scoped count ≥ threshold) is what's fixed.

## Security review

**Verdict:** PASS

**Findings:**

- **[Trust boundaries]** No MUST FIX. The untrusted input is claude's attacker-influenceable rendered PTY output; it crosses into trusted state at the single explicit boundary `DetectModalClass` / `isSlashPickerWithGrid` (`pkg/tuidriver/picker.go:328`, `modal.go:288`). The change tightens that boundary (narrows what classifies as picker); it introduces no new boundary and holds no new data downstream. The classifier output stays a value-typed `ModalClass` enum — no trusted/untrusted confusion for callers.
- **[Threat model alignment]** No MUST FIX — this is the category that matters here. The security concern for a modal classifier is that attacker-controlled output drives an auto-answer of a permission/trust prompt. It does not apply to this arm: (1) `slash-picker` is the **last-resort** arm — MCP/Trust/Permission/ModelSelect/PermissionsConfig/AskUser are all matched first (`detectModalClassWithGrid:289-304`), so tightening slash-picker cannot promote an input into Permission/Trust nor demote a real Permission modal (it wins earlier). (2) `AnswerModal` dispatches keystrokes only for Permission/TrustFolder (#151); a `slash-picker` verdict routes no keystroke. The only state transition this change can cause is `SlashPicker → Unknown` (Unknown is terminal, no arm follows), which routes nothing. Net: the change **removes** a false-positive class; it cannot re-route a real permission modal.
- **[Network & I/O — resource exhaustion]** No findings. The new work (`gridPickerRowCount`) is a bounded linear scan of the bottom `pickerRegionRows` (6) rendered rows only — strictly less work than the existing full-grid render it piggybacks on. No new `Read`, no unbounded input, no new allocation growth; buffer capping remains upstream (`DefaultBufferCap`, unchanged).
- **[Error messages / logs / telemetry]** No findings. No logging, error text, or telemetry added; the classifier returns an enum. Nothing from the untrusted frame is emitted.
- **[Concurrency]** No findings. `isSlashPickerWithGrid` / `gridPickerRowCount` stay pure, synchronous, re-entrant, allocation-light. No goroutines, channels, locks, or shared state introduced.
- **[File operations / Subprocess / Cryptographic primitives]** Not applicable by design — the change is pure in-memory classification over an existing byte slice; it touches no filesystem path, spawns no process, and uses no randomness or crypto.
- **[Residual forgery surface]** OUT OF SCOPE (nuisance-level, no privilege). A hostile actor with full control of rendered output can still forge a `slash-picker` by printing ≥`pickerRowBlockMin` `/<cmd>`-shaped rows plus a highlight shade in the bottom region. Impact is a spurious `slash-picker` state → turn-end suppression — a liveness/DoS nuisance, not keystroke injection or permission bypass (see Threat model above); a stuck turn is additionally covered by the PTY-quiet and JSONL-stall watchdog arms. This change strictly *shrinks* the pre-existing forgery surface (single-row no longer suffices); fully closing multi-row forgery is unobserved (corpus-replay #227 shows only single-row triggers today) and belongs to a future ticket if it ever surfaces — not a pre-emptive widening here (evidence-based fix selection).

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-07-19

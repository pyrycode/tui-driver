# #151 — Rendered-grid refactor 2/6: port slash-picker detection to the grid, require chrome, check modals first

Slice 2/6 of the rendered-grid refactor. Builds on #150's `Grid` accessor
(merged `8cc1aa4`). Fixes CRITICAL A from the Cross-Repo Code Review 2026-07-03:
`DetectModalClass` runs the slash-picker check first, and `findPickerRows`
treats **any** `/`+letter line as a picker row, so an absolute path
(`/Users/x/file.go`) on screen misclassifies a real permission modal as the
picker (→ auto-answer never fires → hang) and phantom-pickers a lone `/path` at
idle (→ turn-end suppressed).

## Files to read first

- `pkg/tuidriver/modal.go:97-131` — `DetectModalClass`. The picker-first check
  (`modal.go:98`) moves to **last**; the anchor `switch` keeps its internal
  order. This is the reorder half of the fix.
- `pkg/tuidriver/modal.go:33-82` — anchor definitions + the doc comments at
  lines 48, 55-62 that describe slash-picker as a pure structural anchor. Those
  comments go stale after this slice and must be updated (chrome is now
  required).
- `pkg/tuidriver/picker.go:71-121` — `pickerRowStartRe` (`^[ \t\r]*/[a-zA-Z]`)
  and `findPickerRows`. **`findPickerRows` and its raw-byte color path stay
  untouched** — it is `ParsePicker`'s row locator. Reuse only the *regex* for
  grid-row location.
- `pkg/tuidriver/picker.go:123-153` — `pickerRowOpenColor`: the byte-walk +
  `parseForegroundSGR` loop shape the new chrome walker mirrors (but scanning
  the whole snapshot, not stopping at the first `/`).
- `pkg/tuidriver/picker.go:159-237` — `ParsePicker`. Must stay behaviourally
  unchanged (AC bullet 3c). It still calls `findPickerRows`; do not touch it.
- `pkg/tuidriver/picker_color.go:60-82` — `pickerHighlightedRGBs` +
  `rgbIsHighlighted`. The chrome check compares parsed foreground RGB against
  this set (the same set `ParsePicker` uses — extended on demand, #90 posture).
- `pkg/tuidriver/picker_color.go:84-132` — `parseForegroundSGR` (returns
  `color, consumed, isFg, isReset`). Reuse verbatim in the chrome walker; it is
  encoding-agnostic across indexed (`38;5;N`) and truecolor (`38;2;R;G;B`).
- `pkg/tuidriver/grid.go:48-105` — `Grid` / `NewGrid` / `Rows`. Grid rows are
  `Render`-produced (StripANSI'd, **no color**) and exclude off-screen history.
  Location uses `Rows()`; color chrome cannot come from here (hence the raw
  scan).
- `pkg/tuidriver/modal_test.go:18-84` — `TestDetectModalClassSyntheticAnchors`.
  Three existing picker-positive cases lose their picker status under the new
  contract (see Testing strategy); the two guard cases stay.
- `pkg/tuidriver/modal_test.go:106-128` — `TestDetectModalClassRealFixtures`.
  This is the **reorder-regression guard**: both real picker fixtures must still
  classify as `slash-picker` after the anchors are checked first.
- `pkg/tuidriver/picker_test.go:40-143` — `TestParsePickerRealFixture` +
  `TestParsePickerHypotheticalThirdEncoding`. Must remain green (ParsePicker /
  `findPickerRows` unchanged).
- `docs/knowledge/codebase/150.md` — Grid contract + two load-bearing lessons:
  (a) synthetic multi-row fixtures must use `\r\n`, not `\n`, or vt10x renders a
  staircase; (b) empty render ⇒ zero rows.

## Context

`DetectModalClass` is the classifier behind `classify()` (`events.go:386`) and
`modalDismissed` (`answer.go:117`). The consumer auto-answers permission/trust
modals off its output. Two failure modes, one root cause (substring-matching a
raw history buffer instead of the rendered screen):

1. **Permission modal + on-screen path → misclassified as picker.** Picker is
   checked first; a `/Users/...` line matches `findPickerRows`; the modal never
   reads as `Permission`, so the consumer never answers it (hang), and any
   in-flight `modalDismissed(Permission)` poll sees `SlashPicker != Permission`
   and falsely reports the modal dismissed.
2. **Lone `/path` at idle → phantom picker.** A single `/`-line reads as a
   picker, so `classify()` reports a modal that isn't there and turn-end is
   suppressed.

This slice fixes **only** the slash-picker path. Per the ticket's scope
boundary, the other anchors (mcp/agents/ask-user/trust/permission/model-select/
permissions-config) stay substring-based; grid-ifying them is #152. Do not
grid-ify them here.

## Design

Two deterministic defenses of different fabric, both required (belt-and-
suspenders):

- **Reorder** — the specific anchors win; the picker is the last resort.
- **Chrome + grid-region** — the picker check requires an on-screen `/`-row
  (located from the rendered grid) **and** a picker highlight color, never a
  lone `/`-line.

### The location/parse split (the crux)

`findPickerRows` serves two masters today: classification (in
`DetectModalClass`) and parsing (in `ParsePicker`). It returns raw bytes so
`ParsePicker` can read highlight RGB. `Grid.Rows()` is StripANSI'd and carries
no color. The ticket's key observation: **`ParsePicker` runs only after
classification confirms a picker, so the two paths can diverge.** So:

- **Classification** locates rows from `Grid.Rows()` (on-screen, color-free) and
  corroborates with a separate raw-byte color scan for chrome.
- **Parsing** keeps `findPickerRows` (raw bytes, color-preserving) unchanged.

Location (grid, StripANSI'd) and chrome (raw color) are independent checks
combined with AND. Verified empirically: `pickerRowStartRe` matches rendered
grid rows 1:1 (real fixtures render picker rows starting `/figma-use`,
`/code-review`, … with no box-drawing prefix).

### New unexported helpers (all in `picker.go`; no exported surface changes)

- `snapHasPickerHighlight(snap []byte) bool` — walks `snap` byte-by-byte via
  `parseForegroundSGR` (mirroring `pickerRowOpenColor`'s loop, but not stopping
  at the first `/`); returns true on the first foreground SGR whose RGB is in
  `pickerHighlightedRGBs` (`rgbIsHighlighted`). This is chrome. Encoding-agnostic
  by construction — `parseForegroundSGR` resolves both `38;5;153` (indexed) and
  `38;2;177;185;249` (truecolor) to a comparable RGB, so a raw-substring match on
  the SGR literal would be wrong (the two real fixtures carry the highlight in
  *different* encodings). Behaviour: one linear pass, ~12-15 lines, no allocation.
- `isSlashPicker(snap []byte) bool` — the classifier the reordered
  `DetectModalClass` calls last. Contract:
  1. `g := NewGrid(snap, 0, 0)`; if no row in `g.Rows()` matches
     `pickerRowStartRe`, return false. *(grid-region: off-screen `/`-lines are
     already gone from `Render`'s output.)*
  2. Return `snapHasPickerHighlight(snap)`. *(chrome: a lone path has no picker
     highlight color.)*

  Row location may be an inline loop over `g.Rows()` or a tiny
  `gridHasPickerRow(*Grid) bool` helper — architect's indifferent; keep it
  within `picker.go`. No new `Grid` method is needed (`Rows()` suffices; #150
  deferred `FirstRowWithPrefix`-style additions to "when a migration needs one"
  — this one doesn't).

### `DetectModalClass` restructure (`modal.go`)

Move the picker check out of the leading `if` and past the anchor `switch`:

```
stripped := StripOSC(StripANSI(snap))
switch { /* mcp, agents, ask-user, trust, permission, model-select,
            permissions-config — unchanged order and predicates */ }
// slash-picker is the last resort: no specific anchor matched.
if isSlashPicker(snap) { return ModalClassSlashPicker }
return ModalClassUnknown
```

The switch's `default` returns nothing now — the trailing `if` + `Unknown`
replaces it. The anchors' relative order and predicates are untouched. Update
the doc comments at `modal.go:48` and `modal.go:55-62` to state that
slash-picker now requires an on-screen row **and** picker chrome, and is
evaluated after the specific anchors.

### Data flow

```
DetectModalClass(snap)
  ├─ StripOSC(StripANSI(snap)) ─ substring anchors (mcp … permissions-config)  ── match ─▶ that class
  └─ isSlashPicker(snap)
       ├─ NewGrid(snap).Rows()  ── any row ~ pickerRowStartRe? ── no ─▶ false ─▶ Unknown
       └─ snapHasPickerHighlight(snap) (raw SGR → rgbIsHighlighted) ── no ─▶ false ─▶ Unknown
                                                                     └─ yes ─▶ SlashPicker

ParsePicker(snap)  ── unchanged ── findPickerRows(snap) (raw bytes, color)
```

## Concurrency model

None. `DetectModalClass`, `isSlashPicker`, `snapHasPickerHighlight` are pure
functions of `snap`. `NewGrid`/`Render` allocate a throwaway vt10x term per
call; `classify()` already pays that cost for other predicates and the snapshot
is bounded by `DefaultBufferCap` (4 KB), so the extra render per tick is
negligible.

## Error handling

No error returns — every degenerate input is absorbed (same posture as `Render`,
`Grid`, and the `DetectModalClass` family). Empty/whitespace snapshot ⇒ zero grid
rows ⇒ `isSlashPicker` false ⇒ `Unknown`. `parseForegroundSGR` already no-ops on
malformed SGR. No new failure mode is introduced.

## Testing strategy

Deterministic, synthetic inline `[]byte` fixtures — no new real captures needed.
Every multi-row fixture uses `\r\n` (the #150 vt10x-staircase lesson). Verified
renders below are from an architect probe against `pkg/tuidriver`.

**New regression cases (the three AC bullets):**

- *Lone path, no chrome → Unknown.* `[]byte("/Users/x/file.go\r\n")` — grid row
  starts with `/`, no highlight color. `gridHasPickerRow=true, chrome=false ⇒
  isSlashPicker=false ⇒ Unknown`. (Confirms chrome is load-bearing: the path is
  on screen, so grid-region alone wouldn't reject it.)
- *Permission modal + path on screen → Permission.* e.g.
  `[]byte("Do you want to proceed?\r\n\x1b[38;2;177;185;249m/Users/x/file.go\x1b[39m\r\n")`
  — deliberately give it **both** picker signals (grid `/`-row + chrome) so the
  test proves the *reorder* decides, not merely the absence of chrome. Expect
  `ModalClassPermission`.
- *Single-match filtered picker with chrome → SlashPicker, ParsePicker
  unchanged.* `[]byte("\x1b[38;2;177;185;249m/figma-use\x1b[39m\r\n")` (truecolor)
  and an indexed twin `\x1b[38;5;153m/figma-use\x1b[39m\r\n`. Both:
  `gridHasPickerRow=true, chrome=true ⇒ SlashPicker`. Assert `ParsePicker` still
  returns the row (one item, command `/figma-use`).

**Reconcile existing `TestDetectModalClassSyntheticAnchors` (`modal_test.go:18`):**
three current picker-positive cases no longer have chrome and must flip to
`Unknown` under the new contract — this is the behaviour change, not a
test-breakage to paper over:

- `"slash-picker bare line (no color)"` (`/figma-use description`, no SGR) →
  now `Unknown`. Re-label as a negative guard: a bare `/`-line is not a picker.
- `"slash-picker indexed-color row"` (`\x1b[38;5;246m…`, normal gray 246) and
  `"slash-picker truecolor row"` (`\x1b[38;2;148;148;148m…`, normal gray) →
  now `Unknown` (normal fg is not a highlight). Either re-label as guards or
  give them a highlighted row to keep them positive — but do not assert
  `SlashPicker` on a normal-color-only row.

The three genuinely-highlighted cases (`filtered (multiple colors mid-row)` with
`38;5;153`, `truecolor highlighted row` with `38;2;177;185;249`) stay
`SlashPicker`. The `hint-bar` and `/usr/local/bin mid-line` guards stay
`Unknown`.

**Must stay green unchanged:**

- `TestDetectModalClassRealFixtures` — both real picker fixtures still
  `SlashPicker` (they carry a real highlighted row; verified). This is the
  reorder guard — it proves no specific anchor steals a real picker (only the
  lone word `"Ask"` appears in picker descriptions, and `permissions-config`
  needs the `"Permissions"` header too, which is absent).
- `TestParsePickerRealFixture`, `TestParsePickerHypotheticalThirdEncoding` —
  `ParsePicker`/`findPickerRows` untouched.

**New focused unit tests (suggested):** `snapHasPickerHighlight` true on indexed
+ truecolor highlight, false on normal-color-only and no-color; `isSlashPicker`
truth table over {grid-row?, chrome?}.

## Security review

Ticket is `security-sensitive`. Adversary model (per the chain): claude's
rendered output is attacker-influenceable (malicious repo content, tool output,
prompt-injected skill descriptions). The gated boundary is `DetectModalClass` →
`AnswerModal` auto-keystrokes (`answer.go`) and `modalDismissed` confirmation.

Trust-boundary facts that bound the blast radius:

- `AnswerModal` dispatches keystrokes **only** for `ModalClassPermission` and
  `ModalClassTrustFolder` (`answer.go:80-99`); every other class, incl.
  `SlashPicker`, hits the `default` and sends **nothing**. A `SlashPicker`
  classification cannot cause an auto-keystroke.
- The permission decision (whether/which/who) lives in the consumer (ADR 025,
  #146 route-on-Index). The library only classifies + drives mechanics.

Findings:

1. **Reorder is a net hardening (fixes the observed CRITICAL A).** A permission
   modal carrying an attacker-planted path (`/…`) previously downgraded to
   `SlashPicker` → the consumer never gated it (hang) and an in-flight
   `modalDismissed(Permission)` falsely reported "dismissed". After the reorder
   the `Permission` anchor wins regardless of any `/`-rows or forged color the
   attacker adds (verified: R2 fixture with both picker signals present still
   classifies `Permission`). This closes a real false-dismissal path on the
   permission gate.

2. **Chrome is a benign-false-positive reducer, not an adversarial barrier —
   stated honestly.** An attacker who controls claude's output can emit
   `\x1b[38;5;153m/x` to forge chrome, so chrome does not stop an adversarial
   picker-forge. It does not need to: forging `SlashPicker` yields no keystroke
   (finding 1's trust-boundary fact), and it cannot downgrade a real permission
   modal (the anchor wins first). Chrome's job is to stop *benign* content (a
   legit absolute path at idle) from phantom-pickering — which it does.

3. **Residual (introduced by the reorder, explicitly deferred by scope):** an
   anchor phrase appearing *inside picker content* (e.g. a skill description
   injected with `"Do you want to proceed"`) now classifies as that anchor's
   modal instead of `SlashPicker`. Bounded because (a) `AnswerModal` only
   auto-acts on Permission/TrustFolder and the consumer's ADR-025 logic decides
   whether to answer at all — the consumer does not drive slash-pickers in the
   auto-answer path; (b) the anchors are distinctive/compound (`permissions-config`
   needs a header + tab; `permission` needs the full `"Do you want to proceed"`
   phrase); (c) the ticket's own scope boundary defers anchor grid-region
   scoping to #152, which is what structurally distinguishes a modal region from
   a picker description. Not fixable here without grid-ifying the anchors, which
   the ticket forbids in this slice.

Net: the slice removes a high-severity misclassification on the permission gate
and adds a low-severity, scope-deferred residual whose exploit path requires the
consumer to auto-answer pickers (it does not). **Verdict: PASS.**

## Open questions

- **Chrome brittleness to a future highlight encoding.** If claude ships a third
  highlight shade/encoding `parseForegroundSGR` can't resolve, a *real* picker
  loses chrome → `Unknown`. This fails safe (no keystroke, treated as no-modal;
  turn-end not suppressed) and is the same "extend `pickerHighlightedRGBs` on
  demand" posture as #90/`ParsePicker`. Left as-is (evidence-based; unobserved).
- **Whole-snap vs row-scoped chrome.** `snapHasPickerHighlight` scans the whole
  (4 KB-bounded) snapshot, not just the located `/`-row's bytes. A contrived
  screen with an incidental highlight-shade elsewhere + an on-screen `/`-path
  could false-positive. Tightening to per-row color would re-derive
  `findPickerRows`'s segmentation for no observed benefit → deferred
  (evidence-based). Documented so #152+ can revisit if a real case surfaces.

## Acceptance criteria

- [ ] `DetectModalClass` evaluates the specific anchors (mcp, agents, ask-user,
      trust, permission, model-select, permissions-config) **before** the
      slash-picker check; the picker is the last resort.
- [ ] Slash-picker classification requires picker chrome (a highlight color from
      `pickerHighlightedRGBs`) to co-occur; a single `/`-line with no chrome does
      **not** classify as `ModalClassSlashPicker`.
- [ ] Picker rows are located from `NewGrid(snap,…).Rows()` (rendered grid), not
      by scanning raw-history bytes; off-screen `/`-lines don't count.
- [ ] `findPickerRows`, `ParsePicker`, and `pickerRowStartRe` are unchanged; the
      raw-byte color path `ParsePicker` depends on is preserved.
- [ ] Regression tests (deterministic, synthetic): lone `/Users/x/file.go` → not
      SlashPicker (Unknown); permission modal + on-screen path → Permission;
      single-match filtered picker with chrome → SlashPicker with `ParsePicker`
      output unchanged.
- [ ] `TestDetectModalClassRealFixtures` and the `ParsePicker` fixture tests stay
      green; the three no-chrome synthetic picker cases are reconciled to the new
      contract.
- [ ] Doc comments at `modal.go:48` and `modal.go:55-62` updated to describe the
      new chrome + reorder contract.
- [ ] `go test -race ./...` and `go vet ./...` green.

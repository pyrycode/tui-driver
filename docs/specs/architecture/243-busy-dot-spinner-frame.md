# #243 — busy predicate recognises the dot spinner frame, shape-bound to the spinner row

## Files to read first

- `pkg/tuidriver/state.go:39-58` — `spinnerGlyphs` (the five sparkle frames the
  busy arm currently iterates) and the ⚠️ "add a frame → add it to the #221
  negative suite" note. The dot frame is a *sixth* frame, but it is **not** added
  here — a bare dot glyph would fire on idle chrome (see Technical Notes below).
- `pkg/tuidriver/state.go:88-106` — `busyInRegion`, THE single busy predicate.
  The new arm goes here. Read the doc-comment's "must never reintroduce a
  StripANSI whole-buffer path" warning: it binds this design.
- `pkg/tuidriver/state.go:60-86` — `InterruptHint` (the space-preserved
  second-anchor precedent this arm mirrors in spirit) and `statusRegionRows` (the
  bottom-window bound the new arm reuses verbatim — the region-scope lives in one
  place).
- `pkg/tuidriver/grid.go:101-122` — `Grid.LastRows(n)` and `Grid.RowHasPrefix`.
  `LastRows(statusRegionRows)` is the region-scoped row set the new per-row shape
  scan iterates. `ContainsInLastRows` (substring, unanchored) is **not** usable
  for this arm — the shape must be anchored to row start.
- `pkg/tuidriver/permission.go:39-45` — `modalOptionRe`, the package's idiom for
  an anchored (`^…`) per-row shape regex over a rendered grid row. The new
  `dotSpinnerRe` follows the same anchored-regex idiom.
- `pkg/tuidriver/mcp_banner.go:23` and `pkg/tuidriver/state.go:148-155` — proof
  the middle dot (·, U+00B7) **already renders in the bottom region as a
  separator** ("… · /mcp", "✻ Verb… (2s · ↓N tokens)"). This is the exact chrome
  a bare-dot glyph would false-fire on, and why the shape (dot at row *start* +
  ellipsis) is load-bearing.
- `pkg/tuidriver/anchor_forgery_test.go:53-82,149-180` — `forgedBodyForms` and
  `TestBusyIdleAnchorsRejectRegionForgery`. AC 3's negative case extends this
  test; read how the busy anchors are fed through `forgedBodyForms` (which pushes
  the anchor ABOVE the region) and asserted non-firing.
- `pkg/tuidriver/state_test.go:10-15,154-176` — `gridRows` helper (join with
  `\r\n` so vt10x renders fresh rows) and `TestIsThinkingRealisticLayoutPinsRegion`
  (the realistic thinking-frame layout the new positive control mirrors).
- `pkg/tuidriver/permission_test.go:12-19` — `loadFixture` helper the new
  fixture-backed positive control uses.
- `docs/knowledge/codebase/242.md` §"Patterns established" — the shape-gate port
  recipe: mirror the helper, and "the region-scope-vs-whole-grid choice is the
  one thing to get right per class." For busy/idle the choice is already made:
  bottom region (`statusRegionRows`), same as the spinner and hint arms.
- `docs/knowledge/codebase/153.md` — why region scoping (not whole-buffer
  substring) is the anti-forgery mechanism this arm must preserve.

## Context

`busyInRegion` (`state.go:99`) is the single predicate behind the idle/busy
axis: the consumer's "claude ready for input" gate (`IsIdle` → `isIdleGrid`) and
its "claude started processing" post-keystroke signal (`IsThinking`, also read by
`promptDidCommit` at `deliver.go:212`). It fires busy on any of five sparkle
spinner glyphs or the "esc to interrupt" hint, all region-scoped to the bottom
`statusRegionRows`.

The pinned claude paints a **sixth** thinking frame the predicate misses: a plain
middle-dot painted at the spinner position (row start), in the spinner's colour,
with the verb and its ellipsis following. Per-frame replay of the recording named
in the ticket shows **764 busy rising edges over 16390 output events, and 21% of
mid-turn frames carrying no busy anchor at all**; a healthy run shows the same
shape (209 rises, ~20% uncovered). The interrupt hint does not cover the gap —
during long turns claude swaps the hint row for a rotating tip line, leaving the
dot frame as the only busy anchor on those frames. The result is a busy axis that
flaps hundreds of times per run: noisy thinking edges on the event stream and
wrong reads for anything gated on busy.

The idle predicate is unaffected *today* (this claude does not draw the ❯ prompt
marker mid-turn, so no idle-true frame lands mid-turn), but a naïve fix — adding
a bare dot to `spinnerGlyphs` — would break it: claude renders middle dots as
in-region separators (`mcp_banner.go:23`, `state.go:155`), so a bare-dot anchor
would hold busy true at idle and defeat `isIdleGrid`'s "❯ present AND not busy"
conjunction (`state.go:131`). The fix is the same shape-over-literal move #219
(trust) and #242 (permission) applied: require the anchor's row **shape**, not a
bare glyph.

## Design

One additive change to `state.go`. No new package, no new exported symbol, no
signature change to any existing function.

### New anchor: the dot-frame row shape

A new unexported regex expresses the frame's row shape against a **single
rendered grid row** (space-preserved, exactly as `busyInRegion`'s other anchors
key on):

> A status-region row whose first non-space rune is the middle-dot spinner glyph
> (·, U+00B7), followed by whitespace, then a verb token, then claude's ellipsis
> (…, U+2026) somewhere after it.

Recommended expression — an anchored regex, mirroring `modalOptionRe`'s idiom,
with the glyphs written as regexp hex escapes so the literal never renders in
source/comments (the package's self-reference discipline; matches how
`spinnerGlyphs` uses `\xNN` byte escapes):

```go
// dotSpinnerRe matches claude's plain dot-frame spinner ROW: the middle-dot
// spinner glyph (\x{00b7}) as the row's leading non-space rune, then the verb,
// then the ellipsis (\x{2026}). Anchored at row start (^) and applied per
// rendered row — NOT a substring — so a middle dot used mid-row as bottom-chrome
// separator, or a transcript bullet, does not fire. Deliberately NOT a member of
// spinnerGlyphs: a bare dot there would hold busy true at idle (state.go:131).
var dotSpinnerRe = regexp.MustCompile(`^\s*\x{00b7}\s+\S.*\x{2026}`)
```

The two discriminators against the forbidden bare-dot false-fire:
1. **Dot at row start** (`^\s*\x{00b7}`) — separates the spinner frame (dot leads
   the row) from a separator (dot mid-row, e.g. "… · /mcp") and from transcript
   prose. `\s*` tolerates a small leading indent; verify the real frame's indent
   against the extracted fixture and drop `\s*` if it renders at column 0.
2. **Ellipsis present** (`…`) — separates the frame from an incidental bulleted
   chrome/transcript row that happens to start with a dot.

The developer confirms the exact leading-dot codepoint against the extracted
fixture frame (Open questions). U+00B7 is the strong prior: it is the same middle
dot already rendered as the in-region separator in the class-C spinner and the
mcp-failure banner.

### Wiring: one OR arm in `busyInRegion`, region-scope reused

Add a third anchor to `busyInRegion`, after the spinner-glyph loop and the hint
check, keeping region-scoping in the one place it lives:

```go
// (inside busyInRegion, after the InterruptHint check)
for _, row := range g.LastRows(statusRegionRows) {
    if dotSpinnerRe.MatchString(row) {
        return true
    }
}
return false
```

`LastRows(statusRegionRows)` is the same bottom window the existing arms scope to
via `ContainsInLastRows` — the "single window" idiom `242.md` endorses. The new
arm needs `LastRows` (per-row, anchored) rather than `ContainsInLastRows`
(substring) precisely because the shape is anchored to row start. `statusRegionRows`
is reused unchanged; no new bound is introduced.

Update `busyInRegion`'s doc-comment: it now OR's *three* independent anchors
(spinner-glyph cycle, interrupt hint, dot-frame row shape). Update the
`spinnerGlyphs` ⚠️ note (`state.go:49-51`) to point at the dot-frame's separate
negative case as well, so the "new anchor → new forgery case" invariant stays
enforced.

### Data flow (unchanged surface)

`busyInRegion` is a pure internal predicate. `codegraph_impact` confirms every
dependent is either its own caller or a test — `isIdleGrid`, `IsThinking`, `IsIdle`,
`classify` (`events.go:459`), `promptDidCommit` (`deliver.go:206`). None change
signature; all inherit the tightened anchor for free. Beneficial side effect:
`promptDidCommit` reads `IsThinking`, so the post-keystroke "claude started
processing" commit signal now also catches the dot frame — no change there.

### Imports

`state.go` already imports `regexp`. The recommended anchored-regex form needs no
`strings` import (leading indent is absorbed by `^\s*` in the pattern rather than
a `strings.TrimLeft`). Keep it that way to hold the change to one arm.

## Concurrency model

None. `busyInRegion` is a synchronous pure function over a rendered `Grid`; this
change adds a bounded per-row loop over at most `statusRegionRows` (6) rows. No
goroutines, channels, or shared state introduced.

## Error handling

No new failure modes. The regex is compiled once at package init via
`regexp.MustCompile` (same as `spinnerForRe` / `modalOptionRe`); a malformed
pattern is a compile-time panic caught by any test run, not a runtime path. A row
that does not match simply falls through — the predicate's existing
false-negative-costs-a-re-poll, false-positive-costs-a-missed-idle tradeoff is
unchanged in kind. The design's risk is over-firing on idle chrome; that is
bounded deterministically by the forgery case (AC 3) and by every existing idle
positive control staying green (AC 4).

## Testing strategy

Deterministic (`make check`) — all in the existing test files, no new test file:

- **Positive, real fixture (AC 2).** Commit the extracted frame as
  `pkg/tuidriver/testdata/dot-spinner-snapshot.bin` (name at developer's
  discretion; `dot-spinner-snapshot.bin` matches the `<class>-snapshot.bin`
  convention). Assert `IsThinking(fixture) == true`. This is *observed* evidence
  (a real frame pulled from the recording), not a self-descriptive pin — the
  distinction that makes it valid per [[spike-gated-fix-split]]. Add it as a
  positive control in `state_test.go` (and, if extending
  `TestNegativeSuitePositiveControls`'s spirit, assert the busy axis fires).
- **Positive, synthetic in-region (non-vacuity, fixture-independent).** A
  `gridRows` snapshot mirroring `TestIsThinkingRealisticLayoutPinsRegion` but with
  the dot-frame row (dot at start, verb, ellipsis) in place of the ✻ line, in the
  bottom region → `IsThinking == true`, `IsIdle == false`. Guards the arm even if
  the `.bin` is later regenerated.
- **Negative, region forgery (AC 3).** Extend `TestBusyIdleAnchorsRejectRegionForgery`:
  add the dot-frame row string to the `busy` slice it feeds through
  `forgedBodyForms`. `forgedBodyForms` embeds the anchor as prose ("assistant: it
  reads …") and as a source-read line, both pushed ABOVE the region by 20 filler
  rows — so the dot frame quoted as transcript body must fire nothing, mirroring
  the spinner-glyph and hint negatives already there.
- **Negative, shape forgery in-region.** A bare middle dot as a *mid-row*
  separator inside the bottom region (e.g. a "…failed · /mcp"-shaped chrome row,
  or a "foo · bar" hint) → `IsThinking == false`, proving the dot must lead the
  row, not merely appear in it. Pair with an idle-bearing screen that still reads
  `IsIdle == true` (AC 1's "committed idle fixture still reads idle" — `state.go:83`
  documents `mcp-empty-snapshot.bin` as carrying a real idle ❯; the developer may
  use it or the synthetic idle rows already in `state_test.go`).
- **Regression (AC 4).** `TestIsIdle*`, `TestIsThinking*`,
  `TestNegativeSuitePositiveControls`, and
  `TestClassifyBehaviorUnchangedAfterSingleRender` (`events_test.go`) stay green —
  deterministic proof the new arm does not over-fire on any committed idle/chrome
  fixture.

Hand-run, outside `make check` (AC 5, operator confirmation):

- Re-run the per-frame busy census against the two named recordings from the
  machine-local corpus (`$HOME/.local/share/pyry-recordings`, the same store
  `#242`'s `make corpus-replay` reads). Confirm the uncovered mid-turn fraction
  drops to near zero with no new false busy rises at idle. `make e2e` where
  relevant. This is evidence, not a CI gate — the deterministic lock is the
  `anchor_forgery_test.go` case above.

## Trust-boundary note (label-gated review NOT triggered)

This ticket is **not** labelled `security-sensitive`, so the security-review pass
is skipped (the label is the contract). It is worth recording *why* the forgery
discipline still applies: the busy anchor keys on claude output, which is
prompt-/tool-influenceable, so a transcript quotation of the dot frame is an
attacker-reachable forgery vector. The mitigation is structural and already
mandated by the ACs — region scope (`statusRegionRows`) plus row-start shape,
locked by the `anchor_forgery_test.go` negative case. Unlike the modal-class
chain (#242 → `modal_shown`/`modal_answer`), a forged *busy* read does not route
a grant or an answer keystroke; it at worst delays a keystroke or adds a noise
edge. Keeping both guards (region **and** shape) is non-negotiable regardless:
dropping either re-opens the mid-transcript forgery #153 closed.

## Open questions

- **Exact leading-dot codepoint.** The design assumes U+00B7 (·, MIDDLE DOT) —
  the same dot already rendered as the in-region separator. Confirm against the
  extracted fixture frame's leading bytes before finalising `dotSpinnerRe`; adjust
  the `\x{…}` escape if the recording paints a different dot (e.g. U+2022 •,
  U+2219 ∙). This is a hand-run extraction step (the ticket's self-reference
  warning: work it outside the pipeline).
- **Verb word count.** The recommended `\S.*\x{2026}` matches 1- or 2-word verbs
  (the ellipsis need not attach to the first token). If the fixture and forgery
  tests show a tighter shape is warranted, the developer may narrow toward
  `ParseSpinner`'s verb form (`\S+(?:\s+\S+)?\x{2026}`); the loose form is the
  safe default and the tests bound the over-fire risk either way.
- **Leading indent.** `^\s*` tolerates indentation. If the extracted frame renders
  the dot at column 0 (as the ✻ spinner does in the realistic fixtures), drop
  `\s*` for a tighter anchor.

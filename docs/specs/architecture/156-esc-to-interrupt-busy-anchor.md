# #156 — Add `esc to interrupt` as a second busy anchor

**Ticket:** [pyrycode/tui-driver#156](https://github.com/pyrycode/tui-driver/issues/156) · **Size:** XS · **Labels:** `security-sensitive`

Second, independent busy anchor on the idle/busy axis. Today the spinner glyph `✻` is the *sole* busy signal, and it has already drifted once (the `✻ <verb> for Ns` text format matches 0/667 captured frames at claude 2.1.158 — see CLAUDE.md § Spinner caveat). If the glyph itself changes next, `busyInRegion` goes false mid-turn with no error at any layer and the consumer types its next prompt into an in-flight turn. This ticket folds claude's on-screen `esc to interrupt` hint into the *same* region-scoped grid predicate as an OR, so the check survives a change to *either* anchor alone — claude would have to change both the spinner glyph and the interrupt-hint wording in one release to defeat it.

The fold-in point already exists and was designed for this: `busyInRegion` (#153) is THE single busy predicate both `IsIdle` and `IsThinking` route through, and its doc-comment names #156 as the folding-in site. One OR clause makes both predicates coherent automatically and stays region-scoped for free.

## Files to read first

- `pkg/tuidriver/state.go:39-78` — `busyInRegion` (the seam, line 44), `IsIdle`, `IsThinking`. Extract: the single-predicate structure — both route through `busyInRegion`; the load-bearing `IsIdle` conjunction (`❯` present AND `!busyInRegion`); the existing doc-comment that pre-designates this ticket.
- `pkg/tuidriver/state.go:8-37` — `IdleGlyph` / `SpinnerGlyph` `[]byte` vars + `statusRegionRows = 6`. Extract: the anchor-var convention and *why* the glyphs are `[]byte` (other `[]byte` consumers) — the new anchor has none, so it differs (see Design).
- `pkg/tuidriver/grid.go:80-96` — `ContainsInLastRows(sub string, n int) bool`. Extract: takes a `string`, matches within a single rendered row, clamps `n` to row count. This is the whole match primitive; the new anchor is one more `sub`.
- `pkg/tuidriver/state_test.go:86-163` — `TestIsIdleSpinnerGlyphInTranscriptNotThinking` (forgery guard to mirror), `TestIsThinkingRealisticLayoutPinsRegion` (positive-layout shape to mirror), and the `gridRows` helper (lines 10-15, `\r\n`-joins so vt10x renders fresh rows). Extract: the exact test shapes and raw-substring-contrast assertion to reuse.
- `docs/knowledge/codebase/153.md` — the slice that built `busyInRegion`. Extract: "one region-scoping seam per axis" pattern; the explicit note that #156 folds in "as an OR against the grid's space-preserved form"; `statusRegionRows` calibration (don't touch it).
- `docs/knowledge/codebase/13.md` § finding 3 + "Dual-form substring matching" — the space-eating landmine, empirically. Extract: claude renders inter-word gaps as CSI cursor-forward (`\x1b[1C`); `StripANSI` collapses them to nothing (`Doyouwanttoproceed`), the **grid repaints them to real spaces** (`Do you want to proceed`). This is why the anchor keys on the grid, not a stripped buffer.
- CLAUDE.md § "Spinner caveat" + § "Scope discipline" — the spinner is effectively dead; `esc to interrupt` is the documented reliable in-flight anchor; and this is a library-owned state-detection concern (correct home).

## Context

Filed from the Cross-Repo Code Review 2026-07-03. Root cause of that review's two criticals: state was classified by substring-matching a raw append-only history buffer, not the rendered grid — remediated by the #150–#153 grid port. `busyInRegion` is the surviving single seam. This ticket adds the second anchor the review recommended, onto that seam. Dependency (#153, "shared busy predicate") is CLOSED and on `main`. Independent of #154 (removes whole-buffer paths; finds none on this axis by construction).

## Design

One production file: `pkg/tuidriver/state.go`. No signature changes anywhere; `busyInRegion`, `IsIdle`, `IsThinking` keep their exact signatures, so all ~15 call sites (`events.go`, `ready.go`, `deliver.go`, every `cmd/spike-*`/`cmd/probe-*`) are untouched. No edit fan-out.

### The anchor

Add one package-level constant beside the glyph vars:

```go
// InterruptHint is claude's on-screen "esc to interrupt" hint — the second,
// independent busy anchor (see busyInRegion). Space-preserved rendered form:
// it must be matched over the Grid, never a StripANSI whole-buffer scan.
const InterruptHint = "esc to interrupt"
```

Decisions:

- **`const string`, not a `[]byte` var.** The `IdleGlyph`/`SpinnerGlyph` vars are `[]byte` only because they have external `[]byte` consumers (`deliver_test.go`, spike PTY-quiescence predicates) — see 153.md. `InterruptHint` has none, and `ContainsInLastRows` takes a `string`, so a `const string` is the natural fit and needs no `string(...)` conversion at the call site. Immutable literal ⇒ `const`.
- **Exported.** Symmetric with the two exported glyph anchors, and CLAUDE.md designates this hint the *preferred* in-flight anchor now that the spinner is dead — consumers writing their own PTY-quiescence checks are the documented next users. This is documented demand, not speculative surface.
- **Full contiguous phrase, not a shorter substring — load-bearing.** Verified against the committed captures: `picker-snapshot.bin` and `picker-truecolor-snapshot.bin` both render `…without interrupting the main conversation` (the `/btw` command description) at row −4, *inside* the status region, and currently classify `IsThinking=false`. A shortened anchor (`interrupt`, `to interrupt`) would flip both to busy on an idle picker. The full phrase `esc to interrupt` does not occur in that benign content, so it stays false. Do not shorten the anchor.

### The seam

Fold the anchor into `busyInRegion` as an OR (the one and only edit to behaviour):

```go
func busyInRegion(g *Grid) bool {
    return g.ContainsInLastRows(string(SpinnerGlyph), statusRegionRows) ||
        g.ContainsInLastRows(InterruptHint, statusRegionRows)
}
```

Both predicates inherit it with no further edits:

- `IsThinking(snap)` = `busyInRegion(...)` → true when spinner glyph **or** interrupt hint is in the region. (AC #2, AC #3)
- `IsIdle(snap)` = `❯` in region **AND** `!busyInRegion(...)` → false whenever *either* anchor is present. The `❯`-present conjunction is unchanged and still load-bearing (`❯` is redrawn beneath the running turn). (AC #1, AC #2)

Update `busyInRegion`'s doc-comment: it currently says #156 *will* fold the hint in — change it to describe both anchors as present, and keep the "must not reintroduce a whole-buffer path" warning (now doubly important, since the multi-word anchor is exactly what a stripped-buffer path would corrupt).

### Why the grid, not StripANSI (the space-eating landmine)

The single-rune glyphs (`✻`, `❯`) sidestepped claude's `\x1b[1C` inter-word cursor-forward rendering — one rune, no interior gaps. A multi-word anchor does not. Empirically (13.md finding 3): claude emits inter-word gaps as CSI cursor-forward; `StripANSI` collapses them to nothing (`esc to interrupt` → `esctointerrupt`), while vt10x inside `Render`/`Grid` repaints each skipped column as a real space (→ `esc to interrupt`). Matching over the Grid via `ContainsInLastRows` therefore sees the space-preserved form. This is already how `busyInRegion` works — the anchor is simply one more `sub`. **Constraint: do not add any `StripANSI`/whole-buffer substring path for this hint.** That would both re-corrupt the phrase and re-open the mid-transcript forgery #153 closed.

## Concurrency model

None. Pure functions over a byte snapshot; no goroutines, channels, or shared state introduced.

## Error handling

No new failure modes. `ContainsInLastRows` already handles empty/`nil` snapshots (empty render ⇒ zero rows ⇒ false) and clamps `n` to the row count. A snapshot where the hint never appears behaves exactly as today (spinner-only detection).

## Testing strategy

Two required tests (AC #3, AC #4) plus one cheap real-fixture regression guard, all in `state_test.go` using the existing `gridRows` helper. Written as scenarios; the developer writes the Go in the file's table-test idiom.

- **Positive — hint drives busy without the spinner (AC #3).** Mirror `TestIsThinkingRealisticLayoutPinsRegion`, but drop `✻` and put `esc to interrupt` in the bottom region alongside a redrawn `❯` input line. Fixture: a few transcript rows, then within the last 6 rows a status line carrying `esc to interrupt`, the input box, `❯`, hint bar — and **no** `✻` anywhere. Assert `IsThinking == true` and `IsIdle == false`. The `❯`-present-but-still-not-idle assertion is the meaningful one: it proves the hint alone flips busy.
- **Forgery guard / region-scoping (AC #4).** Mirror `TestIsIdleSpinnerGlyphInTranscriptNotThinking`. Fixture: `esc to interrupt` placed ≥ 8 rows above the bottom (clear of the 6-row region regardless of layout drift), a genuine idle `❯` line at the bottom, no `✻`. Assert `IsThinking == false` and `IsIdle == true`. Add the raw-substring contrast the sibling forgery tests use: assert `strings.Contains(string(snap), "esc to interrupt")` is **true** — i.e. a whole-buffer path *would* forge busy, and only region-scoping gets it right. This is the security-relevant test (see Security review).
- **Real-capture false-positive guard (recommended, cheap).** Assert `IsThinking(picker-snapshot.bin) == false` and `IsThinking(picker-truecolor-snapshot.bin) == false`. These committed captures render `interrupting` in-region; the assertion pins the full-phrase-vs-substring decision against a future shortening of the anchor. Justified by an *observed* false-positive (not speculative), so it belongs; keep it to two assertions against existing fixtures — do not capture new ones for it.

## Open questions

1. **Exact rendered space identity — the one real risk; resolve before wiring.** The anchor assumes single ASCII spaces (U+0020) between the three words. No committed `.bin` captures the hint, so this is unverified. Counter-evidence that it *might* not be single ASCII spaces: `picker-truecolor-snapshot.bin` renders `❯ /` — claude emits a literal NBSP (U+00A0) in its input area, so an NBSP (or a multi-column gap) between the hint words would make `"esc to interrupt"` (ASCII spaces) fail to match. **Primary path per ticket:** use `esc to interrupt` with regular spaces; the synthetic test fixture is explicitly sanctioned. **But a synthetic fixture cannot catch a wrong space-identity assumption** — only a real frame can. If a real thinking frame showing the hint is cheaply capturable (spike-cancel #11 observed it at runtime), capture one, confirm the exact rendered bytes, and — if real — commit it as the positive fixture in place of the synthetic one. Do not let fixture-capture balloon this out of XS; if a clean capture isn't quick, ship the regular-space anchor + synthetic fixture and leave this note for code-review.
2. **Case.** The hint is lowercase `esc to interrupt` (spike-cancel #11 / CLAUDE.md). `ContainsInLastRows` is case-sensitive. If a future claude capitalizes it, this anchor silently goes dead — acceptable (the spinner anchor still covers busy), and symmetric with how glyph drift is handled today. Not worth a case-insensitive match now (no evidence claude varies the case).

## Scope self-check

Production source files with new/modified content: **1** (`state.go`). New files: 0. Total written work (1 const + 1 OR clause + doc-comment edit + 2–3 table tests): well under 100 lines. New exported symbols: 1 (`InterruptHint`, a const — not a type/interface). Consumer call sites needing simultaneous update: 0. ACs: 4. No state machine. No red line tripped — confirmed XS.

## Security review

**Verdict:** PASS

This is a defensive, security-*motivated* change: it hardens the idle/busy classifier against a single-point-of-failure (the sole spinner anchor) whose failure mode is exactly the attacker-relevant one — the consumer typing into a live turn. The adversarial pass below treats the design as if it has holes.

**Findings:**

- **[Trust boundaries] No findings — boundary is explicit and preserved.** The trust boundary is "rendered status region (bottom `statusRegionRows` rows, the live overlay claude draws) = trusted; everything above = attacker-influenceable transcript." claude's output is attacker-influenceable content (a hostile prompt/tool result can print the literal string `esc to interrupt` into the transcript body). The design routes the new anchor through the *same* `ContainsInLastRows(..., statusRegionRows)` region scope as the spinner, so the boundary stays a single named function (`busyInRegion`) and the new anchor cannot widen it. AC #4's forgery test (hint in transcript body ⇒ NOT busy) is the deterministic enforcement of this boundary — a code-level test, not an advisory rule (belt-and-suspenders: different fabric).
- **[Trust boundaries — false-positive on benign in-region content] No findings; mitigation is the full-phrase anchor.** A shorter anchor would let ordinary in-region content (`interrupting` in the picker's `/btw` description, observed at row −4 of two committed captures) forge busy. Matching the full contiguous phrase `esc to interrupt` avoids it; the recommended real-capture guard pins it against regression. This *narrows* the match surface vs. the naive design — strictly safer.
- **[File operations] Not applicable — no filesystem access.** Pure in-memory predicate over a byte snapshot. No paths, no reads/writes, no temp files.
- **[Subprocess / external command execution] Not applicable — no `exec`.** No commands spawned; no keystrokes injected by this code (it only classifies). The keystroke-injection consumers are downstream and unchanged.
- **[Error messages, logs, telemetry] No findings.** The functions return `bool`; they log nothing and surface no snapshot bytes. No new leak surface. (The anchor string is a constant, not user data.)
- **[Concurrency] Not applicable — no shared state, no goroutines, no locks.** `busyInRegion`/`IsIdle`/`IsThinking` remain pure functions; adding an OR term introduces no data race.
- **[Cryptographic primitives] / [Tokens, secrets, credentials] / [Network & I/O] Not applicable — none present in this design.**
- **[Threat model alignment] Aligned.** The relevant threat is CRITICAL-B from the Cross-Repo Code Review 2026-07-03: mid-turn busy→idle flip letting the consumer inject into an in-flight turn. This ticket directly reduces the probability of that threat (two independent anchors instead of one) without introducing a new one. The residual risk (Open Question #1: the anchor never matches because the rendered space identity differs from the assumption) degrades *safely* — it falls back to spinner-only detection, i.e. today's behaviour, never to a *more*-idle classification. A wrong anchor cannot make a running turn look idle; at worst it fails to add the extra protection.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-07-04

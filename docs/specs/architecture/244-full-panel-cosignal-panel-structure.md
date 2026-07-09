# Spec #244 — Bind the full-panel co-signals to panel structure (mcp, permissions-config)

**Ticket:** pyrycode/tui-driver#244
**Size:** S (confirmed — one package, ~120–160 lines total written work, no consumer fan-out, public API unchanged)
**Security-sensitive:** yes (see § Security Review — a forged class fires `EventKindPtyModalShown` and poisons the startup readiness report)
**Self-reference warning:** this ticket is about detectors. Working it renders the anchor literals on screen, which can forge live detection. Hand-run outside the pipeline, as #219–#227 were.

---

## Context

Two full-panel modal classes still rest on generic words plus an unbound colour check, so ordinary transcript prose forges them:

- **mcp** (`modal.go:231`): `gridContains(g, anchorMCPSpaced) && snapHasPickerHighlight(snap)` — the header phrase anywhere on the grid, plus a highlight shade **anywhere** in the raw snapshot.
- **permissions-config** (`modal.go:239–244`): `"Permissions"` anywhere **AND one of** `Allow`/`Ask`/`Deny` anywhere **AND** the same anywhere-highlight.

The 2026-07-07 review addendum established that `snapHasPickerHighlight`'s shade (xterm-256 index 153 → `175,215,255`, and its truecolor twin) is a **general light blue claude paints on paths and links**, present in a large fraction of frames. So the colour co-signal adds almost nothing; the effective protection is the header/tab words alone. The recording `20260703T194012Z-1e437c7a-…-ok.cast` classifies permissions-config four times on plain prose — the header word and a tab word matched independent locations while the general highlight was present elsewhere. Damage: a forged class fires `EventKindPtyModalShown` onto `Session.Events()` and reports a wrong class on the startup readiness gate (`UnexpectedModalError`).

This is the same hardening pattern as #219 (trust) and #242 (permission): bind each class's co-signal to **rendered-panel structure** so quoted prose can no longer forge it. mcp and permissions-config are the last two full-panel classes still on whole-grid word matches + anywhere-highlight.

**Out of scope — the agents arm.** #245 retired the agents classifier arm entirely (already merged into this branch's base — `detectModalClassWithGrid` has no agents case). That false-fire is already resolved; do not add an agents arm.

---

## Files to read first

- `pkg/tuidriver/modal.go:229–257` — `detectModalClassWithGrid`: the two arms to edit (mcp at `:231`, permissions-config at `:239–244`). The `switch` order is load-bearing (comment `:193–216`); do not reorder.
- `pkg/tuidriver/modal.go:111–120` — the anchor vars (`anchorMCPSpaced`, `anchorPermissionsHeader`, `anchorPermissionsTabAllow/Ask/Deny`). Reuse as-is.
- `pkg/tuidriver/modal.go:175–191` — `gridContains(g, sub)`: the whole-visible-grid row-scan the full panels use. Template for the new "all three tabs on one row" grid helper.
- `pkg/tuidriver/picker.go:128–153` — `pickerRowOpenColor`: the foreground-SGR **byte-walk** the ticket says to reuse (the `parseForegroundSGR` state machine returning the colour active at a row's opening glyph). **Do not modify it** — it has ParsePicker semantics and its own tests (`picker_color_test.go`). Copy the walk shape into a new focused helper.
- `pkg/tuidriver/picker.go:92–121` — `findPickerRows`: the **line-segmentation idiom** (`StripOSC`, then split on `\n` **and** `\r`). The new helper reuses this segmentation but must consider **every** line, not only `/`-rows.
- `pkg/tuidriver/picker.go:159–190` — `snapHasPickerHighlight`: the anywhere-scan being replaced in the two arms. **Keep this function** — slash-picker still calls it (`picker.go:255`, `picker_test.go:165`). The new helper is a sibling, not a replacement.
- `pkg/tuidriver/picker_color.go:60–87` — `pickerHighlightedRGBs`, `rgbIsHighlighted`, and the `parseForegroundSGR` contract (`:105–137`). The highlight set and the comparator the new walk tests against.
- `pkg/tuidriver/trust.go:33–51` — `gridHasTrustDialog`: the #219 structural-co-signal template (header row + shape). Same shape the two arms move toward.
- **LANDMINE →** `pkg/tuidriver/modal_test.go:311–345` — `TestDetectModalClassSyntheticGridFixtures`. The synthetic permissions-config input at `:333–336` renders `"Permissions"` on row 0 and `Allow   Ask   Deny` on **row 1 — a different row**. This committed positive control forces the tab compound to be **"three tabs on one row" with the header matched separately**, not "header + tabs on one row." See § Design.
- `pkg/tuidriver/modal_test.go:263–308` — `TestDetectModalClassRealFixtures`: guards AC1 (`mcp-snapshot.bin → MCP`, `permissions-config-snapshot.bin → PermissionsConfig`).
- `pkg/tuidriver/anchor_forgery_test.go:244–327` — the negative suite: `TestWholeGridAnchorsRejectContentForgery` (the mcp/permissions-config prose cases at `:253`/`:258`) and the **non-vacuity pattern** at `:304` (`if !snapHasPickerHighlight(snap) { t.Fatal(...) }`). Model the new negative case on this.
- `pkg/tuidriver/testdata/mcp-snapshot.bin`, `permissions-config-snapshot.bin` — the two real panels (AC1). Verified facts below so you need not re-derive them.

---

## Verified fixture facts (measured — do not re-derive)

Rendered grids and raw row-opening colours, gathered for this spec:

**`permissions-config-snapshot.bin`** — grid row 22 is the tab row:
```
   Permissions  Recently denied   Allow   Ask   Deny   Workspace
```
Header **and all three tabs on one rendered row**. The **only** highlight-shade escape (`38;5;153`, one occurrence) opens the panel's **top border rule** (`▔▔▔…`, grid row 21) — **not** the tab row, **not** an option row. Footer row 39: `←/→ to switch · ↓ to select · Esc to cancel`.

**`mcp-snapshot.bin`** — header `Manage MCP servers` on grid row 3. Three `38;5;153` occurrences, each at a **row opening**: the top separator rule (grid row 2), the header row itself (`Manage MCP servers`), and the ❯-selected server row (`❯ qmd · …`, grid row 7). Footer row 26: `↑/↓ to navigate · Enter to confirm · Esc to cancel`.

**Consequence:** a "does any rendered row *open* in the highlight shade" check holds on **both** real panels (via the border rule alone, even if claude repaints the header/selected row). The general light-blue on prose paths/links renders **mid-line**, so it does not satisfy a row-opening check — that is the discrimination.

Every committed input that must classify as MCP/PermissionsConfig carries its highlight at a row opening — verified: `modal_test.go:169`, `events_test.go:155`, the synthetic grid `modal_test.go:334` (row 1 opens at `Allow` in `38;5;153`), and both `.bin` fixtures. So the row-bound check breaks **no** positive control.

---

## Design

Two co-signals, both deterministic code, replacing the near-useless anywhere-highlight. No public API change; the edit is contained to `detectModalClassWithGrid`'s two arms plus one new unexported helper and a small grid helper.

### 1. Row-opening highlight (both arms)

New unexported helper — behaviour contract (name developer's choice, e.g. `snapHasRowOpeningHighlight(snap []byte) bool`):

- Segment `snap` into physical lines using the `findPickerRows` idiom (`StripOSC`, split on `\n` and `\r`).
- For each non-empty line, walk its raw bytes tracking active foreground colour with `parseForegroundSGR` (the exact state machine in `pickerRowOpenColor`). At the **first visible non-whitespace glyph** (first byte not in `{' ', '\t', '\r'}` and not inside an ESC sequence), test `rgbIsHighlighted` on the active colour.
- Return true iff **some** line's opening colour is a `pickerHighlightedRGBs` shade.

Difference from `snapHasPickerHighlight`: that returns true for a highlight **anywhere** on a line (mid-word paths included); this returns true only when the highlight is the colour of a line's **opening** glyph. Both real panels satisfy it (top rule); prose paths/links do not (they render mid-line).

> The ticket says "reuse the byte-walk at `picker.go:128`." Reuse the **walk shape**, do not generalise `pickerRowOpenColor` in place — its `findPickerRows` caller depends on its stop-at-`/` semantics and `picker_color_test.go` pins them. A focused sibling keeps the change additive and the existing tests green.

### 2. Single-row tab compound (permissions-config only)

Require the three tab words on **one** rendered grid row, instead of three independent anywhere-matches. Behaviour contract for a small grid helper (e.g. `gridRowContainsAll(g, subs...) bool`): some row of `g.Rows()` contains **all** of `Allow`, `Ask`, `Deny` (`strings.Contains` per sub, mirroring `gridContains`).

Keep the `"Permissions"` header as a separate whole-grid `gridContains` — **not** required on the tab row. This is forced by the committed synthetic fixture (`modal_test.go:334`), where `"Permissions"` sits on a different row from the tabs. Requiring header-on-the-tab-row would break that positive control; the real `.bin` happens to have both on row 22, but the contract must satisfy **both** committed fixtures.

mcp needs no compound — its header (`Manage MCP servers`) is already a single contiguous phrase; its only weak link is the anywhere-highlight, closed by co-signal 1.

### Arm rewrites (contract level)

- **mcp:** `gridContains(g, anchorMCPSpaced) && <rowOpeningHighlight>(snap)`
- **permissions-config:** `gridContains(g, anchorPermissionsHeader) && <gridRowContainsAll>(g, Allow, Ask, Deny) && <rowOpeningHighlight>(snap)`

No other arm changes. Do **not** touch trust/permission/model-select/ask-user/slash-picker. Do **not** remove `snapHasPickerHighlight`.

### Why this rejects the recording and every committed fixture still classifies

| Input | header | 3 tabs / mcp phrase on one row | row-opening highlight | result |
|---|---|---|---|---|
| `permissions-config-snapshot.bin` | ✓ (row 22) | ✓ (row 22) | ✓ (top rule) | **PermissionsConfig** |
| synthetic grid `modal_test.go:334` | ✓ (row 0) | ✓ (row 1) | ✓ (`Allow` opens 153) | **PermissionsConfig** |
| `mcp-snapshot.bin` | — | ✓ phrase (row 3) | ✓ (top rule/header/sel) | **MCP** |
| recording `…-ok.cast` (header + one tab scattered across prose lines) | maybe | ✗ (three tabs never cluster on one prose row) | ✗ (light-blue is mid-line) | **Unknown** |
| existing prose negatives (`anchor_forgery_test.go:253/258`, `modal_test.go:33/39`) | maybe | maybe | ✗ (no highlight) | **Unknown** (unchanged) |

The recording is rejected by **either** guard independently (belt-and-suspenders, both deterministic).

### Doc-comment updates (part of the change; counted in the size)

The mcp/permissions-config co-signal is described in several comments that still say "picker highlight anywhere." Update them to the row-bound form:
- `modal.go:70–97` (the anchor doc block — mcp and permissions-config lines).
- `modal.go:211–216`, `:223–228` (mentions of `snapHasPickerHighlight` for full panels).
- `picker_color.go:76–79` ("reused as the #223 second-fabric co-signal for the full-panel modal classes") — now `snapHasPickerHighlight` serves slash-picker only; the panels use the row-bound sibling.
- `anchor_forgery_test.go:46–54` (tier-2 description of the mcp/permissions-config co-signal).

---

## Concurrency model

None. `DetectModalClass` and all helpers are pure functions over a byte slice; no goroutines, channels, or shared state. The new helper is one linear, allocation-light pass per snapshot (segment + per-line SGR walk), on the same order as the existing `snapHasPickerHighlight`.

## Error handling

No I/O, no error paths. The only "failure" is a misclassification, and the direction matters:

- **False positive** (prose forges a class) — the vulnerability being closed. This fires a spurious `EventKindPtyModalShown` / poisons `UnexpectedModalError`. The change strictly **reduces** this surface.
- **False negative** (a real panel not detected) — the availability risk a tightening could introduce: a real modal missed means a consumer types into it and hangs. Guarded by the two `.bin` positive controls plus the synthetic grid, all measured to still classify (§ Verified fixture facts). The row-opening highlight is robust because the panel **border rule** carries the shade independent of which content row is selected.

---

## Testing strategy

Verify AC1–AC3 in the claude-free `make check`. AC4 (corpus-replay over `$HOME/.local/share/pyry-recordings`) is **operator/hand-run** — not in `make check`; do not block the dev turn on it. The in-repo negative case is the deterministic proxy for the recording's failure mode.

**AC1 — fixtures still classify.** No new test needed; `TestDetectModalClassRealFixtures` (`modal_test.go:272`, `:296`) and `TestDetectModalClassSyntheticGridFixtures` (`:333`) already assert both. Confirm they stay green.

**AC2 — new negative case in `anchor_forgery_test.go`** pinning the single-line prose forgery of the permissions-config tab words. Build a forged frame (bullet-pointed scenario, not a full body):
- A prose line rendering the three tab words together, e.g. `assistant: the Permissions view has Allow, Ask and Deny tabs`. Include `"Permissions"` in the frame so the header-anywhere and three-tabs-on-one-row guards are **both** satisfied — otherwise the case passes vacuously on a missing word instead of exercising the new colour guard.
- The general highlight shade present **mid-line** elsewhere, as a real production frame carries it (light-blue on a path), e.g. a separate line `see \x1b[38;5;153m/Users/x/settings.json\x1b[39m` where the highlight is **not** the row's opening glyph.
- **Non-vacuity guard** (mirror `anchor_forgery_test.go:304`): assert `snapHasPickerHighlight(snap) == true` — the old anywhere-highlight IS present, so the new row-opening check is provably what rejects it.
- Assert `DetectModalClass(frame) == ModalClassUnknown`.
- Optional strengthening (pins the tab-compound guard independently): a second sub-case with `"Permissions"` and the three tab words scattered across **separate** lines plus a **row-opening** highlight — must also be `Unknown` (three tabs never share a row).

Keep existing negatives green: `TestWholeGridAnchorsRejectContentForgery` (`:251`) cases for mcp/permissions-config have no highlight, so they stay `Unknown` under the row-bound check.

**AC3 — `make check` green** (build, vet, existing suite + new case).

Use `\r\n` row separators in synthetic grids so vt10x renders flat rows (the #150 grid-fixture lesson); reference anchors by symbol, never by re-typed literal (self-reference discipline).

---

## Security Review (label-gated pass — `security-sensitive`)

**Trust boundary.** claude's rendered PTY output is **attacker-influenceable**: an agent transcript can contain arbitrary quoted text, including this ticket's own anchor literals. That output flows into `DetectModalClass`, whose result (a) fires `EventKindPtyModalShown` on `Session.Events()` and (b) sets the startup readiness gate's `UnexpectedModalError`. Downstream consumers (mobile `modal_shown`, remote `modal_answer`) treat a modal-class event as ground truth. So a forged class is an **event-stream poisoning** primitive, not merely a cosmetic bug. This is exactly the boundary #219/#242 hardened for other classes.

**Category walk:**
- *Injection / forgery (the target class).* Before: header/tab words matched anywhere + anywhere-highlight → one prose line forges the class (observed 4× in production). After: the class requires a rendered-panel structural invariant (mcp phrase + row-opening highlight; permissions-config header + three-tabs-on-one-row + row-opening highlight) that prose does not reproduce. Surface **strictly reduced**. Confirmed by the new negative case and the corpus-replay AC.
- *Residual forgery.* A frame that both quotes `Manage MCP servers` (or `Permissions` + `Allow Ask Deny` on one row) **and** opens some row in the highlight shade (e.g. a file path at a row start) could still forge. This is a far narrower surface than "highlight anywhere," matches the accepted #219/#242 landing point, and is not the observed production failure (the recording scatters the tab words). Noted, not blocking; a future ticket could region-scope the panels as #237 did the picker, but there is no evidence that failure mode occurs (Evidence-Based Fix Selection — do not build a defence for an unobserved mode).
- *Availability / false negative.* A tightening that hid a **real** modal would be worse than the forgery (consumer types into an undetected modal → hang). Mitigated: both real `.bin` panels and the synthetic grid are measured to still classify; the row-opening highlight rides the panel **border rule**, which is present regardless of selection state. No availability regression.
- *No new trust boundary, no I/O, no privilege, no secret handling* introduced — pure classifier refinement.

**Verdict: PASS.** The change closes an observed event-stream-poisoning surface, introduces no availability regression against committed fixtures, and its one residual surface is narrower than the status quo and unobserved. No spec revision required before commit.

---

## Open questions

- None blocking. The developer picks the exact helper signatures; the two `.bin` fixtures, the synthetic grid, and the new negative case are the arbiter (per the ticket). If the corpus-replay operator pass (AC4) later surfaces a residual mcp/permissions-config fire that a row-opening highlight does not catch, that is a follow-up region-scoping ticket (the #237 pattern), not a change to this contract.

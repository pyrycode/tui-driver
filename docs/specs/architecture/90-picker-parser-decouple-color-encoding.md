# #90 — Decouple picker parser from color-code encoding

Replace the picker's color-SGR-keyed row regex with a **structural row anchor** and a **parsed-RGB highlight classifier**, so that future claude renderer changes (palette swaps, theme tweaks, color-encoding migrations like indexed → truecolor) stop repeatedly breaking `DetectModalClass` / `ParsePicker`.

## Files to read first

- `pkg/tuidriver/picker.go:20-54` — existing color-SGR regexes (`pickerItemStartRe`, `pickerColorCodeRe`, `pickerCsiCursorFwdRe`) and the doc comment explaining unfiltered vs filtered picker shape. The doc comment's structural facts (rows separated by `\r\r\n`; rows start with a color SGR then `/<letter>`; description follows after CSI cursor-forward as inter-word "space"; trailing `\x1b[39m` resets) carry into the new design — only the row-finding mechanism changes.
- `pkg/tuidriver/picker.go:74-161` — `ParsePicker` body. The text-reconstruction inner loop (strip color SGRs → convert cursor-forward to space → strip remaining CSI → collapse whitespace → split on first space → parse `(category)` prefix) is reused verbatim; only the row-boundary discovery and highlight classification change. Per AC, the in-body **description-reconstruction** color stripper may remain — the constraint targets row detection and highlight classification only.
- `pkg/tuidriver/modal.go:76-103` — `slashPickerRowRe` and the modal-detector branch that checks it on the RAW snapshot. The "check picker first on raw bytes" ordering exists because the old anchor needed colors; the new structural anchor works against StripANSI'd lines, so the ordering rationale changes (covered in Design).
- `pkg/tuidriver/modal_test.go:18-66` — synthetic-anchor table. Five picker-related cases (`slash-picker SGR row 246`, `… 153 highlighted`, `… filtered`, the hint-bar false-positive guard, plus the existing classification cases) update to the new shape. The hint-bar false-positive guard is the most load-bearing — the new structural anchor MUST still reject `"? for shortcuts · ← for agents"`.
- `pkg/tuidriver/picker_test.go:39-87` — `TestParsePickerRealFixture`. Currently reads `testdata/picker-snapshot.bin` (indexed-color). New test fixture (`testdata/picker-snapshot-truecolor.bin`) is added; the fixture-based test runs against BOTH and asserts the same structural facts (first item `/figma-use`, highlighted=true, at least one item with category=`figma`, all commands start with `/`, exactly one highlighted in unfiltered mode).
- `pkg/tuidriver/ansi.go:21-37` — `StripANSI` / `StripANSIString` / `StripOSC` semantics. The new anchor calls `StripANSI` per-line; the `oscRe` payload-content note (used for predicates over the stripped buffer) carries.
- `pkg/tuidriver/grid.go:36-46` — `Render` (vt10x-backed) — referenced as an alternative anchor candidate evaluated and rejected in Design (vt10x discards SGR information, so a Render-based anchor cannot drive highlight classification without a second pass).
- `cmd/spike-multiselect/main.go:270-280` — the sole consumer of `DetectModalClass` + `ParsePicker`. Confirms zero signature change is required (this ticket is API-stable internal refactor).
- `docs/knowledge/architecture/system-overview.md` § "Permission modal present (PTY side)" — precedent for structural/literal-text predicates over the stripped buffer (the modal-detector pattern this spec follows for slash-picker).
- `pkg/tuidriver/mcp.go:71-79` — example of a parser that **already uses** the structural-anchor pattern (Render + stripped-text matching) instead of color SGRs. The picker is being brought in line with this precedent.

## Context

This ticket supersedes [#85](https://github.com/pyrycode/tui-driver/issues/85). #85 framed the failure as "indexed-color regex doesn't match claude 2.1.148's truecolor SGRs — widen the regex to capture both shapes." The architect's own technical-notes comment on #85 flagged the deeper smell: classifying highlight state by string-equality on captured SGR text re-couples to whatever shape claude paints next time. This ticket promotes that observation to the spec premise.

**Same layered-volatility shape as #72 → #81** (snapshot-drift coupled to raw bytes vs decoupled via parsed-shape comparison). Each renderer-version change unmasks the next coupling layer until presentation is decoupled entirely.

Concrete failure mode carried forward: `pkg/tuidriver/picker.go:47-48` and `pkg/tuidriver/modal.go:80-81` recognise picker rows via `\x1b\[38;5;(246|153)m/<letter>`. claude 2.1.148 emits `\x1b\[38;2;<r>;<g>;<b>m` truecolor SGRs instead. Indexed-color regex misses → `DetectModalClass` returns empty → `ParsePicker` returns nil → `spike-multiselect` logs `modal-class detected=` / `no-parser-for-modal-class class=`. Blocks the picker AC in [#81](https://github.com/pyrycode/tui-driver/issues/81).

The `mcp` and `agents` parsers are unaffected — both key on stripped-text anchors (`ManageMCPservers`, `Agents` + tab labels). That's the model this ticket adopts for picker: structural anchor for row-finding, parsed-RGB for highlight classification.

## Design

### Two concerns, two seams

1. **Row finding** — locate picker rows via a structural property of the stripped text, with **no reference to color SGRs**.
2. **Highlight classification** — for each found row, walk the raw bytes of that row's segment, parse foreground-color SGRs to RGB triplets, and compare to a known-highlighted RGB constant.

The two seams compose: `findPickerRows(snap) → []pickerRow` returns row boundaries + the open-color RGB; `ParsePicker` consumes the rows and reuses the existing text-reconstruction inner loop; `DetectModalClass` checks `len(findPickerRows(snap)) > 0`.

### Row-finding anchor

**Anchor:** a line whose `StripANSI(StripOSC(line))` content, after trimming leading whitespace, begins with `/` followed by a `[a-zA-Z]` name character.

Rationale:
- Survives any future SGR encoding migration (indexed, truecolor, hypothetical HSL, none-at-all).
- Survives the unfiltered/filtered structural difference: in both modes the row's first visible character is `/` (filtered mode just interleaves color shifts inside the row; the stripped shape is identical).
- Distinguishes picker rows from idle text mentioning a slash-command (banner text like `? for shortcuts · ← for agents` doesn't start with `/`; markdown body would mention `/foo` mid-line, not as the line's first non-space character).

**Line segmentation:** `bytes.Split(snap, []byte{'\n'})`. claude emits `\r\r\n` between picker rows (observed in the existing fixture); `Split` on `\n` keeps the `\r\r` as trailing noise on the preceding segment which is harmless — `StripANSI` + leading-whitespace trim absorbs it for the per-line check, and the raw-byte color walker (below) ignores it because it scans for `/`, not for line endings.

**Rejected alternatives:**
- *Render (vt10x) → grid → lines starting with `/`*: works for row-finding but discards SGR data, so highlight classification would need a second pass over the raw bytes to correlate stripped positions back to raw offsets. The line-based StripANSI approach keeps both concerns in a single pass per line.
- *Leading-glyph anchor (`❯` marks the highlighted row)*: claude's `/mcp` listing uses this pattern, but the slash-picker doesn't paint a leading `❯` per the existing fixture and the doc comment in `picker.go:20-49`. Confirm empirically against the new 2.1.148 fixture; if `❯` IS present, prefer it as the anchor (simpler than RGB comparison for highlight too). If not, fall back to the structural slash-letter anchor specified above. **Open question — resolves at fixture capture time.**
- *Box-drawing framing (`╭╰│─`)*: locates the picker as a region but doesn't identify individual rows or their highlight state. Useful for "is there a picker at all" but degenerate for row enumeration.

### Highlight classification

**Approach:** a tiny SGR state machine. For each found row segment (raw bytes from line start to `\n`), walk byte-by-byte tracking the current foreground RGB. When the walker reaches the `/` character that the structural anchor identified, capture the current RGB as the **row open color**. Compare to the known-highlighted RGB constant.

State machine rules:
- Default RGB: a sentinel zero value (e.g. `RGB{}` with an `ok bool` companion to distinguish "default/unknown" from "actual black"). Treat unknown as **not highlighted**.
- `\x1b[38;5;<n>m` → look up `<n>` in the xterm-256 palette → set RGB.
- `\x1b[38;2;<r>;<g>;<b>m` → set RGB directly.
- `\x1b[39m` → reset to default/unknown.
- Any other CSI sequence (including `\x1b[<N>C` cursor-forward, style SGRs like `\x1b[1m`, etc.) → no change.
- Literal bytes → no change.

**Highlighted RGB constant:** populate empirically. xterm-256 index 153 maps to RGB(175, 215, 255) (light blue); the gray of index 246 is RGB(148, 148, 148). claude 2.1.148 truecolor mode may paint at slightly different triplets — the developer captures a fresh 2.1.148 snapshot, runs `xxd` to identify the highlighted-row triplet, and encodes it as the constant. The ticket body's "RGB(153,153,153) / RGB(204,204,204)" guesses are unconfirmed; do not encode them without verification. **Open question — resolves at fixture capture time.**

If indexed 153 and the truecolor variant resolve to the same RGB (likely — both renderers ultimately target the same on-screen color), the constant covers both. If they differ, the classifier accepts a small set of "known highlighted" RGB values rather than a single constant — keep the set in `pickerHighlightedRGBs` so a future palette tweak is one-line.

### Indexed-to-RGB palette

xterm-256 layout is deterministic:
- **Indices 0–15** (system + bright ANSI): fixed 16-entry lookup table. Standard values; reference `https://en.wikipedia.org/wiki/ANSI_escape_code#8-bit` or any xterm source. Encoded as a `[16]RGB` literal.
- **Indices 16–231** (6×6×6 color cube): `n = 16 + 36*r + 6*g + b` where each component ∈ {0, 1, 2, 3, 4, 5} mapping to {0, 95, 135, 175, 215, 255}. Compute arithmetically; do NOT embed a 216-entry literal.
- **Indices 232–255** (grayscale ramp): `gray = 8 + 10 * (n - 232)`, applied to all three RGB components. Compute arithmetically.

Total helper: ~30 lines (16-entry literal + two arithmetic branches). Do NOT generate a 256-entry literal — the arithmetic is the elegant form.

### Public surface

**Unchanged.** `DetectModalClass(snap []byte) ModalClass` and `ParsePicker(snap []byte) []PickerItem` keep their signatures. Consumer (`spike-multiselect`) requires zero changes. Out of scope: exposing colour-parsing helpers to consumers (ticket explicitly excludes a library-API redesign).

### Internal surface (new, unexported)

- `pkg/tuidriver/picker_color.go` (new file):
  - `type rgb struct{ R, G, B uint8 }` (unexported; package-internal RGB triplet)
  - `xterm256ToRGB(n uint8) rgb` — palette mapping (16-entry literal + arithmetic for 16+)
  - `parseForegroundSGR(b []byte) (color rgb, consumed int, isFg bool, isReset bool)` — recognise `38;5;N`, `38;2;R;G;B`, `39`; consumed=length of the recognised escape including ESC `[` and final `m`; isFg=true means `color` is meaningful; isReset=true overrides isFg (means clear-to-default)
  - `pickerHighlightedRGBs []rgb` — known-highlighted colour set (populate from fixture)
- `pkg/tuidriver/picker.go` (modified):
  - Remove `pickerItemStartRe`. Keep `pickerColorCodeRe`, `pickerCsiCursorFwdRe`, `pickerCategoryRe` (still needed for description reconstruction).
  - New `findPickerRows(snap []byte) []pickerRow` where `pickerRow` is an unexported struct `{ raw []byte; openColor rgb; openColorKnown bool }`.
  - `ParsePicker` now iterates `findPickerRows` output instead of regex matches; highlight set by `openColorKnown && rgbIsHighlighted(openColor)` (or the fallback "first row in filtered mode" rule documented below).
- `pkg/tuidriver/modal.go` (modified):
  - Remove `slashPickerRowRe`.
  - `DetectModalClass` slash-picker branch: `if len(findPickerRows(snap)) > 0 { return ModalClassSlashPicker }`. May now run AFTER the `StripANSI` step rather than before — the new anchor doesn't need raw SGR access; the "check picker first on raw bytes" ordering rationale (currently at `modal.go:97-103`) is obsolete. **Re-evaluate the order**: place slash-picker check immediately after the cheap `StripANSI`/`StripOSC` call, ahead of the substring switch. Confirm the hint-bar false-positive guard test still passes after the reorder.

### Filtered-mode highlight semantics

The existing code documents two modes (`picker.go:20-49`):
- **Unfiltered** (`/` only): exactly one row's open-color is the highlight color; the rest are normal.
- **Filtered** (e.g. `/fi`): every row's open-color is the normal color; substring highlights live mid-row; the default-selected row is the first match (claude convention).

The new classifier preserves this:
- Count rows whose `openColor` matches the highlight set.
- If `count >= 1`: highlight is `openColor matches highlight set` (per-row, same as today's `openColor == "153"` check).
- If `count == 0`: fall back to "first row is highlighted" (same fallback as today's `i == 0` branch).

Same control flow as today's `highlightedCount153` logic, but keyed on RGB instead of captured colour-index string.

## Concurrency model

N/A — these are pure functions over a byte snapshot. No goroutines, no shared state.

## Error handling

- `ParsePicker(nil)` → nil (preserve current behaviour, covered by `TestParsePickerReturnsNilOnNonPicker`).
- `ParsePicker` on a snapshot with no matching rows → nil.
- `parseForegroundSGR` on a malformed sequence (e.g. `38;5;` with no digits) → `isFg=false`, `consumed=` whatever was scanned past `m`, walker treats as no-op.
- Unknown SGR shape (hypothetical `38;6;…`) → `isFg=false`, walker treats as no-op → row found but unhighlighted. AC #5 (third-encoding test) asserts this.
- No panics. No errors returned (the API is `func(snap) result`, not `func(snap) (result, error)` — preserve existing surface).

## Testing strategy

### Unit tests — colour helper (new file `picker_color_test.go`)

- **`xterm256ToRGB` table**: cover the four regions — `(0, RGB{0,0,0})`, `(15, RGB{...bright white...})`, `(16, RGB{0,0,0})`, `(196, RGB{255,0,0})` (cube), `(231, RGB{255,255,255})`, `(232, RGB{8,8,8})`, `(255, RGB{238,238,238})`. Plus the two empirically relevant indices: `(153, ...)` and `(246, RGB{148,148,148})`.
- **`parseForegroundSGR` table**: indexed (`\x1b[38;5;153m`), truecolor (`\x1b[38;2;175;215;255m`), reset (`\x1b[39m`), unrelated CSI (`\x1b[1m` style, `\x1b[43C` cursor-forward) → all return correct `isFg`/`isReset`/`consumed`/`color`.

### Unit tests — modal/picker

- **`TestDetectModalClassSyntheticAnchors` (modified)**: replace the three `slash-picker SGR row …` cases with structural-anchor cases — `/figma-use\n` (bare line), `\x1b[38;5;246m/figma-use\x1b[39m` (indexed-coloured line), `\x1b[38;2;148;148;148m/figma-use\x1b[39m` (truecolor-coloured line), `\x1b[38;5;246m/\x1b[38;5;153mp\x1b[38;5;246mlugin` (filtered-mode line). All four MUST detect as `ModalClassSlashPicker`. The hint-bar false-positive guard case (`? for shortcuts · ← for agents`) MUST still detect as `ModalClassUnknown`.
- **`TestParsePickerRealFixture` (modified)**: parametrise over both fixtures — `testdata/picker-snapshot.bin` (existing indexed-color) and `testdata/picker-snapshot-truecolor.bin` (new, captured during this work). Same structural assertions for both: first item `/figma-use`, `Highlighted=true`, at least one item with `Category=="figma"`, all commands start with `/`, exactly one highlighted in unfiltered mode.
- **Hypothetical-third-encoding test** (new, `picker_test.go`): synthesise a snapshot with rows using `\x1b[38;6;X;Y;Zm/foo\n\x1b[38;6;A;B;Cm/bar\n` (a made-up SGR shape). Assert `ParsePicker` returns 2 rows (anchor doesn't depend on colour encoding), commands `/foo` and `/bar`, both with `Highlighted=false` (unknown encoding → unknown colour → fallback to "first row is highlighted" since no row is in the highlight set). The test's docstring states: "to support a new colour encoding, add one case to `parseForegroundSGR`; no edits to the row anchor or the highlight classifier are required." This is the AC #5 seam proof.
- **Fixture capture procedure** (note in PR description, not a test): `make build` → `TUIDRIVER_STRICT_MCP_CONFIG=1 ./bin/spike-multiselect -trust-folder=accept -trigger='/'` against claude 2.1.148+ → copy the resulting `/tmp/spike-multiselect-bytes-*.bin` to `pkg/tuidriver/testdata/picker-snapshot-truecolor.bin`. Verify with `xxd` that the file contains `\x1b[38;2;` sequences (not `\x1b[38;5;`). Verify with `make e2e snapshot-check` that the read-only byte-compare harness (`cmd/e2e-snapshot-check`) is updated to know about the new fixture.

No `t.Skip` in any added or modified tests (AC requirement).

## Open questions

1. **Leading-glyph anchor vs slash-letter anchor.** Does claude 2.1.148's slash-picker render a leading `❯` on the highlighted row? Inspect the new fixture; if present and reliable, the anchor simplifies to "line starts with `❯` or `/<letter>`" and highlight classification becomes "line starts with `❯`" (no RGB needed). If not, keep the slash-letter + RGB design. Resolve at fixture capture time, BEFORE writing the helper.
2. **Highlight RGB value(s).** Capture truecolor fixture, identify highlighted-row RGB via `xxd` on `\x1b[38;2;` sequences in the row that should be selected (`/figma-use`). Verify whether indexed 153 and the truecolor variant resolve to the same RGB; if not, `pickerHighlightedRGBs` is a small set, not a single constant.
3. **`cmd/e2e-snapshot-check` integration.** This binary byte-compares testdata snapshots against re-captures. Adding `picker-snapshot-truecolor.bin` to the testdata set requires updating its registry (find via `grep -rn picker-snapshot cmd/e2e-snapshot-check/`); confirm the binary auto-discovers `*.bin` in `testdata/` or needs an explicit list. **Verify before adding the fixture, not after** — a missing registry update means CI doesn't notice fixture drift on this file.

## Out of scope

- The snapshot-drift parsed-shape redesign ([#81](https://github.com/pyrycode/tui-driver/issues/81)) — downstream consumer; depends on this landing.
- Refactoring `mcp` or `agents` parsers — they already use the stripped-text-anchor pattern.
- Generalising the structural-anchor approach to other modal classes (permission, trust-folder, AskUserQuestion). Each modal has its own anchor; this ticket scopes to picker only.
- Library-API redesign exposing parsed-colour helpers to consumers. Internal helpers only.
- Pulling `picker_color.go` into a shared `color.go` reusable by other modules. Premature — picker is the only consumer today.

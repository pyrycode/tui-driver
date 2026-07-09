# Spec #242 — permission modal class requires the option-row dialog shape

> **Hand-run this ticket outside the pipeline.** It is about a detector whose
> anchor is a screen phrase. Working the ticket means opening source that renders
> that phrase, which can forge the very state being fixed. This spec references
> the anchor **only by symbol** (`anchorPermissionSpaced`) and never quotes the
> literal; keep that discipline in code, comments, and commit messages, exactly
> as #219 and #223–#237 did.

## Files to read first

- `pkg/tuidriver/trust.go:1-73` — **the precedent to mirror.** `gridHasTrustDialog`
  (header anchor + pointer-marked option row within a downward lookahead) is the
  #219 shape-gate this ticket applies to permission. Copy its structure; change
  exactly one thing (region scope — see Design).
- `pkg/tuidriver/modal.go:104-116` — anchor var block; `anchorPermissionSpaced` is
  the permission anchor (defined here, used by the arm).
- `pkg/tuidriver/modal.go:118-127` — `permissionRegionRows` (=12) and why the
  bottom-window scope is load-bearing (rejects above-window transcript forgery).
- `pkg/tuidriver/modal.go:182-215` — `detectModalClassWithGrid`; the permission arm
  is line 193 (the one line that changes). Read the surrounding switch and its
  `#223`/co-signal preamble comment (lines 163-168) — permission is the last arm
  still matching on anchor-alone; the preamble already claims "each class also
  requires a structural co-signal," which this ticket makes true for permission.
- `pkg/tuidriver/permission.go:41-45` — `modalOptionRe`; group 1 is the `❯` marker,
  group 2 the number. The shared option-row shape `gridHasTrustDialog` reuses.
- `pkg/tuidriver/dialog_shape.go:8-36` — the alternative co-signal
  (`selectionOptionRe` / `gridHasSelectionDialog`). Not chosen here (see Design §
  "Which option-row matcher") but read it to understand why.
- `pkg/tuidriver/grid.go:79-113` — `Grid.Rows()`, `ContainsInLastRows`, `LastRows`.
  The new helper scopes via `LastRows(permissionRegionRows)`; understand its
  clamp-to-len and "aliases backing store, read-only" contract.
- `pkg/tuidriver/anchor_forgery_test.go:82-116` — `TestModalPhraseAnchorsRejectBodyForgery`
  (permission's *above-window* forgery, already green) and
  `pkg/tuidriver/anchor_forgery_test.go:181-195` —
  `TestTrustAnchorRejectsStatusRegionQuotation` (the *in-region* case the new
  permission test mirrors).
- `pkg/tuidriver/anchor_forgery_test.go:282-302` —
  `TestNegativeSuitePositiveControls`; asserts `permission-snapshot.bin` still
  classifies as `ModalClassPermission`. This is the positive control the fix must
  not break.
- Test helpers: `gridRows(rows ...string) []byte` (`pkg/tuidriver/state_test.go:13`)
  builds a flat rendered grid from rows via `\r\n`; `loadFixture`
  (`pkg/tuidriver/permission_test.go:12`).

## Context

The permission class is the **last single-fabric class** in `DetectModalClass`.
Every other class was given a structural co-signal of a different fabric (#219
gave trust the pointer-marked option row; #223 gave the full-panel classes the
picker-highlight color or the selection-option shape) so that one on-screen line
quoting a class's header text no longer classifies as that modal. Permission
still fires from one region-scoped phrase alone: the arm at `modal.go:193` fires
whenever `anchorPermissionSpaced` appears anywhere inside the bottom
`permissionRegionRows` window.

That forges in production. Replaying the corpus (this org's tickets are *about*
this detector, so its agents stream the phrase through the bottom rows chronically)
shows the class flapping shown/hidden six times in one recording and five more
times in another, on frames whose rendered screen is plain transcript prose — no
dialog. Two consumers make this matter: `ParseModalContent` (permission.go:66,
which calls `DetectModalClass`) surfaces a forged dialog to a human operator (the
mobile `modal_shown` path, pyrycode #716), and `Session.AnswerModal`
(answer.go — dispatch accepts `ModalClassPermission`) can route an answer
keystroke into a live turn (the gated remote `modal_answer` path, pyrycode
#717/#791).

The fix is the one #219 applied to trust: **require the dialog shape, not the
prompt phrase alone.** A real permission overlay always renders a pointer-marked
numbered option row directly below the prompt; a transcript quotation of the
phrase does not.

**Empirical confirmation of the layout** (rendered `permission-snapshot.bin`,
11 rows total, all inside `permissionRegionRows`=12):

| row (from bottom) | content shape | matches |
| --- | --- | --- |
| bottom-5 | the prompt (`anchorPermissionSpaced`) | anchor |
| bottom-4 | `❯ 1. …` | `modalOptionRe` group 1 = `❯ ` (non-empty) ✓ |
| bottom-3 | `  2. …` | number-only (group 1 empty) |
| bottom-2 | `  3. …` | number-only |
| bottom-1 | the `(Esc to cancel …)` hint bar | — |

The `❯`-marked option row sits **directly below** the prompt (gap of 1), exactly
as trust does — so requiring it is safe and the lookahead mirrors trust's.

## Design

One new region-scoped shape helper in `modal.go` + a one-line arm swap. No new
exported types, no signature changes, no new regex.

### The helper (`modal.go`)

Add next to `permissionRegionRows` / `gridContains` (its natural home — the
classification file that already owns the region-scope constant and the anchor):

```go
// permissionDialogLookahead bounds how many rows below anchorPermissionSpaced the
// pointer-marked numbered option row may sit and still count. Mirrors
// trustDialogLookahead: claude renders the ❯-marked option directly below the
// prompt (permission-snapshot.bin: prompt at bottom-5, option at bottom-4); the
// slack tolerates a blank or wrapped prompt line between them.
const permissionDialogLookahead = 3

// gridHasPermissionDialog reports whether g renders claude's real permission
// overlay: anchorPermissionSpaced inside the bottom permissionRegionRows window
// AND, within the next permissionDialogLookahead rows of that same window, a
// pointer-marked numbered option row (modalOptionRe, group 1 = ❯ marker). Region
// scope AND shape — the #242 fix.
func gridHasPermissionDialog(g *Grid) bool
```

**Behavior contract** — structurally identical to `gridHasTrustDialog`
(`trust.go:33-47`) with **one deliberate difference**: it iterates
`g.LastRows(permissionRegionRows)` instead of `g.Rows()`. Concretely:

- `rows := g.LastRows(permissionRegionRows)` — the bottom overlay window.
- For each `i, row` where `row` contains `string(anchorPermissionSpaced)`:
  - Look ahead `j` from `i+1` to `min(i+1+permissionDialogLookahead, len(rows))`.
  - If `modalOptionRe.FindStringSubmatch(strings.TrimLeft(rows[j], " "))` returns a
    match with a **non-empty group 1** (the `❯` marker), return `true`.
- Return `false` otherwise.

Because `rows` is already the bottom window and the lookahead below the anchor
can only move *toward* the screen bottom (never above the anchor), **both the
anchor and the option row are guaranteed inside `permissionRegionRows`** — this
is the AC's "all inside the existing region window," satisfied by construction,
not by a second explicit bound. (Invariant verified against the fixture: anchor
at window index for bottom-5, option at bottom-4, `end = min(i+1+3, len(rows))`
finds it; when the anchor is the *last* row of the window, `end == len(rows)`,
the loop runs zero times, and the helper correctly returns `false` — the negative
case.)

### The arm swap (`modal.go:193`)

Replace the anchor-alone arm with the shape helper:

```go
case gridHasPermissionDialog(g):
    return ModalClassPermission
```

This is a strict **tightening** of `DetectModalClass`: every snapshot that fired
before AND has the option row still fires; a snapshot with the phrase but no
option row no longer does. No caller changes — `ParseModalContent`,
`Session.AnswerModal`, and `modalDismissed` all inherit the tightened
classification for free (the desired security outcome: a forged permission no
longer parses, surfaces, or accepts an answer).

### Landmine — do NOT regress to a whole-grid match

`gridHasTrustDialog` scans **the whole grid** (`g.Rows()`) because trust is a
startup/full-panel class. **Permission is bottom-region scoped and that scope is
load-bearing** — it is what rejects an *above-window* transcript forgery (the
case `TestModalPhraseAnchorsRejectBodyForgery` already pins green). The new helper
must keep **both** guards: region scope (`LastRows(permissionRegionRows)`) **and**
shape (the pointer-marked option row). Iterating `g.Rows()` here would silently
drop the region guard and reintroduce the above-window forgery. This is the one
place trust and permission legitimately differ; the rest of the helper is a
byte-for-byte mirror.

### Which option-row matcher: `modalOptionRe`, not `selectionOptionRe`

Use `modalOptionRe` with group-1-required (the trust helper's exact choice), not
`gridHasSelectionDialog`/`selectionOptionRe`. Two reasons:

1. **Faithful mirror.** `gridHasTrustDialog` uses `modalOptionRe` + `m[1] != ""`;
   using the same keeps the two dialog-shape helpers structurally identical, so
   they can't drift (the #163 no-drift discipline).
2. **Tighter fabric.** `selectionOptionRe` additionally admits the `❯ [✔]`
   checkbox form (the 2.1.199 MCP-enablement modal). A permission overlay never
   renders a checkbox option — it is always numbered — so admitting the checkbox
   form would only widen the match with no benefit.

### Doc hygiene (part of this ticket)

- Update the `permission →` line in the anchor-catalog comment (`modal.go:85-86`)
  to state the new co-signal: anchor **AND** a pointer-marked numbered option row
  directly below it, region-scoped — reference `gridHasPermissionDialog`. Mirror
  the wording of the `trust-folder →` entry above it (`modal.go:79-84`).
- Optional: the tier-1 list in `anchor_forgery_test.go:38-43` says permission is
  protected by "bottom-region scoping"; it is now "bottom-region scoping AND the
  option-row shape." A one-clause update keeps the comment honest but is not
  required for green.

## Concurrency model

None. `gridHasPermissionDialog` is a pure, synchronous predicate over an
already-rendered `Grid` — no goroutines, no shared state, no I/O. It runs inside
the existing single-render `detectModalClassWithGrid` path (the grid is rendered
once per tick and threaded in, #225); this change adds no render.

## Error handling

None to add. The helper returns `bool`; there is no error path. Degenerate inputs
are already safe: an empty snapshot yields a zero-row `Grid` (`grid.go:67-73`),
`LastRows` clamps `n` to the row count and returns `nil` for an empty grid, and
the range over `nil` is a no-op → `false`. A snapshot with the anchor but no
option row → `false` (the fix). A detected-but-unparseable modal continues to
yield `nil` from `ParseModalContent` (permission.go:87), unchanged.

## Testing strategy

### Required — new in-region negative case (AC #3)

Add `TestPermissionAnchorRejectsStatusRegionQuotation` to
`anchor_forgery_test.go`, mirroring `TestTrustAnchorRejectsStatusRegionQuotation`
(`anchor_forgery_test.go:181-195`). Scenario, as bullets (developer writes it in
the file's idiom):

- Build ~20 filler rows of ordinary transcript text, then a final row that quotes
  `string(anchorPermissionSpaced)` as content (e.g. a `log:` line), via
  `gridRows(...)`. Reference the anchor **by symbol**, never the literal.
- The anchor lands on the **last** row → inside `permissionRegionRows`, with **no
  option row below it** anywhere.
- Assert `DetectModalClass(snap) != ModalClassPermission`. Before the fix this
  fired (in-region anchor alone); after, it must not.
- Guard against a vacuous fixture: assert the snapshot bytes still contain the
  anchor (mirror trust's `strings.Contains` fixture check pattern) so a rendering
  change can't silently void the contrast.

### Recommended — synthetic in-test positive control (non-vacuity)

Follow the `TestSlashPickerRejectsBodyForgery` precedent
(`anchor_forgery_test.go:268-279`) and add a positive control in the same test:
the same filler, then the anchor row, then a `❯ 1. …`-shaped option row directly
below it, all in the bottom region → assert `DetectModalClass(snap) ==
ModalClassPermission`. This proves the lookahead fires on the real shape
independent of the `.bin` fixture, so the negative case can't pass merely because
`gridRows` or the region math broke. (The committed-fixture positive control
already lives in `TestNegativeSuitePositiveControls`; this synthetic one guards
the *new* lookahead path specifically.)

### Existing controls that must stay green (AC #2)

- `TestNegativeSuitePositiveControls` — `permission-snapshot.bin` still
  classifies as `ModalClassPermission` **and** `ParseModalContent` still extracts
  its options. Verified safe above: the fixture's selected option row carries the
  `❯` marker (bottom-4), so the shape gate passes.
- `TestModalPhraseAnchorsRejectBodyForgery` — permission's above-window forgery
  stays green (the region scope is preserved).

### CI gate vs. hand-run confirmation

- **Deterministic CI gate** (`make check` = `vet test`, claude-free): the new
  `anchor_forgery_test.go` case. This is what locks the fix as a regression.
- **Hand-run only** (AC #5): `make corpus-replay` reads an external, machine-local,
  uncommitted corpus (`CORPUS_DIR`, default `$HOME/.local/share/pyry-recordings`);
  it is the operator's empirical confirmation that the two named recordings'
  permission false-fires are gone with the fixture positive control unchanged. It
  is **not** part of `make check` and the developer cannot run it in the
  claude-free gate — do not add it as an automated dev deliverable.

## Security review

Ticket is `security-sensitive`. The label-gated review pass is recorded below the
Open questions section.

## Open questions

- **Lookahead value.** `permissionDialogLookahead = 3` mirrors `trustDialogLookahead`
  and covers the observed gap-of-1 with slack. If a future permission layout
  variant (e.g. a wrapped multi-line prompt) inserts more than 2 rows between the
  prompt and its first option, the helper would miss it — but no such variant is
  in the corpus today, so 3 is the evidence-based value. Do not widen speculatively
  (Evidence-Based Fix Selection); a real capture showing a larger gap is the
  trigger to revisit.
- **Residual (accepted, same as trust).** A forgery that renders BOTH the anchor
  phrase AND a `❯`-marked numbered option row within 3 rows below it, inside the
  bottom window, would still classify. This is a far higher bar than a bare phrase
  and matches exactly the residual #219 accepted for trust; closing it further is
  out of scope for this ticket.

---

## Security-review pass (`security-sensitive`)

Adversarial self-review of this spec, per the architect security-review protocol.

**Trust boundary.** The input `snap` is claude's PTY byte stream — fully
attacker-influenceable: a hostile prompt can make claude render arbitrary text,
including a verbatim rendering of a permission dialog's prompt line, anywhere on
screen. The security property this detector must hold: **claude's own live
permission overlay classifies as `ModalClassPermission`; a rendering of that
prompt as ordinary transcript content does not.** Downstream, that classification
gates a human-facing "a permission dialog is up" surface (pyrycode #716) and a
keystroke-into-live-turn answer path (pyrycode #717/#791) — so a false positive is
the exploitable direction (forge a dialog / mis-route an answer), and a false
negative degrades to "operator sees no dialog," the fail-safe direction.

**Threat: content forgery (the class of bug being fixed).**
- *Above-window forgery* — phrase quoted high in the transcript body: rejected by
  the preserved region scope (`LastRows(permissionRegionRows)`). Pinned by the
  existing `TestModalPhraseAnchorsRejectBodyForgery`.
- *In-region forgery* — phrase scrolling through the bottom window with no dialog:
  rejected by the new shape requirement (no pointer-marked option row → no fire).
  Pinned by the required new test. This is the exact production forgery the ticket
  documents.
- *Shape-plus-phrase coincidence* — attacker renders both the phrase and a
  `❯`-numbered row within lookahead: still classifies. Accepted residual,
  identical to #219's trust decision; a numbered `❯`-marked row is chrome content
  agents essentially never emit as prose. Documented in Open questions, not left
  implicit.

**Threat: fabric collapse (belt-and-suspenders check).** The fix pairs two guards
of **different fabric** — region position (deterministic grid geometry) and dialog
shape (deterministic regex on the rendered row). Neither is a stochastic agent
rule; both are code. The landmine section forbids collapsing them into one
(whole-grid match drops region; anchor-alone drops shape). The spec keeps both.
Pass.

**Threat: co-signal chosen from the same fabric as the anchor.** Rejected design
would reuse the *phrase* as its own co-signal (e.g. requiring the phrase twice).
The chosen co-signal (`modalOptionRe` pointer marker) is a structurally different
feature (a glyph+number row shape) from the anchor (a prose phrase), so a single
quoted line cannot satisfy both. Pass.

**Threat: the fix silences a real dialog (fail-open).** A real overlay always
renders the `❯`-marked numbered row directly below the prompt
(`permission-snapshot.bin`, empirically confirmed). The positive controls
(committed fixture + synthetic in-test) assert this. If a genuine future dialog
rendered no `❯` marker, permission would fail to *classify* — but the #224
`HasUnknownDialog` layer (`dialog_shape.go:38-63`) still catches an unrecognized
`❯`-selection dialog at readiness, so the run surfaces `*UnexpectedModalError`
rather than delivering a prompt into it. The fail-safe net exists and is
unchanged. Pass.

**Threat: information leak via the spec/commit itself.** The self-reference risk
is real for this ticket: rendering the anchor literal on an operator's screen can
forge the state. The spec references the anchor only by symbol and instructs the
same for code/comments/commits. The new test constructs the forgery via
`string(anchorPermissionSpaced)`, not a quoted literal. Pass.

**Verdict: PASS.** The spec adds a deterministic, different-fabric structural
co-signal that closes the observed in-region forgery while preserving the region
scope that closes the above-window forgery, keeps the fail-safe `HasUnknownDialog`
net intact, and maintains the by-symbol discipline. No unresolved security
concern blocks implementation.

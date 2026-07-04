# #153 — Rendered-grid refactor 4/6: port idle/busy detection to the grid status region

Slice 4/6 of the rendered-grid refactor (Cross-Repo Code Review 2026-07-03, fixes CRITICAL A + B). This slice ports the **idle/busy axis** — `IsIdle` and `IsThinking` — from substring-matching the raw append-only history buffer onto the addressable rendered `Grid` shipped by #150, scoped to the **bottom status region** of the visible screen.

## Files to read first

- `pkg/tuidriver/state.go:9-54` — the two anchor `[]byte` vars (`IdleGlyph` ❯, `SpinnerGlyph` ✻) and the current `IsIdle`/`IsThinking` bodies you are rewriting. Keep the vars; keep the exported signatures. Note the docstring contract you must preserve: the idle conjunction is load-bearing (`❯` present AND `✻` absent), and `❯` is redrawn **beneath** the spinner while thinking.
- `pkg/tuidriver/grid.go:64-105` — `NewGrid(snap, cols, rows) *Grid`, `Rows()`, `ContainsInLastRows(sub string, n int) bool`. `ContainsInLastRows` takes a **`string`**, not `[]byte`; `n<=0`→false, `n` clamped to row count, match confined to a single row (off-screen/above-region text excluded — the whole point). Empty render → zero rows, so degenerate input falls through to a defined `false`.
- `pkg/tuidriver/state_test.go:5-72` — the existing idle/thinking unit tests that MUST continue to pass. All use tiny 1–2-row inline fixtures, so they pass for any region size ≥ 1. Your new regressions extend this file.
- `pkg/tuidriver/modal.go:96-143` — `DetectModalClass`: the sibling detector's posture. It also keeps `(snap []byte)` and builds its view internally. This slice mirrors that "grid built inside, dimensions never leak to callers" shape (with `NewGrid(snap, 0, 0)` instead of `StripANSI`).
- `docs/knowledge/codebase/150.md` §§ "Lessons learned" — two fixture invariants you inherit: (1) `strings.Split("", "\n")` gives `[""]`, so empty render is special-cased to **zero** rows (already handled by `NewGrid`); (2) **vt10x treats a bare `\n` as line-feed-only** — multi-row grid fixtures MUST use `\r\n` between rows or they render as a staircase.
- `docs/knowledge/architecture/system-overview.md:101-105` — the `classify`/`Events` merge loop (modal axis dominates, idle/thinking suppressed under a modal) and the documented `IsIdle` "stuck-✻-in-4KB-rolling-buffer" wedge (#69) the spikes work around with PTY-quiescence. Context for why region-scoping is safe and beneficial (see Design § Beneficial consequence).

## Context

`IsIdle` (`state.go:38-44`) and `IsThinking` (`state.go:52-54`) classify state by `bytes.Contains` over `StripANSI(snap)` of the **whole** append-only history buffer. Two forgeries follow (CRITICAL B, idle/busy axis):

1. A literal spinner glyph `✻` printed as rendered markdown or tool output **mid-transcript** forges "thinking" while the session is actually idle.
2. A stale input-prompt glyph `❯` from a previous turn's input line, still living in **scrolled-off history** (within the 4 KB rolling buffer), forges "idle" while a turn is running — the consumer then writes its next prompt into the in-flight turn, with no error at any layer.

Both share the chain's root cause: **state is classified from a raw history buffer instead of the rendered grid.** #150 shipped the `Grid` accessor (the visible screen, off-screen history excluded). This slice consumes it for the idle/busy axis. Forgery (2) is the dangerous direction (a corrupted live turn); (1) is the conservative direction (the consumer waits). Region-scoping removes both.

**Scope boundary.** This ticket ports *where* idle/busy is decided (whole buffer → bottom status region). *What* set of anchors constitutes "busy" is #156's concern (adds `esc to interrupt` as a second busy anchor). Both edit `state.go`'s idle/busy predicates and must converge on **one** region-scoped busy predicate — see Design § #156 seam.

## Design

One production file: `pkg/tuidriver/state.go`. No new exported symbol; both signatures unchanged (`IsIdle(snap []byte) bool`, `IsThinking(snap []byte) bool`), so **zero caller edits** across the ~15 call sites (`events.go:384-385`, `ready.go:43`, `deliver.go:185`, every `cmd/spike-*`/`cmd/probe-*`). Grid dimensions never leak — each predicate builds `NewGrid(snap, 0, 0)` internally, the same posture as `DetectModalClass`.

### The status region

Idle/busy is a **bottom overlay**, not a tall panel. The input line `❯` is the bottom-most content; the spinner `✻` (and #156's `esc to interrupt` hint) render in the handful of rows just above it. Calibrated against the committed `.bin` captures rendered through `NewGrid(snap, 0, 0)` (rows-from-bottom, −1 = last rendered row):

| Capture | `❯` row(s)-from-bottom | Meaning |
|---|---|---|
| `mcp-empty-snapshot.bin` | **−3**, −8 | real idle input line at −3; echoed `❯ /mcp` at −8 |
| `permission-snapshot.bin` | −4 | selection cursor (`Do you want to proceed?` at −5) |
| `trust-folder-snapshot.bin` | −3 | selection cursor |
| `agents-snapshot.bin` | −8 | echoed `❯ /agents` (modal below) |
| `mcp-snapshot.bin` | −20, −27 | scrolled off — the forgery (2) shape |
| `picker-truecolor-snapshot.bin` | −21 | scrolled off |

The real idle input line sits at **−3** (hint bar −1, input-box bottom border −2, `❯` line −3). No committed capture contains `✻`, but `state.go`'s docstring states `❯` is "redrawn beneath the spinner" — i.e. the spinner renders **adjacent above** the input line, ~−4/−5.

**Introduce one unexported constant:**

```go
// statusRegionRows bounds the idle/busy status region to the bottom N
// rendered rows of the grid. Sized to include the input line (❯, ~row −3
// from bottom) and the spinner line redrawn just above it (✻, ~row −5),
// while excluding transcript body above the overlay (~row −6 and up when
// idle). Small by construction — widening toward the whole grid would
// reintroduce the mid-transcript forgeries this slice removes.
const statusRegionRows = 6
```

Calibration rationale for `6`: catches the idle `❯` (−3) and the adjacent spinner (~−5) with one row of margin, excludes idle transcript body (−6+). This value is pinned in both directions by the regressions below — the realistic-thinking fixture fails RED if it is too small; the forgery fixtures fail RED if it is widened toward the whole grid. If a live thinking frame ever shows the spinner sitting higher than −6, bump the constant (one line; the tradeoff is recorded here) rather than widening ad hoc.

### The predicates

```go
// busyInRegion reports whether a busy anchor is present in the status
// region. THE single busy predicate — region-scoping lives in exactly one
// place so IsIdle and IsThinking stay coherent. #156 folds its
// "esc to interrupt" hint in here as an OR (see § #156 seam).
func busyInRegion(g *Grid) bool  // g.ContainsInLastRows(string(SpinnerGlyph), statusRegionRows)
```

- `IsThinking(snap)` — build the grid once, return `busyInRegion(g)`. No raw-history path.
- `IsIdle(snap)` — build the grid once; return `false` unless the prompt anchor is in the region (`g.ContainsInLastRows(string(IdleGlyph), statusRegionRows)`); then return `!busyInRegion(g)`. The conjunction stays load-bearing, now region-scoped: idle ⇔ `❯` in the status region AND no busy anchor in the status region.

Anchors are the existing `[]byte` vars converted to `string` at the call (`string(IdleGlyph)`, `string(SpinnerGlyph)`) — keep the `[]byte` vars, they have other consumers (e.g. `deliver_test.go:220`). `StripANSI` is no longer used by these two functions (vt10x inside `Render` handles CSI-wrapped glyphs), but the import stays for `ParseSpinner*` in the same file.

### Why CSI-wrapped glyphs still classify (behaviour preservation)

`TestIsIdleStripsANSIBeforeChecking` feeds `\x1b[38;5;246m❯\x1b[39m hint text`. `Render` interprets the SGR and lands `❯` in a cell; the grid row is `❯ hint text`; `ContainsInLastRows("❯", 6)` (clamped to the 1 row) matches. Same mechanism covers the CSI-wrapped `✻` cases in `TestIsThinkingPositive`.

### Consumer impact (all behaviour-preserving)

- `classify` (`events.go:382`) — sets idle/thinking on the state event; the only classifications that change are the forgery cases (the intended fix). Note: the `classify` docstring says "Each predicate strips ANSI internally"; after this change these two render internally instead. The 4 KB `DefaultBufferCap` keeps the vt10x pass cheap (~34 rows). Updating that one-line comment is optional and out of this file's required edits.
- `promptDidCommit` (`deliver.go:185`) — post-keystroke warm-up waits for `IsThinking`. A stale history-`✻` previously risked a premature true; region-scoping tightens it to the current bottom-region spinner. Path is fail-safe ("a false negative costs one extra re-delivery, never a corrupted live turn").
- `WaitReady` (`ready.go:43`) — startup idle box renders `❯` at −3, inside the region → `IsIdle` true → returns. No regression.

### Beneficial consequence (note, not scope)

Region-scoping the spinner check on the **rendered** grid means a stale `✻` still living in the 4 KB rolling *history* buffer — the documented #69 wedge that keeps whole-buffer `IsThinking` true forever after `end_turn` — is no longer seen: once claude redraws the idle box, the old spinner line has scrolled above the last-`N` window (or off the 40-row screen). The spikes work around #69 with PTY-quiescence and deliberately avoid `IsIdle`; **this ticket does not touch them.** Whether the now-region-scoped `IsIdle` lets a consumer drop that workaround is a separate consumer-side investigation — out of scope here. Flagged for documentation, not actioned.

### #156 seam — one coherent busy predicate

#156 (`size:xs`, open, no branch yet) adds `esc to interrupt` as a second busy anchor: busy becomes `spinner glyph OR interrupt hint`. Under WIP=1 + Backlog ordering these serialise (as #151→#152 did). This slice lands first and defines the region-scoped busy predicate in `busyInRegion`; #156, landing second, adds its anchor **as an OR inside `busyInRegion`** — automatically region-scoped, editing exactly one place. It must NOT reintroduce a parallel whole-buffer path (#154 would then have to remove it). #156's multi-word hint must key on the grid's **space-preserved** form (`esc to interrupt`), never a `StripANSI` space-stripped variant (the #90 Render-vs-StripANSI split; #152's `Doyouwanttoproceed` landmine) — that anchor is #156's to validate. Either landing order is correct: neither ticket produces a missing API for the other.

## Concurrency model

None. Both predicates are pure functions of `snap`; `NewGrid`/`Render` allocate a throwaway vt10x terminal per call and hold no shared state, no goroutines, no locks. Callers already invoke them at ~1 Hz off a single merge/poll goroutine.

## Error handling

No error returns (mirrors `Render`/`Grid` — every degenerate input is absorbed). `NewGrid(nil)`/empty/whitespace-only → zero rows → `ContainsInLastRows` returns `false` → `IsIdle` false, `IsThinking` false (matches `TestIsIdleEmpty` / `TestIsThinkingNegative`).

## Testing strategy

Existing `state_test.go` idle/thinking tests pass unchanged (tiny inline fixtures; region clamps to all rows). Add these regressions. **Every multi-row fixture uses `\r\n` between rows** (bare `\n` staircases through vt10x — codebase/150.md). Describe scenarios; the developer writes them in the project idiom.

- **Spinner-forgery (AC #3a).** Grid: `✻` as literal transcript content near the top (place it ≥ 8 rows above the bottom so it is clearly above the overlay), a real idle input line `❯` at the bottom row. Expect `IsIdle` **true**, `IsThinking` **false**. Assert the contrast: `strings.Contains(string(snap), "✻")` is **true** for the same bytes (raw match would forge thinking).
- **Scrolled-prompt-forgery (AC #3b).** Grid: `❯` present only near the top (≥ 8 rows above the bottom), bottom rows are non-prompt content (e.g. a mid-turn transcript line, no `❯`, no `✻`). Expect `IsIdle` **false**. Assert the contrast: `strings.Contains(string(snap), "❯")` is **true** (raw match would forge idle).
- **Realistic thinking layout (pins the lower bound of `statusRegionRows`).** Grid mirroring a real thinking frame: a few transcript rows, then the spinner line `✻ Simmering… (7s)` rendered **adjacent above** the redrawn input box (`❯` at the bottom, box border/hint below it — spinner ends up ~−5). Expect `IsThinking` **true** and `IsIdle` **false**. This fails RED if the region is sized too small to reach the spinner line.
- **Behaviour-preservation guard over a real capture (AC #4).** Load `testdata/mcp-empty-snapshot.bin` (carries a genuine on-screen input line, `❯` at −3) → assert `IsIdle` **true**, unchanged from today. Do NOT assert the modal/scrolled captures as "unchanged": `agents`/`mcp`/`picker` intentionally reclassify under region-scoping (their `❯` is an echoed slash-command or scrolled off, not a bottom input line) — and are downstream harmless because the merge loop's modal axis dominates idle/thinking (system-overview:101).
- Confirm the existing `TestIsThinkingPositive` class-A/B/C single-line cases and `TestIsIdleBareInputPrompt`/`TestIsIdleStripsANSIBeforeChecking` still pass after the rewrite (behaviour preserved).

## Open questions

- **Exact `statusRegionRows` value.** `6` is calibrated from the committed captures + the adjacent-spinner docstring, and pinned by the realistic-thinking and forgery regressions. If the developer's realistic-thinking fixture (or a future live capture) shows the spinner higher than −6, bump the constant and note it — do not widen toward the whole grid. Resolve during RED.

## Security review

**Verdict:** PASS

**Findings:**

- **[Trust boundaries]** No MUST FIX. The trust boundary is explicit and single: claude's rendered subprocess output (untrusted — attacker/prompt-influenceable via markdown, tool results, assistant text) crosses into trusted driver state at exactly `IsIdle`/`IsThinking`. This slice *tightens* that boundary: it moves the decision from "substring anywhere in raw history" to "anchor present in the bottom `statusRegionRows` of the rendered screen," which is the mitigation for both documented forgeries (spinner-forges-thinking; scrolled-`❯`-forges-idle). Enforcement stays **deterministic code** (grid region check), not a stochastic agent — belt-and-suspenders fabric is correct. Residual (accepted, documented): a literal `✻`/`❯` rendered by claude *within* the last-`N` region can still forge; inherent to pattern-matching rendered output, bounded by the small `N`. The dangerous direction (scrolled-`❯` → false idle → write into live turn) is the one region-scoping most directly closes.
- **[Error messages, logs, telemetry]** No findings. Both predicates return `bool`; no string, no snapshot content, no glyph position is logged, returned, or put in an error. Nothing to leak.
- **[Subprocess / external command execution]** No findings. Pure read-side classification of already-captured PTY bytes; issues no keystroke, spawns no process, follows no path. (Contrast the write-side `AnswerModal`, #147.)
- **[File operations]** N/A. Reads no path. The regression that loads `testdata/mcp-empty-snapshot.bin` is a committed, static test fixture, not a runtime path.
- **[Concurrency]** No findings. Pure functions, no shared state, no goroutine, no lock (see Concurrency model). No TOCTOU: each call renders its own throwaway grid from the caller-owned `snap` copy `Buffer.Snapshot` returns.
- **[Tokens / secrets]**, **[Cryptographic primitives]**, **[Network & I/O]** — N/A. No credentials, no randomness, no sockets in this design.
- **[Threat model alignment]** The two CRITICAL-B forgeries on the idle/busy axis are the in-scope threats; both are addressed by region-scoping. The busy *anchor set* (`esc to interrupt`) is explicitly OUT OF SCOPE here → **#156**. Slice **#154** removes the now-dead whole-buffer substring paths across the chain. This slice must not leave a parallel whole-buffer path for #154 to clean up (enforced by the single `busyInRegion` seam).

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-07-04

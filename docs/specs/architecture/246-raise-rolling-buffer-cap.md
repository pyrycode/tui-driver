# Spec #246 — Raise the rolling buffer cap so an on-screen panel survives ongoing repaints

**Ticket:** [#246](https://github.com/pyrycode/tui-driver/issues/246) · **Size:** S · **Labels:** `security-sensitive`

## Files to read first

- `pkg/tuidriver/buffer.go:8-13` — `DefaultBufferCap` const + doc comment. The **only** production edit: value `4096` → `16384` and the "known limitation / deferred architectural decision" prose is removed.
- `pkg/tuidriver/buffer.go:40-50` — `Append` trim logic (`fresh := make([]byte, b.cap)`; drops oldest bytes on overflow). This is the eviction mechanism the whole ticket turns on; no change here.
- `pkg/tuidriver/buffer_test.go:81-89` — `TestBufferNewBufferDefaultsCap`. **Contains a hardcoded `5000`-byte append (line 84) that breaks at the new cap** — see § "Correction to AC2". Must be edited.
- `pkg/tuidriver/session_test.go:384-390` — `TestSpawnSetsBufferDefault`. Appends `DefaultBufferCap+100` (already symbolic) → passes unchanged. Read it as the template for how the buffer_test append should have been written.
- `pkg/tuidriver/mcp.go:71-79` — `ParseMcpStatus`: renders `snap` via `Render(snap, DefaultGridCols, DefaultGridRows)` (vt10x), then parses lines. **This is the classifier the regression test asserts against** (the ticket points at `mcp_banner.go`, but the `mcp-snapshot.bin` fixture is actually classified by `ParseMcpStatus`, not `HasMcpFailureBanner`).
- `pkg/tuidriver/mcp_test.go:19-108` — `TestParseMcpStatusRealFixture`: the existing fixture test. Reuse its exact expected shape (`TotalServers == 10`, 3 categories `Project/User/Built-in MCPs`) as the "panel still classifies" assertion. The new test goes in this file.
- `pkg/tuidriver/grid.go:39-49` — `Render`: fixed `DefaultGridRows × DefaultGridCols` (40 × 120) vt10x render. **Key safety fact:** the grid is always 40 rows regardless of buffer size; a larger buffer only starts reconstruction further back. `pkg/tuidriver/pty.go:16-17` — `DefaultPtyRows = 40`, so the bottom row is row 40 (1-indexed).
- `pkg/tuidriver/grid.go:101-113` — `LastRows(n)`: the region-scoping every security-relevant detector uses. Reads the bottom `n` **rendered** rows, never the raw history. Central to the security argument.
- `cmd/corpus-replay/main.go:169,188` — `NewBuffer(0)` + a comment referencing "the same DefaultBufferCap window". `0` inherits the new default automatically; the comment stays accurate. **No edit.**
- `pkg/tuidriver/testdata/mcp-snapshot.bin` — the committed fixture, exactly 4096 bytes, fills the window exactly, header paint ~1.3 KB in. The regression test appends to a copy of these bytes.

## Context

`DefaultBufferCap` (4096 B) is the rolling window the PTY read-goroutine trims to on every `Append`. A full-screen panel that fills the window exactly (the committed `mcp-snapshot.bin`, 4096 B) stops classifying while it is *still on the physical screen*: as small status repaints (spinner / token-counter frames) trickle in, the panel's header bytes (~1.3 KB into the window) evict from the front of the rolling buffer even though the panel never left the screen. A boundary probe shows classification is lost after ~1.4 KB of repaints at 4096; at 16384 the same fixture survives >3× that repaint volume intact.

The per-tick render-cost objection to a bigger cap is gone: the render-once change landed (`network.go`, `modal.go` — the per-tick classifier renders each snapshot once per tick, `#225`). Raising the cap makes VT100 reconstruction start from further back, which is strictly *more* faithful (fewer dropped characters where the old window began mid-escape-sequence). The grid stays 40 rows; region-scoped detectors read only the bottom rows and are unaffected.

## Design

Three changes, one production file + two test files. No call-site changes anywhere (`pyry agent-run` passes `BufferCap: 0`; `cmd/corpus-replay` calls `NewBuffer(0)` — both inherit the default).

### 1. The constant + doc comment (`pkg/tuidriver/buffer.go`)

- Value: `const DefaultBufferCap = 16384`.
- Doc comment contract — the new comment must:
  - State the cap is the rolling window sized so a full-screen panel keeps classifying for as long as it is physically on screen while ordinary status repaints trickle in.
  - **Drop** the "Known limitation … deferred architectural decision" framing entirely (AC1).
  - Note that region-scoped detectors read only the bottom rendered rows, so a larger window only starts VT100 reconstruction further back (strictly more faithful) and no detector loses protection.
  - Drop or correct the stale "used by the 6 spike binaries" clause — the live consumer is `pyry agent-run` via `BufferCap: 0`; keep the comment accurate rather than re-asserting the spike framing.

### 2. Correction to AC2 — `buffer_test.go` MUST change (the AC wording is wrong here)

AC2 states both cap-assertion tests "read the constant symbolically, so they must still pass unchanged." That is **true for `session_test.go:386`** (`bytes.Repeat(..., DefaultBufferCap+100)` scales with the constant → trims → passes) but **false for `buffer_test.go:84`**, which appends a **hardcoded `5000`** bytes. At cap 16384, `5000 < 16384` → no trim → `len(Snapshot()) == 5000 != DefaultBufferCap` → the test **fails** and `make check` goes red.

Fix: change `buffer_test.go:84`'s append volume from the literal `5000` to `DefaultBufferCap+100` (mirroring `session_test.go`'s already-symbolic form). Update the adjacent line-83 comment to match ("Append > cap bytes; should trim to DefaultBufferCap."). This restores the intended "append past the cap, assert trim-to-cap" invariant and makes it future-proof against further cap changes. The line-86 *assertion* is already symbolic and stays as-is.

> Developer note: this is not scope creep — changing `DefaultBufferCap` directly breaks this test; correcting the append is part of landing the constant change. Treat AC2's "unchanged" as a documentation error in the ticket, not a constraint.

### 3. Regression test — panel survives repaints at the default cap (`mcp_test.go`, AC3)

A deterministic, claude-free test that drives bytes through the **real `Buffer`** (not a manual slice) so it exercises the actual trim path, and asserts the boundary-crossing property that gives it value.

**Repaint payload contract.** Build ~2 KB of "representative status repaints": small, in-place, **cursor-addressed frames targeting the bottom status row only** — e.g. move to the bottom row + clear-line + a spinner/token-counter string (`\x1b[40;1H\x1b[2K` followed by a short `✻ <verb>… (Ns · esc to interrupt)`-style line), repeated ~30–40 times to reach ~2 KB. Requirements the developer must honour:
  - **No screen-home / full-screen redraw** (`\x1b[H` + panel repaint). A full redraw would reconstruct the panel even after eviction and destroy the negative-control property. Frames address the bottom row(s) only.
  - **Deterministic** — vary a plain counter across frames; no `time`/`rand` (those are unavailable and would make the test flaky). Any per-frame variation is cosmetic.
  - Rationale: at 16384 the repaints overwrite only row 40 (the footer/hint line, which `ParseMcpStatus` ignores), leaving the panel body rows intact → full parse. At 4096 the ~2 KB of appends evict the front ~2 KB of the fixture — including the "N servers" count and the top header — before Render sees them.

**Test scenarios (bullet form — developer writes the Go in the project idiom):**

- **Positive (exercises the new default).** `b := NewBuffer(0)` (→ `DefaultBufferCap` = 16384). `b.Append(fixtureBytes)`; `b.Append(repaints)`. `status := ParseMcpStatus(b.Snapshot())`. Assert the full fixture shape survives — reuse `TestParseMcpStatusRealFixture`'s expectations: `status.TotalServers == 10` **and** `len(status.Categories) == 3`. This is "the mcp panel still classifies." Because it goes through `NewBuffer(0)`, a future revert of the constant to 4096 makes this case fail too — the test guards the constant, not a magic number.
- **Negative control (old cap, same bytes).** `b4 := NewBuffer(4096)` (the pre-change cap, as a literal). Identical `Append` sequence. `status4 := ParseMcpStatus(b4.Snapshot())`. Assert classification is **lost** — the boundary is genuinely crossed. Primary discriminator: `status4.TotalServers != 10` (the count line, ~1.3 KB in, is the first thing to evict). See Open Questions for confirming the exact degraded value against the real fixture.
- **Sanity (optional, cheap).** Assert `len(b.Snapshot()) == len(fixtureBytes)+len(repaints)` (nothing trimmed at the default cap) and `len(b4.Snapshot()) == 4096` (trimmed at the old cap). This makes the "why does the negative control degrade" mechanism legible in the test itself.

The two-cap structure is what makes the test meaningful: if it were green at *both* caps, it would prove nothing. The `NewBuffer(4096)` control asserts the ~2 KB payload actually crosses the eviction boundary.

## Concurrency model

Unchanged. `Buffer` is already `sync.Mutex`-guarded: `Append` trims under the lock, `Snapshot` copies under the lock. No new goroutines, no new shared state, no lock-ordering change. The only quantitative shift: the buffer now holds up to 16 KB (was 4 KB), and each per-tick `Snapshot` copies up to 16 KB. At the 50 ms tick cadence that is a ~4× bump on an allocation that is microseconds today — accepted, and the explicit subject of the operator-run render-timing sanity check (AC5). The cap is a fixed upper bound; claude cannot grow the buffer past it (Append trims), so there is no attacker-driven memory-exhaustion vector.

## Error handling

- No new failure modes. `ParseMcpStatus` already returns a non-nil, possibly-empty `*McpStatus` for unparseable input — the negative control relies on exactly this (degraded/empty parse after eviction), it does not error.
- The existing fixture tests (`mcp_test.go`, `network`, `permission`, `dialog_shape`, `anchor_forgery`, `grid`) call their classifiers **directly on the raw fixture bytes**, never through a capped `Buffer`, so they are structurally unaffected by the cap — AC2's "every committed fixture classifies identically" is satisfied trivially for them. Only tests that route bytes through `Buffer.Append` past the cap are cap-sensitive (the two cap-assert tests + the new regression test).

## Testing strategy

**Dev-run (claude-free, resolves in `make check`):**
- Constant is 16384; doc comment no longer frames the cap as a limitation (AC1).
- `buffer_test.go` corrected per § 2; `session_test.go` passes unchanged; `make check` green (AC2).
- New regression test per § 3 — positive + negative-control + sanity (AC3).

**Operator-run (hand-run outside the pipeline; record in the PR description — see Self-reference warning):**
- Corpus-replay pass over ok-tagged production recordings: no new fires attributable to the larger window; no previously-firing positive control goes silent (AC4). `cmd/corpus-replay` already uses `NewBuffer(0)`, so it exercises the new cap with no code change.
- Per-tick render-timing sanity check at 16384 recorded in the PR; `make e2e` green (AC5).

## Open questions

- **Exact degraded value at 4096 (negative control).** After eviction the buffer is `fixture[last 2 KB] + repaints`, rendered mid-stream. The top header and "N servers" count (evicted) will not reconstruct, so `TotalServers` drops to a non-10 value (expected `0`). Lower category headers (`User MCPs`, `Built-in MCPs`) may survive partially, so **do not** over-constrain on `len(Categories) == 0`. The developer confirms the concrete degraded `TotalServers` against the committed fixture and encodes the strongest stable discriminator; the invariant is "positive parse full, negative parse degraded, same appended bytes." This resolves entirely against the committed fixture — no live claude needed.
- **Repaint frame count for ~2 KB.** Pick a frame count so the payload comfortably exceeds the empirical ~1.4 KB eviction threshold at 4096 while staying well under the 16384 headroom (4096 + 2 KB = 6 KB ≪ 16384). ~2 KB is the target; tune to whatever cleanly produces the boundary crossing against the real fixture.

## Security review

**Verdict:** PASS

This change enlarges the attacker-influenceable rolling window (4096 → 16384 B) that **all** detectors render from — including the modal / permission / trust classifiers that feed the auto-answer gate. The pass below is the adversarial re-read of whether content that now survives the larger window can reach the region-scoped detection rows and forge a classification.

**Findings:**

- **[Trust boundaries] No MUST FIX.** The boundary is PTY subprocess stdout (untrusted — claude renders attacker-influenceable content: tool output, file contents, quoted web/transcript text) → `Buffer` → detectors → classification → (for permission) the auto-answer gate. The cap change moves *more* untrusted bytes across this boundary per snapshot, but the security-relevant detectors (`DetectModalClass`, permission, trust, mcp-banner) read `Grid.LastRows(n)` of the **rendered** grid, which is always exactly `DefaultGridRows` (40) rows regardless of buffer size. vt10x renders into a fixed 40×120 cell matrix; a larger buffer only starts reconstruction further back and later cursor-addressed writes overwrite earlier content in the same cells. Enlarging the buffer therefore **cannot inject extra rows into the bottom region** — it can only make the reconstruction of those same 40 rows more faithful (the ticket's dropped-character improvement). The boundary is explicit and cap-independent.
- **[Threat model alignment — forgery] No MUST FIX.** The rendered-grid forgery defenses (`#150`–`#155`, `#219`, `#242`, `#223`: region-scoping + a structural co-signal of a different fabric — pointer-marked option row, picker-highlight color) all operate on the fixed 40-row rendered grid and are cap-independent. A bigger cap adds no new attacker-controlled co-signal: if claude actually drew a permission/trust/mcp modal, it is real at any cap; if it did not, the bottom rows carry none of the required co-signals no matter how far back rendering starts. Attacker text quoted in the transcript **body** renders in body rows, not the bottom status/modal region — exactly what region-scoping already rejects, cap-independently. The cap governs how far the history extends, not which cells land in the bottom region. No new forgery surface; the change is strictly a fidelity improvement.
- **[Concurrency / resource exhaustion] No MUST FIX.** `Buffer` stays mutex-guarded; no locking change. Memory/copy footprint grows ~4× (16 KB ceiling, copied per 50 ms tick) — bounded and trivial. The cap is a fixed upper bound Append enforces by trimming; claude cannot grow the buffer past it, so there is no attacker-driven memory-exhaustion vector. The 4× render/copy cost is the explicit subject of operator-run AC5.
- **[Error messages / logs] No findings.** The change adds no logging. It does not introduce any new path that emits the (now-larger) snapshot to logs or errors; `Snapshot()`'s callers are unchanged.
- **[Tokens/secrets, File operations, Subprocess execution, Cryptographic primitives, Network & I/O] Not applicable.** This change is a single integer constant + doc comment + tests. No tokens, no filesystem paths, no `exec`, no crypto, no sockets are introduced or touched.
- **[Regression coverage of the safety claim] SHOULD (satisfied by AC3, no gate).** The core safety claim — "the cap change is a no-op for any content already within the old window; it only affects content the old window would have truncated" — is inherently true (for a snapshot ≤ 4096 B both caps retain all bytes → byte-identical render) and is covered: existing fixture tests classify directly on raw ≤4096-B fixtures and must stay green (AC2/AC4), and the new regression test's positive case confirms the panel classifies only because the header now survives. No additional gating test required.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-07-09

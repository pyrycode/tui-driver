# Spec #158 — Add a general "unrecognized modal" axis to readiness

## Files to read first

- `pkg/tuidriver/ready.go:10-31` — the `Readiness` struct: where the new `UnknownModal` field lands and the exact doc-comment style its siblings use (one blank-line-separated block per field, first sentence names the condition, second gives the consumer-policy hint).
- `pkg/tuidriver/ready.go:42-54` — `WaitReady`: the single site that fills `Readiness`. Your derivation line goes here, after `snap := s.Snapshot()`.
- `pkg/tuidriver/modal.go:22-42` — the `ModalClass` constants: `ModalClassUnknown` (the no-anchor case) and `ModalClassTrustFolder` (the one class already surfaced via `TrustModal`). These are the two values the derivation excludes.
- `pkg/tuidriver/modal.go:96-143` — `DetectModalClass`: the whole-buffer classifier you consume unchanged. Note the doc comment at `:107-108` — `ModalClassUnknown` means **either** "no modal at all" (the common idle case) **or** "a modal I don't recognize." This overload is exactly why the naive `== ModalClassUnknown` derivation is wrong (see Design).
- `pkg/tuidriver/ready_test.go:9-51` — `TestWaitReady`: table-driven, prepends a modal anchor to an `idle` base (`idleGlyphTest + " "`), asserts exact struct equality (`got != tt.want`). Your new case is one more table row; existing rows need no edit (new field zero-values to `false`).
- `pkg/tuidriver/deliver_test.go:17` — `const idleGlyphTest = "\xe2\x9d\xaf"`, the `❯` the test's `idle` base relies on to satisfy `IsIdle`.

## Context

`WaitReady` classifies the first post-idle screen against a closed set of conditions — trust modal, MCP-failure banner, network-failure banner (`ready.go:49-52`). Any startup dialog outside that set leaves `Readiness` clean, so the consumer types its first prompt straight into an unrecognized dialog.

`DetectModalClass` already recognizes eight modal classes, but only the trust modal reaches `Readiness` (as `TrustModal`). The other recognized classes — permission, ask-user-question, slash-picker, model-select, permissions-config, `/mcp`, agents — are invisible at readiness time, even though any of them being up at idle is a dialog the consumer would type into.

This ticket adds one catch-all axis, `UnknownModal`, derived from the existing `DetectModalClass`. It rides on today's whole-buffer classifier and inherits forgery-resistance for free once #152 region-scopes modal detection onto the rendered grid (soft dep, no native blocker). Part of the rendered-grid refactor family (#150–#155).

## Design

Two edits, both in `pkg/tuidriver/ready.go`. No change to `modal.go` — `DetectModalClass` and the `ModalClass` constants are already exported within the package and consumed as-is.

**1. New `Readiness` field** (append after `NetworkFailure`; field order is free, so this keeps the diff minimal). Contract:

```go
// UnknownModal bool — true when a recognized modal class is up at idle that no
// other Readiness field surfaces. The consumer owns abort-vs-continue.
```

Doc-comment content to convey (write it in the siblings' two-sentence style, not verbatim this): claude is displaying a recognized blocking modal that no other `Readiness` field carries — any `DetectModalClass` result other than the no-modal case (`ModalClassUnknown`) and the trust modal (which `TrustModal` already carries). It signals "a dialog the first prompt would land in is up"; the library reports, the driver decides.

**2. Derivation in `WaitReady`.** After `snap := s.Snapshot()`, classify once and set the field in the returned literal:

- `class := DetectModalClass(snap)`
- `UnknownModal: class != ModalClassUnknown && class != ModalClassTrustFolder`

That is the whole behavior. The predicate reads directly off the AC: **a class is detected AND it isn't the one `Readiness` already surfaces.**

**Why not `DetectModalClass(snap) == ModalClassUnknown`.** `ModalClassUnknown` is returned for the *common no-modal idle screen* as well as for a genuinely unrecognized dialog (`modal.go:107-108`, `:140-142`). The equality form would fire `true` on nearly every clean readiness — a false positive that defeats the axis. The axis is deliberately the *inverse*: a class must be positively detected.

**Exclude trust only — not `McpFailure`.** The single excluded class is `ModalClassTrustFolder`. Do **not** also exclude `ModalClassMCP`: `McpFailure` surfaces the *"N MCP servers failed" status banner* (`HasMcpFailureBanner`), a different thing from claude's interactive `/mcp` modal (`ModalClassMCP`). If the `/mcp` modal is genuinely up at idle, that *is* a dialog to flag, so `UnknownModal` should be `true` for it. The AC is explicit: exclude `ModalClassUnknown` and the trust modal, nothing else.

**Data flow:** `WaitReady` → `WaitUntil(IsIdle)` (unchanged) → `s.Snapshot()` (unchanged) → `DetectModalClass(snap)` (new read) → `Readiness{…, UnknownModal: …}`. One extra pure-function call on the same snapshot the other axes already classify; no new I/O, no new state.

## Concurrency model

None new. `WaitReady` remains a single synchronous call on one snapshot; `DetectModalClass` is a pure function over `[]byte`. No goroutines, channels, or locks are introduced. The added call reuses the exact `snap` the sibling detectors already read, so all axes classify one consistent snapshot.

## Error handling

No new failure modes. The added derivation is total (`DetectModalClass` returns a value for every input; the two `!=` comparisons cannot error). The existing contract is unchanged: on `WaitUntil` cancellation, `WaitReady` still returns `(Readiness{}, err)` with the zero value — where `UnknownModal` is `false` — and `nil` otherwise.

Failure semantics of the axis itself are fail-safe: a *false* `UnknownModal` when a modal is up (the risk under a forged/suppressed anchor) is the dangerous direction, and that risk is inherited from today's whole-buffer `DetectModalClass` and deferred to #152 (see Security review). A spurious *true* only makes the consumer more cautious.

## Testing strategy

Extend `TestWaitReady` in `pkg/tuidriver/ready_test.go`. The existing table already covers two of the ACs for free once the field exists — verify, don't rewrite:

- **"clean idle"** (existing row, `want: Readiness{Idle: true}`) — `DetectModalClass` returns `ModalClassUnknown` → `UnknownModal` stays `false`. Covers the "no false positive on the normal ready path" AC with no edit.
- **"trust modal at idle"** (existing row, `want: Readiness{Idle: true, TrustModal: true}`) — `DetectModalClass` returns `ModalClassTrustFolder` → excluded → `UnknownModal` stays `false` while `TrustModal` is `true`. Covers "trust stays surfaced via `TrustModal`, not double-counted here" with no edit.

Add one new row for the positive case:

- **"unrecognized modal at idle"** — snap = a recognized-non-trust modal anchor prepended to the `idle` base, e.g. `append([]byte("Enter to select"), idle...)`. `"Enter to select"` is the `ask-user-question` anchor (`modal.go:84`) → `ModalClassAskUserQuestion` → not `Unknown`, not trust. Expected: `Readiness{Idle: true, UnknownModal: true}`.
  - Confirm the anchor still satisfies `IsIdle` (the `❯` glyph in the last rows is untouched by the prepended text — the existing trust/mcp/network rows already prove this prepend pattern) and does not trip `findPickerRows` (it has no `/<letter>` line-start).

Optional belt (cheap, not required by the AC): a second positive row using the permission anchor `"Do you want to proceed"` (`modal.go:87`) → `ModalClassPermission`, to show the axis is class-family-wide, not ask-user-specific.

Run: `go test ./pkg/tuidriver/ -run TestWaitReady`.

## Open questions

- **Field placement / ordering** — spec recommends appending after `NetworkFailure` for a minimal diff. Placing it adjacent to `TrustModal` (both concern modals) is equally valid; struct-field order carries no behavior. Developer's call; the doc-comment style is what the AC gates on.

## Security review

**Verdict:** PASS

**Findings:**

- **[Trust boundaries]** No MUST FIX. The relevant boundary is claude's rendered TUI bytes (untrusted — attacker-influenceable model output) crossing into consumer readiness state. This axis reads that boundary through the single existing `DetectModalClass` classifier and produces one conservative boolean. It adds no new boundary and no new trusted/untrusted transition; it consumes the same `snap` the sibling detectors already classify.
- **[Threat model alignment]** SHOULD FIX handled by design + OUT OF SCOPE residual. The rendered-grid family's threat is content forgery: an attacker who can influence claude's output injects a modal anchor into the append-only scrollback. For `UnknownModal` the two directions differ sharply:
  - *Forged-present* (attacker injects a recognized anchor to force `UnknownModal = true`) is **fail-safe** — it only makes the consumer more cautious (flag a possible dialog); the consumer owns abort-vs-continue and never types blindly on a `true`.
  - *Forged-absent / suppression* (attacker keeps a real modal from being classified so `UnknownModal` stays `false` and the consumer types into it) is the dangerous direction. This risk is **inherited from today's whole-buffer `DetectModalClass`** — it is not introduced by this ticket, which adds no new matching logic. Forgery-resistance for the whole classifier is the job of #152 (region-scope modal detection onto the rendered grid), on which this ticket carries a documented soft dependency. **OUT OF SCOPE here; picked up by #152.** Naming it is the point: this axis does not claim forgery-resistance it doesn't have, and it inherits #152's for free once #152 lands.
- **[Error messages, logs, telemetry]** No findings. The change adds a boolean to a returned struct; it logs nothing and puts no snapshot content, path, or secret into any message.
- **[Subprocess / file ops / crypto / tokens / network I/O]** Not applicable — this change performs no filesystem, subprocess, network, cryptographic, or credential operation. It is a pure in-memory read of an existing snapshot plus two enum comparisons.
- **[Concurrency]** No findings — no goroutine, channel, or lock is introduced; the derivation runs synchronously on the same snapshot as the sibling axes (see Concurrency model).

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-07-04

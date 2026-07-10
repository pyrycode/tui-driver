# Spec #264 — Re-gate `probe-cwd-encoding` (drop `NonGating`)

**Ticket:** #264 · **Size:** XS · **Security-sensitive:** no · **Blocked by:** #263 (cleared)

Fix-half of the #263/#264 spike-from-fix pair. #263 (PR #265) re-anchored the
probe to a settled-state readiness gate and the operator ran the reliability
observation; the operator's re-promote of this ticket to Backlog is the
option-1 positive verdict. This ticket applies the gated flip: drop
`NonGating: true` so `probe-cwd-encoding` counts toward `make e2e`'s exit code
again.

## Files to read first

- `cmd/e2e-runner/main.go:346-385` — the `probe-cwd-encoding` check definition:
  the de-gate rationale comment (346-371), `SuccessMarker: observedSuccess`
  (376, **already set — do not touch**), and `NonGating: true` (377, **the line
  to remove**).
- `cmd/e2e-runner/main.go:62-75` — `Check` struct doc for `NonGating` (64-68)
  and the `gateFailed(c, status)` helper (72-75): `status != "pass" && !c.NonGating`.
  This is the entire gating mechanism — removing the field flips the check from
  informational to gating deterministically.
- `cmd/e2e-runner/main.go:40-41` — `observedSuccess = regexp.MustCompile(`(?m)^OBSERVED`)`,
  the marker that defines "pass" for this probe (it observed → `^OBSERVED` line
  → pass; timeout → no marker → non-pass → now gates).
- `cmd/e2e-runner/main.go:326-337` — the **sibling** `probe-first-prompt-hang`
  block, also `NonGating: true`. Scope guard: it stays NonGating; do **not**
  touch it. Confirms which `NonGating: true` line is #264's (377, not 337).
- `cmd/e2e-runner/main_test.go:315-337` — `gateFailed` table test. It builds a
  synthetic `Check{NonGating: …}`; it does **not** enumerate the real checks
  list, so removing the field from `probe-cwd-encoding` breaks no test.

## Context

`probe-cwd-encoding` is an observation rig (#206 shape) that records how claude
encodes a non-ASCII cwd into its `~/.claude/projects/<dir>` name. It was
de-gated by the closed #251 (`NonGating: true`, main.go:377) because on claude
2.1.199 the probe timed out on a **false idle** — the bare `❯` glyph appeared
before claude's input handler was wired, so the throwaway keystroke that forces
the deferred session-JSONL write was dropped and the projects-dir never
materialised. At de-gate time there was no positive ready-signal to wait on
(#173 Open Q1), so gating it reddened the suite for a library gap, not a real
regression.

#263 (child A, PR #265) closed that gap: `cmd/probe-cwd-encoding/main.go` now
anchors its keystrokes on a **settled-state** gate (`settledPredicate` /
`isSettled` — target predicate holds AND PTY quiet for `settleWindow`, measured
via `Session.LastAppendAt()`) instead of bare `IsIdle`. The operator then ran
the reliability observation on the pinned claude 2.1.199 and re-promoted this
ticket, signalling the streak succeeded. With the false idle fixed, gating the
check is now correct: a real projects-dir regression reddens `make e2e` instead
of passing silently.

## Design

Single struct-field removal plus a comment rewrite in `cmd/e2e-runner/main.go`.
No new types, no signature changes, no new files.

1. **Remove `NonGating: true`** (main.go:377) from the `probe-cwd-encoding`
   `Check` literal. Leave every other field — `Name`, `Kind`, `Binary`, `Args`,
   `SuccessMarker: observedSuccess`, `OnFailure` — unchanged. `NonGating`'s
   zero value is `false`, so `gateFailed(check, "timeout")` now returns `true`:
   a non-pass status gates the suite exit code.

2. **Rewrite the check-definition comment** (main.go:346-371) from the de-gate
   rationale to a re-gate record. The comment is the primary human-facing
   deliverable here; it must record:
   - The check is now **gating** again — its non-pass status counts toward
     `make e2e`'s exit code.
   - The re-anchor evidence: #263 / PR #265 replaced bare `IsIdle` with a
     settled-state readiness gate (`settledPredicate` / `isSettled`) in
     `cmd/probe-cwd-encoding/main.go`, fixing the false-idle timeout that
     forced the #251 de-gate.
   - The operator's reliability observation on the pinned claude 2.1.199 as the
     verdict that authorised the re-gate.
   - How pass/fail now reads: `SuccessMarker: observedSuccess` (`^OBSERVED`)
     means the probe passes when the re-anchored gate lets it observe, and
     **fails (gates) if it times out**.
   - A one-line nod to the de-gate provenance (#251 → #263 → #264) so the
     history isn't lost.

   Contract sketch for the replacement comment (developer adjusts wording to
   match surrounding comment style; keep it to the same ~15-line block):

   ```
   // Re-gated (#264): an observation rig recording how claude encodes a
   // non-ASCII cwd into its projects-dir name (#206; ^OBSERVED gate, ships
   // green when it observes). De-gated by #251 on claude 2.1.199 (it timed
   // out on a false idle — bare ❯ before claude's input handler was wired —
   // so the deferred session JSONL, and thus the projects-dir, never
   // materialised, with no library ready-signal to wait on: #173 Open Q1).
   // #263 (PR #265) re-anchored the probe onto a settled-state readiness gate
   // (settledPredicate / isSettled in cmd/probe-cwd-encoding/main.go: target
   // predicate + PTY quiet for settleWindow) instead of bare IsIdle, fixing
   // the false idle. The operator's reliability observation on the pinned
   // claude 2.1.199 confirmed it reaches OBSERVED reliably, so NonGating is
   // dropped: SuccessMarker (observedSuccess / ^OBSERVED) passes when it
   // observes and now GATES make e2e's exit code if it times out — a real
   // projects-dir regression reddens the suite instead of passing silently.
   // Timeout stays unset (60s default) so the probe's internal 30s poll fits.
   ```

## Concurrency model

None. The change touches a package-level struct literal read once at startup by
`checks()`. Gating is the pure function `gateFailed(c Check, status string)
bool` (main.go:75). No goroutines, channels, or shared state involved.

## Error handling / safety net

The deterministic belt to this ticket's stochastic read of the
re-promote-as-verdict signal is the QA-stage live `make e2e` run (AC4). If the
re-anchor has in fact regressed since the operator's observation, dropping
`NonGating` makes `probe-cwd-encoding` gating + timing out, which reddens
`make e2e` and routes the PR back **before merge**. No new defensive code — the
existing gating path is the safety net.

## Testing strategy

- **Developer turn (claude-free `make check` = `go vet` + `go test -race`):**
  the whole deliverable is confirmable here. The removal compiles; the
  `gateFailed` table test (main_test.go:315-337) still passes unchanged because
  it constructs synthetic `Check{}` values and never enumerates the real checks
  list. No test asserts `probe-cwd-encoding` is NonGating, so none breaks.
  Adding a test is **not** required for this XS flip — the gating logic is
  already covered by the existing `gateFailed` table test; a new assertion
  pinning this specific check's gating would just duplicate that coverage.
- **QA stage / operator (out of band, AC4):** `make e2e` against claude 2.1.199
  must report `probe-cwd-encoding` as **gating** and green across ≥2 consecutive
  runs. This needs live claude and **must not** run in the developer turn — it
  would hang/burn the turn on a live session (#206/#180 boundary).

## Developer boundaries (scope guard)

- Touch `cmd/e2e-runner/main.go` **only**.
- Do **not** run `make e2e` (live-claude, operator/QA-run).
- Do **not** touch `EncodeCwd` (`pkg/tuidriver/cwd.go`) or the probe
  (`cmd/probe-cwd-encoding/main.go`) — this ticket is the e2e-runner gating flag
  alone. The #263 review's deferred "fold `settledPredicate`'s inline
  quiescence into `Session.QuietFor()`" nicety lives in the probe, **not** here;
  it is out of scope and needs its own ticket if wanted.
- Do **not** touch the sibling `probe-first-prompt-hang` block (main.go:326-337),
  which legitimately stays `NonGating`.
- Do **not** change `SuccessMarker` — `observedSuccess` is already the correct
  marker.

## Open questions

None. The design is a one-field removal with a comment rewrite; the pass/fail
semantics (`observedSuccess` marker + zeroed `NonGating`) are fully determined
by existing code. The only live-verification (AC4) is explicitly operator-run.

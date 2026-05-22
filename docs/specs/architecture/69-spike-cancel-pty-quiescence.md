# Spec: `spike-cancel` predicates — PTY-quiescence in both `waitReappeared` and `runRecovery`

**Ticket:** [#69](https://github.com/pyrycode/tui-driver/issues/69)
**Size:** S
**Status:** ready for development (re-spec round 4; supersedes the
round-1 Branch-A/B routing design)
**Posture:** evidence-confirmed predicate swap. Round 1's spec asked the
developer to diagnose first and routed two branches; the developer's
Step 1 evidence (captured 2026-05-22, two reproducible runs on `claude
2.1.148`) landed in Branch B — Probe 1 wedges in `waitReappeared`, not
in `runRecovery`. Same root mechanism as PR #74 (`✻` glyph stuck in the
4 KB rolling buffer), different predicate site. This re-spec drops the
diagnose-first scaffold and prescribes the confirmed fix in both
predicates of `cmd/spike-cancel/main.go`.

## Evidence (Step 1 already done)

Captured by the developer on 2026-05-22 against `claude 2.1.148`
(`feature/69` @ `87ba97d`, unmodified worktree). Full diagnosis:
[issue #69 comments](https://github.com/pyrycode/tui-driver/issues/69)
("Branch B fires: cancel-probe wedge…", "Step 1 diagnosis — Branch B…",
"Developer diagnosis — Step 1 evidence").

### Confirmed failure mode (Probe 1, `waitReappeared`)

```
2026/05/22 18:54:40.479710 probe=1 spinner-or-tool-visible kind=spinner-glyph
2026/05/22 18:54:40.480091 probe=1 cancel-sent keystroke=1b
<~few hundred bytes of redraw: input box restored as drafted prompt,
 title bar updated to "]0;✳ Essay on the philosophy of monads", then silence>
2026/05/22 18:55:10.482774 shutdown-signalled
spike failed: probe 1: watchdog: stuck after cancel for 30s
```

Byte-count math (from `/tmp/spike-cancel-69-baseline.log`):

- Pre-cancel rendering: 2008 raw bytes, 1 `✻` paint at raw position
  1817 / 2008.
- Post-cancel rendering: 1386 raw bytes, **zero** new `✻` paints.
- Total churn since the last `✻` paint at predicate-evaluation time:
  **1577 bytes** — well under the 4096-byte rolling-buffer cap.
- Result: the single pre-cancel `✻` stays painted; `IsIdle`'s
  spinner-absent half (`pkg/tuidriver/state.go:43`) never flips true;
  `❯`-present half is satisfied (input-box restored); `QuietFor()`
  satisfies the 1500 ms window trivially; the predicate is wedged by
  the `IsIdle` clause alone, and `cancelRecoveryLimit = 30s` fires.

This is the same stuck-`✻`-in-4 KB-buffer mechanism that PR #74 fixed
for `spike-multi-turn`'s `runTurn`. The original spec's *Important
nuance* paragraph explicitly named this as a speculative risk against
the cancel probes; on `claude 2.1.148` it empirically materialised.

### Unobserved-but-very-likely failure mode (Probes 3/4/5, `runRecovery`)

`runRecovery`'s predicate at `cmd/spike-cancel/main.go:638-651` is the
**verbatim pre-#74 spike-multi-turn predicate**: `gotEndTurn ∧
tuidriver.IsIdle(rb) stable for idleStableWindow = 250ms`. The wedging
clause is the same `IsIdle` spinner-absent dependency that took down
`waitReappeared`. The byte-count substrate is the same (post-`end_turn`
claude emits a small redraw, then quiet — well under 4 KB).

Probes 3/4/5 were not reached in Step 1's runs because Probe 1's
30 s watchdog fires first in the linear flow. The recovery-probe wedge
is therefore not directly observed in this rework round — but it is
empirically observed in PR #74 (same predicate shape, same buffer math,
same claude version family). The original spec's strong prior holds;
the cancel-probe diagnosis just changes which wedge fires first.

This re-spec treats the `runRecovery` wedge as confirmed by structural
identity transfer from PR #74's evidence, not as unobserved. The reason
to fix both in one ticket: same file, same mechanism, same fix shape,
same constant reuse. Shipping `waitReappeared` alone almost certainly
unmasks the recovery-probe wedge on the next `make e2e` run; routing
that through a second rework cycle would burn a dispatcher cycle for
structurally-identical work. The Pipeline-Wide Principle "Evidence-Based
Fix Selection" is satisfied because the evidence for `runRecovery` is
the union of (a) the pre-#74 wedge it shares with `runTurn` (PR #74) and
(b) the byte-count substrate the Step 1 diagnosis confirms for this
same file on the same claude version.

## Files to read first

Load these before touching anything. They cover both predicate sites
this fix targets, the proven sibling that defines the shape, and the
buffer/state primitives the new predicates compose.

- `cmd/spike-cancel/main.go:513-568` — `waitReappeared`. Lines 545-549
  are the failing `stable` closure. Lines 517-525 are the function-level
  comment that describes the rationale-but-not-the-fix; the comment
  needs updating to reflect that the fix this spec applies finally
  drops the `IsIdle` wrapper for direct `IdleGlyph` + `QuietFor`.
  **This is the empirically-confirmed wedge site (Probe 1).**
- `cmd/spike-cancel/main.go:593-688` — `runRecovery` and its failing
  predicate. Lines 637-651 are the wedging `check` closure
  (`gotEndTurn ∧ IsIdle stable for idleStableWindow = 250ms`); lines
  593-596 are the function-level comment that becomes stale after the
  swap. Both blocks get rewritten. **This is the structural-identity
  wedge site (Probes 3/4/5).**
- `cmd/spike-cancel/main.go:62-91` — constants. `idleStableWindow =
  250ms` (line 63) becomes dead after both predicates are swapped and
  is removed. `disappearedWindow = 500ms` (line 62) is kept — it bounds
  the `❯-disappeared` observer goroutine (lines 606-626), not a wedging
  predicate. `ptyQuietWindow = 1500ms` (lines 82-91) already exists
  with its empirical-derivation comment — reuse as-is for both predicate
  sites, do NOT redefine.
- `cmd/spike-multi-turn/main.go:353-417` — PR #74's exact precedent.
  Lines 353-381 are the rationale-block comment style the new
  `waitReappeared` and `runRecovery` comments must match; lines 391-400
  are the predicate-shape this spec adopts. **Same fix, two new
  call sites.**
- `pkg/tuidriver/state.go:9-13, 29-44` — `IdleGlyph` exported byte
  constant (line 12) and `IsIdle` (lines 29-44). The new predicates
  call `bytes.Contains(tuidriver.StripANSI(snap), tuidriver.IdleGlyph)`
  directly rather than going through `IsIdle`. The spinner-absent
  clause of `IsIdle` (line 43) is the wedging clause this spec
  deliberately drops in both sites. `spike-cancel` already imports
  `bytes` (line 31), so no new import.
- `pkg/tuidriver/buffer.go:62-71` — `QuietFor()`. Already used inside
  `waitReappeared` and remains the temporal half of both predicates
  after this change.
- `cmd/spike-cancel/main.go:140-146` — the probe table. Probes 1, 2 are
  cancel probes (run `waitReappeared` after sending cancel). Probes 3,
  4, 5 are `kindRecovery` (run `runRecovery`). Probes 3/4/5 are gated
  behind Probes 1/2 succeeding, which is why the cancel-probe wedge
  hid the recovery-probe wedge in Step 1.
- `cmd/spike-cancel/README.md:377-565` (§ *Surprises / findings*) —
  existing findings 1–7. Finding 1 (lines 379-422) is the closest prior
  art (the `hasSpinnerGlyph false AND isIdle true` predicate failed
  empirically; PTY-quiescence replaced it for cancel-recovery). Add a
  new finding 8 documenting the `claude 2.1.148` regression: post-cancel
  byte volume in `waitReappeared` shrank enough that the stuck `✻`
  isn't rolled out even with `❯` present + quiescence — predicate had
  to drop the `IsIdle` wrapper and go direct on `IdleGlyph`. Mirror PR
  #74's reasoning into this README.
- `docs/specs/architecture/73-spike-multi-turn-pty-quiescence.md` § Safety
  — the safety argument transfers unchanged to both new call sites in
  this spec.
- `cmd/spike-cancel/main.go:606-626` — the `❯-disappeared` observer
  goroutine inside `runRecovery`. Calls `tuidriver.IsIdle` (line 613)
  but is bounded by `disappearedWindow = 500ms` deadline (line 617),
  so it cannot wedge the spike. **Leave this goroutine unchanged** —
  the observer is best-effort logging, not a wedging predicate.

## Context

### Symptom

`make e2e` invokes `cmd/e2e-runner/main.go`, which runs `spike-cancel`
with `["-trust-folder=accept"]` and requires `successSuccess`
(`(?m)^SUCCESS`) on stdout. On the dispatcher host against `claude
2.1.148`, the check fails every run at Probe 1 with `watchdog: stuck
after cancel for 30s` — the `cancelRecoveryLimit` deadline inside
`waitReappeared`. Wall time ~32.9 s (Probe 1 setup + 30 s watchdog).

The failure was previously hidden by the `claude-version-lock`
short-circuit (now removed). It surfaces every run on `claude` versions
past the locked baseline (`2.1.144` → `2.1.148`, two patches).

### Root cause (single mechanism, two predicate sites)

`tuidriver.IsIdle(snap)` returns `true` iff `❯` is present AND `✻` is
absent in the ANSI-stripped buffer (`pkg/tuidriver/state.go:38-44`).
The 4096-byte rolling buffer only forgets a paint after enough new
bytes overwrite it. On `claude 2.1.148`:

- **`waitReappeared` (post-cancel)**: cancel-time pre-cancel rendering
  contains one `✻ Forming…` paint at ~byte 1817/2008. Post-cancel
  rendering is ~1.4 KB (input-box restore + title-bar update). Total
  churn since the last `✻` paint: ~1577 bytes. Below the 4096-byte
  cap; the glyph stays painted; `IsIdle` returns false forever; the
  predicate wedges; the 30 s watchdog trips.
- **`runRecovery` (post-`end_turn`)**: the predicate is the verbatim
  pre-#74 `runTurn` predicate that PR #74 already proved wedges. After
  a fast assistant response, the `✻ Brewed for Ns` glyph is painted in
  the buffer and post-`end_turn` claude emits well under 4 KB. The
  glyph never rolls out; the conjunction never holds; the
  session-level watchdog at 30–60 s would fire (this wedge is currently
  masked by `waitReappeared`'s earlier 30 s fire).

`QuietFor() >= ptyQuietWindow = 1500ms` is satisfied trivially in both
cases — the wedging clause is `IsIdle`'s spinner-absent half, not the
temporal half.

### Why this fix shape

PR #74's `runTurn` swap is the proven precedent. It replaced
`gotEndTurn ∧ IsIdle stable for 250ms` with
`gotEndTurn ∧ IdleGlyph present ∧ rb.QuietFor() ≥ 1500ms` —
direct `bytes.Contains` for the `❯` half (dropping the spinner-absent
dependency), `QuietFor` for the temporal half (subsuming the safety
that the spinner-absent clause was meant to provide). This spec
applies the same shape in both `waitReappeared` and `runRecovery`
inside `cmd/spike-cancel/main.go`.

The other fix shapes the original ticket enumerated (bigger buffer;
`spinnerGone` heuristic from `spike-one-turn`; no-op-keystroke to force
buffer churn) are not chosen for the same reasons spelled out in
[`73-spike-multi-turn-pty-quiescence.md`](73-spike-multi-turn-pty-quiescence.md) §
*Why fix shapes 1, 2, 4 are not chosen*: bigger buffer changes the
substrate for every consumer and overlaps with library extraction
(#58–#62); the `spinnerGone` heuristic regresses safety in multi-turn /
recovery contexts by predicate-shape luck; "send a no-op keystroke"
couples the consumer contract to claude's renderer internals.

### Why not split this work

After the cancel-probe wedge is fixed, the recovery-probe wedge is
near-certain to fire on the next `make e2e` run — same predicate
shape, same buffer math, same claude version. Splitting would file a
follow-up ticket for one-closure-rewrite-in-the-same-file. The
combined scope is one file, ~30-40 net production lines, zero exported
API change, zero consumer cascade — comfortably inside size S.

## Design

Two-step plan: apply the fix, then verify against `make e2e`. The
README update lives in step 1 alongside the code change (single
commit covers both).

### Step 1 — Apply the fix

All changes are in `cmd/spike-cancel/main.go` and
`cmd/spike-cancel/README.md`.

#### 1a. Constants block (lines 52-96)

- **Remove** `idleStableWindow = 250 * time.Millisecond` (line 63) and
  any one-line comment that immediately surrounds it. After both
  predicates are swapped, this constant has no other consumers.
- **Keep** `disappearedWindow = 500 * time.Millisecond` (line 62)
  unchanged — it bounds the `❯-disappeared` observer goroutine that
  this spec leaves alone.
- **Keep** `ptyQuietWindow = 1500 * time.Millisecond` (lines 82-91)
  **unchanged**, including its empirical-derivation comment. Both
  new predicates reuse it.

#### 1b. `waitReappeared` (lines 513-568)

Function-level comment (lines 513-530): rewrite to describe the new
predicate. Mandatory content:

- The predicate is now `❯ glyph present (direct check) AND
  rb.QuietFor() ≥ ptyQuietWindow`. Explicitly note the call to
  `tuidriver.IsIdle` is replaced by a direct
  `bytes.Contains(tuidriver.StripANSI(snap), tuidriver.IdleGlyph)`
  check because the spinner-absent half of `IsIdle` is the wedging
  clause on `claude 2.1.148` (see README finding 8 and ticket #69).
- Cross-reference: cite PR #74 / ticket #73 as the sibling consumer
  (`spike-multi-turn`'s `runTurn`) that established this exact shape.
- Note that `preCancelHadSpinner` continues to be recorded but not
  used in the predicate (same as today).

`stable` closure (lines 545-549) — contract sketch (developer writes
the actual closure in the binary's in-file idiom):

```
stable := func() bool {
    stripped := tuidriver.StripANSI(rb.Snapshot())
    if !bytes.Contains(stripped, tuidriver.IdleGlyph) {
        return false
    }
    return rb.QuietFor() >= ptyQuietWindow
}
```

Behavior contract:

- Returns `true` only when the `❯` glyph is present in the
  ANSI-stripped rolling buffer AND `rb.QuietFor() ≥ ptyQuietWindow`.
- Does NOT call `tuidriver.IsIdle`. The spinner-absent clause is
  dropped; PTY-quiescence subsumes the safety it was meant to provide.
- No new local variables; no `idleSince`-style temporal accumulator
  (the timer lives in `QuietFor` now).

No other lines in `waitReappeared` change. `bytes` and `tuidriver`
already imported.

#### 1c. `runRecovery` (lines 593-688)

Function-level comment (lines 593-596): rewrite to describe the new
predicate. Same content shape as 1b's comment, adapted to the
recovery-probe context. Mandatory content:

- The predicate is now `gotEndTurn ∧ ❯ glyph present (direct check)
  ∧ rb.QuietFor() ≥ ptyQuietWindow`. Same wording as 1b for the `❯`
  / `QuietFor` halves; the `gotEndTurn` half stays.
- Cross-reference: cite PR #74's `runTurn` (`spike-multi-turn/main.go:391-400`)
  as the verbatim precedent. Cite `waitReappeared` (this same file,
  post-#69) as the in-file sibling on the same shape.
- Same safety argument as PR #74: `gotEndTurn` proves the assistant
  turn completed; `❯` present proves the input prompt is ready;
  `QuietFor ≥ 1500ms` proves no further redraws are in flight.

`check` closure (lines 637-651) — contract sketch:

```
check := func() bool {
    if !gotEndTurn {
        return false
    }
    stripped := tuidriver.StripANSI(rb.Snapshot())
    if !bytes.Contains(stripped, tuidriver.IdleGlyph) {
        return false
    }
    return rb.QuietFor() >= ptyQuietWindow
}
```

Behavior contract:

- Returns `false` until `gotEndTurn` is observed for the recovery turn
  (same as today).
- After `gotEndTurn`, returns `true` only when BOTH the `❯` glyph
  is present AND `rb.QuietFor() ≥ ptyQuietWindow`.
- Does NOT call `tuidriver.IsIdle`. Spinner-absent clause dropped, same
  reason as 1b.

Local-variable cleanup: remove `var idleSince time.Time` (line 637).
No longer needed.

#### 1d. `❯-disappeared` observer goroutine (lines 606-626)

**Unchanged.** The observer calls `tuidriver.IsIdle(rb.Snapshot())`
at line 613 but is hard-bounded by `disappearedWindow = 500ms`
deadline at line 617 — even if `IsIdle` never flips false (the same
stuck-glyph mechanism), the goroutine exits cleanly at the deadline
and emits no log line. It is best-effort logging, not a wedging
predicate. Leave it alone.

#### 1e. README finding 8

Append a new subsection at the end of § *Surprises / findings*
(after finding 7 at line 555, before § *Why no automated tests* at
line 567). Use the same heading style as findings 1–7 (`### 8. <title>`).
Mandatory content:

- **Symptom on `claude 2.1.148`**: Probe 1 wedges in `waitReappeared`
  with `watchdog: stuck after cancel for 30s`. Total wall time ~32.9 s
  (Probe 1 setup + 30 s watchdog). Reproduced in Step 1 of ticket #69.
- **Mechanism**: same stuck-`✻`-in-4 KB-rolling-buffer family as
  finding 1 and ticket #73 / PR #74. The empirical byte budget that
  finding 1 documented for cancel-recovery (~1.4 KB of redraw, then
  silence) has shrunk just enough on `claude 2.1.148` that the
  pre-cancel `✻` paint isn't pushed out — `IsIdle`'s spinner-absent
  half stays false, the predicate wedges. Reference the developer's
  byte-count evidence (1577 bytes of churn since the last `✻` paint
  at watchdog-trip time, well under the 4096-byte cap).
- **Fix**: both predicates in this file (`waitReappeared.stable` and
  `runRecovery.check`) drop the `tuidriver.IsIdle` wrapper and use a
  direct `bytes.Contains(tuidriver.StripANSI(snap),
  tuidriver.IdleGlyph)` check for the `❯` half. The `QuietFor() ≥
  ptyQuietWindow` half is unchanged (and `runRecovery` adopts it for
  the first time, in place of `idleStableWindow`). Mirrors PR #74's
  `runTurn` shape — three consumers now share the PTY-quiescence
  pattern (this file's two + `spike-multi-turn`'s `runTurn`).
- **Recovery-probe note**: Probes 3/4/5 didn't reach in Step 1's
  diagnosis (Probe 1 fired first), but their predicate shape was the
  verbatim pre-#74 wedge. Bundled in the same fix to avoid the
  unmasking-on-next-run trap.
- **Post-fix run timings** (optional but encouraged): one or two
  successful `bin/spike-cancel -trust-folder=accept` total wall times
  for the 5-probe shape. The historical table in § *Per-probe observed
  timing* covers a 3-probe shape; an additive line for the 5-probe
  shape is appropriate.

Do NOT rewrite findings 1–7 or reorder existing sections. Finding 8
is additive.

The header's *Status* line (line 13ff) may optionally grow a
parenthetical pointer to finding 8 ("…all green on `claude 2.1.144`;
see finding 8 for the `2.1.148` regression and fix"). Optional, not
mandatory.

### Step 2 — Verify

Run the binary directly, then `make e2e`. Both required for ACs 2 + 5
+ 6.

```sh
go build -o bin/spike-cancel ./cmd/spike-cancel
TUIDRIVER_STRICT_MCP_CONFIG=1 bin/spike-cancel -trust-folder=accept
```

Must exit 0; stdout must contain a `SUCCESS:` line; all 5 probes
report green. Total wall time should be in the same ballpark as #11's
README baseline scaled up for two extra probes (#11 was 3 probes in
~13 s; expect ~18-25 s for 5 probes — anything close to 30 s suggests
a probe is hitting a watchdog rather than passing on the predicate).

```sh
make e2e
```

`spike-cancel` must report `pass`. `claude-version-lock` must report
`pass` (AC 5). No previously-passing check may regress (AC 6); the
runner emits per-check status — diff against a recent baseline if
doubt remains.

**Stability** (recommended, not strictly required): run `make e2e` a
second time to confirm the fix is not a single-run anomaly. PR #74's
fix has been stable across multiple `spike-multi-turn` invocations
under the same mechanism; same expectation here.

## Safety

Identical to PR #74's safety argument (see
[`73-spike-multi-turn-pty-quiescence.md`](73-spike-multi-turn-pty-quiescence.md) §
Safety), now applied at two call sites instead of one. Briefly:

- The new predicate is **stronger on the rendering-activity axis**
  (1500 ms of zero PTY bytes vs. 250 ms of glyph-absence — quiescence
  cannot be faked by a transient buffer state) and **weaker on the
  spinner-glyph axis** (the glyph is no longer required to be absent
  — that's exactly the wedging clause being dropped).
- `typePrompt`'s 10 ms inter-byte pacing
  (`cmd/spike-cancel/main.go:760-772`) remains the independent guard
  against "input handler still transitioning from prior turn" and
  absorbs any micro-redraw that lands between predicate-fires and
  first-prompt-byte.
- For `runRecovery` specifically: it always runs **after** a cancel
  probe completed, and `runProbe` sends `Ctrl-U` to clear the dirty
  input box (lines 378-380) **before** the predicate runs. The
  predicate therefore observes a clean post-prompt-submission settling,
  not a dirty pre-clear state. No new race introduced.

The `❯-disappeared` observer goroutine in `runRecovery` (lines 606-626)
still calls `IsIdle` but is bounded by `disappearedWindow = 500ms` and
emits best-effort logging only — it cannot wedge the spike.

## Concurrency model

Unchanged. Same three goroutines (`main`, PTY reader, JSONL tailer)
coordinated by the single `context.WithCancelCause`. The new `stable`
and `check` closures run inside their respective tick loops in `main`'s
goroutine; both read `rb.Snapshot()` and `rb.QuietFor()` (both
mutex-protected) plus closure-local state. No new shared state, no new
goroutines, no new channels.

## Error handling

Unchanged. The session-level watchdog (`TrackerOpts.PTYQuietLimit =
60s` / `SpinnerFreezeLimit = 30s`) continues to fire if either new
predicate fails to converge. Acceptable failure modes:

- If claude keeps redrawing indefinitely (livelocked title-bar updater,
  paint storms), `QuietFor` never reaches 1500 ms and the session-level
  watchdog fires at 60 s. Same failure surface as today.
- If `gotEndTurn` is never observed (tailer dies; claude never writes
  the `end_turn` line) in `runRecovery`, the predicate never fires and
  the session-level watchdog fires at 60 s. Identical to today.
- If the cancel keystroke is dropped, `waitReappeared` observes neither
  redraw nor input-box restoration; `QuietFor` reaches 1500 ms with
  `❯` still absent (the in-flight spinner state still being painted),
  `bytes.Contains` returns false, the predicate stays false until the
  30 s `cancelRecoveryLimit` deadline. Same surface as today — the
  watchdog message changes nothing.

The two current wedges (Probe 1 `waitReappeared` confirmed; Probes 3/4/5
`runRecovery` near-certain) are gone because neither new predicate
gates on `IsIdle`.

## Testing strategy

Per AC 2 + AC 5 + AC 6 — all verified by running the actual binary and
the runner. No unit tests; this is spike-quality code and the predicates
are validated by end-to-end behavior against real `claude`.

- **Pre-fix baseline (already captured)**: developer's Step 1 runs
  saved at `/tmp/spike-cancel-69-baseline.log` (and
  `/tmp/spike-cancel-69-baseline-2.log` from the second deterministic
  run) on the dispatcher host. Two reproductions of `watchdog: stuck
  after cancel for 30s` at Probe 1. AC 1 is satisfied by reference to
  these and the diagnosis comments on the issue.
- **Post-fix direct binary run** (§ Step 2): exit 0, `SUCCESS:` on
  stdout, all 5 probes green, wall time < 30 s. AC 2.
- **Post-fix `make e2e`** (§ Step 2): `spike-cancel: pass`,
  `claude-version-lock: pass`, no regression in any previously-passing
  check. ACs 2 + 5 + 6.
- **Stability** (optional second `make e2e`): confirms the fix is
  deterministic, not a lucky single run.

No new unit tests in `pkg/tuidriver/`. `IsIdle` is unchanged (other
consumers, including the `❯-disappeared` observer, still use it
unchanged) and its existing tests still apply.

## Acceptance criteria mapping

- **AC 1 (architect names failing probe and mechanism, matched against
  PTY logs / JSONL / mirror; not pattern-matched against #73)** —
  satisfied by the developer's Step 1 evidence (referenced in § Evidence
  above) and by this spec's § Context / Root cause. Probe 1 named
  explicitly; mechanism named via byte-count math against the captured
  log. The recovery-probe wedge is named as structural-identity-transfer
  from PR #74's evidence (also concrete, also non-pattern-match — the
  predicate shape is verbatim identical).
- **AC 2 (`spike-cancel` passes under `make e2e`)** — covered by
  § Design / Step 1 (both predicate swaps) and § Step 2 (verify with
  direct binary + `make e2e`).
- **AC 3 (diagnosed cause named in PR description, one or two
  sentences)** — the developer's PR body cites § Context / Root cause
  above (cancel-probe wedge confirmed at Probe 1 in `waitReappeared`,
  same stuck-`✻`-in-4 KB-buffer mechanism as #73 → PR #74; recovery-probe
  wedge bundled in the same fix by structural identity). README
  finding 8 is the durable record.
- **AC 4 (no `t.Skip` or known-fail mute; no removal-instead-of-fix
  without explanation)** — the spec prescribes only the fix path. No
  skip, no removal.
- **AC 5 (`claude-version-lock` keeps passing)** — covered by § What
  does NOT change below: the runner and the lockfile are untouched.
- **AC 6 (`make e2e` runs cleanly end-to-end; no adjacent regressions)**
  — covered by the single-file change scope (one production file +
  one README) and by the post-fix `make e2e` run in § Step 2.

## What does NOT change

- `pkg/tuidriver/state.go` — `IsIdle` unchanged. Other consumers
  (including the `❯-disappeared` observer in this file) keep current
  behavior.
- `pkg/tuidriver/buffer.go` — `QuietFor` already exists; no changes.
- The `❯-disappeared` observer goroutine in `runRecovery`
  (`cmd/spike-cancel/main.go:606-626`) — unchanged; bounded by
  `disappearedWindow = 500ms`; can't wedge.
- The session-level idle wait pre-Probe-1 (lines 266-270) — unchanged;
  pre-prompt-1; no stuck-glyph risk.
- The probe table (lines 140-146) — same 5 probes, same prompts.
- The session-level watchdog (60 s PTY-quiet / 30 s
  spinner-counter-freeze; lines 208-211) — unchanged; PTY-quiescence
  at 1.5 s is the predicate firing, 60 s PTY-quiet is the failsafe —
  different scales, different purposes.
- `cmd/spike-multi-turn/main.go` — unchanged. PR #74's fix stays.
- `cmd/e2e-runner/main.go` — unchanged. AC 5 + AC 6 are upheld by not
  touching the runner.
- `claude-version.lock` — unchanged. Lock rotation is a separate
  concern (ticket constraint § *Do not bundle*).

## Out of scope (re-stating ticket § Out of Scope for the developer)

- The library-extraction architectural arc (#58–#62). After this spec
  lands, three consumers will share the PTY-quiescence shape
  (`waitReappeared`, `runRecovery`, `spike-multi-turn`'s `runTurn`) —
  that's the integration-pressure data point library extraction will
  use to settle the API.
- `claude-version.lock` rotation.
- The other four currently-failing e2e checks (#70, #71, #72, and the
  fifth in the set).
- Any further investigation of cancel-keystroke semantics or JSONL
  schema changes. The diagnosis surfaced the same mechanism as #73;
  the alternative-cause branches the round-1 spec listed are closed.

## Open questions

None. Predicate shape, window length, fix surface, and the safety
argument all transfer directly from PR #74 (validated in production
for the same buffer-residue mechanism on the same claude version
family). The structural-identity decision for bundling the
`runRecovery` swap is named explicitly in § Context / Why not split.

# Spec: `spike-permission` post-approve predicate — PTY-quiescence to clear stuck `✻` glyph

**Ticket:** [#70](https://github.com/pyrycode/tui-driver/issues/70)
**Size:** S
**Status:** ready for development
**Posture:** predicate swap. `runAutoRespond` (Probe 2) currently uses
`gotEndTurn ∧ tuidriver.IsIdle(rb) stable for idleStableWindow = 250 ms`
— the verbatim pre-#74 spike-multi-turn predicate, copy-pasted into
spike-permission's auto-respond path. Against `claude 2.1.148` this
wedges for the same `✻ … for Ns` stuck-glyph-in-4-KB-rolling-buffer
reason PR #74 fixed in spike-multi-turn and PR ?? (issue #69) is fixing
in spike-cancel's `runRecovery`. Replace with the PTY-quiescence
predicate that two sibling spikes already validate empirically. No new
package symbols; no behavioural change to `IsIdle`; one binary touched.

## Files to read first

Load these before touching anything. They cover the failing predicate,
the proven sibling pattern this spec adopts, and the buffer/state
primitives the new predicate composes.

- `cmd/spike-permission/main.go:655-695` — the failing
  `runAutoRespond` predicate. Lines 658-672 are the `check` closure
  that wedges; lines 628-647 are the rationale comment block that
  needs rewriting. The surrounding `for !check()` loop, the
  `modalCleared` first-event flip, and the `gotEndTurn` flag set on
  `isEndTurn(ev)` all stay as-is. Only the closure body and the
  rationale comment change.
- `cmd/spike-permission/main.go:97-103` — constants block where
  `idleStableWindow = 250 * time.Millisecond` (line 99) is defined.
  Drop the line; add `ptyQuietWindow = 1500 * time.Millisecond` with a
  comment block citing the empirical derivation from sibling spikes.
- `cmd/spike-permission/main.go:1240-1246` — compile-time anchor block
  (`var ( _ = hasSpinnerGlyph; _ = isToolUse; _ = disappearedWindow;
  _ = idleStableWindow )`). The `_ = idleStableWindow` line must be
  removed when the constant is removed; the other three anchors stay.
- `cmd/spike-multi-turn/main.go:353-400` — the PR #74 reference
  implementation. The new spike-permission `check` closure mirrors this
  shape: drop the `idleSince` tracking, compose `gotEndTurn` ∧
  `bytes.Contains(StripANSI(rb.Snapshot()), tuidriver.IdleGlyph)` ∧
  `rb.QuietFor() >= ptyQuietWindow`. The comment block at 353-381 is
  the template for the new spike-permission rationale block.
- `cmd/spike-cancel/main.go:82-91` — the rationale block for
  `ptyQuietWindow`. Same failure mode; same fix shape; same calibrated
  1500 ms window. Cite this file:line in the new spike-permission
  comment block.
- `cmd/spike-cancel/main.go:513-568` — `waitReappeared`, the original
  proven PTY-quiescence predicate that introduced this pattern to the
  spike suite. Cite for the proven-sibling argument.
- `pkg/tuidriver/state.go:9-13` — `IdleGlyph` exported byte constant.
  The new predicate calls `bytes.Contains(StripANSI(snap),
  tuidriver.IdleGlyph)` directly rather than going through
  `tuidriver.IsIdle`. `spike-permission` already imports `bytes`
  (line 47) and `tuidriver` (line 65); no new imports.
- `pkg/tuidriver/state.go:38-44` — `IsIdle` definition. The new
  predicate **does NOT** require `IsIdle`'s spinner-absent half (that
  is the wedging clause). It does require the `❯`-glyph-present half.
  Same delta as #73 and #69. The AC's safety property holds — see
  § Safety.
- `pkg/tuidriver/buffer.go:62-71` — `QuietFor()`. Returns time since
  last `Append`. Mutex-protected and side-effect-free; safe to call
  alongside `Snapshot()` from `runAutoRespond`'s loop.
- `cmd/spike-permission/main.go:474-489` — `runSession`'s probe-kind
  dispatch. Confirms that `runObserve` (Probe 1) and `runEscalate`
  (Probe 3) **do not** use this predicate. Only `runAutoRespond`
  (Probe 2) needs the change. Cited to bound the blast radius.
- `cmd/spike-permission/main.go:381-408` — the session-level watchdog
  goroutine (`ptyQuietLimit = 30 s`, `spinnerFreezeLimit = 30 s`).
  Untouched. The new predicate fires at 1.5 s of PTY silence; the
  watchdog at 30 s of PTY silence — different scales, different
  purposes.
- `cmd/e2e-runner/main.go:239-245` — the `spike-permission` check
  definition. Uses `commonArgs = ["-trust-folder=accept"]` and expects
  the `SUCCESS:` marker (`successSuccess`). The fix must produce a
  `SUCCESS:` line on the runner host. Untouched by this spec.
- `docs/knowledge/architecture/system-overview.md` § *Key signals*
  — `Approve keystroke (permission modals)` and `Turn-complete
  predicate (multi-turn)` entries will become stale once the new
  spike-permission predicate is in place (the multi-turn key-signal
  paragraph effectively becomes a two-consumer pattern, plus the
  spike-cancel `runRecovery` consumer when #69 lands). Documentation
  phase owns the update; do **not** edit this file from the developer
  worktree.

## Context

### Symptom

Reproducible against `claude 2.1.148` (runner host as of 2026-05-21):

```
make e2e
… spike-permission -> fail (~54 s)
```

Reproduces on both `feature/73` and the merge-base baseline `40690c7`
(current `main`). The failure pre-exists PR #74; was previously masked
by the `claude-version-lock` short-circuit at
`cmd/e2e-runner/main.go:181-183` (removed earlier, correctly).

The ~54 s runtime is consistent with one watchdog firing inside
`runAutoRespond`: Session A's `runObserve` completes in ~15-20 s; the
fresh Session B starts up and reaches the modal in ~5-10 s; sending the
approve keystroke kicks claude into tool execution + assistant response
+ end_turn; the predicate then wedges and ~30 s later the session
watchdog (`ptyQuietLimit = 30 s` or `spinnerFreezeLimit = 30 s`,
whichever trips first) cancels the run with the watchdog-shaped error.

### Root cause

`runAutoRespond`'s predicate
(`cmd/spike-permission/main.go:658-672`) is the **verbatim pre-#74
spike-multi-turn predicate**:

```
check := func() bool {
    if !gotEndTurn { return false }
    if !tuidriver.IsIdle(rb.Snapshot()) {
        idleSince = time.Time{}
        return false
    }
    if idleSince.IsZero() {
        idleSince = time.Now()
        return false
    }
    return time.Since(idleSince) >= idleStableWindow   // 250 ms
}
```

`tuidriver.IsIdle` (`pkg/tuidriver/state.go:38-44`) requires `❯`
present **AND** `✻` absent. The wedging mechanism is identical to
#73 / #69:

1. Probe 2's prompt is `"list the files in /tmp"`. Claude shows a
   permission modal for the Bash tool.
2. Spike sends `1\r` to approve; claude executes `ls /tmp`, streams
   tool output, then writes the assistant response with `stop_reason =
   end_turn`.
3. During the planning + tool-execution phase claude paints `✻ Verb
   for Ns` spinner glyphs into the rolling buffer.
4. After the assistant message completes, claude emits a small redraw
   + title-bar updates, then goes quiet. The post-`end_turn` traffic
   is well under the 4 KB rolling-buffer cap
   (`pkg/tuidriver/buffer.go:8-13`); the last `✻` paint does not roll
   out.
5. `gotEndTurn` flips to true on the assistant event. `IsIdle`'s `❯`
   half holds (input box restored). `IsIdle`'s `✻` half does not:
   the stuck glyph keeps it false forever.
6. The conjunction never holds. Watchdog fires ~30 s later.

This is the same failure-mode chain documented and fixed at
[`cmd/spike-multi-turn/main.go:353-400`](../../../cmd/spike-multi-turn/main.go)
(#73 / PR #74). The runtime byte-count math the spike-cancel #69
re-spec captured (1577 bytes of post-cancel churn — well under
4 KB) is essentially the same envelope as spike-permission's
post-`end_turn` shape: a small final assistant message (a `/tmp`
listing summary), a brief redraw, then silence.

### Why this is the same problem spike-multi-turn / spike-cancel already solved

`cmd/spike-cancel/main.go:82-91` documents the empirical derivation of
`ptyQuietWindow = 1500 ms` for the analogous post-cancel readiness
predicate:

> The spec's first-cut predicate ("hasSpinnerGlyph becomes false AND
> isIdle true") fails empirically because the spinner glyph the
> wait-for-kickoff step observed sits in the 4096-byte rolling buffer
> and post-cancel claude doesn't emit enough bytes to roll past it
> (observed empirically: ~1.4 KB of redraw + title-bar updates, then
> silence). The PTY-quiescence proxy detects that silence directly.

PR #74 adopted the same predicate for `spike-multi-turn`'s post-`end_turn`
readiness; the spike-cancel `runRecovery` predicate (issue #69, in
flight on `origin/feature/69`) adopts the same predicate against the
same wedge. Spike-permission's `runAutoRespond` is the third
consumer of this exact failure mode.

The Possible-Causes list on the ticket mentions three rival hypotheses
(marker change, keystroke regression, tool-specific auto-approve change).
This spec rules each out by code inspection:

- **Marker change.** `runAutoRespond` does not gate on any new marker
  text post-keystroke — it waits on `eventCh` (JSONL `type=assistant`
  events from the tailer) and on `IsIdle`. The JSONL contract for
  `stop_reason=end_turn` has not changed (spike-multi-turn / spike-cancel
  / spike-one-turn all detect it correctly on the same runner host on
  current `main`). The PTY-side `IsIdle` predicate is exactly what
  wedges; that's the diagnosed bug, not a marker drift.
- **Keystroke regression.** If `1\r` were no longer the correct
  approve keystroke, Probe 2 would hang at `waitForModal` →
  `modal-detected` → … with the modal still up (because claude
  ignored the keystroke). The ticket evidence shows ~54 s runtime,
  which is incompatible with a stuck-modal failure (the
  `modalDetectLimit = 30 s` would fire much earlier, returning a
  cleanly distinct error). And: spike-cancel's `sendKeystroke` shares
  the same single-byte write path; if `1\r` were broken claude would
  also not recover from cancel + approve cycles in other contexts,
  which it does on the same runner.
- **Tool-specific auto-approve drift.** `runAutoRespond` reaches
  `gotEndTurn=true` (otherwise the watchdog would fire from a
  different state). That implies the modal cleared, the tool executed,
  and claude emitted the assistant response — i.e. permission flow
  succeeded. The wedge is **post**-end_turn, on the readiness
  predicate.

### Chosen fix shape (PTY-quiescence — same as #73, #69)

Replace `runAutoRespond`'s `check` closure with:

```
turn-complete (auto-respond, post-fix) =
    gotEndTurn
  ∧ ❯ glyph present in StripANSI(rb.Snapshot())
  ∧ rb.QuietFor() ≥ ptyQuietWindow                  (1500 ms)
```

The `gotEndTurn` clause and the `eventCh` accumulation are unchanged.
The wedging `IsIdle` call goes away (replaced by the inlined
`❯`-present check). The `idleSince` time tracking goes away
(`rb.QuietFor()` carries the temporal dimension).

## Design

### Predicate shape

Identical composition to `spike-multi-turn`'s post-#74 predicate
(`cmd/spike-multi-turn/main.go:391-400`):

```
check := func() bool {
    if !gotEndTurn { return false }
    stripped := tuidriver.StripANSI(rb.Snapshot())
    if !bytes.Contains(stripped, tuidriver.IdleGlyph) { return false }
    return rb.QuietFor() >= ptyQuietWindow
}
```

This is the contract sketch, not the implementation — the developer
writes the actual closure in the spike's idiom (the existing closure
lives inside `runAutoRespond` and the locals are already in scope).

### Code changes

**File: `cmd/spike-permission/main.go`**

Constants block (lines 97-103):

- Remove `idleStableWindow = 250 * time.Millisecond` (line 99) and any
  associated comment.
- Add `ptyQuietWindow = 1500 * time.Millisecond` with a comment block
  that:
  (a) names the empirical derivation site —
      `cmd/spike-cancel/main.go:82-91`,
  (b) names the proven sibling at
      `cmd/spike-multi-turn/main.go:353-400` (post-#74), and
  (c) calls out the 4 KB rolling-buffer constraint at
      `pkg/tuidriver/buffer.go:8-13` as the load-bearing reason for
      using quiescence instead of glyph-absence.

Compile-time anchor block (lines 1240-1246):

- Remove the `_ = idleStableWindow` line. Keep `_ = hasSpinnerGlyph`,
  `_ = isToolUse`, `_ = disappearedWindow` — those still exist as
  unused symbols carried for parity with the rest of the spike suite.

Rationale comment block (lines 628-647):

- Rewrite to describe the new predicate. Mandatory content:
  (a) what the predicate is (`gotEndTurn ∧ ❯-present ∧ PTY-quiet
      for 1.5 s`),
  (b) why the conjunction is safe (no race window — see § Safety),
  (c) the failure mode this replaces (stuck `✻` glyph in 4 KB
      rolling buffer post-`end_turn`),
  (d) references to the two proven siblings —
      `cmd/spike-multi-turn/main.go:353-400` (#73) and
      `cmd/spike-cancel/main.go:513-568` (the original PTY-quiescence
      consumer), and the empirical-derivation citation at
      `cmd/spike-cancel/main.go:82-91`.

`runAutoRespond`'s `check` closure (lines 658-672):

- Replace with the sketch under § Predicate shape above.
- Remove the `var idleSince time.Time` declaration directly above the
  closure (line 658).

No other functions change. No new exported package symbols. No new
imports (`bytes` and `tuidriver` already in the import block at lines
47 / 65).

### Why no `pkg/tuidriver` helper

This spec deliberately keeps the predicate inside the spike binary,
matching the project's standing pattern documented in
[`docs/knowledge/architecture/system-overview.md` § Current state]:
"All four spikes share ~600 LOC of helpers under `// copied from
cmd/spike-...` attribution comments. The duplication is deliberate —
every spike binary deletes when `pkg/tuidriver/` lands."

The PTY-quiescence pattern now has three consumer call sites across
the spike suite (post-cancel readiness in spike-cancel's
`waitReappeared`, post-`end_turn` readiness in spike-multi-turn's
`runTurn` after #74, post-approve readiness in spike-permission's
`runAutoRespond` after this spec — and post-recovery readiness in
spike-cancel's `runRecovery` after #69). That growing consumer set
is exactly the kind of integration pressure the library-extraction
work (#58–#62) needs to settle on an API shape. Lifting into
`pkg/tuidriver/` now would prematurely commit a shape; the
extraction ticket cycle is the right place.

### What does NOT change

- `pkg/tuidriver/state.go`'s `IsIdle` is untouched. Every other
  consumer (`runObserve`, `runEscalate`, the post-spawn idle wait at
  lines 412-415, the post-trust-accept idle wait at lines 430-433,
  spike-one-turn, spike-cancel's `waitReappeared`, spike-multi-turn's
  session-level wait at lines 202-206, all the modal/picker probes,
  etc.) keeps its existing behaviour. The new PTY-quiescence
  predicate composes around `IsIdle` rather than replacing it.
- `pkg/tuidriver/buffer.go` is untouched. `QuietFor` and `Snapshot`
  are already public and used by spike-cancel + spike-multi-turn.
- `runObserve` (Probe 1, lines 499-576) is untouched. It does not
  use the wedging predicate — it only waits for the modal, snapshots
  bytes, runs a 5 s observation window, returns. The session-level
  shutdown defer SIGTERMs claude on return.
- `runEscalate` (Probe 3, lines 712-785) is untouched. Same shape:
  waits for modal, snapshots, sleeps `escalationWindow = 3 s`,
  verifies modal still open, returns.
- The session-level watchdog (lines 387-408,
  `ptyQuietLimit = 30 s` and `spinnerFreezeLimit = 30 s`) is
  untouched. PTY-quiescence at 1.5 s is the predicate firing; 30 s
  PTY-quiet is the watchdog rule — different scales, different
  purposes.
- The `modalDetectLimit = 30 s` wait inside `runAutoRespond` (line
  613) is untouched. It gates on `hasModal`, not on `IsIdle`.
- The post-keystroke `modalCleared` first-event flip (lines 680-684)
  is untouched. The JSONL tailer pre-filters to `type=assistant`
  events, so the first event after approve IS the modal-cleared
  signal — this is independent of the readiness predicate.
- The session-scoped `eventCh` drain in `runEscalate` (lines 728-735)
  is untouched.
- `cmd/e2e-runner/main.go`'s `spike-permission` check definition
  (`commonArgs`, `successSuccess`) is untouched. The fix is contained
  to the spike binary.

## Safety

The AC's implicit safety contract is the same as #73's AC 2: the SUCCESS
line is only printed when (a) `stop_reason=end_turn` has been observed
AND (b) the TUI is verifiably ready (input box restored, claude no
longer redrawing). The new predicate satisfies both:

(a) `gotEndTurn` is unchanged.

(b) "TUI verifiably ready" means two things in practice:
- claude has stopped emitting bytes — `rb.QuietFor() >= 1500 ms`
  observes this directly (the rolling buffer is the byte sink, so
  "buffer has been quiet for 1.5 s" ↔ "claude has emitted no bytes
  for 1.5 s").
- the input prompt is on screen — `❯` glyph present in the stripped
  buffer establishes this.

Compared to the OLD predicate (`IsIdle` stable for 250 ms), the NEW
predicate is:
- **Stronger** on the rendering-activity axis: 1500 ms of zero bytes
  is a much higher bar than 250 ms of "`✻` glyph not in buffer."
  Quiescence cannot be faked by a transient buffer state.
- **Weaker** on the spinner-glyph axis: we no longer require the
  glyph to be absent. This is the entire point — the OLD requirement
  is exactly what wedges, and an absent `✻` glyph is neither
  necessary (PTY-quiescence covers "claude is done") nor sufficient
  (a glyph rolling out doesn't mean claude is done) for "TUI ready."

There is no race window where claude could emit a fresh
prompt-relevant byte between the predicate firing and the `SUCCESS:`
print: the predicate fires only on observing 1500 ms of silence, so by
construction claude has been doing nothing for 1.5 s by the time the
extraction + print runs.

`runAutoRespond` does not write any further keystrokes after approve
(unlike `runTurn` in spike-multi-turn, which writes the next turn's
prompt). The output after the predicate is the `SUCCESS: <text>` print
to stdout and a return to the caller. No PTY write race surface.

## Concurrency model

Unchanged. Same three goroutines as the rest of the spike suite
(`main`, PTY reader inside `tuidriver.Spawn`, JSONL tailer started by
the `openTailerOnce` hook) coordinated by a single
`context.WithCancelCause`. The session-level watchdog goroutine is
also unchanged. The `check` closure runs inside `runAutoRespond`'s
`for !check()` loop and reads `rb.Snapshot()` and `rb.QuietFor()`
(both mutex-protected) plus the local `gotEndTurn` flag. No new shared
state.

## Error handling

Unchanged. The session-level watchdog continues to fire on
`ptyQuietLimit = 30 s` or `spinnerFreezeLimit = 30 s` if the new
predicate fails to converge for any reason. Acceptable failure modes:

- If post-`end_turn` claude keeps redrawing indefinitely (e.g. a
  livelocked title-bar updater), `QuietFor()` never reaches 1500 ms,
  the watchdog fires at 30 s. Same failure surface as today, with
  the same recovery (read the watchdog log, escalate).
- If `gotEndTurn` is never observed (e.g. the tailer dies or claude
  never writes the end_turn line), the predicate never fires, the
  watchdog fires at 30 s. Identical to today.

The current bug (`gotEndTurn=true, IsIdle wedged false, watchdog fires
at 30 s`) is gone because the new predicate doesn't gate on `IsIdle`.

## Testing strategy

Per AC 1 + AC 4 + AC 5 — verified by running the actual e2e runner.

Scenarios the developer must observe:

- **AC 1 + AC 5, baseline run.** Build, then run the runner against
  current `claude` on the dispatcher host:
  ```
  make e2e
  ```
  The report must show `spike-permission` as `pass`. No new failures
  in adjacent checks (`spike-one-turn`, `spike-multi-turn`,
  `spike-cancel`, `spike-multiselect`, `spike-ask-user`,
  `spike-long-prompt`, `probe-first-prompt-hang`, `snapshot-drift`).
  Expected wall-time for `spike-permission` post-fix: in the same
  ballpark as the pre-`claude 2.1.148` baseline that PR #13 / spec 13
  established for this check (a clean run; not the wedged ~54 s).
- **AC 4, version-lock not regressed.** Confirm
  `claude-version-lock` continues to pass in the same report. (The
  fix touches only `cmd/spike-permission/main.go`; no lock-file or
  flag/value drift introduced.)
- **Targeted single-binary smoke (optional but recommended).** Run
  the spike directly with the same args the runner uses:
  ```
  go build -o bin/spike-permission ./cmd/spike-permission
  TUIDRIVER_STRICT_MCP_CONFIG=1 bin/spike-permission -trust-folder=accept
  ```
  Must exit 0 with one `SUCCESS:` line on stdout. The session-A
  Probe 1 observe log + session-B Probe 2 SUCCESS + session-B Probe 3
  escalation log lines should all appear on stderr in the expected
  order (`probe=1 modal-detected`, `probe=2 modal-detected`,
  `probe=2 modal-cleared`, `probe=2 end-turn-detected`,
  `probe=2 assistant-text-extracted`, `probe=3 modal-detected`,
  `probe=3 modal-still-open=true`).

No new unit tests in `pkg/tuidriver/`. `IsIdle` is unchanged and its
existing tests at `pkg/tuidriver/state_test.go` still apply. `QuietFor`
is unchanged and covered by `pkg/tuidriver/buffer_test.go`. The new
predicate composition lives in a single binary and is verified by the
binary's e2e behaviour against the canonical reproduction case (the
e2e runner).

The README at `cmd/spike-permission/README.md` may be updated with a
post-fix empirical-timing observation by the developer if they choose,
following the existing finding-pattern format — but the documentation
phase owns the system-overview key-signals update, not the developer.

The PR description must name the diagnosed cause per AC 2 — one or two
sentences along the lines of:

> `runAutoRespond`'s readiness predicate
> (`gotEndTurn ∧ tuidriver.IsIdle ∧ stable 250 ms`) wedged on
> `claude 2.1.148` because a `✻` spinner glyph painted during
> tool execution stays in the 4 KB rolling buffer post-`end_turn`,
> keeping `IsIdle` false forever. Replaced with the PTY-quiescence
> predicate (`gotEndTurn ∧ ❯-present ∧ rb.QuietFor() ≥ 1500 ms`)
> that PR #74 already validated for the same failure mode in
> `spike-multi-turn`.

## Open questions

None. The predicate shape, window length, and migration path are all
copied from spike-multi-turn's PR #74 implementation (which copied
from spike-cancel's `waitReappeared`). The architect-side decision
was confirming the diagnosis from code inspection (§ Context); the
developer-side decision surface is empty.

## Acceptance criteria mapping

- **AC 1** (`spike-permission` PASSES in `make e2e` against current
  `claude`) — covered by § Design's predicate swap and § Testing
  strategy's `make e2e` run.
- **AC 2** (diagnosed cause named in PR description) — covered by
  the PR-description sketch in § Testing strategy.
- **AC 3** (no `t.Skip` / "known-fail" mute; no removal of the
  check) — covered by § Design: the fix changes the spike's predicate;
  `cmd/e2e-runner/main.go:239-245` keeps `spike-permission` in the
  check list with the same `SuccessMarker = successSuccess`.
- **AC 4** (`claude-version-lock` continues to pass) — covered by
  § Testing strategy. The lock-file and the version-lock check
  implementation are untouched.
- **AC 5** (`make e2e` runs cleanly end-to-end, no new adjacent
  failures) — covered by § Testing strategy.

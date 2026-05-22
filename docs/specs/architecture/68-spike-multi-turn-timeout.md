# Spec: diagnose and fix `spike-multi-turn` e2e timeout

**Ticket:** [#68](https://github.com/pyrycode/tui-driver/issues/68)
**Size:** S
**Status:** ready for development
**Posture:** diagnosis-first — the fix shape is selected by what the developer
observes when running the spike binary directly. Two plausible fixes are
pre-authorised below; a third path (escalate-and-stop) covers the case where
the diagnosis exposes a non-trivial regression.

## Files to read first

Load these before touching anything. They cover the spike's empirical
baseline, the runner's timeout machinery, and the recent change that
unmasked the timeout.

- `cmd/spike-multi-turn/main.go:92-98` — the prompt list. Five prompts:
  three from the original spike charter (`say hello`, `list the files in
  /tmp`, `think carefully and compute 1+2+3+...+100`) and two appended
  during follow-up experiments (`read /etc/hosts and /etc/passwd ...`,
  parallel-tool stress variant of same). The comment block above the
  slice explicitly flags 1-3 as load-bearing and 4-5 as experimental.
- `cmd/spike-multi-turn/main.go:255-264` — the prompt loop. Drives one
  `runTurn` per prompt, sequentially. Failure on any turn aborts the
  spike with a non-zero exit. SUCCESS is printed once per turn (line
  413), so the runner's `(?m)^SUCCESS` regex matches on the FIRST turn's
  stdout — but it only inspects stdout after `cmd.Run()` returns, which
  requires every turn to complete or the process to be killed by the
  timeout.
- `cmd/spike-multi-turn/main.go:463-475` — `typePrompt`, char-by-char
  with 10 ms inter-byte delay + 50 ms tail pause. Per the spike README
  surprise #2, bulk-writing fails after a tool-use turn; this is the
  current workaround. Each prompt incurs `len(prompt)*10ms + 50ms`
  before submit (prompt 5 = ~2.5 s of typing latency alone).
- `cmd/spike-multi-turn/README.md` § *Per-turn observed timing* — the
  ONLY recorded empirical baseline for this binary. Three runs (5, 6,
  7) at the 3-turn shape took **16–21 s wall time**. No timings have
  been recorded for the 5-turn shape against any claude version. This
  gap IS the bug — the experimental prompts were appended without
  re-baselining the e2e timeout budget.
- `cmd/e2e-runner/main.go:31-36` — timeout constants.
  `defaultCheckTimeout = 60 * time.Second` is what `spike-multi-turn`
  inherits (no per-check override in `buildChecks`).
- `cmd/e2e-runner/main.go:218-231` — the `spike-multi-turn` Check entry.
  Note the absence of an explicit `Timeout` field — this is what the
  fix may need to add.
- `cmd/e2e-runner/main.go:493-562` — `runCheck`. Confirms that
  `status = "timeout"` fires only on `context.DeadlineExceeded`; the
  spike's own non-zero exit (from a `runTurn` failure mid-spike) would
  surface as `"fail"` instead. **The reported symptom is `timeout`, so
  the cause is wall-clock exhaustion, not a clean spike failure.**
- `cmd/e2e-runner/main.go:181-183` + commit `080cea8` — the
  short-circuit that masked this. Before #64, `claude-version-lock`
  failed whenever the installed `claude` mismatched the locked version
  (`2.1.144`) and short-circuited the rest of the run. #64 narrowed
  the lock to flag/value substring assertions only, so version drift
  no longer trips the gate — and the downstream timeout now surfaces.
- `cmd/e2e-runner/main.go:92-117` — the `-timeout NAME=DUR` flag parser
  on the runner. The override path already exists; per-check `Timeout`
  values in `buildChecks` set the baseline that `-timeout` overrides.

Not load-bearing for this ticket, but useful context if the diagnosis
goes off-script:

- `claude-version.lock` — informational `version=2.1.144`; flag/value
  contract verified independently. No version pin to update here.
- `docs/specs/architecture/47-spike-long-prompt.md` § *Timeout* — sets
  precedent for "default 60 s is the baseline; bump only with evidence."

## Context

`make e2e` invokes the e2e-runner, which spawns each spike as a subprocess
under `defaultCheckTimeout = 60 s`. The `spike-multi-turn` check has been
timing out every run on the dispatcher host since the
`claude-version-lock` short-circuit (line 181-183) stopped firing — #64
narrowed the lock to flag/value presence, so the downstream timeout that
was being masked now surfaces.

The spike binary is unchanged in shape since its spec landed (#9), but
prompts 4 and 5 were appended later (commits `d423222`, `4908bd1` on
2026-05-18) as follow-up empirical probes for parallel-tool behavior.
These additions doubled the per-run wall time without anyone reviewing
the runner's 60 s budget. The spike's README documents the **3-turn**
timing baseline (16–21 s) but has no 5-turn baseline. We have **no
recorded run** of the current binary against the current claude that
both (a) succeeded and (b) measured total wall time. The first job of
the developer is to produce that measurement.

The diagnostic surface is small. The fix surface is even smaller — at
most two files (`cmd/e2e-runner/main.go`, `cmd/spike-multi-turn/main.go`)
and at most ~30 lines of production change.

## Empirical facts already in hand (do NOT rediscover)

1. **The spike worked end-to-end against an earlier claude at the
   3-turn shape.** Three captured runs in `cmd/spike-multi-turn/README.md`
   at 16, 16, and 21 s. The msg_id-grouped extractor, the char-by-char
   `typePrompt`, the `gotEndTurn ∧ isIdle stable` predicate are all
   validated empirically. Do not relitigate them — they are out of
   scope for this fix.
2. **Prompts 1, 2, 3 are part of the spike's stated charter
   (ticket #9 AC).** Prompts 4 and 5 are explicitly experimental
   additions (see the comment block at lines 80-91 of `main.go`).
   Removing 4-5 reverts the binary to its sanctioned 3-turn shape;
   removing 1-3 would change the spike's purpose and is out of scope.
3. **The runner's timeout error is wall-clock, not a spike-side
   exit-code failure.** `runCheck` distinguishes `"timeout"` (deadline
   exceeded) from `"fail"` (non-zero exit or marker miss). The ticket
   reports `timeout`, so we know the spike process was killed by the
   runner, not crashing/exiting cleanly with a `runTurn` error.
4. **`claude` version on the dispatcher host has moved within the
   2.1.x line** (issue body cites 2.1.144 → 2.1.146 as a candidate
   change window). The lock's flag/value contract still passes — so
   the flags this spike uses (`--session-id`, `--permission-mode`,
   `bypassPermissions`) still exist in `claude --help`. Whether their
   *semantics* changed is open.

## Diagnosis (do this first, before changing any code)

The whole point of this spec is to let the developer's first move be
**observation**, not edit. The diagnostic procedure is short:

### Step 1 — run the spike standalone, time it, save the log

From the repo root, with `bin/spike-multi-turn` built:

```
go build -o bin/spike-multi-turn ./cmd/spike-multi-turn
time bin/spike-multi-turn -trust-folder=accept 2>spike.stderr 1>spike.stdout
```

Three outcomes worth distinguishing. The diagnosis branches on which
one fires:

- **(A) Spike completes successfully** (exit 0, five `SUCCESS:` lines on
  stdout, `time` shows wall < 5 min). Look at the wall clock.
  - If wall is **≤ 60 s**: the runner's 60 s timeout should not be
    tripping. Either you got lucky, or something else is going on —
    run the spike 2 more times, record each wall time, and look for
    variance. Median run that stays under 60 s but spikes over it is
    still a timeout fix.
  - If wall is **> 60 s and ≤ 180 s**: confirmed — the 5-turn run does
    not fit the default budget. Go to **Fix A**.
  - If wall is **> 180 s**: unexpectedly slow. Inspect `spike.stderr`
    to see which turn ate the time. May point to a real claude
    behavior change that warrants deeper investigation; in that case
    escalate per **Path C**.
- **(B) Spike fails cleanly mid-run** (exit 1, `spike failed: turn N:
  ...` on stderr, fewer than five `SUCCESS:` lines on stdout). The
  runner would surface this as `"fail"`, not `"timeout"`. If you see
  this, the dispatcher's observation that the runner reports `timeout`
  is inconsistent with what you're seeing locally — re-run and confirm.
  If it reproduces: this is a spike-binary regression against the
  current claude. Go to **Path B** (analysis below).
- **(C) Spike hangs forever** (no progress, no new `turn=N` log lines
  for ≥60 s, watchdog at line 53 ought to trip but if `time` shows
  significantly more than 60 s wall before manual kill, the watchdog
  isn't firing). This is the worst case — a state-detection failure
  where the spike thinks it's still making progress. Almost certainly
  the result of a claude TUI rendering change. Escalate per **Path C**.

Capture the stderr log either way. The `turn=N` and `end-turn-detected`
log lines reveal per-turn wall time at log-microsecond precision; this
is the data needed to justify a per-check timeout value if **Fix A** is
taken.

### Step 2 — branch on diagnosis

The decision is binary at first cut: does the spike SUCCEED end-to-end
when given enough wall budget?

- **Yes** → **Fix A** (raise the runner's per-check timeout for
  `spike-multi-turn`). This is the expected path given the unrebaselined
  prompt additions.
- **No, fails cleanly** → **Path B** (analyse the failing turn).
- **No, hangs** → **Path C** (escalate).

## Fix A — raise the per-check timeout for `spike-multi-turn`

This is the simplest viable fix and the one most likely warranted by the
diagnosis. **Take this path iff** Step 1 outcome (A) reproduces with
wall time > 60 s and ≤ 180 s across at least two runs.

### Change

In `cmd/e2e-runner/main.go`, `buildChecks`, add a `Timeout` field to the
`spike-multi-turn` Check entry (current entry is at lines 225-231 of
the file). The new value MUST be the **observed median wall time plus a
safety margin**:

- Run the spike standalone **at least 3 times** to characterise variance.
- Pick `Timeout = max(observed) * 1.5`, rounded up to the next
  `15 * time.Second` boundary. If the maximum observed run is 50 s, the
  new timeout is `90 * time.Second`. If 70 s, then `105 * time.Second`.
  If 90 s, then `135 * time.Second`. **Do not** pick a round-number
  default (120 s, 180 s) without evidence — the budget has to be
  motivated by the recorded data, and the data goes into the PR
  description per AC.
- Cap at `180 * time.Second`. Anything beyond that means the spike
  has a real performance issue that demands investigation, not a
  bigger budget — escalate via **Path C**.

The Check entry shape after the change (sketch, exact Go is the
developer's call):

```
{
    Name:          "spike-multi-turn",
    Kind:          "spike",
    Binary:        "spike-multi-turn",
    Args:          commonArgs,
    SuccessMarker: successSuccess,
    Timeout:       <chosen-duration>,
},
```

Add a 1-line comment IMMEDIATELY above `Timeout:` naming the empirical
basis: e.g. `// 5-turn spike, measured ~Ns median across 3 runs at claude X.Y.Z`.
This is the future operator's only signal for why the budget
is what it is. No long block comment.

**Do not** modify `defaultCheckTimeout` itself — that is the baseline
for every other spike, all of which fit the 60 s budget. Per-check
overrides are the right granularity (the existing `probeCheckTimeout`
and `snapshotDriftTimeout` constants establish the pattern; reuse the
style or inline the literal — both are acceptable. Inline literal +
comment is fine and consistent with what `runCheck` already accepts).

### Why not also trim prompts 4 and 5?

Considered and rejected. Reasoning:

- Trimming prompts 4-5 would return the binary to its sanctioned 3-turn
  shape and restore the 16–21 s baseline. That would let the default
  60 s timeout cover it.
- BUT — prompts 4 and 5 exist for a reason: prompt 5 is the only place
  in the e2e suite that exercises the parallel-tool shape claude can
  emit (see the spike's source comment at lines 86-91). Removing it
  would silently drop a piece of empirical coverage that someone
  deliberately added.
- AND — the spike's binary purpose was always "drive multi-turn
  interactive claude end-to-end." That contract is no weaker at 5
  turns than at 3. The 60 s budget is the artifact that needs to flex.

If Step 1 reveals that prompt 5 specifically is the load-bearing slow
turn AND the diagnosis indicates the time spent there is not gathering
new empirical data (e.g. it's just claude refusing the parallel pattern
in a verbose way every time), **Path B** is the appropriate exit, not
a sneaky prompt trim under cover of **Fix A**.

### Test

`make e2e` end-to-end. The spike must show `pass` for `spike-multi-turn`
in the report, and `claude-version-lock` must still show `pass`. The
report file (`./e2e-report.json`) is the contract — inspect it. Do not
infer pass from exit code alone.

No new unit tests. The change is a one-token timeout constant; the
existing checks (the spike binary's own validation) cover the substance.

## Path B — spike fails cleanly on a specific turn

Take this iff Step 1 outcome (B) reproduces — the spike exits non-zero
with `spike failed: turn N: ...` on stderr.

The failing turn N tells you everything:

- **Turn 1 fails** (`say hello`) — extremely unlikely; this is the
  baseline. If it does fail, the spike's foundation is broken, not
  this ticket's scope. **Escalate via Path C.**
- **Turn 2 fails** (Bash tool — `list the files in /tmp`) — claude's
  Bash tool semantics may have changed, or `bypassPermissions` no
  longer suppresses the modal. Investigate via the spike's stderr log
  (`turn=2 prompt-written`, then what?). If a permission modal text is
  visible in the rolling buffer at the wedge point and the spike
  doesn't handle it, the modal interpreter is out of this ticket's
  scope — **escalate via Path C** (file the underlying issue, do not
  ship a hack here).
- **Turn 3 fails** (slow thinking — Gauss sum) — likely the same
  spinner/thinking-detection edge the README documents (`finding #8`
  from spike #1, regex misses real spinner verbs). The current
  `gotEndTurn ∧ isIdle` predicate doesn't rely on the spinner, so this
  shouldn't wedge unless claude's `end_turn` semantics changed.
  Investigate; do not patch the predicate as part of this ticket.
- **Turn 4 or 5 fails** (Read tool — sequential and parallel) — most
  plausible candidates for a recent claude regression. Read the
  failing turn's `end-turn-detected` line (if any), the assistant text
  extracted, and the JSONL on disk (path is in the spike's
  `session-jsonl-opened` log line). If the failure is "no end_turn
  ever arrives within the per-turn budget," the turn is genuinely
  wedging; if the failure is "extractByMsgID returned empty," the
  content-block layout changed.

In every Path B branch, the right answer is **one of**:

1. **Remove prompts 4-5** iff the failing turn is 4 or 5 AND the
   failure isn't a one-line fix. The spike returns to its sanctioned
   3-turn shape; the experimental coverage from prompts 4-5 is filed
   as a follow-up. **This is the only place in this spec where
   trimming prompts is allowed**, and it requires Path B evidence.
2. **Escalate via Path C** for anything else (turn 1/2/3 failure, or
   turn 4/5 failure with a non-obvious root cause).

If option 1 is taken: edit `cmd/spike-multi-turn/main.go`, trim the
`prompts` slice to its first three entries, and update the source
comment above the slice to remove the prompt 4/5 description (lines
80-91 of the current file). Do NOT add a `t.Skip`-style mute; the
binary is a real e2e check, not a test with a known-fail switch.

## Path C — escalate

Take this iff Step 1 outcome (C) reproduces, OR Path B's investigation
surfaces a non-trivial regression (claude behavior change, modal
interpreter needed, state-detection broken).

Concrete action:

1. Do **not** commit any spec changes on this branch.
2. Open a new GitHub issue describing the diagnosis (which turn,
   observed symptoms, captured log, hypothesis). Label `bug,size:?`
   and leave sizing to PO.
3. Add a comment on #68 linking the new issue and explaining why the
   ticket-as-scoped cannot be resolved here. Add `needs-rework:po`
   to the label set. The original PO writeup says:

   > If the architect's diagnosis surfaces a multi-week refactor,
   > STOP and file the underlying refactor as a separate ticket rather
   > than ballooning this one.

   This is that escape hatch. Use it.

## Constraints (load-bearing, repeated from the ticket body)

- **No `t.Skip` / "known-fail" mutes.** Silent skips are how this
  fragility hid behind the version-lock short-circuit for weeks.
- **No `claude-version.lock` bump as part of this PR.** Lock rotation
  is a separate concern (lessons from #47). The lock's flag/value
  contract is satisfied today; leave it alone.
- **No incidental refactor of `cmd/spike-multi-turn/main.go`'s
  helpers.** The README's "copy-pasted from spike-one-turn" attribution
  comments are load-bearing for the post-spike library extraction
  (#58–#62). Touch only the `prompts` slice (Path B) or nothing in
  this file (Fix A path).
- **No changes to the runner's overall wall budget
  (`defaultWallBudget = 10 * time.Minute`).** A per-check timeout
  fits inside that ceiling at any value ≤ 180 s.

## Testing strategy

- **Pre-fix**: capture the standalone `time bin/spike-multi-turn`
  measurement (the diagnostic data). Include median wall time and
  per-turn breakdown (extract from the `turn=N end-turn-detected` log
  lines) in the PR description.
- **Post-fix**: run `make e2e` end-to-end. Both `claude-version-lock`
  and `spike-multi-turn` must be `pass` in `./e2e-report.json`. No
  adjacent check (spike-one-turn, snapshot-drift, etc.) may regress
  from its prior state.
- **No new automated tests.** Per the spike's contract, verification
  is by execution against real `claude`; mocks are for the post-spike
  library, not this ticket.

## Open questions for the developer

- **Which turn is the slow one?** The standalone run's stderr log
  (look for `turn=N end-turn-detected` lines and their timestamps)
  will reveal this. Mention it in the PR description even if the fix
  is just a timeout bump — it informs whoever revisits the budget
  later.
- **Has the median wall time grown since the 3-turn baseline of
  16-21 s?** If a 5-turn run is taking 80+ s, the per-turn time has
  also grown (the 3-turn baseline implied ~6 s/turn average). Worth
  noting; not blocking.
- **Does prompt 5 ever get the parallel-tool shape it was probing
  for?** If empirically claude has been emitting serial tool_use for
  this prompt across recent runs, the prompt's stated empirical
  purpose has decayed. Filing a follow-up to either reword the prompt
  or trim it is out of scope for this ticket but worth flagging in
  the PR description.

## Files the developer will touch

Maximal footprint, depending on which path the diagnosis selects:

- **Fix A only:** `cmd/e2e-runner/main.go` (1 field added, 1 comment line).
- **Path B option 1:** `cmd/spike-multi-turn/main.go` (trim `prompts`
  slice to 3 entries, update the prompt-list source comment).
- **Path C:** no source edits on this branch.

The PR per AC must explicitly name the diagnosed cause. The diagnosis
goes in the PR description, not in source comments — source comments
should describe the as-changed behavior, not the bug they were chosen
to address.

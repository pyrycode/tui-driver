# Spec: `spike-ask-user` e2e check — diagnose 11.6 s fail-fast and apply targeted fix

**Ticket:** [#71](https://github.com/pyrycode/tui-driver/issues/71)
**Size:** S
**Status:** ready for development
**Posture:** diagnosis-then-fix. The ticket's hypothesis ranking is a
starting hypothesis, not a verdict. The architect cannot run binaries;
the developer's **first turn is reproduction + stderr capture**, not a
code edit. The fix shape branches on what the captured stderr says.
Every branch in this spec is bounded — single binary at risk
(`cmd/spike-ask-user/main.go`), optional small edit to
`cmd/e2e-runner/main.go` only in the removal branch.

## Files to read first

Load these before touching anything. They cover the spike, the runner's
view of it, the sibling spikes whose error paths look identical, and
the library primitives the spike composes.

- `cmd/spike-ask-user/main.go` (full file, 426 lines) — the binary under
  test. Pay particular attention to:
  - lines 55-76 — timeout constants (`sessionFileWait = 10 s`,
    `askUserQuestionLimit = 60 s`, `postAnswerLimit = 60 s`,
    `ptyQuietLimit = 120 s`). These drive the fail-fast budget; the
    ticket's 11.6 s observation is the strongest signal of WHICH
    timeout fired (see § Diagnostic flow).
  - lines 166-189 — wait-idle + trust-folder handling.
  - lines 191-203 — prompt write + `openSessionJSONL`.
  - lines 222-232 — modal-detection loop.
  - lines 282-298 — post-answer end_turn loop.
  - lines 324-335 — `hasAskUserModal` literal-text predicate
    (`Enter to select` / `Entertoselect`). This is the keystone
    PTY-side anchor.
- `cmd/e2e-runner/main.go:38-42, 253-259, 493-562` — markers and the
  runCheck classifier. Status `fail` is set on any of: subprocess
  non-zero exit, deadline exceeded (which would show as `timeout` not
  `fail`), or stdout missing the `^OBSERVED` regex. The 11.6 s
  observation rules OUT the runner's per-check timeout (60 s default)
  and rules IN an internal spike error path that exits non-zero.
- `cmd/spike-permission/main.go:1180-1210` — the **same**
  `openSessionJSONL` pattern, in a check that **passes**. If the
  failure is in JSONL discovery for spike-ask-user, the divergence
  from spike-permission is the diagnostic seam.
- `cmd/spike-multiselect/main.go:1-100` — the **other** spike that uses
  `observedSuccess` (`^OBSERVED`) and passes. Confirms the success
  regex itself is not at fault.
- `cmd/spike-cancel/main.go:55, 830-845` — same `sessionFileWait =
  10 s` constant. Identical timeout shape; passes. Reinforces that any
  JSONL-discovery failure here is spike-ask-user-specific, not a
  library-side break.
- `pkg/tuidriver/state.go:9-13, 29-44` — `IdleGlyph`, `IsIdle`,
  `HasSpinner`. Relevant if the diagnosis points at wait-idle or
  watchdog (Branch C).
- `pkg/tuidriver/trust.go:18-26` — `HasTrustModal`. The runner passes
  `-trust-folder=accept`; the spike auto-answers with `1\r`. If
  claude 2.1.148 reworded the trust modal, this would surface in
  Branch C.
- `pkg/tuidriver/cwd.go` (and `cwd_darwin.go`) — `EncodeCwd`, used by
  `projectsDir()` at `cmd/spike-ask-user/main.go:354-364`. If the
  JSONL path is wrong (Branch A), this is the seam.
- `docs/knowledge/architecture/system-overview.md` § *Key signals* —
  background on `IsIdle`, spinner regex, `❯` glyph, JSONL layout.
- `docs/specs/architecture/57-encode-cwd-canonicalise.md` — prior
  encoded-cwd fix; useful only if Branch A leads to an `EncodeCwd`
  diagnosis.
- `claude-version.lock` (repo root) — pins `version=2.1.144` advisory,
  plus `flag=--session-id` etc. Runner host is on `2.1.148` per ticket.
  The lock's flag/value substrings are still satisfied by 2.1.148 (the
  check passes; only spike-ask-user fails). Do **not** rotate the lock
  as part of this PR.

## Context

`make e2e` invokes the e2e-runner, which runs `spike-ask-user` with
`-trust-folder=accept` and expects an `^OBSERVED` line on the spike's
stdout. On the dispatcher host as of 2026-05-21, the spike exits
**non-zero at ~11.6 s** without printing OBSERVED. Confirmed by QA on
both `feature/73` and `main` baseline `40690c7` — the break is
**older than PR #74** (post-approve predicate swap) and is not a
regression from any recent work. Runner host's claude is **2.1.148**;
the rendering / behaviour delta to investigate is `2.1.144 → 2.1.148`,
not just `2.1.144 → 2.1.146`.

The architect's reading of the 11.6 s evidence: this is **too short for
any modal-wait or post-answer watchdog** (both 60 s) and **too short
for any PTY-quiet / spinner-freeze watchdog** (120 s / 30 s). The
fail-fast budget that fits 11.6 s is:

- `sessionFileWait = 10 s` (JSONL discovery deadline at line 58), plus
  a few seconds of claude startup and idle-wait ≈ **11.6 s match**.
- A claude-side early exit (claude itself crashes or rejects the
  invocation), giving an EPIPE on `ptmx.Write(prompt + "\r")` — would
  exit much sooner than 11.6 s, so this is **less likely**.
- A bug in the prompt write or trust-handle paths returning an error
  string — also typically sub-second to a few seconds, **less likely**.

This shifts the architect's prior away from the ticket's hypothesis
ordering (which leads with "marker pattern mismatch"). The
`^OBSERVED` regex is shared with `spike-multiselect`, which passes —
the regex is not the fault. The most parsimonious 11.6 s explanation
is **JSONL-discovery timeout**, with **modal-wedge-on-rendering-change**
as the strong alternative if the 11.6 s reading is wrong.

The developer's first turn must be **reproduce locally and capture the
spike's stderr line at the failure point**. Every branch below
identifies the unique stderr signature that triggers it.

## Diagnostic flow

### Step 1 — Reproduce locally

```bash
make build-bin              # builds ./bin/* (no run)
./bin/e2e-runner -bin-dir ./bin -report ./e2e-report.json 2>&1 | tee /tmp/run.log
```

The runner mirrors each spike's stderr to its own stderr via
`io.MultiWriter` (`cmd/e2e-runner/main.go:523`). The spike's mirrored
log will show the timestamped `RecordTransition` markers and the final
`spike failed: <message>` line that drives the non-zero exit (see
`cmd/spike-ask-user/main.go:94-97`).

Alternative — run the spike directly (faster iteration, identical
behaviour):

```bash
go build -o ./bin/spike-ask-user ./cmd/spike-ask-user
TUIDRIVER_STRICT_MCP_CONFIG=1 ./bin/spike-ask-user -trust-folder=accept 2>&1 | tee /tmp/run.log
```

The runner sets `TUIDRIVER_STRICT_MCP_CONFIG=1` for every child (see
`cmd/e2e-runner/main.go:518`); export it manually for direct runs to
keep behaviour identical.

### Step 2 — Branch on the stderr signature

Find the `spike failed: <message>` line in the captured log (or the
last `time-X.XXX RecordTransition` entry before the spike exits).
Match the message text against the branches below. Each branch's
header is the **literal substring** of the error message; the spike's
error returns are at:

- `wait idle: ...` — line 167
- `claude shows the trust-folder dialog — pass -trust-folder accept` —
  line 175 (not possible under runner's `-trust-folder=accept`)
- `write trust-accept: ...` — line 178
- `wait idle post-trust: ...` — line 186
- `write prompt: ...` — line 193
- `open session jsonl: ... session JSONL did not appear at ... within ...` —
  line 201 / 374
- `watchdog: AskUserQuestion modal not seen within ...` — line 229
- `watchdog: end_turn not seen within ... after answer` — line 295
- (any `cancelCause` from the watchdog goroutine) — line 159

## Branch A — "session JSONL did not appear at ... within 10s"

**Highest-prior branch** given the 11.6 s reading.

Symptoms: the spike's last `RecordTransition` before failure is
`prompt-written`. The error message names the exact path the spike was
polling.

Diagnostic actions:

1. From the error message, copy the printed `path=...`. Check whether
   claude *did* write a JSONL anywhere with that session UUID:

   ```bash
   # SessionID is the basename minus .jsonl. The error message prints
   # path=~/.claude/projects/<encoded-cwd>/<sessionID>.jsonl
   find ~/.claude/projects -name "<sessionID>.jsonl" -mmin -2
   ```

2. If the find prints a path **different from** the polled path:
   `EncodeCwd` is wrong. The bug is in `pkg/tuidriver/cwd.go`, not in
   the spike. **STOP**. Do not bundle this fix with the spike-ask-user
   ticket — file a separate ticket for the encoded-cwd correction and
   route #71 back to Backlog with `needs-rework:po`. Per ticket
   constraint: "If the architect's diagnosis surfaces a multi-week
   refactor, STOP and file the underlying refactor as a separate
   ticket." A library-side path-encoding bug also fails the
   "spike-ask-user-specific" requirement — `spike-permission` and
   `spike-cancel` would also break, but they pass, so this would
   indicate the spike's cwd at e2e-runner-launch time differs from
   what `EncodeCwd` produces (e.g. spike-ask-user does some
   `os.Chdir` that the others don't — verify by grep before filing).

3. If the find prints **no file**: claude did not write a session
   JSONL within 10 s of receiving the prompt. Hypotheses:
   - claude 2.1.148 rejects the `AskUserQuestion`-triggering prompt
     immediately and does not start a turn. Look at the mirrored PTY
     output around the failure for an error banner or refusal text.
   - The model is too slow to produce its first JSONL line in 10 s
     under the runner's `TUIDRIVER_STRICT_MCP_CONFIG=1` regime. Try
     bumping `sessionFileWait` from 10 s to 20 s **as a diagnostic
     probe** (not a fix). If the spike then progresses past
     `RecordTransition("session-jsonl-opened")`, the underlying issue
     is something else (modal-wedge — see Branch B).

4. If the find prints the polled path but the file is empty or has no
   `assistant` events: same as (3) — investigate the prompt rejection
   path.

Fix shape (Branch A):

- **If the spike was polling the wrong path**: split into separate
  ticket (see step 2 above). Do not ship a spike-side workaround.
- **If `sessionFileWait` was the binding constraint**: bump to a
  conservatively larger value (e.g. 20 s) **with a comment** citing
  the empirical claude-2.1.148 first-line latency observation, and
  match the change in sibling spikes that share the 10 s constant.
  Apply with caution — bumping a watchdog is the kind of "Band-Aid
  fix" that can mask a real regression. Only do this if the
  diagnostic in step 3 conclusively shows claude DID write the JSONL
  within (say) 15 s.
- **If claude refuses the prompt outright**: the spike's prompt is
  too directive ("Use the AskUserQuestion tool to..."). The model may
  no longer treat `AskUserQuestion` as a built-in tool name visible
  to the prompt. See § Removal — if the prompt cannot be reliably
  steered to trigger an `AskUserQuestion` modal, the check is
  removable.

## Branch B — "watchdog: AskUserQuestion modal not seen within 60s"

Symptoms: total runtime ~62 s+ (not 11.6 s). The 11.6 s reading would
make this branch wrong. Include it only because the modal predicate
is the ticket's hypothesis #2.

Diagnostic actions:

1. The /tmp dump at line 246 is written *after* modal detection
   succeeds, so on this branch it does **not** exist. Add a one-time
   pre-fail dump in the modal-wait loop: right before the
   `return fmt.Errorf("watchdog: ...")` at line 229, write
   `rb.Snapshot()` to `/tmp/spike-ask-user-watchdog-bytes-*.bin`. Keep
   the diagnostic dump in the final fix — it's cheap insurance against
   future regressions of the same shape.
2. Strip ANSI + OSC from the dump (same recipe as line 249: `oscRe`
   then `StripANSI`). Inspect for either:
   - The hint-bar text in any form (`Enter to select`, `Entertoselect`,
     or a reworded variant from claude 2.1.148). If a different
     wording is present, update `hasAskUserModal` to match the new
     wording in addition to the existing two variants (keep both for
     backward-compat unless the variants are confirmed gone).
   - Absence of any modal at all — claude rendered a regular text
     response instead. Implies claude 2.1.148 is not honouring the
     `AskUserQuestion` tool-use directive for this prompt. See
     § Removal.

Fix shape (Branch B):

- Update `hasAskUserModal` (`cmd/spike-ask-user/main.go:324-335`) to
  include the new hint-bar variant. Keep the existing two anchors;
  add the new one as a third `bytes.Contains` clause. Document the
  source of the new variant in the comment block.
- **Do not** widen the predicate to a box-drawing or general-modal
  fallback. The spec for #13 explicitly notes box-drawing is
  false-positive at idle (claude's input box uses box-drawing chars).
- If no modal is present at all: § Removal.

## Branch C — "wait idle:" / "wait idle post-trust:" / "write prompt:" / watchdog from `tr.CheckWatchdog`

Symptoms: spike exits before reaching `prompt-written` transition.
Sibling spikes that use the same primitives (`spike-permission`,
`spike-cancel`, `spike-multi-turn`) all pass. A failure here that's
**unique to spike-ask-user** would be unusual — verify by running
`spike-permission` and `spike-cancel` standalone on the same host.
If they also fail in the same shape, the bug is library-side and
not in scope for #71; file separately and route #71 back to Backlog.

If the failure is genuinely spike-ask-user-specific (e.g. claude
crashes only when launched with this spike's exact arg set), the
fix shape depends on the diagnosis; add a stderr-side log line at
the offending site and re-run before committing to a fix.

## Branch D — "watchdog: end_turn not seen within 60s after answer"

Symptoms: total runtime ~62 s+ (not 11.6 s). Reach this branch only
if the 11.6 s reading is wrong and the actual failure surfaces here.

Diagnostic actions:

1. The /tmp dump from line 246 *does* exist on this branch. Inspect
   it to confirm the modal was rendered, the answer keystroke was
   sent (`tr.RecordTransition("answer-sent")` present in stderr),
   and what claude rendered post-answer.
2. The default answer is `1\r` (per `-answer 1`). If claude 2.1.148
   changed the AskUser modal's input acceptance (e.g. no longer
   accepts digits, requires arrow-key + Enter, or has changed the
   option-numbering convention), the keystroke lands on nothing.

Fix shape (Branch D):

- Try alternate answer shapes by running the spike with
  `-answer dismiss` (ESC) and then `-answer 1` (digit). If ESC
  produces an `end_turn` but digit does not, the modal's keystroke
  contract changed. Update `answerBytes` at lines 266-273 accordingly.
- If `end_turn` never arrives regardless of keystroke, see § Removal.

## Branch E — anything else

If the stderr signature doesn't match A–D, the architect's prior is
wrong. **Do not fix blind**. Add a targeted stderr log line at the
suspected fault site, re-run, and post the new evidence as a comment
on the ticket. If the fix shape is not clear after a second diagnostic
pass, route the ticket back to PO with `needs-rework:po` and a
summary of what was observed.

## Removal (last-resort branch)

The ticket explicitly authorises this: *"if the check is genuinely
unfixable, remove it entirely and explain why in the PR."* Justified
when:

- The diagnosis shows the AskUserQuestion tool is no longer reliably
  triggerable in claude 2.1.148 by any prompt the spike could send, **or**
- The modal's rendering or input contract changed so radically that
  matching it requires a fundamentally different design (terminal
  emulation, JSONL-only flow, etc.) that breaks the spike's
  "one-binary, no public API" charter.

Removal is **not** justified by "the fix is hard" or "the rendering
shifted slightly." Threshold is: fix would exceed S size or require
library-side changes.

Removal steps (if taken):

1. Delete the `spike-ask-user` Check entry at
   `cmd/e2e-runner/main.go:253-259`.
2. Delete `cmd/spike-ask-user/` (the entire directory).
3. Remove `spike-ask-user` from the `SPIKES` list in `Makefile`.
4. Search `docs/` for `spike-ask-user` references and update or remove
   them (`grep -rn 'spike-ask-user' docs/`). Architects do not edit
   `docs/PROJECT-MEMORY.md`, `docs/lessons.md`, or
   `docs/knowledge/INDEX.md` — flag any required edits to those for
   the documentation phase.
5. PR description names the diagnostic evidence that ruled out a
   non-removal fix.

## Constraints

- **No `t.Skip`, no "known-fail" muting.** Hard rule from the ticket.
  If the check can't be fixed, remove it; do not silence it.
- **Do not bundle with `claude-version.lock` rotation.** Lock policy
  is its own concern (#47, #64).
- **Do not touch the library JSONL API refactor (#58–#62).** This
  ticket runs against the current spike binary shape.
- **Do not introduce changes to sibling spikes** unless Branch A's
  `sessionFileWait` bump applies and you choose to mirror it for
  consistency. Even then, keep the change additive (no behavioural
  drift in sibling spikes' pass paths).
- **Keep the diagnostic dump in the final fix** (Branch B step 1):
  the pre-watchdog snapshot is cheap, useful for the next
  rendering-change regression, and harmless on the pass path.
- **No box-drawing or other general-modal fallback in
  `hasAskUserModal`.** Literal-text anchors only; see #13 for the
  false-positive-at-idle rationale.

## Acceptance criteria (restated, with diagnostic anchors)

- [ ] `make e2e` runs end-to-end with `spike-ask-user` reporting
  `pass` in `e2e-report.json`.
- [ ] The PR description names the diagnosed cause in 1–2 sentences
  (what was broken, why). The stderr signature from § Diagnostic
  flow Step 2 is the source.
- [ ] No `t.Skip`, no "known-fail" patterns introduced; if removed,
  the rationale is in the PR.
- [ ] `claude-version-lock` continues to pass cleanly (it should be
  untouched by this PR).
- [ ] No new failures introduced in adjacent checks (run the full
  `make e2e` at least once on the final diff before opening the PR).

## Testing strategy

- The check itself is the test. There is no unit test layer for
  spike binaries — they are throwaway by design (see
  `docs/knowledge/architecture/system-overview.md` § *Layout*).
- Reproduce → fix → re-run `make e2e`. If the diagnostic adds a
  one-shot stderr dump (Branch B), confirm the dump fires only on
  the failure path (idempotent on pass).
- Standalone re-run: `./bin/spike-ask-user -trust-folder=accept`.
  Should print `OBSERVED:` and exit 0 in well under 60 s.

## Open questions

- **What is the actual stderr message?** This is the gating fact for
  every branch above. The architect could not capture it; the
  developer's first action does.
- **Does the same failure reproduce on a host with claude 2.1.144?**
  Out of scope to test (requires holding a stale claude install), but
  useful prior — if a 2.1.144 host passes, the version drift is
  conclusively the cause and the fix targets 2.1.148's new behaviour
  specifically.
- **Is there a `TUIDRIVER_STRICT_MCP_CONFIG=0` reproduction?** Worth
  one diagnostic run — if the failure disappears without strict-mcp,
  the bug is in the strict-mcp interaction with `AskUserQuestion`,
  which is a different shape of fix.

## Out of scope

(restated from ticket)

- The library JSONL API refactor (#58–#62).
- Dispatcher pipeline contract changes.
- Claude version pinning policy.
- The other 4 currently-failing e2e checks (each has its own ticket;
  see sibling branches `feature/68`, `feature/69`).

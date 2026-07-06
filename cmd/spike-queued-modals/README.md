# spike-queued-modals

Live-claude observation spike that determines claude's number-select
**modal commit semantics under two queued modals**: does the digit alone
commit a permission modal, or is the trailing `\r` the commit byte?

Drives a single `claude` turn engineered to queue two permission modals
(B behind A) via two parallel tool calls, answers modal A through the
shipped sealed `Answer("1")` path (the exact `1\r` keystroke), then
observes whether the trailing `\r` leaked onto modal B and auto-accepted
its highlighted default — a grant nobody issued.

See [ticket #199](https://github.com/pyrycode/tui-driver/issues/199) and
`docs/specs/architecture/199-*.md`. Split from #165; **blocks the fix
[#200](https://github.com/pyrycode/tui-driver/issues/200)** — this spike's
recorded finding is the sole input that scopes it.

## Status

**Empirical result: TBD — populate from a `make e2e` run.**

The developer works in the claude-free `make check` gate and cannot run
the live experiment or fabricate its result. What ships:

1. the harness (`main.go`) that *will* record the finding when run live;
2. the deterministic classifier (`classify`) + its table test
   (`main_test.go`), which run under `make check`;
3. this README skeleton with the decision table below and this `TBD`
   placeholder.

The actual answer is produced when an operator runs `make e2e`
(post-merge). The spike prints `OBSERVED: finding=<finding> …` to stdout;
transcribe that finding and the raw observation fields into the
**Empirical log** section below, and it becomes the input that scopes #200.
Do **not** invent a finding — an unobserved "safe"/"dangerous" claim is
worse than an honest TBD.

## Why this spike exists

`Answer("1")` writes the two bytes `1\r` in one atomic PTY write
(`pkg/tuidriver/keys.go`). The open safety question, split from #165:
**when two permission modals queue (B behind A), does the trailing `\r`
reach B and auto-accept its highlighted default?**

- **If the digit alone commits modal A**, the trailing `\r` is a
  redundant, dangerous byte that can land on the freshly-surfaced modal B
  and accept its default → a real wrong-grant bug #200 must close.
- **If claude requires digit-then-`\r`**, `1\r` is atomic-and-safe: A
  consumes its own `\r`, nothing reaches B → #200 collapses to a
  regression test.

Spike [#13](../../docs/knowledge/codebase/13.md) (`cmd/spike-permission`)
established `1\r` and that a bare `\r` alone commits the highlighted
default, but **deliberately never drove two queued modals** ("never
trigger the same tool twice and expect both modals to fire"). This spike
closes that gap.

## What it does

Single session, single probe:

1. Spawn `claude --session-id <id>` (permission mode NOT bypassed, so
   modals fire). Start the watchdog goroutine.
2. Wait for idle; handle the trust-folder modal per `-trust-folder`.
3. Tail the session JSONL from offset 0 (the authoritative queuing
   disambiguator — permission modals have zero JSONL footprint).
4. Type a prompt engineered to induce **two distinct parallel tool calls**
   (Bash + Read) in one assistant message.
5. Wait for permission **modal A** (bounded, 60 s). Never appearing is a
   hard failure (exit 1) — the setup could not trigger even one modal.
   Snapshot A (0600); record A's signature (Title + option labels).
6. Answer A via **`Answer("1")`** — the exact sealed `1\r` under test.
7. Observe **B's fate** while draining JSONL to `end_turn`:
   - a permission modal with a *different* signature than A appears → **B
     survived** A's `\r`; snapshot B (0600) and answer it via `AnswerModal`
     (here the class-level confirm is sound — no successor expected);
   - no modal, the turn proceeds → **B not visible**.
8. Accumulate from JSONL: `parallelToolUse` (any assistant message —
   **grouped by `msg_id`** — carrying ≥2 `tool_use` blocks; claude serialises
   one message as N lines, one content block each, sharing a `msg_id`, so the
   count is summed per `msg_id`, not per JSONL line), `distinctTools`,
   `toolsExecuted` (`tool_result` blocks in `user` entries).
9. Feed the observation into the pure classifier and print `OBSERVED:`.

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `-trust-folder` | `fail` | `fail` errors if the trust dialog appears; `accept` sends `1\r` to auto-trust. e2e-runner passes `accept`. |
| `-prompt` | two-tool Bash+Read prompt | Override the trigger prompt. |

## Decision table (the classifier)

The live observation is stochastic; its interpretation is deterministic
and unit-tested (`classify` in `main.go`, `TestClassify` in
`main_test.go`). Each row is a test case.

| Condition | Finding | Meaning for #200 |
|---|---|---|
| `!parallelToolUse` | `inconclusive-no-queuing` | Modals did not queue (single tool, or sequential across messages). Commit semantics under queuing **undetermined** — NOT "safe". |
| `parallelToolUse && modalBObserved` | `safe-atomic-digit-cr` | B survived A's `1\r` → commit is **digit-then-`\r`**; A consumed its own `\r`. #200 → regression test. |
| `parallelToolUse && !modalBObserved && toolsExecuted > modalsAnswered` | `dangerous-digit-commits-cr-leaks` | A tool executed that the spike never approved → its default was auto-accepted by the leaked `\r`. **Digit alone commits.** #200 → remove/reorder the trailing `\r`. |
| `parallelToolUse && !modalBObserved && toolsExecuted <= modalsAnswered` | `inconclusive-no-queuing` | Queued but the outcome is ambiguous (B rejected, turn cancelled). Record honestly. |

**Guard rail:** a single-modal run can never be reported as "trailing `\r`
is safe" — it cannot distinguish the safe case from a failed setup. Only
`parallelToolUse && modalBObserved` yields `safe`.

## Wiring

- `Makefile` `SPIKES` — the pattern rule auto-builds `./cmd/spike-queued-modals`.
- `cmd/e2e-runner` `buildChecks()` — `SuccessMarker: observedSuccess`
  (`^OBSERVED`), 120 s timeout (≈3–6× the expected ~20–40 s wall; tune
  after the first live run). Inconclusive is a legitimate recorded
  observation — the check passes (exit 0) and this README records "could
  not reproduce two queued modals".

## Empirical log

> Populate from a `make e2e` run. Paste the `OBSERVED:` line and the
> relevant `make e2e` stderr transitions, then state the finding and its
> consequence for #200.

- **Finding:** _TBD_
- **`OBSERVED:` line:** _TBD_
- **Two modals queued?** _TBD_ (`parallel_tool_use`, `modal_b_observed`)
- **Prompt reproduced parallel tool use?** _TBD_ (if sequential, tune the
  prompt — a follow-up, not a faked result)
- **Consequence for #200:** _TBD_

## Open questions (resolve on first live run)

- Does the default prompt reliably induce **parallel** (not sequential)
  tool use on the pinned claude version? If sequential, the run is honestly
  `inconclusive` and the prompt is tuned as a follow-up.
- Is option 1 always the highlighted default on modal B? #13 observed
  `❯1.Yes`. Even if a future claude renders a different default, the
  `toolsExecuted > modalsAnswered` signal catches the auto-grant regardless
  of *which* option the `\r` accepted.
- e2e Check timeout (120 s): an estimate; tune from the first live run's
  wall time (same posture as spike-cancel #69 / spike-multi-turn #111).

# #199 — Spike: number-select modal commit semantics under two queued modals

Issue: https://github.com/pyrycode/tui-driver/issues/199 · Split from #165 · Blocks the fix #200.

## Files to read first

- `cmd/spike-ask-user/main.go` (whole file, ~323 LOC) — **the structural template.** This is the modern, library-based observation-spike shape: `Spawn` → `RunWatchdog` goroutine → `WaitUntil(IsIdle)` → trust-folder handling → `TypePrompt` → `TailJSONL` event loop → PTY-side modal detection → snapshot → answer keystroke → wait for `IsEndTurn` → `OBSERVED:` line. Copy this scaffold; swap the AskUserQuestion specifics for the two-queued-permission-modal flow. Note its `ptyQuietLimit = 120s` and finding #25 comment — the modal-active quiet state must not trip the watchdog (§ Concurrency).
- `cmd/spike-permission/main.go:1-60` + its `writeTempSnapshot` helper — the **0600-mode raw-byte snapshot** helper (`os.OpenFile(..., O_WRONLY|O_CREATE|O_TRUNC, 0600)`). Use this mode, not `os.WriteFile(..., 0644)` (spike-ask-user's 0644 is the older shape; the Technical Notes call for spike-permission's security-reviewed 0600).
- `pkg/tuidriver/keys.go:40-54` — `Answer(choice string)` sends `choice + "\r"` through the sealed `writeRaw`; `AcceptTrust()` sends `"1\r"`. `Answer("1")` = the exact 2-byte `1\r` keystroke under test. `SendKeys` (lines 67-75) is the raw escape hatch — **do NOT use it to answer modal A** (Technical Notes: sealed path only).
- `pkg/tuidriver/answer.go` (whole file) — `AnswerModal(ctx, AnswerModalOpts{Class, Choice})` and `modalDismissed`. **Critical subtlety:** `modalDismissed` returns "dismissed" only when `DetectModalClass != class`. When answering modal A surfaces a *same-class* modal B, `DetectModalClass` still reports `permission`, so `AnswerModal` returns its **"still present" error even though A committed** (documented in #147 lessons — "a still-present same-class modal is a deliberate conservative false-negative"). This is why the spike answers A with `Answer("1")` and does its **own** B-vs-A observation, rather than trusting `AnswerModal`'s confounded confirm.
- `pkg/tuidriver/modal.go:119-146` — `DetectModalClass`; `ModalClassPermission` is anchored on `Doyouwanttoproceed` / `Do you want to proceed`. This is the PTY-side present/absent signal for both A and B.
- `pkg/tuidriver/permission.go:11-88` — `ParseModalContent(snap) *ModalContent{Class, Title, Prompt, Options, Default}`. Use `Prompt` (or joined `Options` labels) to tell modal B apart from modal A on screen (their command/path text differs: Bash vs Read). Diagnostic comparison only — fine for a spike.
- `pkg/tuidriver/jsonl.go:108-133,171,297` — `JSONLEntry{Type, Message *EntryMessage, Raw, RawLine}`, `EntryMessage.Content []ContentBlock`, `ContentBlock{Type, Raw}`, `TailJSONL`, `IsEndTurn`. `entry.Message.Content` is exported — iterate it to **count `tool_use` blocks in a single assistant message** (the parallel-queuing disambiguator) and to count `tool_result` blocks in `user` entries (tools-executed). `ContentBlock.Raw["name"]` gives the tool name.
- `pkg/tuidriver/tool_use.go:43` — `ParseToolUse(rawLine) *ToolUse{ID, Name, Input}` extracts the *first* tool_use per line. Useful for the single-tool case; for the ≥2-blocks-per-message count, walk `entry.Message.Content` directly.
- `cmd/e2e-runner/main.go:39-42,205-316` — `observedSuccess = ^OBSERVED` regex; `buildChecks()` is where the new `Check` entry goes (mirror `spike-ask-user` / `spike-multiselect`, both `SuccessMarker: observedSuccess`). Note the `-trust-folder=accept` in `commonArgs`.
- `Makefile:11` — the `SPIKES :=` list; the `$(BIN_DIR)/%` pattern rule auto-builds any `./cmd/$*`, so adding the name to `SPIKES` is the only Makefile edit needed.
- `docs/knowledge/codebase/13.md` findings #1-#2 and the pattern "never trigger the same tool twice and expect both modals to fire" — approve = `1\r`; **bare `\r` alone commits the highlighted default** (option 1 = Yes); permission modals have **zero JSONL footprint** until answered. These priors drive the experiment design.
- `docs/knowledge/codebase/147.md` — `AnswerModal` sealed-answer contract and the "two consecutive modals" follow-up this spike finally exercises live.

## Context

`Answer` / `AnswerModal` commit a modal with the chosen digit + carriage return in one atomic write (`Answer("1")` → `1\r`). The open safety question, split from #165 and blocking the fix #200: **when two permission modals queue (B behind A), does the trailing `\r` reach B and auto-accept its highlighted default (a grant nobody issued)?** The answer depends on one empirically-undetermined fact:

- **If the digit alone commits modal A**, the trailing `\r` is a redundant, dangerous byte that can land on the freshly-surfaced modal B and accept its default → a real wrong-grant bug #200 must close.
- **If claude requires digit-then-`\r`**, `1\r` is atomic-and-safe — A consumes its own `\r`, nothing reaches B → #200 collapses to a regression test.

Spike #13 established `1\r` and that bare `\r` commits the default, but **deliberately never drove two queued modals**. This spike closes that gap. It is a **live-claude observation spike**: the answer only surfaces on the operator/CI `make e2e` path, never in the developer's claude-free `make check` gate — the reason it is its own ticket ahead of the fix.

**A second untested assumption rides underneath (#13 only proved the negative):** that two *distinct* tools in one turn actually *queue* two modals. The spike must confirm the queued-B precondition empirically before drawing any commit-semantics conclusion, and must be prepared to record an **inconclusive** ("could not reproduce two queued modals") outcome — which must **never** be reported as "trailing `\r` is safe."

## Design

### New binary: `cmd/spike-queued-modals/`

One new file `main.go` (production, ~330-400 LOC, modeled line-for-line on `spike-ask-user`), one `main_test.go` (the pure classifier's table test), one `README.md` (empirical-log skeleton in the shape of `cmd/spike-permission/README.md`'s findings section). **No new `pkg/tuidriver/` surface** — the spike composes only shipped primitives.

### Experiment flow (single session, single probe)

1. Resolve `sessionID` + `jsonlPath` (`SessionJSONLPath`); spawn `claude --session-id <id>` via `Spawn` + `EnsureClaudeEnv` (drop `bypassPermissions` so modals fire — as spike-permission does). Start the `RunWatchdog` goroutine.
2. `WaitUntil(IsIdle)`; handle the trust-folder modal per the `-trust-folder` flag (`accept` → `AcceptTrust`, `fail` → error), mirroring spike-ask-user exactly.
3. `WaitForSessionJSONL` + `TailJSONL(ctx, jsonlPath, 0)` — open the event stream **before** delivering the prompt (JSONL is the authoritative disambiguator; see below).
4. `ClearInputLine` + `TypePrompt(prompt)` with a prompt engineered to induce **two distinct, independent tool calls in one assistant turn** (default below). Char-by-char delivery per the spike convention.
5. Wait for **modal A**: poll `DetectModalClass(Snapshot()) == ModalClassPermission` (bounded, e.g. 60s). If it never appears → **hard failure** (exit 1): the harness could not trigger even one permission modal. Snapshot A's raw bytes (0600). `ParseModalContent` A → capture `A.Prompt` (+ options) for later B-vs-A discrimination.
6. **Answer A via `Answer("1")`** — the exact sealed `1\r` keystroke under test (not `SendKeys`, not `AnswerModal`; see the answer.go subtlety above). Record `modalsAnswered = 1`.
7. **Observe B's fate.** After a settle window, re-snapshot and classify the screen:
   - `DetectModalClass == ModalClassPermission` **and** `ParseModalContent.Prompt != A.Prompt` → **modal B present and distinct** (`modalBObserved = true`). Snapshot B (0600). Then answer B via **`AnswerModal(ctx, {Class: ModalClassPermission, Choice: 1})`** — here the confirm is meaningful (no successor expected), so this second answer exercises the production `AnswerModal` surface where its class-level dismissal check is sound. Record `modalsAnswered = 2`.
   - otherwise (idle / turn proceeding) → `modalBObserved = false`; B is not visible.
8. Keep draining `TailJSONL` until `IsEndTurn` (bounded). While draining, accumulate from `entry.Message.Content`:
   - `parallelToolUse` — true if **any single assistant message** carried ≥2 `tool_use` blocks (genuine queuing, not sequential).
   - `distinctToolCount` — number of distinct `tool_use` names seen.
   - `toolsExecuted` — count of `tool_result` blocks across `user` entries.
9. Feed the accumulated observation into the pure classifier (below), print the `OBSERVED:` line carrying the finding, and record the finding in the README empirical log.

### Pure classifier — the testable heart

Factor the epistemic decision into one pure function so the stochastic live observation feeds a **deterministic, unit-tested** classification (belt-and-suspenders: the observation is stochastic; its interpretation is not). Contract sketch (≤20 lines — the developer writes the bodies + struct docs):

```go
type observation struct {
    modalBObserved    bool // a distinct permission modal was present after A's 1\r
    parallelToolUse   bool // some assistant message carried >=2 tool_use blocks
    modalsAnswered    int  // modals the spike explicitly answered (1 or 2)
    toolsExecuted     int  // tool_result blocks seen in JSONL
}

type finding string // "safe-atomic-digit-cr" | "dangerous-digit-commits-cr-leaks" | "inconclusive-no-queuing"

func classify(o observation) finding // pure; the table test's subject
```

Classification rules (the README's decision table; each is a test case):

| Condition | Finding | Meaning for #200 |
|---|---|---|
| `!parallelToolUse` | `inconclusive` | Modals did not queue (single tool, or sequential across messages). Commit semantics under queuing **undetermined** — NOT "safe". |
| `parallelToolUse && modalBObserved` | `safe-atomic-digit-cr` | B survived A's `1\r` → commit is **digit-then-`\r`**; A consumed its own `\r`; trailing `\r` did not reach B. #200 → regression test. |
| `parallelToolUse && !modalBObserved && toolsExecuted > modalsAnswered` | `dangerous-digit-commits-cr-leaks` | B was auto-consumed: a tool executed that the spike never explicitly approved → its default was auto-accepted by the leaked `\r`. **Digit alone commits.** #200 → remove/reorder the trailing `\r`. |
| `parallelToolUse && !modalBObserved && toolsExecuted <= modalsAnswered` | `inconclusive` | Queued but the outcome is ambiguous (e.g. B rejected, turn cancelled). Record honestly. |

### Default prompt

Engineered to bias claude toward two independent, permission-gated tool calls in one message (both tools are known permission-modal triggers per #13 — Bash from Probes 1/2, Read from Probe 3):

> "In a single step, do these two independent things at once: (1) use the Bash tool to run the shell command `echo queued-modal-probe`, and (2) use the Read tool to read the file `/etc/hostname`. Issue both tool calls together, not one after the other."

Overridable via `-prompt`. Reproduction of parallel queuing is itself an observed variable — if claude issues the calls sequentially, `parallelToolUse` is false and the run is honestly `inconclusive`; the README notes prompt-tuning as a follow-up rather than the spike faking a result.

### Flags

`-trust-folder` (`fail`|`accept`, default `fail`; e2e-runner passes `accept`), `-prompt` (override). Match spike-ask-user's flag surface and validation.

### Wiring (matches existing spike convention exactly)

- `Makefile:11` — append `spike-queued-modals` to `SPIKES`. (Pattern rule auto-builds it; no other Makefile edit.)
- `cmd/e2e-runner/main.go` `buildChecks()` — add a `Check{Name: "spike-queued-modals", Kind: "spike", Binary: "spike-queued-modals", Args: commonArgs, SuccessMarker: observedSuccess, Timeout: 120 * time.Second}`. `observedSuccess` (`^OBSERVED`) is the observation-spike marker (as spike-ask-user / spike-multiselect). The 120s timeout gives ~3-6× margin over the expected ~20-40s wall (two approvals + two fast tool executions + settle windows); the spike's own 120s PTY-quiet watchdog bounds a true hang. Tunable after the first live run.

## Concurrency model

Identical to spike-ask-user: main goroutine drives the linear flow; one `RunWatchdog` goroutine watches liveness; `TailJSONL` owns its own reader goroutine feeding the event channel; `context.WithCancelCause` + a `sync.WaitGroup` coordinate shutdown; the `defer session.Close()` → SIGTERM → `ShutdownGrace` → SIGKILL chain reaps claude. **Watchdog caveat:** a permission modal is a PTY-quiet state (claude waits for input), so set `ptyQuietLimit = 120s` (spike-ask-user finding #25) — the modal-active quiet window must not trip the PTY-heartbeat arm before the spike answers. `spinnerFreezeLimit` is a retained no-op (CLAUDE.md / #164); set it to any value, it is not armed.

## Error handling

- **Modal A never appears** (bounded wait elapses) → exit 1. The two-tool prompt failed to trigger even one permission modal; the harness assumptions are broken, not an inconclusive finding.
- **`AnswerModal` returns "still present" for B** → expected and ignored; the spike's own snapshot observation (step 7) is authoritative, not `AnswerModal`'s same-class-confounded confirm.
- **`end_turn` never arrives** (bounded wait) → exit 1 (harness/hang), same as every other spike.
- **Inconclusive finding** (`!parallelToolUse`, or queued-but-ambiguous) → **exit 0**, print `OBSERVED: inconclusive ...`. Inconclusive is a legitimate recorded observation, not a harness failure; the check passes and the README records "could not reproduce two queued modals."
- Spawn / trust / prompt-delivery / JSONL-open failures → exit 1 with a wrapped error, as spike-ask-user.

## Testing strategy

- **`cmd/spike-queued-modals/main_test.go`** — table test over the pure `classify(observation)` function, one row per decision-table branch above (safe, dangerous, both inconclusive cases, plus the `toolsExecuted == modalsAnswered` boundary). This is the AC's "its `main_test.go` assertion": it gives `make check` real teeth on the only logic that *can* run claude-free — the classification — while the live observation that feeds it runs on `make e2e`. No live PTY, no claude.
- **`cmd/e2e-runner/main_test.go` needs no change.** Its tests cover pure helpers (`parseSnapshotResults`, `parseClaudeVersion`, `parseLockFile`, `evaluateClaudeVersionLock`), not the `buildChecks()` list — appending a `Check` entry is test-neutral. Stated explicitly so the developer does not hunt for a per-spike assertion that the existing suite does not carry.
- **`make check`** (`go vet ./...` + `go test -race ./...`) compiles the new spike and runs its classifier test — the developer's full gate.
- **`make e2e`** (operator/CI, live claude) runs the actual observation and records the empirical finding.

### Where the empirical finding is recorded (process note — read this)

The developer works in the **claude-free `make check` gate** and therefore **cannot run the live experiment or fabricate its result.** The developer ships:
1. the harness (`main.go`) that *will* record the finding when run;
2. the classifier + its test (deterministic, runs under `make check`);
3. a **`README.md` skeleton** carrying the decision table above and a clearly-marked `Empirical result: TBD — populate from a make e2e run` placeholder, in the shape of `cmd/spike-permission/README.md`'s findings section.

The **actual** commit-semantics answer is produced when an operator runs `make e2e` (post-merge); the spike prints `OBSERVED: <finding>` to stdout so the operator transcribes it into the README, and that recorded finding is the sole input that scopes #200. The developer must **not** invent findings — an unobserved "safe"/"dangerous" claim is worse than an honest TBD. (Consistent with the #165 split rationale: an AC that needs `make e2e` cannot resolve in the `make check` gate.)

## Open questions

- **Does the default prompt reliably induce parallel (not sequential) tool use on the pinned claude version?** Unknown until the first `make e2e`. If sequential, the run is honestly `inconclusive` and the prompt is tuned as a follow-up (the classifier already distinguishes the two via `parallelToolUse`). Not a blocker — inconclusive is a valid outcome.
- **Is option 1 always the highlighted default on modal B?** #13 observed `❯1.Yes` for the permission class; the `dangerous` detection assumes B's leaked-`\r` default is an *approve*. If a future claude renders a different default, the `toolsExecuted > modalsAnswered` signal still catches the auto-grant regardless of *which* option the `\r` accepted — so the detection is robust to that drift.
- **e2e Check timeout (120s):** an estimate; tune from the first live run's observed wall time (same posture as spike-cancel #69 / spike-multi-turn #111 timeout tuning).

# Spec #263 — Re-anchor `probe-cwd-encoding` to a settled readiness signal

**Ticket:** [#263](https://github.com/pyrycode/tui-driver/issues/263) — spike (child A of the spike-from-fix split; #264 is the fix half, blocked-by this).
**Size:** S (developer deliverable is small; the reliability observation spans operator-run live runs).
**Security-sensitive:** No (`security-sensitive` label absent; this is a diagnostic spike binary that changes *when* it types a throwaway prompt — it touches no modal-classification or grant/answer path).

---

## Files to read first

- `cmd/probe-cwd-encoding/main.go:146-216` — the spawn → `WaitUntil(IsIdle)` → `if HasTrustModal` → `SendKeys("hi\r")` sequence. **This is the re-anchor site.** Extract the exact ordering; the fix reshapes lines ~163-197.
- `cmd/probe-cwd-encoding/main.go:218-249` — `discoverProjectsDir`: the session-id glob-poll bounded by `sessionFileWait = 30s`. Unchanged by this ticket, but it is the timeout that currently reddens; understand it so you don't move it.
- `cmd/probe-cwd-encoding/main.go:49-76` — the existing constants (`sessionFileWait`, `wallTimeout`, `promptText`, `nonASCIILeaf`). New constants go here.
- `cmd/probe-first-prompt-hang/main.go:150-199` — the **structural twin**. Note line 160 `session.LastAppendAt()` used as a "PTY has produced bytes" signal, and the identical `WaitUntil(IsIdle)` → trust → `SendKeys` sequence it *characterises but does not fix*. There is no already-proven readiness signal to copy from here.
- `pkg/tuidriver/wait.go:8-43` — `WaitUntil(ctx, predicate)` + `DefaultPollInterval = 50ms`. The re-anchor is expressed as new predicates passed to this, not a new loop.
- `pkg/tuidriver/ready.go:81-120` — `WaitReady` / `Readiness` contract (#173). **Read to understand why it is *not* the fix:** its idle gate is bare `IsIdle` (line 95), so it fires on the same false idle. We reuse its *primitives*, not the call.
- `pkg/tuidriver/session.go:404,413` — `Snapshot() []byte` and `LastAppendAt() time.Time`. `LastAppendAt` is the quiescence clock.
- `pkg/tuidriver/trust.go:71` — `HasTrustModal(snap []byte) bool`. The semantic anchor for phase 1.
- `pkg/tuidriver/keys.go:45-51` — `AcceptTrust()` (sends `1\r`). Unchanged.
- `pkg/tuidriver/state.go:152-166` — `IsIdle` / `isIdleGrid`: idle = `❯` in the status region and not busy. This is the predicate that mis-fires early.
- `cmd/e2e-runner/main.go:346-385` — the `probe-cwd-encoding` check def + the `#251` de-gate rationale comment. **Read-only for this ticket.** Do NOT drop `NonGating` or edit this comment here — that is #264.

---

## Context

`probe-cwd-encoding` is an observation rig (#206): it spawns `claude` in a non-ASCII temp cwd, types one throwaway `"hi\r"` purely to force claude to write its deferred session JSONL, then discovers the projects-dir name by globbing `~/.claude/projects/*/<session-id>.jsonl`. It ships **green on any observation** (`^OBSERVED` marker), red only when it can observe nothing.

On claude 2.1.199 it observes nothing, every run: it fires `WaitUntil(IsIdle)` on a **false idle** — the bare `❯` glyph appears ~0.25s into startup, before claude's input handler is wired. At that instant `HasTrustModal` is still false (the modal has not rendered), so the `if HasTrustModal` block is skipped, and `"hi\r"` is typed into a not-yet-ready claude. The keystroke is dropped, no JSONL is written, the projects-dir never appears, and `discoverProjectsDir` times out at its internal 30s poll. #251 owns this diagnosis; it is the structural twin of `probe-first-prompt-hang` (#181), which characterises the same false idle but never solved it. `EncodeCwd` is *excluded* as a cause (the glob is session-id-based, path-independent; the same claude version observed cleanly on 2026-07-06).

**Why this is a spike.** #173 Open Q1 established there is no in-library *positive* readiness signal that a claude-free `make check` can validate — a clean idle and pre-ready chrome are indistinguishable without a live capture. So a re-anchor candidate can only be *proposed* here and *validated* by the operator watching real `make e2e` runs against the pinned claude. **The negative finding (no signal proves reliable) is a real terminal outcome, not a formality.**

**Developer-turn boundary (same split as every #206/#180-shape observation rig).** The developer's deliverable is AC1 + AC4: the re-anchored probe that builds and passes the claude-free `make check` gate (`go vet ./...` + `go test -race ./...`). **The developer must NOT run `make e2e`** — it needs live claude 2.1.199 and will hang/burn the dev gate. AC2 (10 consecutive live runs) and AC3 (verdict) are **operator-run, out-of-band**. Do not add `docs/knowledge/codebase/263.md` or any live-run record as a developer AC — that evidence is the operator's.

---

## Design

### The candidate readiness signal: a *settled-state* gate

Replace the two edge-triggered `IsIdle`/`HasTrustModal` checks with a **level-triggered "settled" gate**: a target predicate must hold **and** the PTY must have emitted no new bytes for a `settleWindow`. The false idle is a transient produced *while claude is still rendering* (the trust modal is about to paint, or claude is re-wiring input after dismissing it). Requiring the screen to be quiescent for a window waits past the render burst that produces the premature `❯`.

Quiescence is measured with the existing `Session.LastAppendAt()` clock — no new library surface:

```
settled(want) := want(Snapshot()) && time.Since(session.LastAppendAt()) >= settleWindow
```

Where a stronger **semantic** anchor exists, use it in preference to raw quiescence. For a brand-new temp cwd the trust modal is guaranteed (the probe's own invariant, `main.go:171`), so phase 1 anchors on `HasTrustModal` — its appearance means claude finished the startup render and is showing an interactive dialog. Phase 2 has no post-dismiss semantic signal, so it uses `!HasTrustModal && IsIdle` gated by quiescence.

**Principle:** prefer a semantic readiness signal; fall back to quiescence where none exists. Quiescence is deterministic code, not a second stochastic heuristic layered on the first.

### Re-anchored sequence

Replaces `main.go:163-197`. The phases (each a `WaitUntil` over `rootCtx`, so the 50s wall timeout still bounds every wait):

1. **Wait for the trust modal, settled.** `WaitUntil(rootCtx, settled(HasTrustModal))`, bounded by a `trustModalWait` sub-context so a (rare) auto-trusted cwd does not hang to the wall timeout. On success record marker `trust-modal-settled elapsed=… quiet=…`.
   - **Fallback (no modal within `trustModalWait`):** record `trust-modal-absent-within=… falling-back-to-quiescent-idle`, skip to phase 3 (a clean-idle path for the auto-trusted case). This preserves today's tolerance of a missing modal.
2. **Answer trust per policy.** Under `-trust-folder=fail`, return the existing clear error (only reached once the modal is genuinely up — no longer a false-idle race). Under `accept`, `AcceptTrust()`; record `trust-accepted`.
3. **Wait for a settled post-trust idle, then send.** `WaitUntil(rootCtx, settled(!HasTrustModal && IsIdle))`; record `post-trust-settled-idle elapsed=… quiet=…`; then `SendKeys(promptText)`; record `prompt-written`. This is the actual send-readiness gate and the one the root cause names ("after trust-accept").

`discoverProjectsDir` and everything downstream (lines 199-216) are unchanged.

### Probe-local helper (no library change)

A single closure factory in `main.go`:

```
// settledPredicate returns a WaitUntil predicate that holds once want(snapshot)
// is true AND the PTY has been quiet for window (claude finished the render
// burst that produced this state). window == 0 degrades to a bare want() check.
func settledPredicate(s *tuidriver.Session, window time.Duration, want func([]byte) bool) func() bool
```

Its quiescence decision is a pure, claude-free-testable core — extract it so `make check` asserts the contract:

```
func isSettled(want bool, sinceLastAppend, window time.Duration) bool
```

### New constants (`main.go:49-76` block)

- `settleWindow = 1 * time.Second` — must exceed the inter-render gap during startup/trust transitions, stay far under `sessionFileWait`/`wallTimeout`. **Explicitly a tuning lever** the operator adjusts across the 10 live runs (same posture as spike timeouts in `cmd/e2e-runner/main.go:300-303`).
- `trustModalWait = 15 * time.Second` — sub-timeout for phase 1's modal-appearance wait before the auto-trust fallback. Under the 50s `wallTimeout` with room for phases 2-3 + the 30s `sessionFileWait`.

### Recording hook (AC2/AC3 evidence for the operator)

The probe already logs `logger.Printf` markers (the runner mirrors stderr) and writes `observation.log`. Two additions make the readiness timing part of the **durable** artifact the operator collects across runs:

- The phase markers above (`trust-modal-settled`, `trust-accepted`, `post-trust-settled-idle`, `prompt-written`, and the fallback marker), each with `elapsed=` from `startedAt`.
- One extra line in the `OBSERVED:` block (`writeObservation`, `main.go:405-411`): `OBSERVED: readiness_signal=<trust-modal-settled|quiescent-idle-fallback> elapsed_to_ready=… settle_window=…`. This lands in `observation.log` and on stdout, so each of the 10 runs leaves a per-run record the operator can diff.

---

## Concurrency model

Unchanged from today's probe: one main goroutine, a `context.WithCancelCause` root, a `time.AfterFunc(wallTimeout)` that cancels the root, and `WaitUntil` polling at `DefaultPollInterval` (50ms). The re-anchor adds only the bounded `trustModalWait` sub-context around phase 1 (`context.WithTimeout(rootCtx, trustModalWait)`), which must be `cancel()`-ed on exit. No new goroutines. `LastAppendAt()` is read-only and already concurrency-safe (buffer-guarded).

---

## Error handling

- **Trust modal never appears** (auto-trusted cwd): `trustModalWait` elapses → fallback to phase 3 quiescent-idle path. Recorded, not fatal.
- **Never reaches settled idle** (persistent false idle / claude wedged): the wall timeout cancels `rootCtx`; the phase-3 `WaitUntil` returns the ctx cause → probe exits non-zero with a clear error → check goes red (correct: nothing observed). **This is exactly the AC3 negative-finding signal** — if it recurs across the operator's runs, the streak never reaches 10 and the verdict is negative.
- **`-trust-folder=fail`:** unchanged error, now reached only once the modal is settled (no false-idle skip).
- `discoverProjectsDir` timeout at `sessionFileWait`: unchanged — still the "could not observe" red path.

---

## Testing strategy

**Claude-free (developer, AC4 — the whole deliverable is confirmable here):**
- `make check` builds `cmd/probe-cwd-encoding` and runs `go test -race ./...`. The probe compiling under `go vet` is the primary claude-free confirmation.
- `cmd/probe-cwd-encoding/main_test.go` — table test for `isSettled` (bullet scenarios, developer writes them in the project idiom):
  - want=true, `sinceLastAppend >= window` → `true` (settled).
  - want=true, `sinceLastAppend < window` → `false` (still rendering — the false-idle case).
  - want=false, any duration → `false` (target state not present).
  - window=0, want=true → `true` (degrades to bare check).
- No new library predicates, so no `pkg/tuidriver` tests change.

**Operator, out-of-band (AC2/AC3 — developer does NOT run this):**
- 10 consecutive clean `make e2e` runs on the pinned claude 2.1.199. `probe-cwd-encoding` must reach `OBSERVED` with no 30s timeout every run; any timeout breaks the streak and restarts the count from zero (decided 2026-07-09).
- Verdict recorded (comment and/or `docs/knowledge/codebase/263.md`): **positive** (10-in-a-row) → unblocks #264 to drop `NonGating`; **negative** → probe stays `NonGating`, finding documented, #264 closes `wontfix`.
- The per-run `readiness_signal` / `elapsed_to_ready` lines and phase markers are the evidence; `settleWindow` is the tuning knob if a run settles prematurely or too slowly.

---

## Scope boundaries

- **Do NOT touch `EncodeCwd` (`pkg/tuidriver/cwd.go`).** #251's stable-version evidence excludes an encoding change; the failure is JSONL non-appearance from a false idle.
- **Do NOT edit `cmd/e2e-runner/main.go`** — not the `NonGating` flag, not the de-gate comment. Re-gating is #264, gated on this ticket's positive verdict. Leaving it untouched also keeps this branch clear of sibling in-flight work.
- **Do NOT write `docs/knowledge/codebase/263.md`** as part of the dev turn. AC2/AC3 recording is operator-run; the documentation phase owns any knowledge-base note after merge.
- Keep the change inside `cmd/probe-cwd-encoding/` (`main.go` + a new `main_test.go`). No new exported library surface.

---

## Open questions

1. **Does quiescence actually track "input handler wired"?** The false idle correlates with an active render burst, but claude *could* go PTY-quiet for >`settleWindow` mid-startup while wiring input asynchronously (e.g. background MCP registration with no PTY output — see the `deferred_tools_delta` / `pendingMcpServers` snapshots in `probe-first-prompt-hang/main.go:224-238`). If so the settle fires prematurely and the re-anchor fails. This is the core empirical uncertainty only the live runs resolve; it is *why* the negative finding is a real outcome.
2. **`settleWindow` value.** 1s is a first guess. The operator tunes it across the 10 runs; document the value that reaches 10-in-a-row (or that no value does).
3. **Trust-modal guarantee.** The design assumes a fresh temp cwd always shows the trust modal on 2.1.199. The `trustModalWait` fallback covers the auto-trust case defensively, but if the modal is *intermittently* absent the fallback path (quiescent idle only, no semantic anchor) is weaker — watch the `trust-modal-absent` marker frequency across runs.

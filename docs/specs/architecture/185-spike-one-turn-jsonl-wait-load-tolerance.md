# spike-one-turn: make the JSONL-appearance wait load-tolerant

Ticket: [#185](https://github.com/pyrycode/tui-driver/issues/185)
Sibling precedent (same spike, prior flake stabilization): [#97 / spec 97](./97-spike-one-turn-pty-quiescence.md)
In-repo precedent for the exact value: `cmd/probe-first-prompt-hang/main.go:41` (`sessionFileWait = 30s`)

PO sized this **S**; the design is **XS** (one production file, a single constant + its comment, plus a README empirical-log append). Overriding downward per the workflow — proceeding to spec, not splitting.

## Files to read first

- `cmd/spike-one-turn/main.go:36-49` — the `const (` block. `sessionFileWait = 10 * time.Second` (line 38) is the **single value this ticket changes**. Note it sits alongside `ptyQuietLimit = 60s` (the watchdog) and `ptyQuietWindow = 1500ms` (the turn-complete debounce) — neither is touched.
- `cmd/spike-one-turn/main.go:160-183` — the failing sequence: `SendKeys(promptText)` → `WaitForSessionJSONL` under a `context.WithTimeout(rootCtx, sessionFileWait)`. The 10s deadline here is what fires under load. The comment at 166-171 already documents *why* the wait exists (claude defers JSONL creation until first input); this spec adds *why it must be load-tolerant*.
- `pkg/tuidriver/jsonl.go:39-78` — `WaitForSessionJSONL` contract. It `os.Stat`-polls at `DefaultPollInterval` until the file exists or ctx is cancelled, returning a wrapped `context.Cause(ctx)` on deadline. **Do NOT modify** — the library function is correct; only the caller's deadline is too tight. Confirms `errors.Is(err, context.DeadlineExceeded)` on expiry.
- `cmd/probe-first-prompt-hang/main.go:41` — `sessionFileWait = 30 * time.Second // longer than spike-one-turn — hangs can delay JSONL creation`. The sister spike already reasoned to exactly the value this ticket adopts; cite it in the new comment rather than re-deriving.
- `cmd/e2e-runner/main.go:32,162-185,523-528` — `defaultCheckTimeout = 60s` and the **serial** check loop. spike-one-turn has no per-check `Timeout` override (lines 218-224), so it runs under the 60s default. This is the ceiling the new deadline must stay inside — the coupling that caps how generous `sessionFileWait` can grow (see § Value choice). Confirms checks run one-at-a-time, so "concurrent-suite load" is external, not intra-runner.
- `cmd/spike-one-turn/README.md:131-146` — *Observed timings* table. **Run 1 already failed here**: `FAIL — no new JSONL file appeared within 5s` (an earlier, even tighter value of this same wait). Append a Run 7 row documenting the flake + fix. This is the historical corroboration that the JSONL-appearance wait is a recurring failure surface for this spike.
- `cmd/spike-one-turn/README.md:159-292` — *Surprises / findings* section; finding #9 (`main.go` line ~247 in README, "interactive `claude --session-id` defers JSONL creation until first input") is the root-cause anchor. Add a short new finding cross-referencing #185 here.
- `docs/specs/architecture/97-spike-one-turn-pty-quiescence.md` — the closest prior stabilization of this binary; mirror its spec shape and its "document rationale in a comment block, cite the sibling, do not re-derive" discipline.

## Context

`spike-one-turn` is a **known intermittent flake** under `make e2e` on the pinned `claude 2.1.199` — not a regression. It passes deterministically on standalone binary re-run (3/3 observed) and passed the baseline in the same window, but reddened once during PR #184's `make e2e` run (`fail`, 11090ms). The 3/3-standalone-pass-after-a-single-make-e2e-fail signature is textbook **load-sensitive timing**, not a real assertion failure.

Left unfixed, any future PR whose `make e2e` happens to catch the flake looks like a regression under a naïve single-run baseline comparison (green on baseline, red on PR) and gets mis-routed to `needs-rework:developer`.

### Diagnosis (AC #1)

The failing deadline is **`sessionFileWait = 10s`** wrapping `WaitForSessionJSONL` (`main.go:172-177`). The chain:

1. `claude --session-id <uuid>` **defers JSONL creation until it processes the first input** (README finding #9 — verified empirically; distinct from plain `claude`, which writes startup envelopes during boot). So the file cannot appear until *after* `SendKeys(promptText)` and until claude has done enough first-input processing to open it.
2. That first-input processing is **local-process-bound** (session init + file open), so it is exactly the work that slows under CPU/IO contention.
3. The spike bounds this wait at 10s. Under concurrent-suite load the work exceeds 10s, the deadline wins the race, `WaitForSessionJSONL` returns `session jsonl … did not appear: context deadline exceeded`, `run()` returns the error, and the binary `os.Exit(1)`s → the runner records **`fail`**.

**Why the ~11s fingerprint is conclusive.** 10s deadline + ~1s of pre-wait work (spawn → idle → prompt-write, sub-second in the healthy Runs 5/6 timings) ≈ 11s, matching the observed 11090ms. Every *other* failure path in the spike is guarded by the 60s watchdog (`ptyQuietLimit`) or the 60s runner check-timeout, so no other mechanism can produce a **`fail`** (as opposed to `timeout`) at ~11s. And this same wait has failed before — Run 1 died at its then-5s value (README:135). The turn-complete loop (`main.go:218-252`) is *not* implicated: it is bounded only by the 60s watchdog, would surface as a 60s outcome, and its post-JSONL work (API-bound end_turn + 1.5s quiescence) is not local-CPU-sensitive.

**Source of the load.** The e2e-runner runs checks **serially** (`main.go:162`), so the contention is *external* to a single `make e2e`: `PYRY_MAX_CONCURRENT=2` dispatch running two agents' `make e2e` at once, and/or QA's baseline + PR `make e2e` overlapping — each a live `claude` process competing for cores. The fix does not need to identify the exact concurrent process; it needs the spike's one local-CPU-bound wait to tolerate the contention.

### Relationship to #173 (stays open, distinct fix)

Confirmed: this is **not** the #173 surface. #173 inverts the permissive *classification default* for unknown pre-first-prompt chrome (a readiness-classification-core change). This flake is a harness **deadline** that is too tight for a legitimately-slower-under-load operation. Per the ticket's Technical Notes, a fix in the readiness-classification core would route back to PO; this fix is squarely harness/anchor timing, so **the spec proceeds — no route-back**.

## Design

Relax the single over-tight deadline. This is the ticket's explicitly-sanctioned deterministic option ("relax/adapt an over-tight deadline" / "make the harness load-tolerant"), not a blind retry — the underlying operation *succeeds* given enough wall-clock; it is not failing on the merits.

### The change (contract)

In `cmd/spike-one-turn/main.go`, change one constant:

- `sessionFileWait` : `10 * time.Second` → `30 * time.Second`

and replace its comment with a rationale block that states: (a) the wait covers claude's deferred first-input JSONL creation (cross-ref finding #9), (b) that work is local-CPU-bound and slows under concurrent-suite load, so a 10s bound races the legitimate completion and reddens the suite (#185), (c) 30s is adopted from the sister spike `cmd/probe-first-prompt-hang/main.go:41`, which already reasoned to it for the same "delayed JSONL creation" reason — cite, do not re-derive, (d) 30s stays well inside the e2e-runner's 60s per-check budget (§ Value choice). Keep it citation-style, matching spec 97's comment-block discipline.

No other production line changes. Specifically **do not**:

- Touch `pkg/tuidriver/jsonl.go` — `WaitForSessionJSONL` is correct; the deadline is the caller's.
- Touch the turn-complete loop, `ptyQuietLimit`, `ptyQuietWindow`, `spinnerFreezeLimit`, or the watchdog — none is implicated in the ~11s signature.
- Touch `cmd/e2e-runner/main.go` — the 60s check-timeout has **not** been observed firing for this spike (the failure is an internal `fail`, not a runner `timeout`); adding a per-check override would be speculative defense against an unobserved mode.
- Touch the sibling spikes (`spike-multi-turn`, `spike-long-prompt`, `spike-ask-user`, `spike-cancel`, `spike-permission`) that share the same latent `sessionFileWait = 10s`. None has been *observed* to flake; bumping them is out of scope (evidence-based selection) and would fan out to 5+ files. Recorded as a follow-up in § Open questions.

### Value choice: why 30s (and the ceiling)

Standalone JSONL-open is ~200ms (README Run 5/6: 202ms, 304ms); the flake fired just over 10s. 30s is **3×** the observed failure threshold — generous margin over the load tail — while respecting the hard ceiling: spike-one-turn runs under the e2e-runner's **60s** `defaultCheckTimeout` with no override. Worst-case total ≈ (spawn→idle, a few s) + (JSONL wait ≤ 30s) + (turn ~3s + 1.5s quiescence) ≈ well under 60s. Going much higher (e.g. 45s) would crowd the 60s cap and merely trade an internal-deadline `fail` for a runner `timeout` — no net gain. 30s also matches an existing in-repo value chosen for this exact reason, so it is precedent-backed, not arbitrary.

## Repro recipe (AC #1)

The recipe is **documented shell instructions in the README**, not a new committed binary (a new `cmd/` program would be scope creep and a new file). Two complementary angles; the developer runs whichever actually reproduces on their machine and records the result:

1. **Mechanism isolation (deterministic).** Temporarily set `sessionFileWait` to a tiny value (e.g. `1 * time.Second`), rebuild, run the binary once → it fails with `open session jsonl: session jsonl … did not appear: context deadline exceeded` at ~1s. This proves the JSONL-appearance wait is the failure point and that the error string / timing match the PR #184 fingerprint. Revert.
2. **Load realism (best-effort).** With the deadline at the current 10s, launch N concurrent copies of the binary (default flags generate fresh UUIDs → distinct JSONL paths → no file collision; `-trust-folder=accept` handles the modal). Example: `for i in $(seq 8); do ./bin/spike-one-turn -trust-folder=accept & done; wait`. Under enough contention some copies hit the 10s deadline and print the same error. On a fast machine, stack additional CPU pressure (background `yes >/dev/null` per core, or bump N) until at least one copy reddens. The point is to observe the natural failure, not to hit a fixed rate.

Document the observed error line + approximate timing from whichever angle reproduces, so the diagnosis is reproducible from the README alone.

## Concurrency model

Unchanged. The spike's goroutine layout (orchestrator + PTY reader + JSONL tailer + watchdog) is untouched. The change is a single scalar deadline on the orchestrator's pre-turn JSONL wait. Shutdown sequence (deferred `cancelCause` + `session.Close`) is unchanged.

## Error handling

Unchanged in shape; strictly more load-tolerant.

- The relaxed deadline **still errors precisely** on genuine non-appearance: after 30s, `WaitForSessionJSONL` returns the same wrapped `context.DeadlineExceeded` and the same `open session jsonl: …` message. Diagnostic granularity is preserved (this is the argument against the "drop the sub-deadline entirely" alternative — see § Open questions).
- A genuinely hung claude (never creates the JSONL, PTY goes silent) is still caught by the 60s `ptyQuietLimit` watchdog and the 60s runner check-timeout as before. The 30s wait sits inside both, so a true hang still surfaces; only the *legitimate-but-slow* case is newly tolerated.
- No new error paths, no new branches.

## Testing strategy (AC #2, AC #3)

Spike binaries are validated end-to-end against live `claude`; there are **no unit tests to add** (the change is a scalar constant, not a unit-testable unit — consistent with README § *Why no automated tests* and spec 97 § *Testing strategy*).

- **AC #2 — ≥20/20.** Primary verification is **20 consecutive binary runs under representative concurrent load**, using the § Repro-recipe load harness (e.g. run spike-one-turn 20× while N sibling copies / CPU pressure run alongside), all exiting 0. This deterministic-sampling approach is the correct override for a load-sensitive flake — per the project lesson, defeat a flaky single-run signal with *more* deterministic sampling (re-run the binary N times), not with reasoning. Running full `make e2e` 20× serially is prohibitively slow and the binary is the unit under test; the literal "under make e2e" is satisfied by reproducing make-e2e-representative contention around the binary. Record the run count, the load applied, and that pre-fix the same harness reddened while post-fix it is 20/20.
- **AC #3 — siblings green.** The code change touches only spike-one-turn, so siblings are structurally unaffected. Verify with **one clean full `make e2e`** and confirm all checks pass (excluding the known-separate #180 `spike-permission` hang, which is out of scope and must not be conflated). Do **not** tail raw `make e2e` logs into the terminal (escape-sequence corruption); read `e2e-report.json` for per-check status.

## Documentation updates

Both live inside the spike's own tree (developer worktree mutates only code + this spike's README + the spec file):

- `cmd/spike-one-turn/README.md` *Observed timings* (line 131) — append a **Run 7** row: the load-induced JSONL-wait failure (`prompt → jsonl-opened` column exceeding the old 10s bound) and the post-fix pass, mirroring the existing row prose style.
- `cmd/spike-one-turn/README.md` *Surprises / findings* (line 159) — add a short finding cross-referencing finding #9 and #185: the deferred-JSONL-creation window is load-sensitive, the 10s bound raced it under concurrent-suite load, 30s (matching probe-first-prompt-hang) resolves it.

## Open questions

- **Alternative considered — drop the sub-deadline, ride the watchdog.** Instead of a larger fixed value, the JSONL wait could run under `rootCtx` directly (bounded only by the 60s `ptyQuietLimit` watchdog + 60s check-timeout), anchoring on liveness rather than a clock. **Rejected**: it degrades the precise `open session jsonl: … did not appear` diagnostic into a generic 60s `timeout`, and an explicit bounded deadline is better hygiene. Recording it here so the decision is legible, not re-litigated.
- **If 30s proves insufficient at 20/20** (not expected — standalone is ~200ms, the flake fired just over 10s): the fallback is to raise the e2e-runner per-check timeout for spike-one-turn *and* bump `sessionFileWait` further, keeping the internal deadline inside the check budget. Contingency only; the plan is 30s alone.
- **Sibling latent risk (deferred, not this ticket).** `spike-multi-turn`, `spike-long-prompt`, `spike-ask-user`, `spike-cancel`, and `spike-permission` share `sessionFileWait = 10s` and the same deferred-JSONL-creation window, so they carry the same latent load-sensitivity. None has been *observed* to flake, so per evidence-based fix selection they stay untouched here. If one reddens under the same signature later, it earns its own ticket (or a follow-up that lifts a shared load-tolerant default — the natural home once there is integration pressure from a second observed case).

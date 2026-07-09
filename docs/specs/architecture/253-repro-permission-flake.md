# #253 — `repro-permission-flake`: reproduce + diagnose spike-permission's "modal not detected within 30s" load flake

[Issue](https://github.com/pyrycode/tui-driver/issues/253) · Slice A of the spike-from-implement split of #253 (architect override, 2026-07-09) · Slice B (the fix) filed lazily by PO once this harness records the root cause, `blockedBy` this ticket.

Precedents (both in-repo, read before designing):
- **Flake-diagnosis shape** — [spec 185](./185-spike-one-turn-jsonl-wait-load-tolerance.md) (`spike-one-turn`, the sibling load flake: same "passes 3/3 in isolation, reddens once under full-suite load" signature; its § *Repro recipe* is the load model this harness commits to code).
- **Rig-ships-green-then-observe shape** — [spec 206](./206-probe-cwd-encoding.md) (`probe-cwd-encoding`: builds + unit-tests under `make check`, the live observation is produced when an operator runs it post-merge; #206 rig → #207 fix, exactly the A→B split shape here).

PO sized this **S**; the design is **S** (one production `.go` file, zero exported types, ~500–600 lines of total written work, purely additive — no interface change, no consumer cascade). Not overriding. **Not `security-sensitive`** (confirmed: the ticket carries no `security-sensitive` label, and slice A builds instrumentation *around* `./bin/spike-permission` — it changes no modal-detection logic in `pkg/tuidriver` and no modal-answer path). The security-review step is skipped; the precautionary posture attaches to **slice B iff** this harness's diagnosis is root-cause (c) — see § Context and § Diagnosis rules.

## Files to read first

- `cmd/spike-permission/main.go:112-115` — `modalDetectLimit = 30 * time.Second`, the spike's **internal** probe wall (not the runner `Timeout`). This is the deadline the flake races; the harness reproduces its expiry but does **not** change it.
- `cmd/spike-permission/main.go:867-904` — `waitForModal` (polls `hasModal` at `statePollInterval`, returns `"modal not detected within %s"` on expiry at `:878`) + `hasModal` (ANSI-strips, substring-matches). The `"modal not detected within"` string is the **flake fingerprint** the harness classifier keys on.
- `cmd/spike-permission/main.go:150-168` — `modalLiteralTexts` (`Esctocancel`, `Doyouwanttoproceed`, `Do you want to proceed`). These three anchors are what `hasModal` fires on; the harness re-greps the captured PTY stream for the **same** three literals (its own documented copies, cross-referenced to this line range) to answer "did the modal actually render on-screen before the timeout?" — the (c)-discriminator.
- `cmd/spike-permission/main.go:509-534` — Probe 1's sequence: `probe-start` → `TypePrompt` → `prompt-written` → `waitForModal`. The flake is a **pre-clear** timeout here (`:530`), before any `OBSERVED:` line prints. The `logger.Printf("probe=%d …", …)` stage lines land on stderr (mirrored) and are the harness's stage-timeline source.
- `cmd/spike-permission/main.go:601-610` — `formatAutoRespondObservation` / the single `^OBSERVED` line, printed **post-approve** in Probe 2. A flake never reaches it; `^OBSERVED`-present = pass, its absence + the timeout string = flake. This is the pass/fail oracle.
- `cmd/corpus-replay/main.go` (~300 LOC), `report.go` (~188 LOC), `main_test.go` (~91 LOC), `README.md`, `testdata/` — **the hand-run `TOOLS`-category rig to mirror**: flag parsing, a durable report artifact, testdata-driven pure-function unit tests, and a `README` empirical-log shape. Copy this structure; it is the template for a diagnostic binary that builds under `make check` but is *not* enrolled in the sequential `make e2e` suite.
- `cmd/e2e-runner/main.go:40-41` (`observedSuccess = ^OBSERVED`) + `:273-277` (the `spike-permission` `Check`: `Args: commonArgs` = `["-trust-folder=accept"]`, `SuccessMarker: observedSuccess`). The harness spawns `./bin/spike-permission` with the **same** `-trust-folder=accept` the runner passes, and uses the **same** `^OBSERVED` marker as its pass oracle.
- `pkg/tuidriver/ansi.go:21` — `StripANSI(snap []byte) []byte` (exported). The harness ANSI-strips each capture before anchor-searching, reusing the library strip rather than re-implementing it.
- `Makefile:11-16` (`SPIKES` / `PROBES` / `TOOLS` / `ALL_BINS`) + `:32` (`.PHONY`) + `:49-53` (`corpus-replay` phony target). The harness enrolls in **`TOOLS` only** and gets a mirror phony target — **not** `PROBES`, **not** `buildChecks`. Read to copy the exact enrollment lines.
- `.gitignore:8-17` — the per-binary ignore block; add `/repro-permission-flake` as a sibling.

## Context

`spike-permission` is an observation rig (`SuccessMarker: observedSuccess` / `^OBSERVED` since #180). Under full-suite `make e2e` load on PR #249 it failed **once**:

```
spike failed: session A: probe 1: probe 1: modal not detected within 30s
e2e-runner: spike-permission -> fail (31830ms)
```

but passed 3/3 on a direct `./bin/spike-permission` re-run and 2/2 on baseline immediately after — the textbook "passes in isolation, flakes under full-suite load" signature #185 documents for `spike-one-turn`. The 30s wall is the spike's **internal** `modalDetectLimit`, hit **pre-clear** in Probe 1 (before any `OBSERVED:` prints), so `^OBSERVED` never appears → `fail`. The #180 `^OBSERVED` de-gate (a *post*-clear hang) genuinely does not cover this pre-clear detect-wall, and this is **distinct from #180** (a deterministic post-clear 60s hang, different assertion, closed).

**Why a committed harness (and not #185's README-only recipe).** #185's fix was a single constant, so its repro recipe stayed shell-instructions in a README. This ticket's fix branch **and its security posture both fork on the diagnosis**, so a reusable, instrumented, artifact-producing harness is warranted — it is re-run to validate slice B against the #185 bar (≥20 consecutive load runs). That is why slice A is a `cmd/` binary, not scope creep.

**The three candidate root causes** (from the ticket) and the fork each implies:

- **(a) contention/scheduling delay** — claude's own process is CPU-starved and simply hasn't *rendered* the modal within 30s. → load-induced; slice B is a load-tolerant readiness margin in `cmd/spike-permission`, **not** security-sensitive. Likely **shared family with #185**.
- **(b) PTY read starvation** — claude rendered the modal promptly, but the spike's PTY reader goroutine was starved, so `session.Snapshot()` stayed stale past the deadline. → also load-induced; slice B is a harness-side timing fix in `cmd/spike-permission`, **not** security-sensitive.
- **(c) detection-timing issue in the probe** — the modal bytes were present and drained, but `hasModal`/`waitForModal` never fired. → the fix must re-anchor modal *detection*, which lives in `pkg/tuidriver` → slice B is **`security-sensitive`** (audited against the #242 forgery threat model, must preserve the region-scoped shape-gate) and is **distinct from #185**.

So the **single highest-stakes job of this harness is to confirm-or-refute (c)** — it decides slice B's security posture. Fortunately (c) is the one cause the black-box capture detects *unambiguously* (see § Diagnosis rules). **Slice A fixes nothing** — it reproduces and records so slice B is authored against a known root cause, not a guess.

## Design

A single new hand-run binary, `cmd/repro-permission-flake`, plus its wiring. It drives `./bin/spike-permission` in concurrent waves under artificial load, timestamps and captures each instance's output, classifies each run's outcome with pure functions, and writes a durable diagnosis artifact. It **treats `spike-permission` as a black box** — it spawns the prebuilt binary and reads its stdout/stderr; it does not import, modify, or link the spike. It reuses exactly one library symbol, `tuidriver.StripANSI`, for anchor-searching captures.

Naming: `repro-permission-flake` (a `TOOLS`-category diagnostic, sibling to `corpus-replay`). Deliberately **not** a `probe-*` name — `probe-*` binaries auto-enroll in the sequential `make e2e` suite; this heavy, live-claude, minutes-long, many-process diagnostic must **not**.

### 1. Load recipe (drive spike-permission under contention)

Mirror #185's load model, committed to code. Per **wave**, launch `P` concurrent `./bin/spike-permission -trust-folder=accept` instances (`exec.CommandContext`, fresh default UUID per instance → distinct session-id / JSONL / tempfile, no collision). Repeat for up to `W` waves. Every instance is **both load and subject** — the harness captures and classifies all `P×W` of them; this reproduces the real `PYRY_MAX_CONCURRENT=2` "two live claude competing for cores" contention exactly, with `P>2` as stack-pressure on a fast machine.

Flags (mirror `corpus-replay`'s flag style; contract, not signatures to copy verbatim):

- `-concurrency P` — instances per wave (default a small value, e.g. `4`; `2` is the real-world floor).
- `-waves W` — max waves (default e.g. `20`).
- `-stop-on-flake` — stop after the first wave that produces ≥1 flake (default `true`; set `false` to accumulate a full sample for the slice-B ≥20-run bar).
- `-cpu-burn N` — optional synthetic CPU pressure: `N` goroutines busy-looping for the run's duration (default `0`; the `yes >/dev/null` analog from #185's recipe, for a machine too fast to flake on concurrency alone). **Leave headroom** — do not default this to `GOMAXPROCS`; starving the harness's own capture goroutines would confound (b) detection (see § Open questions).
- `-bin` — path to the `spike-permission` binary (default `./bin/spike-permission`).
- `-out` — artifact root (default a timestamped dir under `os.TempDir()`).

### 2. Capture (per-instance, harness-owned timestamps)

`spike-permission` runs with `MirrorStderr` (its PTY byte stream **and** its `probe=… …` stage log lines both land on stderr). The harness attaches a pipe to each instance's `Stderr`, drained by one goroutine that **prefixes every line with a harness-owned monotonic elapsed-ms** (relative to that instance's spawn) and writes `wave<w>-inst<i>.log` under the artifact dir. Harness-owned timestamps are used deliberately — they are robust regardless of the spike's `log` flags and give a single consistent clock across stage lines and raw PTY bytes. Stdout is captured separately (the `^OBSERVED` line). No parsing happens in the capture goroutine — it only timestamps and persists; classification is a pure post-pass over the file.

### 3. Classify + diagnose (pure functions — the `make check` unit-test surface)

Two pure functions over a captured stream, the testable core (cf. #206's `perByte`/`perRune`/`perUTF16` and #180's `formatAutoRespondObservation`):

- **`classifyCapture(capture []byte) instanceOutcome`** — parse the timestamped lines into an `instanceOutcome` carrying: `outcome` (`pass` if stdout has `^OBSERVED`; `flake` if stderr has `modal not detected within`; `other` otherwise), `modalSeenAtMs` (earliest elapsed-ms at which any of the three `modalLiteralTexts` anchors appears in the `StripANSI`-ed PTY stream, or *never*), `stageTimeline` (elapsed-ms of each `probe=… <stage>` line — `probe-start`, `prompt-written`), and `maxByteGapMs` (largest inter-line arrival gap in the PTY stream — the bursty-vs-steady signal).
- **`diagnose(o instanceOutcome) rootCauseHint`** — apply the § Diagnosis-rules decision table → one of `{contention-a, pty-starvation-b, detection-bug-c, load-induced-ab-ambiguous, inconclusive}` + a one-line evidence string.

Both are ≤ ~40-line pure functions; no goroutines, no I/O. The struct field set is a contract, not an implementation — the developer chooses field names/types in the repo idiom.

### 4. Diagnosis rules (the decision table)

Applied per flake instance by `diagnose`. The load-bearing column is **`modalSeenAtMs` vs the timeout** — it is what cleanly rules (c) in or out:

| `modalSeenAtMs` | byte-gap profile | → root cause | evidence |
|---|---|---|---|
| **< timeout** (modal in stream well before 30s) | any | **(c) detection-bug** | anchors drained on-screen at `modalSeenAtMs`, yet `waitForModal` timed out → the detector, not the render, failed |
| **never** (no anchor before timeout) | **steady** (many small gaps, no large one) | **(a) contention** | claude never rendered the modal in 30s; byte flow steady but slow → claude-side CPU starvation |
| **never** or late | **bursty** (≥1 large gap, anchor arrives right after it) | **(b) pty-starvation** | bytes (incl. modal) pooled and arrived in a burst after a reader-scheduling gap |
| **never / late**, gaps ambiguous | mixed | **(a/b ambiguous** | load-induced late render; (a) vs (b) not separable from outside (see caveat) — but **not (c)**, which is the security-relevant fact |

**Why (c) is unambiguous.** The stderr mirror and the rolling buffer are fed by the **same** reader goroutine, so a capture showing an anchor at `t < 30s` proves the bytes were both rendered *and drained into the spike's buffer* before the deadline — if `hasModal` still didn't fire, the defect is in detection. This is the exact fork that decides slice B's security posture, and it is the signal the black-box capture reads most reliably.

**(a)-vs-(b) caveat (state it honestly).** Because mirror == buffer (one reader), external capture cannot perfectly separate claude-side slowness (a) from spike-side reader starvation (b) — both surface as "modal bytes late in our capture." The byte-gap distribution is the discriminator (steady-slow → a; bursty-after-a-large-gap → b); when ambiguous the harness reports `load-induced-ab-ambiguous` rather than guessing. **This ambiguity does not block slice B**: both (a) and (b) route to a load-tolerant timing fix in `cmd/spike-permission` and are **not** security-sensitive. Only the (c) determination changes slice B's branch and security posture, and (c) is unambiguous.

**Shared-vs-#185 mapping (feeds AC — slice B's branch selector).** `(a)`/`(b)` = a load-induced timeout of a legitimately-slow-under-load claude operation — the **same family as #185** (consolidate: name `spike-permission` in #185's tracker, or a deterministic margin/anchor fix in `cmd/spike-permission`; no `pkg/tuidriver` change; not security-sensitive). `(c)` = a detection defect → **distinct**, pushes into `pkg/tuidriver` (security-sensitive under #242). The harness surfaces the signals; the final shared/distinct call is the operator's, made from the readout.

### 5. Artifact output

Under the timestamped artifact dir, write: the per-instance `wave<w>-inst<i>.log` captures **and** a `summary` readout aggregating (total runs, pass/flake/other counts, and for each flake its `instanceOutcome` fields + `diagnose` hint + the shared-vs-#185 mapping). Echo the summary to stdout so a terminal run is legible, but the durable file is the deliverable PO authors slice B against (AC: "records to a durable artifact, not just stdout"). A plain-text readout suffices; a machine-readable `summary.json` alongside is optional polish, not required.

**Exit code.** The harness exits **0 whenever it completed its wave plan and wrote the artifact** — including the run where it *reproduced* the flake (a reproduced flake is success for a diagnosis rig, exactly as #206 ships green on a successful observation). Non-zero only on a harness-level failure (binary not found, artifact dir unwritable, all instances failed to spawn). It is **not** a `make e2e` check, so this exit convention governs only hand-runs; do not wire it to `^OBSERVED`.

### 6. README empirical-log scaffold

Ship `cmd/repro-permission-flake/README.md` in the sibling shape (what-it-does, how-to-run, empirical log). It **must** contain an **Empirical log** section and a **Diagnosis (for #253 slice B)** subsection, both marked `_Awaiting first live make e2e-representative run_` at ship time — the diagnosis golden is recorded here (and in the artifact) from the first live operator run, **not** by the developer, whose `make check` gate has no live claude. Structure the log row so the first run's readout transcribes directly:

> | run date | claude version | concurrency / cpu-burn | runs (pass/flake) | modalSeenAt on flake | root cause (a/b/c) | shared with #185? |

### Wiring checklist (verify by reading each target — do not guess)

1. `Makefile:14` — append `repro-permission-flake` to `TOOLS :=` (`ALL_BINS` picks it up via `$(TOOLS)`; the `$(BIN_DIR)/%` rule builds it — no other build edit). Add a `make repro-permission-flake` phony target mirroring `corpus-replay` (`:52-53`) and its `.PHONY` entry (`:32`).
2. `.gitignore` — add `/repro-permission-flake` beside `/probe-cwd-encoding`.
3. **Do NOT** add it to `PROBES` or to `cmd/e2e-runner/main.go` `buildChecks` — it is a hand-run diagnostic, not a suite check (enrolling it would spawn `P×W` live claude every `make e2e`).

After committing, **re-diff any dispatcher safety-net auto-commit** — one has dropped a `Makefile`/`buildChecks`-adjacent entry before (47.md); a binary that builds but is unenrolled in `TOOLS` defeats `make repro-permission-flake`.

## Concurrency model

The harness owns a small, bounded goroutine set under one root `context.WithCancelCause` (cancelled on SIGINT/SIGTERM → children get SIGKILL via `exec.CommandContext`):

| Goroutine | Owns | Exit |
|---|---|---|
| main | wave loop, spawn `P` instances per wave, join, classify, write artifact | wave plan done OR ctx cancelled |
| per-instance capture (×P live) | drain + timestamp one instance's stderr → `wave<w>-inst<i>.log` | that instance's pipe EOF |
| CPU-burn worker (×N, optional) | busy-loop | ctx cancelled |

Each spike instance additionally runs its own internal goroutines (PTY reader etc.) inside its own process — invisible to the harness, reaped by `exec.CommandContext`'s SIGKILL-on-cancel. Keep capture goroutines cheap (buffered line writes, no parsing) so they are not themselves starved by `-cpu-burn` (the (b)-confound; see § Open questions).

## Error handling

| Failure | Handling | Harness verdict |
|---|---|---|
| `./bin/spike-permission` missing / not built | fail fast, clear error | non-zero (harness error) |
| an instance fails to spawn | record `other`, continue the wave | 0 if any instance ran |
| an instance flakes (`modal not detected within 30s`) | capture + classify + diagnose — **this is the target observation** | **0** (reproduced) |
| an instance passes (`^OBSERVED`) | record `pass`, continue | 0 |
| artifact dir unwritable | fail fast | non-zero |
| SIGINT during a wave | cancel ctx → SIGKILL children → write partial artifact → exit | 0 (partial recorded) |

Reproducing the flake is **success**, not failure — the rig records ground truth (cf. #206 § Error handling: red = "could not observe", green = "observed and recorded").

## Testing strategy

Two gates, split exactly as #206/#207:

- **Developer gate (`make check`, claude-free):** `go vet ./...` compiles the new `main` package; `go test -race ./...` runs the pure-function unit tests. Table-test `classifyCapture` and `diagnose` against **synthetic captured streams** committed under `cmd/repro-permission-flake/testdata/` (mirror `corpus-replay/testdata` + `main_test.go`). Scenarios (bulleted, developer writes them in the repo idiom — no test bodies here):
  - a capture whose stdout has `^OBSERVED` → `outcome=pass`.
  - a capture whose stderr has `modal not detected within 30s` and whose PTY stream contains `Esctocancel` at an early elapsed-ms → `outcome=flake`, `modalSeenAtMs` small, `diagnose → detection-bug-c`.
  - a flake capture with **no** anchor anywhere and steady small gaps → `diagnose → contention-a`.
  - a flake capture with a large byte-gap and the anchor arriving right after it → `diagnose → pty-starvation-b`.
  - a flake capture with a late/absent anchor and mixed gaps → `diagnose → load-induced-ab-ambiguous`.
  - an anchor split by CSI (`\x1b[1C`) inside the raw stream → after `StripANSI` the no-space `Doyouwanttoproceed` form matches (pin the ANSI-strip step, the same Bash-vs-Read dual-form subtlety spike-permission handles at `:150-168`).
  - the developer also verifies: builds + vets clean, enrolled in `Makefile` `TOOLS` + phony target, `.gitignore` entry present, README scaffold with the `_Awaiting first live run_` placeholder. The developer **cannot** produce the diagnosis golden — that needs live claude.
- **Live gate (operator, post-merge, `make e2e`-representative):** the operator runs `make repro-permission-flake` (optionally with `-cpu-burn`/higher `-concurrency`) on a box with live claude under representative contention. This must **reproduce** the `modal not detected within 30s` flake (AC), the readout must **name the root cause (a/b/c) with evidence** (AC), and it must **state shared-with-#185 or distinct** (AC). Transcribe the readout into the README Empirical-log + Diagnosis subsections. That recorded golden is what unblocks slice B (which stays `blockedBy` #253 until it exists). Do **not** tail raw captures into a terminal (escape-sequence corruption — read the artifact files); the harness's timestamped-line writes already tame this.

## Open questions

- **Harness-side capture lag under `-cpu-burn` (the (b)-confound).** If synthetic CPU pressure starves the harness's own capture goroutines, timestamps lag and a genuine (a) could masquerade as (b). Mitigation: keep capture goroutines cheap and cap `-cpu-burn` well below `GOMAXPROCS` (leave headroom for the harness + `P` capture goroutines). If the first live run shows suspiciously uniform large gaps across *all* instances, suspect harness-side lag and re-run with `-cpu-burn 0`. Decide on the first live run; do not pre-engineer around an unobserved artifact.
- **Does concurrency alone reproduce it, or is `-cpu-burn` needed?** PR #249 flaked at real `PYRY_MAX_CONCURRENT=2`. Start at `-concurrency 2..4, -cpu-burn 0`; escalate only if it won't reproduce (#185's "observe the natural failure, not hit a fixed rate"). Record which recipe actually reproduced.
- **(a) vs (b) may be genuinely inseparable from outside.** Documented in § Diagnosis rules — acceptable, because both route to the same non-security-sensitive slice-B fix. Only (c) needs an unambiguous verdict, and it has one. If PO needs (a)/(b) separated for slice B, that is a *grey-box* follow-up (opt-in timing instrumentation *inside* `cmd/spike-permission` behind an env flag) — out of scope for slice A, which is black-box by the ticket's not-security-sensitive boundary.
- **Projects-dir / tempfile accumulation.** `P×W` instances each write a `/tmp/spike-permission-probe*-bytes-*.bin` and leave a `~/.claude/projects/<dir>` (matching every sibling spike, which never clean `projects/`). If accumulation becomes noise across repeated runs, add a targeted cleanup in a follow-up — out of scope here.

## Acceptance criteria

**Claude-free deliverable (developer builds; `make check`):**

- [ ] `cmd/repro-permission-flake` drives `./bin/spike-permission -trust-folder=accept` in concurrent waves (`-concurrency` × `-waves`, `-stop-on-flake`, optional `-cpu-burn`) under artificial concurrent-suite load, mirroring #185's load model.
- [ ] Each instance's stderr (PTY mirror + stage lines) is captured with harness-owned per-line timestamps to a durable per-instance file; `classifyCapture` + `diagnose` are **pure, unit-tested** functions (testdata-driven, in the `corpus-replay` shape) that yield `outcome` + `modalSeenAtMs` + byte-gap profile + a root-cause hint distinguishing (a)/(b)/(c).
- [ ] Output is written to a durable artifact (per-instance captures + a `summary` readout), not just stdout.
- [ ] Enrolled in `Makefile` `TOOLS` + a `make repro-permission-flake` phony target; `.gitignore` has `/repro-permission-flake`; README ships with the Empirical-log + Diagnosis subsections scaffolded. **Not** in `PROBES` / `buildChecks`.

**Live-observation deliverable (operator runs post-merge; feeds PO's slice B):**

- [ ] Running the harness **reproduces** the `modal not detected within 30s` timeout under the artificial concurrent-load recipe (observed, not inferred).
- [ ] The root cause is identified from the instrumentation — which of (a)/(b)/(c), with evidence — and recorded in the artifact + README.
- [ ] The root cause is stated as **shared with #185** or **distinct**, explicitly, per the § Diagnosis-rules mapping — the determination that selects slice B's branch (consolidate vs. deterministic stabilisation) and its security posture.

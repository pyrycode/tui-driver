# repro-permission-flake

Reproduces and diagnoses `spike-permission`'s **"modal not detected within 30s"**
load flake ([#253](https://github.com/pyrycode/tui-driver/issues/253), slice A of
the spike-from-implement split). It drives the prebuilt `./bin/spike-permission`
black-box in concurrent waves under artificial load, timestamps each instance's
output, classifies the outcome with pure functions, and writes a durable
diagnosis artifact so slice B (the fix) is authored against a **recorded** root
cause rather than a guess.

This rig **fixes nothing**. It reproduces and records. A reproduced flake is
success (exit 0), exactly as [#206](https://github.com/pyrycode/tui-driver/issues/206)'s
rig ships green on a successful observation.

## The flake

Under full-suite `make e2e` load, `spike-permission`'s probe 1 can hit its
internal 30s `modalDetectLimit` (`cmd/spike-permission/main.go:115`) before claude
renders the permission modal, printing `modal not detected within 30s` on stderr
with no `OBSERVED:` line — so the runner sees a `fail`. It passes 3/3 in
isolation. That "passes alone, reddens under load" signature matches
[#185](https://github.com/pyrycode/tui-driver/issues/185)'s `spike-one-turn` flake.

## The three candidate root causes

The single highest-stakes job of this harness is to confirm-or-refute **(c)** —
it decides slice B's branch and its security posture.

| cause | what it means | slice B fork |
|---|---|---|
| **(a) contention** | claude too CPU-starved to render the modal in 30s | load-tolerant timing fix in `cmd/spike-permission`; **shared family with #185**; not security-sensitive |
| **(b) PTY starvation** | modal rendered, but the spike's PTY reader was starved so the buffer stayed stale | timing fix in `cmd/spike-permission`; **shared family with #185**; not security-sensitive |
| **(c) detection bug** | modal bytes drained on-screen before the deadline, yet the detector never fired | re-anchor detection in `pkg/tuidriver`; **distinct from #185**; **security-sensitive** under #242 (preserve the region-scoped shape-gate) |

**Why (c) is unambiguous.** `spike-permission`'s stderr mirror and its rolling
buffer are fed by the **same** reader goroutine, so a capture showing a modal
anchor at `t < deadline` proves the bytes were both rendered *and* drained into
the spike's buffer in time — if `hasModal` still didn't fire, the defect is in
detection. `(a)` vs `(b)` are not always separable from a black-box capture, and
that is fine: both route to the same non-security-sensitive fix. Only `(c)` needs
an unambiguous verdict, and it has one.

## Run

`make check` builds and unit-tests the pure core (claude-free). The live
diagnosis needs live claude, so run it by hand on a box with a working claude:

```sh
# Builds spike-permission + the harness, then drives the load recipe.
make repro-permission-flake

# Escalate load if concurrency alone won't reproduce it on a fast machine.
make repro-permission-flake CONCURRENCY=6 CPU_BURN=2

# Accumulate the full slice-B sample (>=20 runs) instead of stopping on first flake.
go run ./cmd/repro-permission-flake -bin ./bin/spike-permission -waves 30 -stop-on-flake=false
```

Flags:

- `-concurrency N`: spike instances per wave (default 4; 2 is the real
  `PYRY_MAX_CONCURRENT` floor). Every instance is both load and subject.
- `-waves N`: max waves (default 20).
- `-stop-on-flake`: stop after the first wave that flakes (default true; set
  false to accumulate the slice-B ≥20-run sample).
- `-cpu-burn N`: N busy-loop goroutines of synthetic CPU pressure (default 0).
  **Leave headroom below `GOMAXPROCS`** — starving the harness's own capture
  goroutines would confound (b) detection.
- `-bin`: path to the prebuilt `spike-permission` (default `./bin/spike-permission`).
- `-out`: artifact root (default a timestamped dir under the temp dir).

Read the artifact **files** — do not tail raw captures into a terminal
(escape-sequence corruption). The `summary` file names the root cause and the
shared-vs-#185 mapping.

## ⚠️ Self-reference

This tool's source and its `testdata` fixtures quote claude's modal anchors.
Rendering those on screen mid-run can false-fire the live detection, so build and
run it **by hand**, off the agent pipeline, as the detection tickets
(#152 / #154 / #155) were. It is deliberately in the `TOOLS` category, **not** a
`probe-*` binary — `probe-*` names auto-enroll in the sequential `make e2e`
suite, and this heavy, live-claude, many-process diagnostic must not.

## Empirical log

_Awaiting first live `make e2e`-representative run._ Transcribe the harness
`summary` readout here.

| run date | claude version | concurrency / cpu-burn | runs (pass/flake) | modalSeenAt on flake | root cause (a/b/c) | shared with #185? |
|---|---|---|---|---|---|---|
| _pending_ | | | | | | |

## Diagnosis (for #253 slice B)

_Awaiting first live `make e2e`-representative run._ Once the harness reproduces
the flake, record here: the confirmed root cause with its evidence, and the
explicit **shared-with-#185 vs distinct** determination. That recorded golden is
what unblocks slice B (which stays `blockedBy` #253 until it exists):

- **shared with #185** → consolidate: name `spike-permission` in #185's tracker,
  or a deterministic margin/anchor fix in `cmd/spike-permission`; no
  `pkg/tuidriver` change; not security-sensitive.
- **distinct** → detection defect in `pkg/tuidriver`; security-sensitive under
  the #242 forgery threat model; must preserve the region-scoped shape-gate.

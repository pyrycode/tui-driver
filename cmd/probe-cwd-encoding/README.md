# probe-cwd-encoding

A recording rig that observes how real `claude` encodes a **non-ASCII** working
directory into the `~/.claude/projects/<name>` directory it writes its
per-session JSONL under. Ticket
[#206](https://github.com/pyrycode/tui-driver/issues/206) · Spec
`docs/specs/architecture/206-probe-cwd-encoding.md` · unblocks
[#207](https://github.com/pyrycode/tui-driver/issues/207) (the fix, blocked on
this finding).

## Why it exists

`tuidriver.EncodeCwd` (`pkg/tuidriver/cwd.go`) derives that directory name by
mapping every **byte** outside `[a-zA-Z0-9]` to one `-`. For a multi-byte char
such as `ö` (2 UTF-8 bytes) the byte loop emits **two** hyphens. Whether that
matches `claude` is **unverified** — the empirical derivation behind `EncodeCwd`
exercised only ASCII specials, and the `café → caf--` unit case in
`cwd_test.go` is the byte loop describing *its own output*, never observed from
`claude`. `claude` is a JS/TS CLI; its encoder is most likely
`/[^a-zA-Z0-9]/g → '-'` over **UTF-16 code units** — one hyphen per BMP char,
two per astral char — but that is a hypothesis, not a measurement.

This probe measures it. It **does not** change `EncodeCwd` (that is #207); it
only records the golden value from real `claude` so #207 can pin a unit test
against `claude`'s actual filesystem behaviour rather than an assumption.

## What it does

1. Creates a temp working directory whose **leaf** is `Työ😀` — one BMP
   multi-byte char (`ö`, U+00F6: 2 UTF-8 bytes / 1 UTF-16 unit / 1 rune) **and**
   one astral char (`😀`, U+1F600: 4 UTF-8 bytes / 2 UTF-16 units / 1 rune).
   That single component distinguishes all three candidate rules in one run.
2. Spawns `claude --session-id <fresh-uuid>` with `cmd.Dir` set to that cwd.
3. Waits for idle, accepts the (always-present, brand-new-cwd) trust modal,
   sends one trivial prompt — **only** to make `claude` create the session JSONL
   (deferred until first input lands; see `spike-one-turn` finding #9). It does
   **not** wait for the turn to complete.
4. Discovers `claude`'s projects-dir **independently of `EncodeCwd`** — globs
   `~/.claude/projects/*/<session-id>.jsonl` and takes the parent dir's base
   name. It **never** calls `SessionJSONLPath` / `WaitForSessionJSONL` (both
   derive the path via `EncodeCwd`, so they could only ever confirm the current
   transform, never observe a mismatch).
5. Records the observed dir byte-for-byte and derives which rule produced it via
   a **leaf-suffix** match, then prints an `OBSERVED:` block to stdout and
   mirrors it to `observation.log` under a timestamped recording dir.

It **ships green whenever it makes an observation** — including when the
observation contradicts `EncodeCwd` (the expected, useful result) and when no
candidate rule matches (`derived_rule=unknown`, raw dir still recorded). It
exits non-zero only when no observation could be made (spawn failed, never
idle, JSONL never appeared).

### Candidate rules (leaf `Työ😀`)

| rule | `ö` → | `😀` → | `Työ😀` → |
|------|-------|--------|-----------|
| per-byte (current `EncodeCwd`) | `--` | `----` | `Ty------` |
| per-UTF-16 code unit (hypothesised) | `-` | `--` | `Ty---` |
| per-Unicode character | `-` | `-` | `Ty--` |

`claude`'s encoding is position-preserving, and the `/` separating the leaf from
its parent encodes to `-`, so the observed dir ends with `"-" + <leaf encoding>`
for the winning rule. The one candidate whose leaf encoding is that suffix is
the derived rule.

## How to run

Requirements: `claude` 2.1.x on `$PATH` (authenticated); Go 1.26+.

```sh
go build -o ./bin/probe-cwd-encoding ./cmd/probe-cwd-encoding
./bin/probe-cwd-encoding -trust-folder=accept
```

It is wired into `make e2e` (Makefile `PROBES` + `cmd/e2e-runner` `buildChecks`,
`SuccessMarker: ^OBSERVED`, default 60s timeout). On success, stdout carries the
`OBSERVED:` block; stderr carries the raw `claude` UI plus the recording-dir
path (`probe outDir=<path>`), and the same block is written to
`<recording-dir>/observation.log`.

### Optional flags

- `-trust-folder=accept|fail` (default `accept`) — a fresh temp cwd is never
  trusted, so `claude` always shows the trust modal; `accept` auto-trusts it.
  The runner passes `-trust-folder=accept`.

## Why no automated tests

Repo convention for live-`claude` spikes/probes: there is no `_test.go` in any
`cmd/spike-*` or `cmd/probe-*` (see `spike-one-turn/README.md` § *Why no
automated tests*). The deliverable is the recorded golden, produced by running
the rig against real `claude` — the developer's `make check` gate has no live
`claude`, so it verifies only that the binary builds/vets and is enrolled. The
golden below is filled from the first live `make e2e` run.

## Empirical log

_Awaiting first live `make e2e` run._ Transcribe the first run's `OBSERVED:`
block into the row below, verbatim.

| run date | claude version | leaf | observed projects-dir (byte-for-byte) | derived rule | current `EncodeCwd` matched? |
|----------|----------------|------|----------------------------------------|--------------|------------------------------|
| _pending_ | _pending_ | `Työ😀` | _pending_ | _pending_ | _pending_ |

## Derived rule (for #207)

_Awaiting first live `make e2e` run._ Once observed, state the rule for the
`Työ😀` leaf in a form #207 can pin a unit test against — the leaf-level golden
`Työ😀 → <encoded>` and which of {per-UTF-8 byte, per-UTF-16 code unit,
per-Unicode character} produced it.

### Open questions the first run resolves

- **Does `claude` tolerate the astral char `😀` in a cwd?** The `😀` is what
  separates per-UTF-16 from per-Unicode-character (both give one hyphen for the
  BMP `ö`). The JS-UTF-16 hypothesis predicts `claude` handles it and emits two
  hyphens. If `claude` wedges on it (never idles / never writes JSONL) the first
  run goes red — fallback: swap the leaf to BMP-only `Työ` (still distinguishes
  per-byte from per-character), ship green, and file a follow-up for astral
  behaviour. Decide on the first live run; do not pre-emptively downgrade.
- **Does the whole-path cross-check agree with the leaf-suffix derivation?** The
  probe additionally reports `whole_path_cross_check` (does the observed dir
  equal a candidate's full-string encoding of the canonical cwd?). If it
  disagrees with the leaf-suffix rule, the leaf-suffix wins and the discrepancy
  is a prefix quirk worth a note.
- **NFC/NFD normalisation.** If `claude` (or the filesystem) normalises `ö` to
  `o` + combining diaeresis, the byte/rune/UTF-16 counts shift and
  `derived_rule` may come back `unknown`. That is a real, recordable ground
  truth — capture the raw observed dir and note it here.

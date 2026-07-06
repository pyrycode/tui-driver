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

## Why no automated tests (and the one that should exist)

The **live observation** cannot be unit-tested — the deliverable is a golden
produced by driving the rig against real `claude`, and the developer's
`make check` gate has no live `claude`, so it verifies only that the binary
builds/vets and is enrolled. That part is inherently `make e2e`-only.

The **pure, claude-free transforms** (`perByte` / `perRune` / `perUTF16` /
`deriveRule`) are a different matter: they are deterministic and *should* carry a
table test, precisely to give `make check` teeth for the value #207 pins against.
Code review flagged (PR #209, non-blocking SHOULD-FIX) that they shipped untested
on a **factually false** premise — `cmd/spike-queued-modals/main_test.go` proves
the real repo convention is that claude-free logic in a `cmd/*` binary *does* get
table-tested. The follow-up: add `main_test.go` table-testing `Työ😀 → Ty--- /
Ty------ / Ty--` and `deriveRule`'s leaf-suffix pick (tracked in
[`codebase/206.md`](../../docs/knowledge/codebase/206.md) § Follow-ups, folded
into #207's fix).

## Empirical log

First live run recorded below — the `OBSERVED:` block transcribed verbatim from
the `make e2e` run's `observation.log`.

| run date | claude version | leaf | observed projects-dir (byte-for-byte) | derived rule | current `EncodeCwd` matched? |
|----------|----------------|------|----------------------------------------|--------------|------------------------------|
| 2026-07-06 | 2.1.199 | `Työ😀` | `-private-var-folders-k0-gc07w9ws319b07n0plnw6y8r0000gn-T-probe-cwd-encoding-2549730227-Ty---` | **per-UTF-16 code unit** | **no** — `EncodeCwd` (per-byte) gives `…-Ty------` |

Full `OBSERVED:` block (session `cf16bdbf-c05a-48bf-ac07-f7e127bb0b87`, temp
suffix `…2549730227` elided in the middle for width):

```
OBSERVED: observed_projects_dir=-private-var-folders-…-probe-cwd-encoding-2549730227-Ty---
OBSERVED: leaf=Työ😀 per_byte=Ty------ per_utf16=Ty--- per_rune=Ty--
OBSERVED: derived_rule=per-utf16-code-unit whole_path_cross_check=per-utf16-code-unit
OBSERVED: encode_cwd_current=…-Ty------ matches_observed=false
```

The `…2549730227` segment is the one-run `os.MkdirTemp` random suffix — it is
**not** part of the portable golden. The stable finding #207 pins against is the
**leaf-level** transform in the next section, not the full observed dir.

## Derived rule (for #207)

**`Työ😀 → Ty---` — per-UTF-16 code unit.** Confirmed against real `claude`
2.1.199 (2026-07-06). `claude` encodes the cwd with JS `.replace(/[^a-zA-Z0-9]/g,'-')`
semantics — **one hyphen per UTF-16 code unit**, not per UTF-8 byte:

| char | Unicode | UTF-8 bytes | UTF-16 units | `claude` emits |
|------|---------|-------------|--------------|----------------|
| `ö`  | U+00F6 (BMP)   | 2 | 1 | `-`  (1 hyphen) |
| `😀` | U+1F600 (astral, surrogate pair) | 4 | 2 | `--` (2 hyphens) |

So `Työ😀` → `Ty` + `-` (ö) + `--` (😀) = **`Ty---`** (5 chars). The current
per-byte `EncodeCwd` produces `Ty------` (8 chars) — **wrong**. #207 pins a unit
test on the leaf golden `Työ😀 → Ty---` and reimplements `EncodeCwd` per-UTF-16
code unit (range the string by rune; emit `len(utf16.Encode([]rune{r}))` hyphens
per non-alnum rune — see this probe's `perUTF16` for the reference transform).

**Consequence for the library today:** `EncodeCwd` (and therefore
`SessionJSONLPath` / `WaitForSessionJSONL`, which route through it) computes a
projects-dir path `claude` never writes for **any** cwd containing a multi-byte
char, so those helpers time out against a real non-ASCII cwd. That is the defect
#207 closes; it was latent because every prior derivation exercised only ASCII.

### Open questions the first run resolved

- **Does `claude` tolerate the astral char `😀` in a cwd? → Yes.** It idled,
  accepted trust, wrote the JSONL, and emitted two hyphens for `😀` exactly as
  the JS-UTF-16 hypothesis predicted. The BMP-only `Työ` fallback was **not**
  needed and the astral char stays in the leaf constant.
- **Does the whole-path cross-check agree with the leaf-suffix derivation? →
  Yes.** `whole_path_cross_check=per-utf16-code-unit` matched the leaf-suffix
  rule — no prefix-canonicalisation surprise (`EvalSymlinks` gave the expected
  `/private/var/...` and the ASCII prefix encodes identically under all three
  candidate rules, so the full-string match was unambiguous).
- **NFC/NFD normalisation? → No decomposition observed.** `ö` came through as a
  single BMP code unit (1 hyphen), not `o` + combining diaeresis (which would
  have been 2 units). `derived_rule` resolved cleanly to `per-utf16-code-unit`,
  not `unknown`, so no normalisation caveat applies to this observation.

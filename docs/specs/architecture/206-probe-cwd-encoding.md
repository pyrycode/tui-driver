# #206 — `probe-cwd-encoding`: observe how claude encodes a non-ASCII cwd

[Issue](https://github.com/pyrycode/tui-driver/issues/206) · Split from #166 · Unblocks #207 (the fix, blocked on this finding)

## Context

`tuidriver.EncodeCwd` (`pkg/tuidriver/cwd.go:20-35`) derives the
`~/.claude/projects/<name>` directory claude writes to by mapping every **byte**
outside `[a-zA-Z0-9]` to one `-`. For a multi-byte character such as `ö`
(2 UTF-8 bytes) the byte loop emits **two** hyphens.

Whether that matches claude is unverified. The empirical derivation behind the
current transform (`docs/knowledge/codebase/47.md`, "loop 2 B-4") exercised only
**ASCII** special characters. The `café → caf--` unit case
(`cwd_test.go:31`) is the byte loop describing *its own output* — it was never
observed from claude. claude is a JS/TS CLI; its projects-dir encoder is most
likely `/[^a-zA-Z0-9]/g → '-'` over **UTF-16 code units**, which yields **one**
hyphen per BMP character (é, ö, Cyrillic, CJK) and **two** per astral-plane
character (emoji) — but this is a hypothesis, not a measurement.

This ticket ships a recording rig that observes the golden value from **real
claude** and records it, so #207 can pin a unit test against claude's actual
filesystem behaviour rather than an assumption. It runs under `make e2e` (live
claude), which is exactly why it is split from the fix: the fix's developer gate
is `make check`, which cannot observe live claude. **This ticket does not change
`EncodeCwd`** — it only observes.

## Files to read first

- `pkg/tuidriver/cwd.go:20-35` — `EncodeCwd`, the current per-byte rule. The
  probe reports its output as an **informational** cross-check only; it must
  **not** use it (nor anything routing through it) to *discover* the directory.
- `pkg/tuidriver/jsonl.go:35-91` — `SessionJSONLPath` (line 35, computes the
  path via `EncodeCwd(cwd)`) and `WaitForSessionJSONL` (line ~59). **The probe
  must NOT use either for discovery** — both derive the path from `EncodeCwd`,
  so they can only ever confirm the current transform, never observe a mismatch
  (a wrong `EncodeCwd` → wait on a path claude never wrote → timeout → nothing
  observed). Read to understand precisely what is forbidden and why.
- `cmd/probe-first-prompt-hang/main.go` (whole, ~305 LOC) — the closest
  scaffold to copy: UUID generation, `EnsureClaudeEnv`, `Spawn(SpawnOpts)`,
  idle-wait, `-trust-folder` handling + `AcceptTrust`, prompt send, timestamped
  recording dir under `os.TempDir()`, graceful wall-timeout via
  `context.WithCancelCause`. Defaults `-trust-folder=accept`.
- `cmd/spike-one-turn/main.go:82-192` — the `idle → trust → prompt` sequence and
  the `sessionFileWait = 30s` rationale (deferred-JSONL creation, README
  findings #9/#10). The glob-poll deadline reuses this value and its reasoning.
- `cmd/spike-one-turn/README.md` §"Surprises/findings" #7, #9, #10 — encoded-cwd
  observation format, JSONL-created-only-after-first-input, load-sensitivity.
  The probe README's empirical-log mirrors this "filled post-run" shape.
- `cmd/e2e-runner/main.go:38-51` (marker regexes) + `:210-328` (`buildChecks`) —
  the `Check` literal to add. Reuse `observedSuccess` (`^OBSERVED`). Do **not**
  set `Timeout: probeCheckTimeout` (30s) — the internal glob-poll already needs
  30s; inherit the 60s default.
- `Makefile:11-14` — `PROBES` is an explicit allowlist; append the basename
  (binaries auto-build via the `$(BIN_DIR)/%` rule — no other Makefile edit).
- `.gitignore` (the `/probe-first-prompt-hang` block) — add `/probe-cwd-encoding`
  as a sibling. `*.cast` / `*.log` are already globbed.
- `pkg/tuidriver/session.go:50-98` (`SpawnOpts`) + `:142-168` (`Spawn`) — `Spawn`
  hands `cmd` straight to `StartPTY(cmd)`, so **`cmd.Dir` passes through
  unchanged** — this is the mechanism for launching claude from the non-ASCII
  cwd. `MirrorStderr`, `ShutdownGrace` are the opts to reuse.
- `pkg/tuidriver/keys.go:44` (`AcceptTrust`), `:73` (`SendKeys`);
  `pkg/tuidriver/trust.go:26` (`HasTrustModal`); `pkg/tuidriver/state.go:116`
  (`IsIdle`) — predicates/actions for the idle+trust waits.
- `docs/knowledge/codebase/47.md` §"Patterns established" — `SPIKES`/`PROBES` is
  an allowlist (creating the dir does not enrol it); the dispatcher safety-net
  auto-commit has **dropped a `buildChecks` entry** before (re-diff any
  safety-net commit before signalling done); per-binary `.gitignore` entry is
  required.
- `docs/knowledge/codebase/57.md` — `EncodeCwd` canonicalises via `F_GETPATH`
  (darwin) / `EvalSymlinks` (other); `EvalSymlinks` does **not** case-fold on
  macOS. Relevant only to the *optional* whole-path cross-check, **not** to the
  authoritative leaf-suffix rule derivation (see Design §4).

## Design

A single new binary, `cmd/probe-cwd-encoding`, plus its wiring. It is
**probe-shaped**: a recording/observation rig, not a library regression test. It
drives one trivial claude interaction from a non-ASCII working directory,
discovers the projects-dir claude created **without** `EncodeCwd`, records that
directory byte-for-byte, and derives which encoding rule produced it. It **ships
green whenever it makes an observation** — it never asserts the observation
agrees with `EncodeCwd`.

### 1. Launch claude from a non-ASCII cwd

The probe creates a temporary working directory whose **leaf component** carries
non-ASCII characters, then spawns claude with `cmd.Dir` set to it:

- Create `root, _ := os.MkdirTemp("", "probe-cwd-encoding-*")`, then
  `cwd := filepath.Join(root, nonASCIILeaf)` and `os.Mkdir(cwd, 0o755)`.
- `nonASCIILeaf` is a compile-time Go string constant. **Use
  `"Työ😀"`** — it contains one BMP multi-byte char (`ö`, U+00F6: 2 UTF-8
  bytes / 1 UTF-16 unit / 1 rune) **and** one astral-plane char (`😀`, U+1F600:
  4 UTF-8 bytes / 2 UTF-16 units / 1 rune). This single component discriminates
  all three candidate rules in one observation (see the table in §4). A literal
  in a `.go` source file — no shell interpolation, safe on APFS/ext4/tmpfs.
- `cmd := exec.Command("claude", "--session-id", sessionID)`;
  `cmd.Dir = cwd`; `tuidriver.EnsureClaudeEnv(cmd)`; then
  `tuidriver.Spawn(cmd, tuidriver.SpawnOpts{MirrorStderr: true, ShutdownGrace: 3s})`.
- `sessionID` is a fresh `uuid.NewRandom().String()` — never reused.
- `defer os.RemoveAll(root)` (tidy). Do **not** remove `~/.claude/projects/<dir>`
  claude created — leave the evidence for operator inspection (matches every
  sibling spike, which never clean `projects/`).

A brand-new temp cwd is never trusted, so claude **always** shows the
trust-folder modal here — more deterministic than the repo-root case. Handle it
exactly as `probe-first-prompt-hang` / `spike-one-turn` do: `HasTrustModal` →
`AcceptTrust` → wait for `!HasTrustModal && IsIdle`. Accept the `-trust-folder`
flag (`accept`/`fail`), default `accept`; the runner supplies
`commonArgs = ["-trust-folder=accept"]`.

### 2. Drive one trivial interaction (only to make claude create the JSONL)

Linear state sequence (each stage bounded by a sub-context off a ~50s wall
`context.WithTimeout`, inside the runner's 60s cap):

1. `WaitUntil(IsIdle)`.
2. Trust-modal handling (above).
3. `SendKeys` a trivial prompt — any input works; **we only need claude to
   create the session JSONL, which it defers until first input lands
   (finding #9) — not to complete a turn.** e.g. `"hi\r"`.

There is **no** `TailJSONL`, **no** end-turn detection, **no** watchdog
goroutine. Once the JSONL file exists on disk, the projects-dir exists and is
named — that is the entire observation.

### 3. Discover the projects-dir independently of `EncodeCwd`

After the prompt is sent, poll for claude's session JSONL **by session-id, not
by encoded path**:

- `pattern := filepath.Join(home, ".claude", "projects", "*", sessionID+".jsonl")`.
- Poll `filepath.Glob(pattern)` every `DefaultPollInterval` until it returns a
  non-empty match or a **30s** deadline elapses (same value + reasoning as
  `sessionFileWait`; deferred + load-sensitive JSONL creation).
- On match: `observedDir := filepath.Base(filepath.Dir(match[0]))` — the exact
  directory name claude created, byte-for-byte.

The fresh UUID makes `<session-id>.jsonl` globally unique across all
projects-dirs, so the glob is unambiguous — no stale-match race (the failure
mode `spike-one-turn` finding #9 documents). This path **never calls
`EncodeCwd`, `SessionJSONLPath`, or `WaitForSessionJSONL`** — it reads claude's
ground truth.

Empty glob at deadline → return an error (`"projects-dir for session <uuid> not
found under ~/.claude/projects within 30s"`) → the check goes red. That is the
correct "could not observe" semantics.

### 4. Derive the rule (prefix-independent, from the known leaf)

The authoritative derivation needs only two inputs the probe already has: the
compile-time `nonASCIILeaf` constant and the observed directory name. It does
**not** depend on canonicalising the ASCII prefix.

Implement three local, self-contained transforms in the probe (do **not** import
`EncodeCwd` for these — keep the reference implementations transparent):

- `perByte(s)` — one `-` per non-alnum **byte** (the current `EncodeCwd` rule).
- `perRune(s)` — one `-` per non-alnum **rune** (range over the string).
- `perUTF16(s)` — one `-` per non-alnum **UTF-16 code unit**: for each rune,
  keep it if ASCII-alnum, else emit `len(utf16.Encode([]rune{r}))` hyphens
  (1 for BMP, 2 for astral). This is JS `.replace(/[^a-zA-Z0-9]/g,'-')`
  semantics.

For the leaf `"Työ😀"` the three candidates are distinct, so a single run picks
exactly one rule:

| rule | `ö` → | `😀` → | `Työ😀` → |
|------|-------|--------|-----------|
| per-byte (current) | `--` | `----` | `Ty------` |
| per-UTF-16 code unit | `-` | `--` | `Ty---` |
| per-Unicode character | `-` | `-` | `Ty--` |

**Authoritative match (leaf-suffix).** claude's encoding is
position-preserving, and the `/` separating the leaf from its parent encodes to
`-`, so `observedDir` must end with `"-" + <candidate leaf encoding>`. The
winning rule is the one candidate `c` for which
`strings.HasSuffix(observedDir, "-"+c(nonASCIILeaf))`. This depends only on the
known leaf — immune to any prefix-canonicalisation surprise.

**Optional whole-path cross-check.** Additionally compute
`canonical, _ := filepath.EvalSymlinks(cwd)` and compare `observedDir` to each
candidate's full-string encoding of `canonical`. Report the full-string match as
a bonus; if the leaf-suffix and full-string signals disagree (e.g. a prefix
quirk), the **leaf-suffix wins** for rule derivation and the probe notes the
prefix discrepancy for manual review.

**No hypothesis matched → still green.** If no candidate matches the leaf suffix
(claude did something unmodelled — NFC/NFD normalisation, case-folding, etc.),
the probe records the raw `observedDir`, reports `derived_rule=unknown`, and
**still exits 0**. The probe's job is to record ground truth, not to confirm a
guess — "do not assume."

### 5. Output

Print one or more `OBSERVED:` lines to **stdout** (the runner's `observedSuccess`
= `^OBSERVED` gate) — contract of fields, not a format to copy verbatim:

- `OBSERVED: probe-cwd-encoding session=<uuid> cwd=<cwd> canonical=<canonical>`
- `OBSERVED: observed_projects_dir=<observedDir>`  ← the byte-for-byte golden
- `OBSERVED: leaf=Työ😀 per_byte=<..> per_utf16=<..> per_rune=<..>`
- `OBSERVED: derived_rule=<per-byte|per-utf16-code-unit|per-unicode-character|unknown>`
- `OBSERVED: encode_cwd_current=<EncodeCwd(cwd)> matches_observed=<bool>`  ← informational only

Also write the same block to an `observation.log` under a timestamped recording
dir (`os.TempDir()/probe-cwd-encoding-<ts>/`), and log the dir path to stderr
(the runner mirrors stderr) so a failed run leaves a durable artifact. Exit 0 on
any successful observation; non-zero only when no observation could be made
(spawn failed, never idle, JSONL never appeared).

### 6. README empirical log

Ship `cmd/probe-cwd-encoding/README.md` following the sibling-spike shape
(what-it-does, how-to-run, empirical log). It **must** contain an **Empirical
log** section and a **Derived rule (for #207)** subsection, both marked
`_Awaiting first live make e2e run_` at ship time. Structure the empirical-log
row so the first live run's `OBSERVED:` block transcribes directly:

> | run date | claude version | leaf | observed projects-dir (byte-for-byte) | derived rule | current `EncodeCwd` matched? |

As with every spike README in this repo (all filled post-run — see
`spike-one-turn` timings tables), the **golden value is recorded here from the
first live `make e2e` run**, not by the developer (whose `make check` gate has
no live claude). See Testing strategy for the boundary.

### Wiring checklist (verify by reading each target — do not guess)

1. `Makefile` — append `probe-cwd-encoding` to the `PROBES :=` line
   (`Makefile:12`). No other Makefile change (`$(BIN_DIR)/%` builds it).
2. `cmd/e2e-runner/main.go` `buildChecks` — add one `Check` literal
   (place it next to `probe-first-prompt-hang`):
   `{Name: "probe-cwd-encoding", Kind: "probe", Binary: "probe-cwd-encoding",
   Args: commonArgs, SuccessMarker: observedSuccess}`. **Leave `Timeout` unset**
   → 60s default (the 30s glob-poll must fit inside it).
3. `.gitignore` — add `/probe-cwd-encoding` beside `/probe-first-prompt-hang`.

After committing, **re-diff any dispatcher safety-net auto-commit** — one has
dropped a `buildChecks` entry before (47.md); a probe that builds but is never
enrolled defeats the ticket.

## Concurrency model

Minimal. The only goroutine is the one `Session` owns internally (the PTY reader
feeding the rolling buffer); it is joined by `session.Close()`. The probe body
is otherwise linear: bounded `WaitUntil` waits + a bounded `filepath.Glob` poll
loop, all governed by a single `context.WithCancelCause` (wall-timeout ~50s).
Shutdown: `defer session.Close()` (SIGTERM → 3s grace → SIGKILL) then
`cancelCause`. No `TailJSONL`, no watchdog, no fan-out.

## Error handling

| Failure | Handling | Check verdict |
|---|---|---|
| claude spawn fails | return wrapped error | red (couldn't observe) |
| never reaches idle within wall | ctx deadline → error | red |
| trust modal + `-trust-folder=fail` | error (runner passes `accept`) | red |
| glob empty at 30s deadline | error "projects-dir … not found" | red |
| glob matches >1 (should not with fresh UUID) | use `[0]`, note anomaly on stderr | green |
| no candidate rule matches leaf | record raw, `derived_rule=unknown` | **green** |
| astral char wedges claude | manifests as never-idle / JSONL-never | red → see Open questions |

Red = "could not observe" (a real problem worth surfacing). Green = "observed
and recorded" — **including** the case where the observation contradicts
`EncodeCwd` (that is the expected, useful result).

## Testing strategy

No unit tests — repo convention for live-claude spikes/probes (see
`spike-one-turn/README.md` §"Why no automated tests"; there is no `_test.go` in
any `cmd/spike-*` or `cmd/probe-*`). Verification is by execution against real
claude.

- **Developer gate (`make check`, claude-free):** `go vet ./...` compiles the
  new `main` package; `go test -race ./...` compiles it (no test files). The
  developer verifies: it builds + vets clean, is enrolled in `Makefile` `PROBES`
  **and** `buildChecks`, `.gitignore` entry present, README scaffold (Empirical
  log + Derived rule sections) present with the `_Awaiting first live run_`
  placeholder. The developer **cannot** produce the golden value — that requires
  live claude, which `make check` does not run. This boundary is the reason #166
  was split into #206 (rig) and #207 (fix): the observation is the *output of
  running the rig*, not something derivable in the developer's gate.
- **Live gate (`make e2e`, operator/CI):** the first live run drives real
  claude, prints the `OBSERVED:` block, exits 0 (green). The runner of that
  `make e2e` (code-review's live-capture pass, or the operator) transcribes the
  `OBSERVED:` block into the README's Empirical log + Derived rule sections. That
  recorded golden is what unblocks #207; #207 stays `blockedBy` #206 until it
  exists.

## Open questions

- **Astral-plane char in cwd — does claude tolerate it?** The `😀` in the leaf
  is what separates per-UTF-16 from per-Unicode-character (both give one hyphen
  for BMP `ö`). The hypothesis (JS UTF-16 replace) predicts claude handles it
  and emits two hyphens, but this is unverified — if claude wedges on the astral
  char (never idles / never writes JSONL), the first live run goes red. Fallback:
  swap the leaf constant to BMP-only `"Työ"` (still distinguishes per-byte from
  per-character, satisfying the AC minimum and #207's primary need), ship green,
  and file a follow-up to observe astral behaviour separately. Decide this on
  the first live run — do not pre-emptively downgrade.
- **`EvalSymlinks` vs claude's `getcwd` on the ASCII prefix.** Expected to agree
  for a self-created temp dir (kernel-canonical `/private/var/...`, case as
  created). The authoritative leaf-suffix derivation does not depend on it; it
  affects only the optional whole-path cross-check. Confirm on the first live
  run whether the full-string match also holds.
- **Recording-dir / projects-dir cleanup.** The probe leaves the
  `~/.claude/projects/<observedDir>` claude created (evidence; matches siblings).
  If the accumulation of emoji-named projects-dirs across repeated `make e2e`
  runs becomes noise, add a targeted cleanup in a follow-up — out of scope here.

## Acceptance criteria

- [ ] `cmd/probe-cwd-encoding` drives real claude (pinned `--session-id`) from a
  working directory whose leaf component contains a multi-byte character (`Työ😀`,
  ≥ `Työ`), via `cmd.Dir`.
- [ ] The probe discovers claude's projects-dir **independently of `EncodeCwd`**
  — by globbing `~/.claude/projects/*/<session-id>.jsonl` and taking the parent
  dir's base name — and records that name byte-for-byte. It does **not** use
  `SessionJSONLPath` / `WaitForSessionJSONL`.
- [ ] The recorded finding states the derived rule (per Unicode character / per
  UTF-16 code unit / per UTF-8 byte) via the leaf-suffix match, in a form #207
  can pin a unit test against (the leaf-level golden `Työ😀 → <encoded>`).
- [ ] The probe is wired into `make e2e` (`Makefile` `PROBES` + `buildChecks`),
  reuses `observedSuccess`, and **ships green** — it records/observes and does
  **not** hard-fail against the current per-byte `EncodeCwd`.
- [ ] `.gitignore` has `/probe-cwd-encoding`; README ships with the Empirical-log
  + Derived-rule sections scaffolded for the first live run.

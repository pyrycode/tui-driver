# Spec: fix(spike) — deterministic session ID for JSONL discovery

**Ticket:** [#7](https://github.com/pyrycode/tui-driver/issues/7)
**Size:** S
**Status:** ready for development

## Files to read first

Load these in order; each entry says what to extract.

- `cmd/spike-one-turn/main.go:19-39` — current imports; `flag` and `crypto/rand` (or the new UUID dep — see § Dependency choice) land here.
- `cmd/spike-one-turn/main.go:41-56` — constants block. `sessionFileWait`, `sessionFilePoll` are reused; `sessionFileWait` is bumped (see § Constants).
- `cmd/spike-one-turn/main.go:70-104` — `main()` and the early part of `run()` (`projectsDir` resolution, `exec.Command("claude")`, `pty.Start`). The `claude` spawn line is rewired to pass `--session-id`, and a new pre-spawn flow lands here.
- `cmd/spike-one-turn/main.go:175-196` — current `idle-detected → openSessionJSONL → prompt-written` sequence. The `openSessionJSONL` call signature changes to take the resolved session ID (or, equivalently, the resolved deterministic path); the log line gains the resolved path it already had.
- `cmd/spike-one-turn/main.go:392-489` — `projectsDir`, `encodeCwd`, `openSessionJSONL`, `newestJSONL`, `errNoSessionJSONL`. `newestJSONL` and its mtime heuristic are deleted; `openSessionJSONL` is rewritten to poll a known path (see § Discovery rewrite).
- `cmd/spike-one-turn/README.md:40-58` — *How to run*; the new optional flag is documented here.
- `cmd/spike-one-turn/README.md:60-77` — *Required state log lines (in order)*; a new pre-spawn line is inserted.
- `cmd/spike-one-turn/README.md:100-108` — *Observed timings* table; Run 4 + at least one successful run are appended.
- `cmd/spike-one-turn/README.md:180-200` — finding #8 (so finding #9 follows the same shape). Finding #9 is **authored fresh** in this ticket; it does not yet exist in the README.
- `docs/specs/architecture/1-spike-one-turn.md:31-34` — *Empirical findings already captured*; fact #1 (`<encoded-cwd>/<session-id>.jsonl` layout) is the empirical backing for the deterministic-path design.
- `docs/specs/architecture/1-spike-one-turn.md:101-115` — *JSONL discovery and tailing*. The pseudocode (snapshot → poll-for-new-file) is now stale twice over (#3 already partly replaced it, #7 replaces it again). Reconcile to the deterministic-path model.
- `docs/specs/architecture/3-jsonl-discovery-fix.md` — predecessor spec. Read § *Design / JSONL discovery* and § *Open questions* to understand why mtime was chosen and what it failed at; the operator's vault note `📋 Projects/2026-05-16 - tui-driver/Findings.md` records the run-4 evidence.
- `docs/specs/architecture/4-thinking-detected-optional.md` — predecessor spec. Confirms the state machine is now fast-path-friendly so a successful `SUCCESS:` run is achievable once discovery is fixed.

## Context

Spike #1 ([+#3, +#4](../../../cmd/spike-one-turn/README.md)) is one fix away from its first end-to-end `SUCCESS`. Run 4 (post-#3 + #4) failed in a new shape: the JSONL tailer opened `dddf559a-…jsonl` (a stale file from a prior spike run) and tailed an inert byte range while claude's actual new session JSONL — `9707673d-…jsonl` — was being written ~7 s later (revealed by the `claude --resume 9707673d-…` banner). Claude received "What is 2+2?" and answered `"4"` with `stop_reason=end_turn` in the *correct* session file within ~1 s; the spike just opened the wrong file.

Root cause: `newestJSONL`'s mtime-only heuristic returns whichever `.jsonl` was newest *at the call instant*, and that instant (immediately after `idle-detected`) precedes claude's first JSONL write to the new session by several seconds. With ≥5 stale files now accumulated in the projects-cwd from prior spike runs, every future run hits this.

This ticket eliminates the discovery race entirely by pinning claude's session ID at startup. Two paths: generate a fresh UUID by default (the common case) or accept an operator-supplied UUID via a new optional CLI flag (debugging / parallel-tail / deterministic testing). The deterministic JSONL path is then computed from the resolved ID and `~/.claude/projects/<encoded-cwd>/`, and discovery becomes "wait for *this exact file* to exist."

The CLI shape (generate-by-default / accept-override) is the **library API seed** for the post-spike `pkg/tuidriver/` extraction. Only the spike-level pattern lands here; the library API surface is out of scope.

## Design

### Overview of the change

All production-code edits are inside `cmd/spike-one-turn/main.go`. The reader goroutine, watchdog, shutdown sequence, state machine, JSONL tailer body, JSONL parser tolerance, and assistant-text extraction are unchanged. Four things move:

1. A new `--session-id` flag is parsed at startup. Empty → generate a fresh UUID; non-empty → validate and use as-is.
2. The resolved session ID is logged together with its deterministic JSONL path **before** `pty.Start` so an operator can `tail -f <path>` or `claude --resume <id>` from another terminal while the spike runs.
3. The `claude` spawn is rewired from `exec.Command("claude")` to `exec.Command("claude", "--session-id", sessionID)`.
4. `openSessionJSONL` is rewritten to wait for the deterministic path to exist with a generous timeout. `newestJSONL` is deleted in full.

### Dependency choice

Use `github.com/google/uuid` for generation (`uuid.NewRandom()`) and validation (`uuid.Parse`). Rationale:

- The library is the de-facto standard in Go and ~4 KB compiled.
- Validation requires UUID parsing — hand-rolling against `crypto/rand` and a regex is over-engineered for a spike-quality binary and offers no behavioral upside.
- The eventual `pkg/tuidriver/` consumer (`pyry acp`) will want a typed `uuid.UUID` field on its options struct anyway; using the same library here keeps the seed and the long-term API aligned.

Add the dep with `go get github.com/google/uuid` as part of the implementation; `go.mod` and `go.sum` move with the commit.

### CLI flag

Use stdlib `flag`. One flag:

- `-session-id <uuid>` — optional. Default `""`. When non-empty, the value must parse as a valid UUID via `uuid.Parse`; on failure, print a one-line usage error to stderr and exit non-zero. (The `flag` package's auto-generated usage is enough — no custom help text needed; the README documents the flag.)

Flag parsing happens at the top of `main()` (or, equivalently, at the top of `run()` before any logger setup). On usage error, exit code 2 is conventional for flag errors; `flag.ExitOnError` (the default for `flag.CommandLine`) already produces this behavior — accept it.

### Session ID resolution

A small helper resolves the operator-supplied flag into the session ID + deterministic path:

```
func resolveSession(flagValue string, dir string) (sessionID string, jsonlPath string, err error)
```

Behavior contract:

- If `flagValue == ""` → generate via `uuid.NewRandom()`, format with `.String()` (lowercase, hyphenated — matches claude's filename convention).
- If `flagValue != ""` → parse via `uuid.Parse`. On error, return `fmt.Errorf("invalid --session-id: %w", err)`. The caller surfaces this as the usage-style error (see § Error handling).
- `jsonlPath = filepath.Join(dir, sessionID + ".jsonl")`.

The function does **not** stat the path or check the directory exists — that's the discovery loop's job.

### Pre-spawn logging (new state line)

After `projectsDir()` resolves and `resolveSession(...)` returns, log:

```
session-id-resolved id=<uuid> jsonl=<absolute-path>
```

The line fires **before** `pty.Start(cmd)`. This is the operator-tail hook the AC asks for: at the moment this line appears in stderr, the operator can copy the path or the id and run `tail -f <path>` or `claude --resume <id>` in another terminal, with the guarantee that the path is what the spike will read from.

The log token `session-id-resolved` joins the *Required state log lines* list (see § README updates).

### Spawn rewire

```
cmd := exec.Command("claude", "--session-id", sessionID)
```

No other change to the spawn flow. `cmd.Env` setup, `pty.Start`, `pty.Setsize` all unchanged.

`claude --help` (v2.1.143) documents the flag as `Use a specific session ID for the conversation (must be a valid UUID)`; the empirical record (README finding #1 and `1-spike-one-turn.md` § *Empirical findings* fact #1) confirms claude writes to `<session-id>.jsonl` in `~/.claude/projects/<encoded-cwd>/`. See § Open questions for the snapshot-then-diff fallback if implementation reveals an unexpected behavior.

### Discovery rewrite (`openSessionJSONL`)

New signature:

```
func openSessionJSONL(jsonlPath string) (offset int64, err error)
```

The function no longer returns `path` because the caller already knows it (computed at resolve time and emitted in `session-id-resolved`).

Behavior contract:

- Poll `os.Stat(jsonlPath)` every `sessionFilePoll` (100 ms) until the file exists.
- On first successful stat, return `info.Size()` as the offset. The tailer will seek to this offset and only see lines appended in response to the prompt — same behavior as today; the difference is *which* file we're tailing.
- On `os.IsNotExist`, retry. On any other stat error, return wrapped.
- If the path does not appear within `sessionFileWait` (≥10 s; see § Constants), return a named error: `session JSONL did not appear at <path> within <timeout>`. This is structurally distinct from "no session JSONL found" (the old `errNoSessionJSONL` shape) — there is no more directory scan.

`newestJSONL` and the `errNoSessionJSONL` sentinel are **deleted in full**. They have no remaining callers (verified via codegraph: `newestJSONL` is called only by `openSessionJSONL`).

Call-site change in `run()` (around current line 186): `openSessionJSONL(projDir)` → `openSessionJSONL(jsonlPath)` (where `jsonlPath` is the value returned from `resolveSession` and previously logged in `session-id-resolved`). The subsequent `session-jsonl-opened path=… offset=…` log line is unchanged in shape; the `path=` value is now identical to the `jsonl=` value already logged at startup, which is fine — it confirms the loop succeeded.

### Constants

```
sessionFileWait = 1 * time.Second   → 10 * time.Second   (bumped)
sessionFilePoll = 100 * time.Millisecond   → KEEP
```

The bump is the AC's "generous timeout" requirement. Run 4's empirical gap (idle → new JSONL mtime) was ~7 s; 10 s leaves headroom without making genuine failures (claude crashed before opening its JSONL) wait excessively.

### Concurrency model

Unchanged. Same three goroutines (PTY reader, watchdog, JSONL tailer), same shutdown sequence, same context propagation. The JSONL tailer still starts immediately after `prompt-written` with the path + offset already resolved.

### Error handling

- `flag.Parse` failure (bad UUID) → printed by `flag.CommandLine` via `flag.ExitOnError`, then `os.Exit(2)`. We invoke `flag.Parse` ourselves; on the validation failure we go through `fmt.Fprintf(os.Stderr, "invalid --session-id: %v\n", err)` + `flag.Usage()` + `os.Exit(2)` so the operator sees a single coherent message rather than two.
- `resolveSession` returning the validation error → as above.
- `openSessionJSONL` timeout → propagate from `run()` as `fmt.Errorf("open session jsonl: %w", err)`. The error message names the expected path and the timeout (per AC #2).
- All other failure modes unchanged.

### README updates (required, in this same commit)

`cmd/spike-one-turn/README.md`:

1. **§ *How to run*** — under the `go build … && /tmp/spike-one-turn` block, add an *Optional flags* subsection (one paragraph) documenting `-session-id <uuid>` with the generate-if-absent default behavior. Note that the resolved ID is emitted as `session-id-resolved …` before claude spawns so an external `tail -f` is feasible.

2. **§ *Required state log lines (in order)*** — insert `session-id-resolved id=<uuid> jsonl=<path>` as the **first** state line (it fires before `idle-detected`). No other line in the table changes.

3. **§ *Observed timings*** — append two rows: Run 4 (the failure that motivated this ticket; outcome column: `FAIL — opened stale .jsonl, watchdog: stuck in state prompt-written for 1m1s`) and at least one successful run produced by the implementation (with all timing columns filled and outcome `SUCCESS: 4`). If the operator-supplied-flag path is also run, add a second SUCCESS row labelled accordingly.

4. **§ *Surprises / findings*** — author finding **#9** *JSONL discovery picks stale file when prior runs left .jsonl files in cwd*. Document:
   - The empirical evidence from Run 4 (state log + the `Resume this session with: claude --resume 9707673d-…` banner showing the file mismatch).
   - Why the mtime heuristic from #3 was insufficient (its assumption that "claude touches its log during startup faster than we can stat" was wrong by ~7 s; the new file is mtime-newest only *after* it's created).
   - The chosen mechanism (`claude --session-id <uuid>` + deterministic path).
   - The observed timings from the new successful run.
   - If the snapshot-then-diff fallback (see § Open questions) had to be chosen instead, note what surprised the architect about `--session-id`.

`docs/specs/architecture/1-spike-one-turn.md`:

1. **§ *JSONL discovery and tailing*** (lines 101–115) — replace the snapshot+poll pseudocode with the deterministic-path model:
   - The spike resolves a session ID (generated by default, or operator-supplied via `-session-id`) before spawning `claude`.
   - It spawns `claude --session-id <uuid>`, which guarantees the session JSONL path is `~/.claude/projects/<encoded-cwd>/<session-id>.jsonl`.
   - After `idle-detected`, the spike polls `os.Stat` on that exact path with a ≥10 s timeout, then records the current file size as the tail start offset.
   - The 100 ms poll cadence is retained; the directory snapshot and the new-file diff are gone.

2. **§ *Empirical findings already captured*** — fact #1 (the `<encoded-cwd>/<session-id>.jsonl` layout) is reaffirmed by this ticket and now load-bearing for discovery; no edit needed, but the developer should re-read it to internalize that the deterministic path *only* works because of that flat layout.

`docs/knowledge/architecture/` and `docs/knowledge/decisions/` are **not** touched by this ticket — documentation phase owns reconciliation.

### Code blocks summary

The diff is localized:

- **Add**: `flag.StringVar` (one line) + `flag.Parse` invocation + `resolveSession` helper (~10 lines) + pre-spawn log line (one line) + UUID dep import.
- **Modify**: the `exec.Command` line (one arg change) + `openSessionJSONL` signature + body (~15 lines, replacing the mtime loop with a `Stat` loop) + `sessionFileWait` constant value.
- **Delete**: `newestJSONL` (~30 lines) + `errNoSessionJSONL` sentinel.
- **Unchanged**: everything else.

Operator's 25–50 LOC estimate is realistic; the deletion of `newestJSONL` offsets the additions and the production net is closer to ~+20 LOC.

## Testing strategy

No automated tests (consistent with spike #1, #3, #4 policy — `docs/specs/architecture/1-spike-one-turn.md` § *Testing strategy*). Verification by execution:

1. `go build ./cmd/spike-one-turn` succeeds (catches the new `google/uuid` dep wiring).
2. **Default-flag run.** `/tmp/spike-one-turn` (no flag). Confirm:
   - stderr's first state line is `session-id-resolved id=<some-uuid> jsonl=<absolute-path>`, **before** any claude UI bytes appear.
   - `<absolute-path>` exists at the moment `session-jsonl-opened …` fires.
   - stdout ends with `SUCCESS: <text>` and stderr contains `end-turn-detected`.
   - Record the timing row in the README.
3. **Operator-supplied-flag run.** Generate a UUID locally (`uuidgen | tr 'A-Z' 'a-z'`), pass via `-session-id <uuid>`. Confirm:
   - stderr's `session-id-resolved` line emits the **same** UUID the operator passed.
   - The JSONL file that appears on disk matches the deterministic path.
   - Stdout ends with `SUCCESS: <text>`.
   - Record the second timing row in the README.
4. **Bad-UUID run.** Pass `-session-id not-a-uuid`. Confirm the spike exits non-zero before spawning claude (no `pyrycode-claude` process should be visible via `pgrep -lf claude` during or after) and stderr names the validation failure.
5. **Stale-file regression check.** With ≥5 unrelated `.jsonl` files already present in `~/.claude/projects/<encoded-cwd>/` (the original failure shape), repeat step 2. The spike should still succeed — the deterministic path makes pre-existing stale files irrelevant to discovery.
6. Verify clean exit per README: `pgrep -lf 'claude$'` returns nothing after each run.

AC #4's verification clause is satisfied by steps 2 and 3 together. AC #1's "fails fast on invalid UUID" by step 4. AC #2's "no mtime heuristic, no directory scan" is structural (the only filesystem access in `openSessionJSONL` is a stat on a known path).

## Open questions

- **`claude --session-id` validation.** The help text confirms the flag exists in v2.1.143 and the empirical filename pattern is `<session-id>.jsonl`, which strongly implies passing the flag produces the deterministic path. If implementation reveals an unexpected behavior (claude rejects the supplied UUID, writes to a different path, fails silently when the directory is missing, mutates the UUID before using it as the filename), abandon the `--session-id` design and switch to **snapshot-then-diff**:
  - At spike startup (after `projectsDir` resolves, before `pty.Start`), record the set of pre-existing `.jsonl` filenames in `projDir`.
  - Spawn `claude` *without* `--session-id`.
  - After `idle-detected`, poll `os.ReadDir(projDir)` for a `.jsonl` whose name is NOT in the snapshot set, with the same ≥10 s timeout.
  - The deterministic-log-line shape becomes `session-jsonl-discovered path=…` (post-idle, no `session-id-resolved` pre-spawn line) — operator-tail-from-another-terminal is forfeited in this fallback.
  - Document what surprised the architect about `--session-id` in finding #9 and link this open question.
- **UUID library footprint.** `github.com/google/uuid` is small but the spike's prior policy ("avoid `fsnotify`, polling is fine") leaned toward dep minimalism. The trade-off here is favoring the library API seed alignment with consumers over zero-dep purity; if the operator pushes back, swap to a hand-rolled `crypto/rand`-based v4 generator + a regex validator (~15 extra LOC). Not worth pre-empting in the spec.
- **Empty cwd directory.** If `~/.claude/projects/<encoded-cwd>/` does not yet exist when `os.Stat(jsonlPath)` runs, the stat returns `os.IsNotExist`, which the poll loop treats the same as "file not present" and retries. claude itself will create the directory + file during its startup, well within the 10 s timeout. No special handling needed; document the observation in finding #9 if a fresh-cwd run shows a longer-than-expected wait.

## Out of scope (reminder, mirrors ticket)

- Multi-turn / tool-use / cancellation exploration (covered by upcoming exploration spike #2).
- Extracting reusable primitives into `pkg/tuidriver/` (deliberate post-spike work; the CLI flag shape is the library API seed only).
- Capturing thinking-spinner verbs more reliably (separate refinement; finding #8).
- The full `pkg/tuidriver` options API — only the spike-level CLI flag pattern lands here.
- Updating `docs/knowledge/architecture/jsonl-layout.md` or `system-overview.md` — documentation phase owns those.

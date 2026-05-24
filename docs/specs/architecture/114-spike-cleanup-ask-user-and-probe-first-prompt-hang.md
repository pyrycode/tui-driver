# Spec — #114: spike-cleanup: migrate spike-ask-user + probe-first-prompt-hang off duplicated helpers

Two-file cleanup. Mirrors the just-landed migrations of `cmd/spike-cancel` (PR #120, #117) and `cmd/spike-permission` (PR #121, #118). `cmd/spike-ask-user/main.go` is structurally a single-session twin of spike-cancel's probe-zero (idle → trust → typePrompt → tail → end-turn); the migration is a strict subset of #117. `cmd/probe-first-prompt-hang/main.go` is the observational sibling — it tails *all* envelope types (not assistant-only) and writes a timestamped raw-line log alongside the parsed marker stream. The probe's tail goroutine therefore needs a single-consumer body wrapped around `tuidriver.TailJSONL`, not just a 1:1 helper swap. The probe's "first-PTY-byte" marker goroutine is NOT a watchdog (no Tracker, no spinner/quiet observation) and stays as-is; see § probe-first-prompt-hang deltas.

## Files to read first

- `cmd/spike-ask-user/main.go` — the first file being edited. Specifically:
  - lines 57-60 (`jsonlTailInterval`, `sessionFilePoll`, `watchdogTick`) — deleted constants.
  - lines 79 (`spinnerRe`) and 345-360 (`matchSpinner`) — deleted; only consumer was the inline watchdog.
  - lines 104-108 (`projDir, err := projectsDir()` + `projects-dir path=%s` log) — deleted per precedent (#116, #117, #118).
  - lines 143-164 (inline watchdog goroutine with `matchSpinner` + `tr.ObserveSpinner` + `tr.CheckWatchdog`) — replaced by `tuidriver.RunWatchdog` wrapper (spike-cancel:226-234 shape).
  - line 177 (trust-folder accept `ptmx.Write([]byte("1\r"))`) — STAYS (out of scope, matches all four precedent migrations).
  - lines 208-211 (`openSessionJSONL(jsonlPath)` + log) — replaced by `tuidriver.WaitForSessionJSONL(ctx, jsonlPath)` with a `sessionFileWait`-bounded ctx (spike-cancel:274-280 shape).
  - lines 213-223 (channel + tailer goroutine wrapping local `tailJSONL`) — replaced by `tuidriver.TailJSONL(rootCtx, jsonlPath, 0)`; the `wg.Add(1)/go func` wrap is deleted entirely (library owns the goroutine).
  - lines 288-306 (end-turn receive loop) — receive site adopts the spike-cancel:564-575 closed-channel guard, `ev.Type != "assistant"` filter (re-establishes the pre-migration assistant-only emit), and `tuidriver.IsEndTurn(ev)` instead of local `isEndTurn(ev)`.
  - lines 282-284 (`ptmx.Write(answerBytes)` for the answer keystroke) — STAYS as `ptmx.Write` for now (matches the trust-accept precedent; see § spike-ask-user deltas item 4).
  - lines 362-372 (`projectsDir`), 375-386 (`openSessionJSONL`), 388-427 (`tailJSONL`), 429-433 (`isEndTurn`) — deleted per AC #1.
  - lines 313-343 (`extractAskUserQuestion`, `hasAskUserModal`) — STAY verbatim (spike-specific predicates, no library counterpart).
- `cmd/probe-first-prompt-hang/main.go` — the second file being edited. Specifically:
  - lines 42-43 (`jsonlTailInterval`, `sessionFilePoll`) — deleted constants.
  - lines 131-134 (`home, _ := os.UserHomeDir(); cwd, _ := os.Getwd(); jsonlPath := filepath.Join(home, ".claude", "projects", tuidriver.EncodeCwd(cwd), sessionID+".jsonl")`) — replaced by `home, _ := os.UserHomeDir(); cwd, _ := os.Getwd(); jsonlPath := tuidriver.SessionJSONLPath(home, cwd, sessionID)`.
  - lines 167-184 (first-PTY-byte marker goroutine polling `rb.LastAppendAt()`) — STAYS verbatim. This is NOT a wedge watchdog — no Tracker, no `ParseSpinner`, no `CheckWatchdog`. Replacing it with `RunWatchdog` would actively defeat the probe's purpose (the probe is designed to characterise hangs; a wedge detector would abort runs the probe needs to observe). See § probe-first-prompt-hang deltas item 1.
  - lines 219-222 (`openSessionJSONL(rootCtx, jsonlPath)` + marker) — replaced by `tuidriver.WaitForSessionJSONL` (no ctx-timeout shrink — current code uses `sessionFileWait` indirectly via the deadline inside the local helper; keep the same 30 s budget by wrapping `rootCtx` with `context.WithTimeout`).
  - lines 226-246 (tail goroutine wrapping local `tailJSONLAll` with three behaviours: raw-line log, deferred_tools_delta extraction, onEvent callback) — replaced by a single consumer goroutine that reads from `tuidriver.TailJSONL(rootCtx, jsonlPath, 0)` and performs the three behaviours inline per entry. See § probe-first-prompt-hang deltas item 3.
  - lines 318-336 (`openSessionJSONL`), 338-404 (`tailJSONLAll`), 406-410 (`isEndTurn`) — deleted per AC #2.
  - lines 304-316 (`timestampedWriter` PTY mirror — used by `tuidriver.Spawn`) — STAYS verbatim. Out of scope.
- `cmd/spike-cancel/main.go` post-PR #120 — the closest single-session template for both files:
  - lines 170-189 — `resolveSession` + `home`/`cwd` resolution + `tuidriver.SessionJSONLPath` (relevant only to spike-ask-user; probe-first-prompt-hang generates UUIDs differently and stays single-call).
  - lines 209-234 — `Spawn` + `defer Close` + `wg.Add(1)/go func` `RunWatchdog` wrap (the canonical shape for spike-ask-user's watchdog replacement).
  - lines 267-287 — channel slot + `probe1Hook` (`WaitForSessionJSONL` → `TailJSONL` → assign to outer channel). Spike-ask-user uses the same shape but flattened — no hook, no probe loop; the wait+tail happens inline after `typePrompt`.
  - lines 553-580 — closed-channel + assistant-only filter + receive-case pattern. The canonical reference for spike-ask-user's end-turn loop.
- `cmd/spike-long-prompt/main.go:175-198` — the cleanest "flattened" template (no probes, no hooks; linear `WritePrompt → WaitForSessionJSONL → Events → for-range`). Spike-ask-user is structurally closer to this than to spike-cancel; the only difference is spike-ask-user uses `Session.Events` indirectly via `TailJSONL` because it needs raw `JSONLEntry` access for `IsEndTurn`, not the higher-level `EventKindJsonlEndOfTurn`.

  Note: `Session.Events` exists as a higher-level wrapper (`cmd/spike-long-prompt/main.go:195`) that fuses PTY events with JSONL entries. Spike-ask-user could in principle adopt it, but doing so would expand the diff beyond mechanical cleanup (the existing loop only cares about end-turn detection, not PTY-thinking / PTY-idle markers). Stay on `TailJSONL` for minimal-diff parity with spike-cancel; the per-probe `Events`-vs-`TailJSONL` choice is its own future ticket if a consumer needs both spike binaries on the same surface.
- `pkg/tuidriver/jsonl.go:35, 58, 171, 297-305` — `SessionJSONLPath(home, cwd, sessionID) string`, `WaitForSessionJSONL(ctx, path) error`, `TailJSONL(ctx, path, offset) (<-chan JSONLEntry, error)`, `IsEndTurn(e JSONLEntry) bool`. `TailJSONL` emits ALL entry types (no assistant-only filter) and closes the channel on ctx cancel. `IsEndTurn` is stricter than the local `isEndTurn` — requires assistant + non-empty text in addition to `stop_reason == "end_turn"`; see § Behavioural notes.
- `pkg/tuidriver/jsonl.go:80-135` — `JSONLEntry`, `EntryMessage`, `ContentBlock`. `RawLine` is the verbatim source-line bytes with trailing `\r\n` or `\n` stripped — load-bearing for probe-first-prompt-hang's timestamped raw-line log (the consumer adds the `\n` back when writing to `jsonlLog`).
- `pkg/tuidriver/watchdog.go:66` — `RunWatchdog(ctx, buf, tr, WatchdogOpts{}) error`. Internally drives `ParseSpinner` + `Tracker.ObserveSpinner` + `Tracker.CheckWatchdog`. Replaces spike-ask-user's inline watchdog AND its local `matchSpinner` consumer in one substitution.
- `pkg/tuidriver/session.go:166-230` — `WritePrompt`, `TypePrompt`, `ClearInputLine`, `Write`. Spike-ask-user already uses `session.TypePrompt` and `session.ClearInputLine` (lines 197-200) — no further migration needed on the input-write side. The `Session.Write` exposed at lines 138-140 is the API the answer-keystroke (line 282) and trust-accept (line 177) write paths COULD route through; both stay as `ptmx.Write` for now to minimise diff (see § spike-ask-user deltas item 4).
- `pkg/tuidriver/cwd.go:1-40` — `EncodeCwd(cwd) string`. Already imported by both files; the `projectsDir`/inline `filepath.Join` call sites collapse into `SessionJSONLPath` which re-uses `EncodeCwd` internally.
- `docs/specs/architecture/117-spike-cancel-migrate-off-duplicated-helpers.md` — the close-precedent spec. Single-session shape. Both files in this slice are structurally subsets of #117.
- `docs/specs/architecture/118-spike-permission-migrate-off-duplicated-helpers.md` — the precedent spec covering helper-shape adaptations (`isEndTurn` → `tuidriver.IsEndTurn` Behavioural notes; assistant-only filter re-establishment at receive sites; closed-channel guard pattern). Apply the same patterns at the corresponding sites in spike-ask-user.

## Context

PRs #87/#89/#60/#98/#99 promoted the load-bearing helpers from spike binaries into `pkg/tuidriver/`. PRs #115/#116/#117/#118 finished the equivalent migrations for spike-one-turn / spike-multi-turn / spike-cancel / spike-permission. `cmd/spike-ask-user/main.go` and `cmd/probe-first-prompt-hang/main.go` are the last two non-typing binaries still hand-rolling these primitives. This ticket finishes the cleanup so every spike + probe binary exercises the same library APIs consumers will use.

Mechanical cleanup. No new behaviour. The two binaries are independent; neither is the other's consumer.

Split from #107.

## Design

### Substitution table (applies to both files)

| Local symbol (delete/adapt)                  | Library replacement (use)                                     |
|----------------------------------------------|---------------------------------------------------------------|
| `func projectsDir` (spike-ask-user only)     | `tuidriver.SessionJSONLPath(home, cwd, sessionID)` inline     |
| inline `filepath.Join(home, ".claude", "projects", tuidriver.EncodeCwd(cwd), sessionID+".jsonl")` (probe-first-prompt-hang) | `tuidriver.SessionJSONLPath(home, cwd, sessionID)` inline |
| `func openSessionJSONL`                      | `tuidriver.WaitForSessionJSONL(ctx, jsonlPath)` w/ `sessionFileWait` timeout |
| `func tailJSONL` (spike-ask-user)            | `tuidriver.TailJSONL(ctx, jsonlPath, 0)`                      |
| `func tailJSONLAll` (probe-first-prompt-hang) | `tuidriver.TailJSONL(ctx, jsonlPath, 0)` + per-entry consumer goroutine for raw-line log + deferred_tools_delta marker + first-assistant / end-turn detection (see § probe-first-prompt-hang deltas item 3) |
| `func isEndTurn`                             | `tuidriver.IsEndTurn(ev)`                                     |
| `var spinnerRe` + `func matchSpinner` (spike-ask-user) | (deleted — only consumer was the inline watchdog) |
| inline watchdog goroutine (spike-ask-user)   | `tuidriver.RunWatchdog(ctx, rb, tr, tuidriver.WatchdogOpts{})` wrapped in the same `wg.Add(1)/go func` shape spike-cancel uses |
| `eventCh chan map[string]any` (spike-ask-user, cap 32) | `<-chan tuidriver.JSONLEntry` returned by `TailJSONL` |

### spike-ask-user `run()` body changes

The body keeps the same linear flow (tracker → spawn → defer-close → watchdog → idle wait → trust modal → ClearInputLine + TypePrompt → wait for JSONL → tail → PTY modal-detect → snapshot → answer keystroke → wait for end-turn). Per-phase deltas:

1. **`projectsDir()` call** (lines 104-108). Delete the `projDir, err := projectsDir(); ...; logger.Printf("projects-dir path=%s", projDir)` block. Replace with `home`/`cwd` resolution (`os.UserHomeDir()` + `os.Getwd()` with the same `fmt.Errorf("resolve home dir: %w", ...)` / `fmt.Errorf("resolve cwd: %w", ...)` wrap shape spike-cancel:180-187 uses). Then derive `jsonlPath` via `tuidriver.SessionJSONLPath(home, cwd, sessionID)`.

   The existing `session-id-resolved id=%s jsonl=%s` log line at line 112 stays verbatim.

2. **Inline watchdog goroutine** (lines 143-164). Replace with the spike-cancel:226-234 pattern:

   ```go
   wg.Add(1)
   go func() {
       defer wg.Done()
       if err := tuidriver.RunWatchdog(rootCtx, rb, tr, tuidriver.WatchdogOpts{}); err != nil {
           logger.Printf("%v", err)
           cancelCause(err)
       }
   }()
   ```

   `RunWatchdog` owns its own 1 Hz tick (`DefaultWatchdogTick`), drives `ParseSpinner` + `Tracker.ObserveSpinner` + `Tracker.CheckWatchdog` internally, and returns on ctx cancel or wedge. Subsumes `matchSpinner` entirely.

   Important: spike-ask-user bumps `ptyQuietLimit` to `120 * time.Second` (line 67) precisely because the AskUserQuestion modal is by design quiescent. That value is preserved on the existing `NewTracker(TrackerOpts{...})` call (lines 117-120) — the constant stays, only the inline watchdog body changes. `RunWatchdog` reads the limit through the Tracker, so the bump continues to apply.

3. **`openSessionJSONL(jsonlPath)` call** (lines 208-211). Replace with `tuidriver.WaitForSessionJSONL` wrapped in a `sessionFileWait`-bounded ctx:

   ```go
   jsonlCtx, jsonlCancel := context.WithTimeout(rootCtx, sessionFileWait)
   jsonlErr := tuidriver.WaitForSessionJSONL(jsonlCtx, jsonlPath)
   jsonlCancel()
   if jsonlErr != nil {
       return fmt.Errorf("open session jsonl: %w", jsonlErr)
   }
   logger.Printf("session-jsonl-opened path=%s", jsonlPath)
   ```

   Mirrors spike-cancel:274-280 exactly. The existing `session-jsonl-opened` log line at line 211 stays verbatim.

4. **Tail goroutine** (lines 213-223). Replace the `eventCh := make(chan map[string]any, 32)` + `wg.Add(1)/go func` + local `tailJSONL` block with:

   ```go
   eventCh, terr := tuidriver.TailJSONL(rootCtx, jsonlPath, 0)
   if terr != nil {
       return fmt.Errorf("open events stream: %w", terr)
   }
   ```

   The `wg.Add(1)` wrap is deleted entirely — `TailJSONL` owns its own goroutine and closes the channel on ctx cancel. `eventCh` is now `<-chan tuidriver.JSONLEntry`. The watchdog `wg.Add(1)` (per item 2 above) is still there — `wg` is not removed.

5. **End-turn receive loop** (lines 288-306). Receive site adopts spike-cancel:564-575:

   ```go
   case ev, ok := <-eventCh:
       if !ok {
           eventCh = nil
           continue
       }
       if ev.Type != "assistant" {
           continue
       }
       if tuidriver.IsEndTurn(ev) {
           gotEnd = true
           tr.RecordTransition("end-turn-detected")
           logger.Printf("end-turn-detected")
       }
   ```

   The `ev.Type != "assistant"` filter re-establishes the local `tailJSONL`'s pre-migration assistant-only emit (lines 409-411 of the current file). `tuidriver.IsEndTurn` also enforces the assistant guard internally, so the filter is doubly safe. The closed-channel guard (`eventCh = nil`) prevents a tight loop on a closed channel during shutdown.

### spike-ask-user deltas (vs. the #117 / #118 precedents)

Four things this spec covers that diverge from the spike-cancel / spike-permission migrations:

1. **No probe loop.** Spike-ask-user is single-session, single-input — the linear shape mirrors spike-long-prompt rather than the probe-based spike-cancel. The `wait for JSONL → tail → end-turn` block lives directly in `run()`, not inside a `runProbe` function. No `eventChRef` pointer indirection — `eventCh` is a local in `run()`.

2. **AskUserQuestion modal detection (PTY)** (lines 230-242). The poll-for-`hasAskUserModal` loop and the `hasAskUserModal` predicate at lines 339-343 STAY verbatim — this is spike-specific behaviour with no library counterpart. The bytes snapshot at lines 252-271 also stays. The `extractAskUserQuestion` helper at lines 315-330 stays — also spike-specific.

3. **`oscRe` STAYS** (line 78). Used at line 257 (`oscRe.ReplaceAll(tuidriver.StripANSI(snap), nil)`) for OSC-sequence stripping in the modal snapshot. Library `StripANSI` strips CSI, not OSC; the OSC-strip is an extra modal-specific cleanup that's not in scope for this slice. Keep the local regex.

4. **Answer keystroke STAYS as `ptmx.Write`** (lines 273-284). The answer keystroke is one of two PTY writes that stay direct (the other is the trust-accept at line 177). Spike-permission's #118 migration moved its approve keystroke through `session.Write` via a renamed `sendKeystroke` helper because the migration was already in flight for that file; spike-ask-user's answer keystroke is a single, inline write with no helper to rename. Migrating it would add an arbitrary helper and exceed the mechanical-cleanup scope. The trust-accept precedent (preserved across spike-cancel / spike-multi-turn / spike-long-prompt / spike-permission) covers this same call-site shape.

   Net: `grep -n 'ptmx\.Write' cmd/spike-ask-user/main.go` should return exactly TWO hits post-migration (trust-accept + answer keystroke), down from THREE today (the same two plus the `ptmx` binding that the now-deleted helpers indirectly consumed — but those are receivers, not call sites). The grep is informational, not an AC gate.

### probe-first-prompt-hang `run()` body changes

The body keeps the same flow (mkdir outDir → create logs → start markers → resolve session/jsonlPath → spawn → first-byte observer → idle wait → trust → write prompt → wait JSONL → tail → block until end-turn or wall timeout → summary). Per-phase deltas:

1. **First-PTY-byte marker goroutine** (lines 167-184) — STAYS verbatim. This goroutine has no Tracker, no spinner observation, no PTY-quiet check, and no wedge return. It is an instrumentation observer that records the `first-pty-byte` marker on the first non-zero `rb.LastAppendAt()` tick, then exits. Replacing it with `tuidriver.RunWatchdog` would:
   - Require constructing an unused `Tracker` (probe creates none today).
   - Introduce wedge-detection that ABORTS the probe on a slow-claude start — defeating the probe's hang-characterisation purpose.
   - Drop the `first-pty-byte` marker (RunWatchdog doesn't record any marker).

   The ticket lists "inline watchdog goroutine" in the substitution table for probe-first-prompt-hang. **This is a ticket-body mislabel**: the goroutine is a first-byte marker observer, not a watchdog in the spike-suite sense. Per the AC's standing instruction "If `tailJSONLAll` exposes behavior that `tuidriver.TailJSONL` does not cover, document the gap" — the symmetric reading is to document the mismatch and not migrate. AC #1 only enumerates the four named helpers (`openSessionJSONL`, `tailJSONLAll`, `isEndTurn`) plus the generic "inline watchdog goroutine"; the goroutine present in the file is NOT a watchdog by the matchSpinner/Tracker/CheckWatchdog definition shared across the other migrations. The PR description should note this divergence explicitly.

2. **Inline `filepath.Join(home, ".claude", "projects", tuidriver.EncodeCwd(cwd), sessionID+".jsonl")`** (line 133). Replace with `tuidriver.SessionJSONLPath(home, cwd, sessionID)`. Body shape:

   ```go
   home, _ := os.UserHomeDir()
   cwd, _ := os.Getwd()
   jsonlPath := tuidriver.SessionJSONLPath(home, cwd, sessionID)
   ```

   The current code swallows `os.UserHomeDir` / `os.Getwd` errors with `_`. Preserve that posture — this is throwaway-probe code; the spike binaries' explicit error wrap is not necessary parity here. (If a future ticket adds error handling, that's its scope, not this one.)

3. **Tail goroutine** (lines 226-246). Replace the `_ = tailJSONLAll(...)` call with a single `wg.Add(1)/go func` consumer that ranges over `tuidriver.TailJSONL(rootCtx, jsonlPath, 0)` and performs three per-entry behaviours, in this order (sequencing matters — current code logs raw line before markers):

   1. Raw-line log: `fmt.Fprintf(jsonlLog, "[+%9.3fs] %s\n", time.Since(startedAt).Seconds(), ev.RawLine)`. RawLine strips the trailing newline so the consumer adds it back (see § probe-first-prompt-hang deltas item 3 for the byte-level note).
   2. deferred_tools_delta marker: when `ev.Type == "attachment"`, reach into `ev.Raw["attachment"]` (a `map[string]any`); if its `type` field is `"deferred_tools_delta"`, collect `pendingMcpServers []any` as strings and call `recordMarker(fmt.Sprintf("deferred_tools_delta pending=%d [%s]", len(names), strings.Join(names, ",")))`. Same nested type-assertion ladder as lines 372-384 today, only the top-level key access changes from `ev["attachment"]` to `ev.Raw["attachment"]`.
   3. First-assistant + end-turn markers: same shape as the inline `onEvent` callback in the current code (lines 232-244); flip `gotFirstAssistant` on first `ev.Type == "assistant"`; flip `gotEndTurn` and call `cancelCause(errors.New("end-turn reached"))` on `ev.Type == "assistant" && tuidriver.IsEndTurn(ev)`.

   The `for ev := range eventCh` loop terminates naturally when `TailJSONL` closes the channel on ctx cancel. The existing `wg.Wait()` shape at lines 253-260 (with 1 s closeCtx timeout) is preserved — it now drains this consumer instead of the old `tailJSONLAll` wrapper. `cancelCause(errors.New("end-turn reached"))` preserves the existing graceful-exit pathway: ctx cancel → main goroutine's `<-rootCtx.Done()` returns → summary prints → deferred Close runs.

4. **`openSessionJSONL(rootCtx, jsonlPath)`** (lines 219-222). Replace with:

   ```go
   jsonlCtx, jsonlCancel := context.WithTimeout(rootCtx, sessionFileWait)
   jsonlErr := tuidriver.WaitForSessionJSONL(jsonlCtx, jsonlPath)
   jsonlCancel()
   if jsonlErr != nil {
       return fmt.Errorf("open session jsonl: %w", jsonlErr)
   }
   recordMarker("session-jsonl-appeared")
   ```

   The local `openSessionJSONL` (lines 318-336) drives its own deadline via `sessionFileWait`; preserve the same 30 s budget by wrapping `rootCtx` with `WithTimeout`. The `session-jsonl-appeared` marker stays.

   `sessionFileWait = 30 * time.Second` (line 44) stays — it's now the explicit `WithTimeout` argument.

### probe-first-prompt-hang deltas (vs. the spike-ask-user shape)

1. **No watchdog migration** (see § probe-first-prompt-hang `run()` body changes item 1). The first-byte marker goroutine stays as-is. Document in the PR description.

2. **Behavioural gap on the raw-line log: malformed-JSON lines drop.** The local `tailJSONLAll` writes EVERY line to `jsonlLog` BEFORE attempting to parse it; malformed-JSON lines and empty lines are logged, then suppressed for marker extraction. Library `TailJSONL` silently drops malformed-JSON lines AND empty lines before they reach the consumer (per `tailJSONLLoop` / `parseEntry` behaviour — see `pkg/tuidriver/jsonl.go:200-225`). Post-migration, malformed and empty lines no longer appear in `jsonl.log`.

   The ticket-body permission ("If `tailJSONLAll` exposes behavior that `tuidriver.TailJSONL` does not cover, document the gap in the PR description rather than reintroducing the helper") explicitly authorises this drop. The probe is observational and this is a throwaway diagnostic — accept the drop, document it in the PR body. If the probe ever observes a real malformed-JSON line and that observation matters, a follow-up ticket would either (a) add a `TailJSONLRaw` library API that exposes parse failures or (b) restore a parallel raw reader. Out of scope for this slice.

3. **Behavioural gap on raw-line content: `RawLine` excludes trailing newline.** `bufio.Reader.ReadString('\n')` returns the line INCLUDING the `\n`; `JSONLEntry.RawLine` is the line bytes WITHOUT the trailing `\r\n` or `\n` (per `pkg/tuidriver/jsonl.go:108-113` and `parseEntry` at line 242 which calls `bytes.TrimRight`-and-`bytes.Clone`). The consumer formats `[+%9.3fs] %s\n` — i.e., adds the trailing newline back explicitly. Byte-for-byte the result is one byte longer than today only on lines that ended with `\r\n` (the local code preserved the `\r`; the migrated code drops it). For probe inspection purposes this is invisible.

4. **Behavioural gap on log line ordering vs. marker recording.** The local code writes the raw-line log first, THEN extracts markers in the same goroutine cycle. The migrated consumer does the same (single goroutine, sequential per-entry). Ordering preserved.

### Constants

#### spike-ask-user

| Constant                | Status after migration |
|-------------------------|------------------------|
| `statePollInterval`     | Delete — only consumer was a use site removed with the inline watchdog. (Verify: `grep -n 'statePollInterval' cmd/spike-ask-user/main.go` after migration should return empty.) |
| `jsonlTailInterval`     | Delete — was only the local `tailJSONL` poll. |
| `sessionFileWait`       | Keep — passed to `WaitForSessionJSONL` context timeout. |
| `sessionFilePoll`       | Delete — was only the local `openSessionJSONL` poll. |
| `watchdogTick`          | Delete — library `RunWatchdog` owns its own tick. |
| `ptyQuietLimit`, `spinnerFreezeLimit` | Keep — passed to `NewTracker`. `ptyQuietLimit` STAYS at 120s with its existing inline commentary at lines 61-67 (modal quiescence). |
| `shutdownGrace`         | Keep — passed to `SpawnOpts`. |
| `defaultPrompt`         | Keep — flag default. |
| `askUserQuestionLimit`, `postAnswerLimit`, `settleWindow` | Keep — modal-detection and answer-window deadlines. |

#### probe-first-prompt-hang

| Constant                | Status after migration |
|-------------------------|------------------------|
| `statePollInterval`     | Keep — drives the first-byte marker goroutine ticker. |
| `jsonlTailInterval`     | Delete — was only the local `tailJSONLAll` poll. |
| `sessionFileWait`       | Keep — passed to `WaitForSessionJSONL` context timeout. |
| `sessionFilePoll`       | Delete — was only the local `openSessionJSONL` poll. |
| `wallTimeout`           | Keep — drives the probe's overall deadline via `time.AfterFunc`. |
| `hangThreshold`         | Keep — used in the summary verdict. |
| `promptText`            | Keep — sent verbatim via `ptmx.Write` (NOT TypePrompt; probe uses a short keystroke shape with a literal `\r`). |

### Import diff

#### spike-ask-user

Remove (only used by the deleted helpers):

- `bufio`, `encoding/json`, `io`.

`filepath` STAYS — `extractAskUserQuestion` and modal-snapshot code do not use it; the only remaining use is in `os.WriteFile`'s caller-supplied path (line 254 uses `fmt.Sprintf`, no `filepath`). **Re-check**: line 254 has no `filepath` call; the `path/filepath` import only existed for `projectsDir`'s `filepath.Join`. After deleting `projectsDir`, `path/filepath` is DELETED too.

Revised remove list:

- `bufio`, `encoding/json`, `io`, `path/filepath`.

Keep: `bytes` (modal-shape checks), `context`, `errors`, `flag`, `fmt`, `log`, `os`, `os/exec`, `regexp` (`oscRe` + `spinnerRe`; `spinnerRe` is being deleted — re-check), `sync`, `time`, `github.com/google/uuid`, `github.com/pyrycode/tui-driver/pkg/tuidriver`.

`regexp` re-check: `spinnerRe` (line 79) deletes; `oscRe` (line 78) stays. Keep `regexp`.

#### probe-first-prompt-hang

Remove (only used by the deleted helpers):

- `bufio`, `encoding/json`, `io`.

`strings` STAYS — `strings.Join(names, ",")` is used in the deferred_tools_delta marker (now inside the consumer goroutine).

`path/filepath` STAYS — `filepath.Join` is still used for `outDir`, `ptyLogPath`, `jsonlLogPath`, `markersPath` (lines 76, 81-83).

Revised remove list:

- `bufio`, `encoding/json`, `io`.

Keep: `context`, `errors`, `flag`, `fmt`, `log`, `os`, `os/exec`, `path/filepath`, `strings`, `sync`, `time`, `github.com/google/uuid`, `github.com/pyrycode/tui-driver/pkg/tuidriver`.

## Behavioural notes

`tuidriver.IsEndTurn` is strictly stricter than both files' local `isEndTurn` (additionally requires `e.Type == "assistant"` AND non-empty assistant text via `AssistantText(e) != ""`).

- For **spike-ask-user**: Probe-equivalent flow runs `defaultPrompt` triggering AskUserQuestion → modal → answer keystroke → claude completes the turn with a text-bearing reply. The text-bearing assistant turn always carries `AssistantText() != ""`, so the two predicates are equivalent on the happy path. The stricter library predicate rejects pathological future-claude end_turn-without-text deltas (sibling of spike-permission's same finding).

- For **probe-first-prompt-hang**: The prompt is `"ready?\r"` and the probe waits for `end_turn` to terminate. Claude's response to "ready?" is empirically a short text-bearing reply ("Yes!", "Ready", or similar), so the two predicates are equivalent on the happy path. On the hang path (the probe's named failure mode), neither predicate fires — the probe terminates via wall timeout instead. Both predicates are equivalent under both observed terminating conditions; the stricter library predicate is the documented contract going forward.

Library's `TailJSONL` emits ALL entry types (`assistant`, `user`, `attachment`, `permission-mode`, …). Both files' pre-migration local tailers filtered on type before emit:

- Local spike-ask-user `tailJSONL` filtered to assistant-only (lines 409-411). The migrated end-turn loop re-establishes this with `if ev.Type != "assistant" { continue }` at the receive-case top (per § spike-ask-user `run()` body changes item 5).
- Local probe-first-prompt-hang `tailJSONLAll` deliberately did NOT filter — it emitted all entries to its `onEvent` callback. The migrated consumer also does not filter; the per-entry `if ev.Type == "attachment"` and `if ev.Type == "assistant"` gates are inline inside the consumer goroutine (per § probe-first-prompt-hang `run()` body changes item 3). No filter regression.

## Concurrency model

#### spike-ask-user

Same as today:

- Main goroutine: linear state machine (idle wait → trust → ClearInputLine + TypePrompt → wait JSONL → wait modal → snapshot → answer → wait end-turn).
- Watchdog goroutine: `RunWatchdog` (library-owned, 1 Hz, returns on ctx cancel or wedge).
- Tail goroutine: spawned by `TailJSONL` (library-owned, closes channel on ctx cancel).

Removes the inline watchdog goroutine (replaced by `RunWatchdog`) and the inline tail goroutine (replaced by `TailJSONL`'s internal goroutine). `wg sync.WaitGroup` (line 141) STAYS for template parity with spike-cancel; `wg.Wait()` is never called (same as today).

Shutdown sequence unchanged: `defer cancelCause` → ctx cancellation → watchdog returns → tail goroutine closes channel → `session.Close()` SIGTERMs claude → reader goroutine drains → process exits.

#### probe-first-prompt-hang

Same as today plus one substitution:

- Main goroutine: linear flow (mkdir → markers → spawn → idle wait → trust → prompt → wait JSONL → block on rootCtx → summary).
- First-byte marker goroutine: poll `rb.LastAppendAt()`, record marker, exit on first non-zero value or ctx cancel.
- Tail goroutine: spawned by `TailJSONL` (library-owned, closes channel on ctx cancel).
- Consumer goroutine: ranges over `TailJSONL`'s channel, performs raw-line log + deferred_tools_delta marker + first-assistant / end-turn detection inline. Replaces the old goroutine that wrapped `tailJSONLAll`.

The first-byte marker goroutine and the consumer goroutine are unrelated; both terminate naturally on ctx cancel (the former exits its select loop; the latter sees the channel close).

Shutdown sequence unchanged: end-turn marker → `cancelCause("end-turn reached")` → main goroutine's `<-rootCtx.Done()` returns → `wg.Wait()` (or 1 s timeout) → summary printed → deferred Close runs → process exits.

## Error handling

No new failure modes. Library functions return wrapped errors with the same `fmt.Errorf("…: %w", err)` shape the local helpers produced. Preserve the existing error-wrap text at each call site:

- spike-ask-user: `"resolve home dir"`, `"resolve cwd"`, `"open session jsonl"`, `"open events stream"` (new wrap for `TailJSONL`'s synchronous error return — matches spike-cancel:283).
- probe-first-prompt-hang: `"open session jsonl"`, `"open events stream"` (new wrap).

Closed-channel handling at spike-ask-user's end-turn receive site mirrors spike-cancel's three-clause guard. The consumer goroutine in probe-first-prompt-hang exits naturally on channel close (no explicit guard needed; `for ev := range ch` is the canonical drain pattern).

## Testing strategy

Verified by `make e2e`. Specifically:

1. **AC #1 mechanical check.** This grep must return empty (spike-ask-user):

   ```
   grep -n 'func projectsDir\|func openSessionJSONL\|func tailJSONL\|func isEndTurn' cmd/spike-ask-user/main.go
   ```

   Plus `grep -n 'spinnerRe\|matchSpinner' cmd/spike-ask-user/main.go` must return empty (spinner regex + consumer both deleted with the inline watchdog).

2. **AC #2 mechanical check.** This grep must return empty (probe-first-prompt-hang; the regex matches both `tailJSONL` and `tailJSONLAll`):

   ```
   grep -n 'func openSessionJSONL\|func tailJSONL\|func isEndTurn' cmd/probe-first-prompt-hang/main.go
   ```

3. **Build.** `make build-bin` succeeds — both binaries compile against the new types.

4. **e2e.** `make e2e` — both `spike-ask-user` (success marker `OBSERVED:` per `cmd/e2e-runner/main.go:266`) and `probe-first-prompt-hang` (no success marker; checked via wall-timeout / output-dir extraction at `cmd/e2e-runner/main.go:281-287`) pass.

5. **Snapshot drift.** `snapshot-drift` step does NOT report new drift attributable to this slice — neither binary writes to `pkg/tuidriver/testdata/`, so all snapshot fixtures (`picker`, `mcp`, `agents`, etc.) are untouched. AC #4.

6. **Log shape spot-check (probe-first-prompt-hang).** The probe's `jsonl.log` output should retain timestamp-prefixed lines for every well-formed JSONL entry. Spot-check one run: confirm `[+ N.NNNs] {...json...}` lines appear and the per-marker `[+ N.NNNs] first-pty-byte` / `idle-detected` / `prompt-written` / `session-jsonl-appeared` / `first-assistant-event` / `end-turn-detected` markers appear in `markers.log`. The malformed-JSON drop documented in § probe-first-prompt-hang deltas item 2 will be invisible if no malformed lines occur (the normal case).

No new unit tests added — the e2e binaries are themselves the integration test. Library entrypoints already have their own unit tests under `pkg/tuidriver/*_test.go`.

## Open questions

None. The four preceding migrations (#115, #116, #117, #118) all landed on `main` in the last two days and are mechanically identical patterns. Library entrypoints are stable. The only non-mechanical interpretive call — whether the probe-first-prompt-hang first-byte goroutine is a "watchdog" that should migrate to `RunWatchdog` — is resolved under § probe-first-prompt-hang deltas item 1 with full rationale (substitution would defeat the probe's purpose; the ticket's substitution-table entry is a mislabel). The PR description should call this out explicitly so reviewers and future spec-readers see the deliberation.

# Spec — #116: spike-cleanup: migrate spike-multi-turn off duplicated helpers

Single-file cleanup. Mirror the structure of the already-migrated `cmd/spike-one-turn/main.go` (closest template — same PTY-quiescence predicate, same `TailJSONL`-based receive loop, same `RunWatchdog` goroutine) into `cmd/spike-multi-turn/main.go`, preserving the multi-turn-specific shape (hook-started tailer, msg_id-grouped extraction, drain between turns).

## Files to read first

- `cmd/spike-multi-turn/main.go` — the file being edited; in particular `runTurn` at lines 286-434 (verbatim predicate at 391-400) and the helpers at 481-640 that are being deleted.
- `cmd/spike-one-turn/main.go:170-263` — closest template. Uses `WaitForSessionJSONL` + `TailJSONL` + the same PTY-quiescence predicate (`gotEndTurn ∧ ❯-present ∧ rb.QuietFor() ≥ ptyQuietWindow`) that spike-multi-turn's `runTurn` must keep. Note the closed-channel handling (`if !ok { eventCh = nil; continue }`) at lines 237-245.
- `cmd/spike-long-prompt/main.go:75-150` — second template. Demonstrates the `RunWatchdog` goroutine pattern, `SessionJSONLPath` use, and `resolveSession` shape this spec adopts.
- `pkg/tuidriver/jsonl.go:108-135` — `JSONLEntry`, `EntryMessage`, `ContentBlock` struct definitions. `extractByMsgID` and `msgIDOf` adapt to these.
- `pkg/tuidriver/jsonl.go:171-226` — `TailJSONL` contract. Emits ALL entry types (assistant, user, attachment, …), not just `assistant`; closes the channel on ctx cancel.
- `pkg/tuidriver/jsonl.go:297-345` — `IsEndTurn` and `AssistantText`. `IsEndTurn` is stricter than the local `isEndTurn` (additionally requires `Type == "assistant"` AND a non-empty text concatenation) — see § Behavioural notes.
- `pkg/tuidriver/session.go:200-212` — `Session.TypePrompt`. Byte-by-byte with `PromptInterByteDelay` / `PromptCommitSettle`. 1:1 substitute for the local `typePrompt` (same algorithm, library-owned constants).
- `pkg/tuidriver/watchdog.go:66-85` — `RunWatchdog` contract. Internally drives `ParseSpinner` + `tr.ObserveSpinner` + `tr.CheckWatchdog` — fully subsumes the local watchdog block including `spinnerRe` / `matchSpinner`.

## Context

PRs #87/#89/#60/#98/#99 promoted six load-bearing helpers from `cmd/spike-*` binaries into `pkg/tuidriver/`. `cmd/spike-long-prompt/main.go` and `cmd/spike-one-turn/main.go` are now zero-local-helper-copy. `cmd/spike-multi-turn/main.go` is the last spike still hand-rolling the full set (`projectsDir`, `openSessionJSONL`, `tailJSONL`, `isEndTurn`, `typePrompt`, inline watchdog goroutine + `spinnerRe`/`matchSpinner`). This ticket finishes the migration so all spike binaries exercise the same library APIs that consumers will use.

Mechanical cleanup. No new behaviour. The verbatim PTY-quiescence predicate at `runTurn` lines 391-400 is load-bearing (#73 design) and must not move.

## Design

### Substitution table

| Local symbol (delete)                | Library replacement (use)                                     |
|--------------------------------------|---------------------------------------------------------------|
| `func projectsDir`                   | `tuidriver.SessionJSONLPath(home, cwd, sessionID)` inline     |
| `func openSessionJSONL`              | `tuidriver.WaitForSessionJSONL(ctx, jsonlPath)` w/ timeout    |
| `func tailJSONL`                     | `tuidriver.TailJSONL(rootCtx, jsonlPath, 0)`                  |
| `func isEndTurn`                     | `tuidriver.IsEndTurn(ev)`                                     |
| `func typePrompt`                    | `session.TypePrompt(prompt)` (Session method)                 |
| `var spinnerRe` + `func matchSpinner`| (deleted — only consumer was the inline watchdog)             |
| inline watchdog goroutine            | `tuidriver.RunWatchdog(rootCtx, rb, tr, tuidriver.WatchdogOpts{})` wrapped in the same `wg.Add(1)/go func` shape spike-long-prompt uses |
| `eventCh chan map[string]any`        | `<-chan tuidriver.JSONLEntry` from `TailJSONL`                |
| `extractByMsgID([]map[string]any, …)`| `extractByMsgID([]tuidriver.JSONLEntry, …)` — struct-field access |
| `msgIDOf(map[string]any)`            | `msgIDOf(tuidriver.JSONLEntry)` — nil-guarded struct access   |

### `runTurn` signature change

Two parameter changes; everything else stays:

- `ptmx *os.File` → `session *tuidriver.Session` (so the body can call `session.TypePrompt`). The `rb *tuidriver.Buffer` parameter stays unchanged; callers pass `session.Buffer`.
- `eventCh <-chan map[string]any` → `eventChRef *<-chan tuidriver.JSONLEntry`. The channel is created by turn 1's hook (inside `runTurn`'s flow, after the prompt write — see § Channel ownership), so on turn 1 the value pointed to is `nil` at entry; pre-hook code MUST treat it as such. Pointer indirection lets the hook's assignment (`*eventChRef = ch`) become visible to the post-hook receive loop without restructuring `main`.

### Channel ownership (turn-1 hook)

The session-scoped tail channel is owned by `main` and stored in a `var eventCh <-chan tuidriver.JSONLEntry`. Turn 1's `postPromptHook` populates it:

1. `tuidriver.WaitForSessionJSONL(ctx, jsonlPath)` with a `sessionFileWait`-bounded context (mirror spike-long-prompt:187-192).
2. Log `session-jsonl-opened path=%s offset=0` (preserved verbatim).
3. `tuidriver.TailJSONL(rootCtx, jsonlPath, 0)` — assign the returned channel to the outer `eventCh` via the hook's closure.

No forwarder goroutine. No `wg.Add(1)` for the tailer — `TailJSONL` owns its own goroutine and closes the channel on ctx cancellation.

The current `wg.Add(1)` block that wrapped the local `tailJSONL` (`main.go:248-257`) is deleted entirely. The watchdog `wg.Add(1)` block is replaced (not deleted) by the `RunWatchdog` wrapper.

### `runTurn` body changes

The body keeps the same five phases (drain → typePrompt → observer → hook → receive-loop → extract). Per-phase deltas:

1. **Drain** (`main.go:306-313`). Gate on `*eventChRef != nil` so turn 1's pre-hook drain is a no-op:

   ```go
   if ch := *eventChRef; ch != nil {
       // existing non-blocking drain loop
   }
   ```

2. **typePrompt** (`main.go:315`). Replace with `session.TypePrompt(prompt)`.

3. **Observer goroutine** (`main.go:324-344`). No change.

4. **Hook** (`main.go:346-351`). No signature change to the hook itself — `func() error`. The hook closure captures and assigns the outer `eventCh`; the assignment is observed by `runTurn` via `*eventChRef` after the call returns.

5. **Receive loop** (`main.go:402-417`). Dereference `eventChRef` once after the hook fires:

   ```go
   eventCh := *eventChRef
   ```

   Then loop. Two diffs from the current body:
   - The receive case becomes `case ev, ok := <-eventCh:` with `if !ok { eventCh = nil; continue }` (mirror spike-one-turn:237-245). Required because library's `TailJSONL` closes the channel on ctx cancellation, whereas the local `tailJSONL` did not.
   - `isEndTurn(ev)` → `tuidriver.IsEndTurn(ev)`. `msgIDOf(ev)` updates per § Helpers.

   The check predicate (lines 391-400) — the three-clause `gotEndTurn ∧ IdleGlyph ∧ QuietFor` conjunction — stays VERBATIM. `ptyQuietWindow` constant and its commentary (lines 59-71) stay verbatim. No other constants change.

### Helpers staying in-file (adapted)

```
func extractByMsgID(events []tuidriver.JSONLEntry, targetID string) string
func msgIDOf(ev tuidriver.JSONLEntry) string
```

`extractByMsgID` walks `ev.Message.Content[i].Type == "text"` and reads `ev.Message.Content[i].Raw["text"].(string)`. The early-return `if targetID == ""` stays. The `events` slice's element type changes from `map[string]any` to `tuidriver.JSONLEntry`; the closure shape is otherwise unchanged.

`msgIDOf` returns `""` when `ev.Message == nil` (library emits non-assistant entry types whose `Message` is nil — guard against nil-deref).

After the substitution, `extractByMsgID` does NOT need an extra nil check on `ev.Message`: `msgIDOf(ev) != targetID` short-circuits when `Message` is nil (returns ""), and `targetID == ""` is rejected by the early-return.

### `resolveSession` simplification

Drop the `dir` parameter; return `(sessionID string, err error)`. `main` derives `jsonlPath` directly via `tuidriver.SessionJSONLPath(home, cwd, sessionID)` (mirror spike-long-prompt:91-100). The `projects-dir path=%s` log line at `main.go:131` is removed — the templates do not emit it (it was a `projectsDir()`-incidental log).

### Import diff

Remove (only used by the deleted helpers):
- `bufio`, `encoding/json`, `io`, `path/filepath`, `regexp`, `strconv`

Keep all others. `bytes` stays — `runTurn`'s `check()` predicate still uses `bytes.Contains(stripped, tuidriver.IdleGlyph)`.

### Constants

All constants except `ptyQuietWindow` and `ptyQuietLimit` / `spinnerFreezeLimit` / `shutdownGrace` become unused after the migration:

- `statePollInterval` — still used by `runTurn`'s observer ticker (line 328) and main receive-loop ticker (line 388). Keep.
- `jsonlTailInterval` — was used only by local `tailJSONL`. Delete.
- `sessionFileWait`, `sessionFilePoll` — `sessionFileWait` keeps (passed to `WaitForSessionJSONL`'s timeout context). `sessionFilePoll` was used only by local `openSessionJSONL`; delete.
- `watchdogTick` — was used only by the inline watchdog. Delete (library's `RunWatchdog` uses `DefaultWatchdogTick`).
- `ptyQuietLimit`, `spinnerFreezeLimit` — passed to `NewTracker`. Keep.
- `shutdownGrace` — passed to `SpawnOpts`. Keep.
- `disappearedWindow` — observer goroutine. Keep.
- `ptyQuietWindow` — predicate. Keep verbatim with its commentary.

## Behavioural notes

`tuidriver.IsEndTurn` is strictly stricter than the local `isEndTurn`: it additionally requires `e.Type == "assistant"` AND `AssistantText(e) != ""`. The local version returned true for any entry whose `message.stop_reason == "end_turn"`. For this spike's payloads (final assistant message always carries text — every prompt 1-5 asks for a worded reply or summary), the two are equivalent: only assistant text-bearing lines fire either predicate. Non-assistant entries (`user` tool_result, attachments, etc.) lack `stop_reason` entirely, so the local version was already implicitly assistant-only. The stricter library predicate is safer (rejects pathological future-claude end_turn-without-text deltas) and is the documented contract — accepted by the ticket's mapping table.

Library's `TailJSONL` emits ALL entry types, not just `type=="assistant"` (the local `tailJSONL` filtered to assistant-only). The `events` slice in `runTurn` will therefore accumulate more entries per turn. This is benign:

- `IsEndTurn` returns false for non-assistant entries, so `gotEndTurn` flips only on assistant text-bearing end_turn lines.
- `extractByMsgID` filters by msg_id; non-assistant entries have different (or nil) message IDs and contribute nothing.

Memory cost: a multi-turn spike already holds the full JSONL stream in memory; adding a small number of `user`/`attachment`/`file-history-snapshot` entries per turn is negligible.

## Concurrency model

Same shape as today:

- Main goroutine: linear state machine (idle wait → trust modal handling → per-prompt loop).
- Watchdog goroutine: `RunWatchdog` (library-owned loop, 1 Hz, returns on ctx cancel or wedge).
- Tail goroutine: spawned by `TailJSONL` (library-owned, closes channel on ctx cancel).
- Observer goroutine: per-turn, polls `IsIdle` for up to `disappearedWindow` after `prompt-written`. Unchanged.

Shutdown sequence unchanged: `defer cancelCause` triggers ctx cancellation → watchdog returns → tail goroutine closes its channel → `session.Close()` SIGTERMs claude → reader goroutine drains → `Wait()` returns.

The `wg sync.WaitGroup` stays in `main` even though `wg.Wait()` is never called (template parity with spike-one-turn and spike-long-prompt — both retain the dead-code WaitGroup; removing it here is out of scope for this cleanup).

## Error handling

No new failure modes. Library functions return wrapped errors with the same shape the local helpers returned (`fmt.Errorf("open session jsonl: %w", err)` style is preserved at the call sites). The closed-channel case in `runTurn`'s receive loop is handled by nilling the local copy (mirror spike-one-turn:237-245); the loop continues until `check()` flips or `ctx.Done()` fires.

## Testing strategy

Verified by `make e2e`. Specifically:

1. The mechanical grep check from AC #1:
   ```
   grep -n 'func projectsDir\|func openSessionJSONL\|func tailJSONL\|func isEndTurn\|func typePrompt' cmd/spike-multi-turn/main.go
   ```
   Must return empty.

2. `make build-bin` succeeds (the binary compiles against the new types).

3. `make e2e` — `spike-multi-turn` step prints `SUCCESS: <text>` for all 5 prompts and exits 0. The e2e-runner's only success criterion for this binary is the `SUCCESS:` marker on stdout (`cmd/e2e-runner/main.go:226-231` with `SuccessMarker: successSuccess`).

4. `snapshot-drift` step does NOT report drift on `picker`/`mcp`/`agents` snapshots (this slice does not touch any code those snapshots cover — they live in `pkg/tuidriver/testdata/`; AC #5's snapshot-equivalence phrasing applies to the global snapshot set, not a spike-multi-turn-specific snapshot).

No new tests added — the e2e binary is itself the integration test.

## Open questions

None. Templates are tight; library entrypoints are stable; behaviour is intentionally preserved.

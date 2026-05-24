# Spec — #117: spike-cleanup: migrate spike-cancel off duplicated helpers

Single-file cleanup. Mirror the just-landed migration of `cmd/spike-multi-turn/main.go` (PR #119, the closest template — same set of helpers, same channel-ownership shape, same PTY-quiescence predicates) into `cmd/spike-cancel/main.go`. Spike-cancel additionally migrates `clearInputLine` → `Session.ClearInputLine` and adapts two file-local helpers (`isToolUse`, `logCancelEvent`) that aren't present in spike-multi-turn.

## Files to read first

- `cmd/spike-cancel/main.go` — the file being edited. Specifically:
  - lines 97-98 (`spinnerRe`) and 813-824 (`matchSpinner`) — deleted.
  - lines 240-261 (inline watchdog goroutine) — replaced by `RunWatchdog`.
  - lines 294-312 (channel creation + tailer goroutine) — replaced by hook-owned `WaitForSessionJSONL` + `TailJSONL`.
  - lines 320 + 342-355 (`runProbe` signature) — `ptmx`/`eventCh` swap to `session`/`eventChRef`.
  - lines 547-585 (`waitReappeared` — verbatim predicate) and 632-718 (`runRecovery` — verbatim predicate) — load-bearing per ticket AC #4; bodies preserved verbatim.
  - lines 729-739 (`clearInputLine`), 752-756 (`isToolUse`), 591-608 (`logCancelEvent`), 762-808 (`extractByMsgID`, `typePrompt`, `msgIDOf`), 829-839 (`projectsDir`), 860-936 (`openSessionJSONL`, `tailJSONL`), 941-945 (`isEndTurn`) — deleted or adapted per § Substitution table.
- `cmd/spike-multi-turn/main.go` post-PR #119 — closest template. In particular:
  - lines 214-230 — channel-via-hook ownership pattern.
  - lines 262-272 — `runTurn` signature with `session *tuidriver.Session` + `eventChRef *<-chan tuidriver.JSONLEntry`.
  - lines 283-292 — pre-hook drain gated on `ch != nil`.
  - lines 388-396 — closed-channel handling (`if !ok { eventCh = nil; continue }`).
  - lines 434-464 — adapted `extractByMsgID` + `msgIDOf` on `JSONLEntry`.
- `cmd/spike-long-prompt/main.go:91-100, 131-139, 187-198` — second template. Demonstrates `SessionJSONLPath` use, the `RunWatchdog` wrapping shape, and `WaitForSessionJSONL` w/ bounded ctx.
- `pkg/tuidriver/jsonl.go:80-135` — `JSONLEntry`, `EntryMessage`, `ContentBlock` shapes. `Message` is `*EntryMessage` (nil for non-assistant/user envelopes); `Message.StopReason` is parsed zero-value-on-mismatch (empty string for absent OR null).
- `pkg/tuidriver/jsonl.go:171-226, 297-305` — `TailJSONL` (emits ALL entry types, closes channel on ctx cancel) and `IsEndTurn` (assistant + stop_reason=end_turn + non-empty text).
- `pkg/tuidriver/session.go:166-230` — `WritePrompt`, `TypePrompt`, `ClearInputLine`, `Write` contracts. `ClearLineSettle` lives library-side; the local `clearLineSettle` constant is dropped.
- `pkg/tuidriver/watchdog.go` — `RunWatchdog` + `WatchdogOpts`; internally drives `ParseSpinner` + `Tracker.ObserveSpinner` + `Tracker.CheckWatchdog`, fully subsuming the inline goroutine.
- `docs/specs/architecture/116-spike-multi-turn-migrate-off-duplicated-helpers.md` — the precedent spec. This ticket follows the same structure; deviations are noted under § Spike-cancel deltas.

## Context

PRs #87/#89/#60/#98/#99 promoted six load-bearing helpers from spike binaries into `pkg/tuidriver/`. PR #119 finished the equivalent migration for `cmd/spike-multi-turn/main.go`. `cmd/spike-cancel/main.go` is the last spike still hand-rolling the full set (`projectsDir`, `openSessionJSONL`, `tailJSONL`, `isEndTurn`, `typePrompt`, `clearInputLine`, inline watchdog goroutine + `spinnerRe`/`matchSpinner`). This ticket finishes the cleanup so every spike binary exercises the same library APIs consumers will use.

Mechanical cleanup. No new behaviour. The verbatim PTY-quiescence predicates in `waitReappeared` (lines 547-585) and `runRecovery` (lines 632-718) are load-bearing (#69 design) and must not move — AC #4 spells this out and it is the only non-mechanical correctness constraint in the slice.

## Design

### Substitution table

| Local symbol (delete)                | Library replacement (use)                                     |
|--------------------------------------|---------------------------------------------------------------|
| `func projectsDir`                   | `tuidriver.SessionJSONLPath(home, cwd, sessionID)` inline     |
| `func openSessionJSONL`              | `tuidriver.WaitForSessionJSONL(ctx, jsonlPath)` w/ timeout    |
| `func tailJSONL`                     | `tuidriver.TailJSONL(rootCtx, jsonlPath, 0)`                  |
| `func isEndTurn`                     | `tuidriver.IsEndTurn(ev)`                                     |
| `func typePrompt`                    | `session.TypePrompt(prompt)` (Session method)                 |
| `func clearInputLine`                | `session.ClearInputLine()` (Session method)                   |
| `var spinnerRe` + `func matchSpinner`| (deleted — only consumer was the inline watchdog)             |
| inline watchdog goroutine            | `tuidriver.RunWatchdog(rootCtx, rb, tr, tuidriver.WatchdogOpts{})` wrapped in the same `wg.Add(1)/go func` shape spike-multi-turn / spike-long-prompt use |
| `eventCh chan map[string]any` (created in `main`, cap 32) | `<-chan tuidriver.JSONLEntry` returned by `TailJSONL`, assigned inside `probe1Hook` |
| `func isToolUse(map[string]any) bool`       | `isToolUse(tuidriver.JSONLEntry) bool` — nil-guarded struct access |
| `func logCancelEvent(_, _, map[string]any)` | `logCancelEvent(_, _, tuidriver.JSONLEntry)` — see § Helpers staying in-file |
| `func extractByMsgID([]map[string]any, _)`  | `extractByMsgID([]tuidriver.JSONLEntry, _)` — struct-field access |
| `func msgIDOf(map[string]any) string`       | `msgIDOf(tuidriver.JSONLEntry) string` — nil-guarded |

### Channel ownership (probe-1 hook)

Today the session-scoped channel is created in `main` (`eventCh := make(chan map[string]any, 32)`) and the local tailer goroutine sends into it from `probe1Hook`. Post-migration, `TailJSONL` returns its own channel; the cleanest shape is the spike-multi-turn pattern:

- `main` holds `var eventCh <-chan tuidriver.JSONLEntry` (nil at declaration).
- `probe1Hook` (closure capturing `eventCh`) populates the slot:
  1. `tuidriver.WaitForSessionJSONL(jsonlCtx, jsonlPath)` with a `sessionFileWait`-bounded context (mirror spike-long-prompt:187-192 / spike-multi-turn:217-222).
  2. Log `session-jsonl-opened path=%s offset=0` (verbatim — preserves the existing log line at spike-cancel:300).
  3. `tuidriver.TailJSONL(rootCtx, jsonlPath, 0)` — assign the returned channel to the outer `eventCh` via the closure.
- The current `wg.Add(1)` block that wrapped the local `tailJSONL` (`main.go:301-310`) is **deleted entirely** — `TailJSONL` owns its own goroutine and closes the channel on ctx cancellation. The watchdog `wg.Add(1)` block is **replaced** (not deleted) by the `RunWatchdog` wrapper.

### `runProbe` signature change

Three parameter changes; everything else stays:

- `ptmx *os.File` → `session *tuidriver.Session` (so the body can call `session.TypePrompt` + `session.ClearInputLine`; `runCancel` downstream uses `session.Write` for `sendCancel`). `rb *tuidriver.Buffer` stays — callers pass `session.Buffer`.
- `eventCh <-chan map[string]any` → `eventChRef *<-chan tuidriver.JSONLEntry`. Pointer indirection is required because `probe1Hook` assigns the channel mid-call; turn-1 entry sees a nil channel and post-hook sees the populated one. Same shape as spike-multi-turn's `runTurn`.
- All downstream signatures (`runCancel`, `runRecovery`, `waitForKickoff`, `waitReappeared`) receive `eventCh <-chan tuidriver.JSONLEntry` (the dereferenced value) — `runProbe` does `eventCh := *eventChRef` after the hook fires and passes that value down. The pointer never leaves `runProbe`.

### `runProbe` body changes

The body keeps the same six phases (drain → clearInputLine → typePrompt → hook → dispatch to runCancel/runRecovery). Per-phase deltas:

1. **Drain** (`main.go:361-368`). Gate on `*eventChRef != nil` so probe-1's pre-hook drain is a no-op:

   ```go
   if ch := *eventChRef; ch != nil {
       // existing non-blocking drain loop
   }
   ```

2. **ClearInputLine** (`main.go:377-379`). Replace with `session.ClearInputLine()`. The comment block at lines 370-376 (the "empirical: after a cancelled probe, claude restores the previously submitted prompt…" rationale) stays verbatim.

3. **TypePrompt** (`main.go:381-383`). Replace with `session.TypePrompt(prompt)`.

4. **Hook** (`main.go:387-391`). No signature change to the hook itself — `func() error`. The hook closure captures and assigns the outer `eventCh`; the assignment is observed by `runProbe` via `*eventChRef` after the call returns.

5. **Dispatch** (`main.go:393-396`). After the hook fires, dereference once and pass the channel value into `runCancel` / `runRecovery`:

   ```go
   eventCh := *eventChRef
   if kind == kindRecovery {
       return runRecovery(ctx, logger, probeN, rb, eventCh, tr)
   }
   return runCancel(ctx, logger, probeN, kind, cancelKey, cancelHex, session, rb, eventCh, tr)
   ```

### Receive-loop call sites (closed-channel handling)

Library's `TailJSONL` closes the channel on ctx cancellation; the local `tailJSONL` did not. Three call sites receive from the channel and need the same closed-channel guard — mirror spike-multi-turn:388-396 at each:

- `waitForKickoff` (`main.go:481-499`), the `case ev := <-eventCh:` branch in the `kindToolUse` switch arm. Becomes `case ev, ok := <-eventCh: if !ok { eventCh = nil; continue }`. The existing `isToolUse(ev)` call site is replaced by `isToolUse(ev)` over `tuidriver.JSONLEntry` (same function name, new parameter type) — see § Helpers staying in-file.
- `waitReappeared` (`main.go:577-583`), the `case ev := <-eventCh:` branch. Same closed-channel guard. The body still calls `logCancelEvent(logger, probeN, ev)`. Gate the call on `ev.Type == "assistant"` to preserve the original behaviour: today the local `tailJSONL` filtered to assistant-only before the channel emit, so non-assistant entries never reached `logCancelEvent`; with library `TailJSONL` emitting all envelope types, an explicit `if ev.Type != "assistant" { continue }` re-enforces the same filter at the consumer.
- `runRecovery` (`main.go:683-697`), the `case ev := <-eventCh:` branch. Same closed-channel guard. `isEndTurn(ev)` → `tuidriver.IsEndTurn(ev)`. `msgIDOf(ev)` per § Helpers staying in-file.

The three predicate / loop bodies otherwise stay verbatim — including `waitReappeared`'s `stable()` closure (the three-clause `IdleGlyph ∧ QuietFor` predicate at lines 561-567) and `runRecovery`'s `check()` closure (the same predicate shape at lines 672-681), and `ptyQuietWindow` with its commentary at lines 81-90. AC #4.

### Helpers staying in-file (adapted)

Three helpers stay in `cmd/spike-cancel/main.go` because they encode probe-specific accumulation that's not in the library scope. All three shift from `map[string]any` to `tuidriver.JSONLEntry`:

```
func extractByMsgID(events []tuidriver.JSONLEntry, targetID string) string
func msgIDOf(ev tuidriver.JSONLEntry) string
func isToolUse(ev tuidriver.JSONLEntry) bool
func logCancelEvent(logger *log.Logger, probeN int, ev tuidriver.JSONLEntry)
```

`extractByMsgID` — same body shape as spike-multi-turn:434-453 (read `ev.Message.Content[i].Type == "text"`, take `ev.Message.Content[i].Raw["text"].(string)`, append). Early-return `if targetID == ""` stays. The implicit nil-Message guard via `msgIDOf` returning "" still holds — no extra explicit check needed.

`msgIDOf` — `if ev.Message == nil { return "" }; return ev.Message.ID`. Library emits non-assistant entry types whose `Message` is nil; the guard is load-bearing.

`isToolUse` — `if ev.Message == nil { return false }; return ev.Message.StopReason == "tool_use"`. `EntryMessage.StopReason` is parsed zero-value-on-mismatch (empty string for absent OR null), which is exactly what the original `_, _ := msg["stop_reason"].(string)` produced — both return false on absent/null. Semantically equivalent.

`logCancelEvent` — preserves the three-way `<missing>` / `<nil>` / string discrimination on `stop_reason` (README finding surface; see § Behavioural notes). The typed `Message.StopReason` collapses missing vs. null vs. empty-string into "", so the helper MUST read through `ev.Message.Raw["stop_reason"]` to keep the distinction. Body:

```go
func logCancelEvent(logger *log.Logger, probeN int, ev tuidriver.JSONLEntry) {
    var msgID string
    var rawMap map[string]any
    if ev.Message != nil {
        msgID = ev.Message.ID
        rawMap = ev.Message.Raw
    }

    stopRepr := "<missing>"
    if raw, present := rawMap["stop_reason"]; present {
        if raw == nil {
            stopRepr = "<nil>"
        } else if s, ok := raw.(string); ok {
            stopRepr = s
        } else {
            stopRepr = fmt.Sprintf("%v", raw)
        }
    }

    logger.Printf("probe=%d jsonl-cancel-event type=assistant stop_reason=%s msg_id=%s",
        probeN, stopRepr, msgID)
}
```

The `nil`-rawMap lookup is safe in Go (`m[k]` on nil map returns zero-value-false). Callers gate on `ev.Type == "assistant"` upstream, so in practice `ev.Message` is always non-nil here; the explicit guard is defensive and matches `msgIDOf`'s posture.

### `sendCancel` signature change

`sendCancel(ptmx *os.File, keystroke []byte) error` → `sendCancel(session *tuidriver.Session, keystroke []byte) error`. Body becomes `_, err := session.Write(keystroke); return err`. Trades a direct PTY-file write for the library's exposed `Session.Write` so the spike no longer reaches into `session.PTY` for cancel keystrokes. The comment at lines 720-724 (single-bulk-write reasoning) stays verbatim.

The trust-folder accept write at `main.go:278` (`ptmx.Write([]byte("1\r"))`) is OUT OF SCOPE — spike-multi-turn and spike-long-prompt both still reach through `ptmx` for that one write, and matching their shape minimises this slice's diff. The local `ptmx := session.PTY` binding at `main.go:235` stays for that single call site.

### `resolveSession` simplification

Drop the `dir` parameter; return `(sessionID string, err error)`. `main` derives `jsonlPath` directly via `tuidriver.SessionJSONLPath(home, cwd, sessionID)` (mirror spike-multi-turn:118-126 / spike-long-prompt:91-100). The `projects-dir path=%s` log line at `main.go:194` is removed — neither spike-multi-turn nor spike-long-prompt emit it post-migration (it was a `projectsDir()`-incidental log; the e2e-runner's success criterion is the `SUCCESS:` marker on stdout, not stderr log shape).

### Import diff

Remove (only used by the deleted helpers):

- `bufio`, `encoding/json`, `io`, `path/filepath`, `regexp`, `strconv`

Keep all others. `bytes` stays — `waitReappeared.stable` and `runRecovery.check` still use `bytes.Contains(stripped, tuidriver.IdleGlyph)`. `hasSpinnerGlyph` (still in-file, drives `waitForKickoff`'s `kindThinking` arm) also uses `bytes.Contains` + `tuidriver.SpinnerGlyph`.

### Constants

| Constant                | Status after migration |
|-------------------------|------------------------|
| `statePollInterval`     | Keep — drives observer + receive-loop tickers. |
| `jsonlTailInterval`     | Delete — was only the local `tailJSONL` poll. |
| `sessionFileWait`       | Keep — passed to `WaitForSessionJSONL` context timeout. |
| `sessionFilePoll`       | Delete — was only the local `openSessionJSONL` poll. |
| `watchdogTick`          | Delete — library `RunWatchdog` owns its own tick. |
| `ptyQuietLimit`, `spinnerFreezeLimit` | Keep — passed to `NewTracker`. |
| `shutdownGrace`         | Keep — passed to `SpawnOpts`. |
| `disappearedWindow`     | Keep — `runRecovery` observer goroutine. |
| `cancelRecoveryLimit`   | Keep — `waitReappeared` deadline. |
| `waitConditionLimit`    | Keep — `waitForKickoff` deadline. |
| `toolUseStartGrace`     | Keep — `kindToolUse` post-detection grace. |
| `ptyQuietWindow`        | Keep verbatim with commentary at lines 81-90. AC #4. |
| `clearLineSettle`       | Delete — library `ClearInputLine` owns the settle via `ClearLineSettle`. |

## Spike-cancel deltas (vs. the #116 precedent)

Three things this spec covers that #116 did not:

1. **`clearInputLine` → `Session.ClearInputLine`**. Direct 1:1 method substitution; deletes the `clearLineSettle` constant (library-owned).
2. **`isToolUse` and `logCancelEvent` adaptation**. Spike-multi-turn had neither. `isToolUse` is a trivial nil-guarded StopReason check. `logCancelEvent` must preserve the three-way `<missing>` / `<nil>` / string discrimination (README finding surface) — implemented by reading `ev.Message.Raw["stop_reason"]` directly, not `ev.Message.StopReason`. See § Helpers staying in-file.
3. **Two PTY-quiescence predicates instead of one**. Spike-multi-turn had one (`runTurn.check`). Spike-cancel has two (`waitReappeared.stable` + `runRecovery.check`); both stay verbatim per AC #4. The triple-consumer cross-reference in their commentary (this file's `waitReappeared`, this file's `runRecovery`, `spike-multi-turn/main.go:runTurn`) is already correct in the source — keep as-is.

## Behavioural notes

`tuidriver.IsEndTurn` is strictly stricter than the local `isEndTurn` (additionally requires `e.Type == "assistant"` AND non-empty assistant text). For spike-cancel's payloads — Probe 3+ recovery prompts ("say hello", "What were you working on…") always produce text-bearing assistant replies — the two are equivalent. The stricter library predicate is safer (rejects pathological future-claude end_turn-without-text deltas) and is the documented contract.

Library's `TailJSONL` emits ALL entry types (`assistant`, `user`, `attachment`, `permission-mode`, …); the local `tailJSONL` filtered to assistant-only before emit. Three consumer-side filters re-establish the original behaviour:

- `waitReappeared` gates `logCancelEvent` on `ev.Type == "assistant"`.
- `waitForKickoff`'s `kindToolUse` branch relies on `isToolUse`'s nil-Message guard (non-assistant entries return false).
- `runRecovery`'s receive loop relies on `tuidriver.IsEndTurn`'s Type guard (non-assistant entries return false) and `extractByMsgID`'s msg_id filter (non-assistant entries have different/empty msg_id, contribute nothing).

The `events` slice in `runRecovery` will accumulate more entries per turn than today. Memory cost is negligible — a multi-probe spike already holds the full per-turn JSONL stream.

## Concurrency model

Same shape as today:

- Main goroutine: linear state machine (idle wait → trust modal → per-probe loop).
- Watchdog goroutine: `RunWatchdog` (library-owned, 1 Hz, returns on ctx cancel or wedge).
- Tail goroutine: spawned by `TailJSONL` (library-owned, closes channel on ctx cancel).
- Observer goroutine: per-recovery-probe, polls `IsIdle` for up to `disappearedWindow` after `prompt-written`. Unchanged.

Shutdown sequence unchanged: `defer cancelCause` → ctx cancellation → watchdog returns → tail goroutine closes its channel → `session.Close()` SIGTERMs claude → reader goroutine drains → `Wait()` returns. `wg sync.WaitGroup` stays for template parity even though `wg.Wait()` is never called (same as spike-multi-turn / spike-long-prompt).

## Error handling

No new failure modes. Library functions return wrapped errors with the same `fmt.Errorf("…: %w", err)` shape the local helpers produced; preserve the existing error-wrap text at each call site (`"open session jsonl"`, `"write prompt"`, `"clear input line"`, etc.). Closed-channel handling at the three receive sites mirrors spike-one-turn / spike-multi-turn.

## Testing strategy

Verified by `make e2e`. Specifically:

1. **AC #1 mechanical check.** This grep must return empty:

   ```
   grep -n 'func projectsDir\|func openSessionJSONL\|func tailJSONL\|func isEndTurn\|func typePrompt\|func clearInputLine' cmd/spike-cancel/main.go
   ```

   Plus `grep -n 'spinnerRe\|matchSpinner' cmd/spike-cancel/main.go` must also return empty (AC #1 enumerates both).

2. **Build.** `make build-bin` succeeds — the binary compiles against the new types.

3. **e2e.** `make e2e` — `spike-cancel` step prints `SUCCESS:` for the recovery probes and exits 0. The e2e-runner's only success criterion for this binary is the `SUCCESS:` marker on stdout (`cmd/e2e-runner/main.go:226-231`).

4. **Snapshot drift.** `snapshot-drift` step does NOT report new drift on `picker`/`mcp`/`agents` snapshots — this slice doesn't touch any code those snapshots cover (they live in `pkg/tuidriver/testdata/` and exercise other spike binaries). AC #5.

No new unit tests added — the e2e binary is itself the integration test. Library entrypoints already have their own unit tests under `pkg/tuidriver/*_test.go`.

## Open questions

None. The precedent (PR #119) landed last commit and the diff is mechanical; library entrypoints are stable; the only correctness constraint (verbatim PTY-quiescence predicates) is called out as AC #4.

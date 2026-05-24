# Spec — #118: spike-cleanup: migrate spike-permission off duplicated helpers

Single-file cleanup. Mirror the just-landed migrations of `cmd/spike-multi-turn/main.go` (PR #119, #116) and `cmd/spike-cancel/main.go` (PR #120, #117) into `cmd/spike-permission/main.go`. Spike-permission additionally migrates `sendKeystroke` → `Session.Write` (Probe 2's approve keystroke), simplifies `newSessionID` → `resolveSession`, and adapts the in-file `logObservationEvent` / `isToolUse` / `extractByMsgID` / `msgIDOf` helpers off `map[string]any` onto `tuidriver.JSONLEntry`. The two-`runSession` orchestrator (Session A + Session B, three probes total) is preserved verbatim.

## Files to read first

- `cmd/spike-permission/main.go` — the file being edited. Specifically:
  - lines 69-73 (`jsonlTailInterval`, `sessionFilePoll`, `watchdogTick`) and 117-119 (`clearLineSettle`) — deleted constants.
  - lines 139, 1138-1149 (`spinnerRe` + `matchSpinner`) — deleted; only consumer was the inline watchdog.
  - lines 303-307 (`projDir, err := projectsDir()` + `projects-dir path=%s` log) — deleted per precedent.
  - lines 313, 328 (`newSessionID(flagValue, dir)` call sites) — adapted to `resolveSession(flagValue)` + inline `tuidriver.SessionJSONLPath`.
  - lines 395-424 (inline watchdog goroutine inside `runSession`) — replaced by `tuidriver.RunWatchdog` wrapper.
  - lines 441 (trust-folder accept `ptmx.Write([]byte("1\r"))`) — STAYS (out of scope, matches precedent).
  - lines 463 (`eventCh := make(chan map[string]any, 32)`) — deleted; replaced by `var eventCh <-chan tuidriver.JSONLEntry` populated by hook.
  - lines 465-486 (`openTailerHook` closure with `sync.Once` + inline tailer goroutine) — body replaced by `WaitForSessionJSONL` + `TailJSONL`; the `sync.Once` wrap is preserved; the inline `wg.Add(1)/go func` tailer wrap (lines 474-483) is deleted entirely.
  - lines 488-509 (probe dispatch in `runSession`) — call-site signature changes: `ptmx` → `session`, `eventCh` → `&eventCh`.
  - lines 515-592 (`runObserve`), 597-744 (`runAutoRespond`), 750-823 (`runEscalate`) — signature + body deltas per § runObserve/runAutoRespond/runEscalate body changes.
  - lines 638-640 (`sendKeystroke(ptmx, approveKey)` in `runAutoRespond`) + 1007-1010 (`sendKeystroke` body) — signature `*os.File` → `*tuidriver.Session`; body becomes `session.Write(keystroke)`.
  - lines 691-710 (`runAutoRespond`'s readiness predicate `gotEndTurn ∧ ❯-present ∧ rb.QuietFor() ≥ ptyQuietWindow`) — verbatim per AC #4; load-bearing #70 design.
  - lines 100-115 (`ptyQuietWindow` constant + commentary) — verbatim per AC #4.
  - lines 1037-1054 (`logObservationEvent`), 1060-1066 (`clearInputLine`), 1071-1083 (`typePrompt`), 1094-1098 (`isToolUse`), 1103-1125 (`extractByMsgID`), 1129-1133 (`msgIDOf`), 1155-1165 (`projectsDir`), 1170-1186 (`newSessionID`), 1188-1203 (`openSessionJSONL`), 1205-1264 (`tailJSONL`), 1269-1273 (`isEndTurn`) — deleted or adapted per § Substitution table.
  - lines 1279-1283 (compile-time `_ = hasSpinnerGlyph; _ = isToolUse; _ = disappearedWindow` block) — STAYS as-is. `hasSpinnerGlyph` (lines 1087-1090) also STAYS — not in AC #1's delete list.
- `cmd/spike-cancel/main.go` post-PR #120 — the closest template (same set of helpers, same `runSession`-style orchestrator, same channel-via-hook ownership pattern, same `sendCancel(session, keystroke)` → `session.Write` shape that `sendKeystroke` mirrors). In particular:
  - lines 174 + 180-189 — `resolveSession(flagValue)` (returns just the ID) + `home`/`cwd` resolution + `tuidriver.SessionJSONLPath` inline.
  - lines 226-234 — `RunWatchdog` wrap shape (`wg.Add(1)/go func` with `tuidriver.RunWatchdog(rootCtx, rb, tr, tuidriver.WatchdogOpts{})`).
  - lines 271-287 — channel-via-hook ownership; `var eventCh <-chan tuidriver.JSONLEntry` + `probe1Hook` populating the slot via `WaitForSessionJSONL` → `TailJSONL`.
  - lines 317-377 — `runProbe` signature with `session *tuidriver.Session` + `eventChRef *<-chan tuidriver.JSONLEntry`, pre-probe drain gated on `ch != nil`, `session.ClearInputLine()` + `session.TypePrompt(prompt)` calls, dereference-after-hook.
  - lines 564-580, 564-580 + 687-700 — closed-channel guard pattern `case ev, ok := <-eventCh: if !ok { eventCh = nil; continue }`, plus the `if ev.Type != "assistant" { continue }` filter at `waitReappeared` line 573-575.
  - lines 586-607 — `logCancelEvent(logger, probeN, ev tuidriver.JSONLEntry)` adapted body — the canonical template for `logObservationEvent`'s rewrite (same three-way `<missing>`/`<nil>`/string discrimination via `ev.Message.Raw["stop_reason"]`).
  - lines 728-731 — `sendCancel(session, keystroke)` body shape that `sendKeystroke` mirrors.
  - lines 747-786 — `isToolUse` / `extractByMsgID` / `msgIDOf` adapted bodies on `JSONLEntry`.
- `cmd/spike-multi-turn/main.go:118-126, 214-230, 262-272, 283-292, 388-396` — secondary template for the channel-via-hook + dereference-after-hook + closed-channel-handling pattern, in case the spike-cancel template needs cross-referencing.
- `pkg/tuidriver/jsonl.go:80-135` — `JSONLEntry`, `EntryMessage`, `ContentBlock` shapes. `Message` is `*EntryMessage` (nil for non-assistant/user envelopes); `Message.StopReason` is parsed zero-value-on-mismatch (empty string for absent OR null); `Message.Raw` preserves the original message-map for the `<missing>`/`<nil>`/string discrimination `logObservationEvent` requires.
- `pkg/tuidriver/jsonl.go:35, 58, 171` — `SessionJSONLPath(home, cwd, sessionID) string`, `WaitForSessionJSONL(ctx, path) error`, `TailJSONL(ctx, path, offset) (<-chan JSONLEntry, error)`. `TailJSONL` emits ALL entry types (no assistant-only filter) and closes the channel on ctx cancel.
- `pkg/tuidriver/jsonl.go:297-305` — `IsEndTurn` — assistant + `stop_reason == end_turn` + non-empty assistant text. Stricter than the local `isEndTurn` (which only checked stop_reason); the additional clauses are no-op for Probe 2's text-bearing assistant turns. See § Behavioural notes.
- `pkg/tuidriver/session.go:166-230` — `WritePrompt`, `TypePrompt`, `ClearInputLine`, `Write` contracts. `ClearLineSettle` lives library-side; the local `clearLineSettle` constant is dropped. `Session.Write` is the API surface `sendKeystroke` routes through (current code reaches `session.PTY.Write`).
- `pkg/tuidriver/watchdog.go:66` — `RunWatchdog(ctx, buf, tr, WatchdogOpts{}) error`; internally drives `ParseSpinner` + `Tracker.ObserveSpinner` + `Tracker.CheckWatchdog`, fully subsuming the inline goroutine and replacing the in-file `matchSpinner` consumer.
- `docs/specs/architecture/117-spike-cancel-migrate-off-duplicated-helpers.md` — the precedent spec. This ticket follows the same structure; deviations are noted under § Spike-permission deltas.

## Context

PRs #87/#89/#60/#98/#99 promoted six load-bearing helpers from spike binaries into `pkg/tuidriver/`. PR #119 finished the equivalent migration for `cmd/spike-multi-turn/main.go`; PR #120 for `cmd/spike-cancel/main.go`. `cmd/spike-permission/main.go` is the last spike still hand-rolling the full set (`projectsDir`, `openSessionJSONL`, `tailJSONL`, `isEndTurn`, `typePrompt`, `clearInputLine`, inline watchdog goroutine + `spinnerRe`/`matchSpinner`). This ticket finishes the cleanup so every spike binary exercises the same library APIs consumers will use.

Mechanical cleanup. No new behaviour. `runAutoRespond`'s post-approve readiness predicate (`gotEndTurn ∧ ❯-present ∧ rb.QuietFor() ≥ ptyQuietWindow`) is load-bearing (#70 design) and must not move — AC #4 spells this out and it is the only non-mechanical correctness constraint in the slice. The two-`runSession` orchestrator (Session A + Session B, three probes total) is preserved as-is per AC #5.

## Design

### Substitution table

| Local symbol (delete/adapt)                  | Library replacement (use)                                     |
|----------------------------------------------|---------------------------------------------------------------|
| `func projectsDir`                           | `tuidriver.SessionJSONLPath(home, cwd, sessionID)` inline     |
| `func openSessionJSONL`                      | `tuidriver.WaitForSessionJSONL(ctx, jsonlPath)` w/ timeout    |
| `func tailJSONL`                             | `tuidriver.TailJSONL(ctx, jsonlPath, 0)`                      |
| `func isEndTurn`                             | `tuidriver.IsEndTurn(ev)`                                     |
| `func typePrompt`                            | `session.TypePrompt(prompt)` (Session method)                 |
| `func clearInputLine`                        | `session.ClearInputLine()` (Session method)                   |
| `var spinnerRe` + `func matchSpinner`        | (deleted — only consumer was the inline watchdog)             |
| inline watchdog goroutine                    | `tuidriver.RunWatchdog(ctx, rb, tr, tuidriver.WatchdogOpts{})` wrapped in the same `wg.Add(1)/go func` shape spike-cancel / spike-long-prompt use |
| `eventCh chan map[string]any` (created in `runSession`, cap 32) | `<-chan tuidriver.JSONLEntry` returned by `TailJSONL`, assigned inside `openTailerHook` |
| `sendKeystroke(ptmx *os.File, ...)`          | `sendKeystroke(session *tuidriver.Session, ...)` — body becomes `session.Write(keystroke)` |
| `newSessionID(flagValue, dir) (id, jsonl, err)` | `resolveSession(flagValue) (id, err)` + inline `tuidriver.SessionJSONLPath` at call site |
| `func logObservationEvent(_, _, map[string]any)` | `logObservationEvent(_, _, tuidriver.JSONLEntry)` — see § Helpers staying in-file |
| `func isToolUse(map[string]any) bool`        | `isToolUse(tuidriver.JSONLEntry) bool` — nil-guarded struct access |
| `func extractByMsgID([]map[string]any, _)`   | `extractByMsgID([]tuidriver.JSONLEntry, _)` — struct-field access |
| `func msgIDOf(map[string]any) string`        | `msgIDOf(tuidriver.JSONLEntry) string` — nil-guarded |

### `run()` body changes

Three changes; orchestrator shape (two `runSession` calls) stays:

1. Delete `projDir, err := projectsDir(); ...; logger.Printf("projects-dir path=%s", projDir)` (lines 303-307). The `projects-dir` log line was removed by both #116 and #117 precedents — e2e-runner's success criterion is the `SUCCESS:` marker on stdout, not stderr log shape (`cmd/e2e-runner/main.go:226-231`).
2. Resolve `home`/`cwd` once via `os.UserHomeDir()` + `os.Getwd()` after `startedAt := time.Now()`, with the same `fmt.Errorf("resolve home dir: %w", ...)` / `fmt.Errorf("resolve cwd: %w", ...)` wrap shape spike-cancel:181-187 uses.
3. Rename `newSessionID(flagValue, dir) (string, string, error)` → `resolveSession(flagValue) (string, error)` (matches spike-cancel:791-804 + spike-multi-turn:471-484 + spike-long-prompt:283-296). At each of the two Session-A/Session-B call sites, derive `jsonlA`/`jsonlB` via `tuidriver.SessionJSONLPath(home, cwd, sessionA)` / `tuidriver.SessionJSONLPath(home, cwd, sessionB)`.

The existing `session-id-resolved id=%s jsonl=%s tag=a` (line 319) and `tag=b` (line 332) log lines stay verbatim — they are emitted from `run()`, not from inside `resolveSession`, so the rename doesn't touch their shape.

### `runSession` body changes

The body keeps the same phases (tracker → spawn → defer-close → watchdog → idle wait → trust modal → idle-predicate-check → eventCh slot + openTailerHook → probe loop). Per-phase deltas:

1. **Watchdog block** (lines 395-424). Replace with the spike-cancel:226-234 pattern:

   ```go
   wg.Add(1)
   go func() {
       defer wg.Done()
       if err := tuidriver.RunWatchdog(ctx, rb, tr, tuidriver.WatchdogOpts{}); err != nil {
           logger.Printf("%v", err)
           cancelCause(err)
       }
   }()
   ```

   `RunWatchdog` owns its own 1 Hz tick (`DefaultWatchdogTick`), drives `ParseSpinner` + `Tracker.ObserveSpinner` + `Tracker.CheckWatchdog` internally, and returns on ctx cancel or wedge. Subsumes `matchSpinner` entirely.

2. **Channel slot + hook** (lines 463-486). Replace:
   - Drop `eventCh := make(chan map[string]any, 32)`.
   - Declare `var eventCh <-chan tuidriver.JSONLEntry` (nil at declaration).
   - Replace `openTailerHook`'s body (preserving the `sync.Once` wrap). Inside the Once:
     1. `tuidriver.WaitForSessionJSONL(jsonlCtx, jsonlPath)` with a `sessionFileWait`-bounded ctx (mirror spike-cancel:274-279 / spike-multi-turn:217-222).
     2. Log `session-jsonl-opened path=%s offset=0 tag=%s` verbatim (preserves the existing log line at line 473, including the `tag=` field).
     3. `tuidriver.TailJSONL(ctx, jsonlPath, 0)` — assign the returned channel to the outer `eventCh` via the lexical closure.
   - The current `wg.Add(1)` block that wrapped the local `tailJSONL` (lines 474-483) is **deleted entirely** — `TailJSONL` owns its own goroutine and closes the channel on ctx cancellation. The watchdog `wg.Add(1)` block is **replaced** (per item 1 above), not deleted.

   `sync.Once` STAYS for minimal-diff parity. In practice each `runSession` invocation invokes the hook at most once (Session A: only Probe 1 / `runObserve` passes a hook; Session B: only Probe 2 / `runAutoRespond` passes a hook — `runEscalate` has no `postPromptHook` parameter), so the Once is defensive.

3. **Probe dispatch** (lines 488-505). Call-site signature changes:
   - All three probe-runner calls take `session` (not `ptmx`) and `&eventCh` (not `eventCh`).
   - The probe `for i, p := range probes` loop is unchanged.

### `runObserve` / `runAutoRespond` / `runEscalate` signature changes

Three parameter changes (same shape across all three runners); everything else stays:

- `ptmx *os.File` → `session *tuidriver.Session`. `runObserve` and `runAutoRespond` use this for `session.ClearInputLine()` + `session.TypePrompt(prompt)`. `runAutoRespond` additionally uses it for `sendKeystroke(session, approveKey)`. `runEscalate` uses it for `session.ClearInputLine()` + `session.TypePrompt(prompt)`. `rb *tuidriver.Buffer` stays — callers continue to pass `session.Buffer`.
- `eventCh <-chan map[string]any` → `eventChRef *<-chan tuidriver.JSONLEntry`. Pointer indirection is required because `openTailerHook` assigns the channel mid-call inside `runObserve` (Session A) and `runAutoRespond` (Session B); both probe-runners see a nil channel at entry and a populated one post-hook. Same shape as spike-cancel's `runProbe` (line 327). For `runEscalate`, the channel was populated by the prior probe (Probe 2's `runAutoRespond` hook fire), so the pointer is non-nil at entry — but it still receives `eventChRef *<-chan tuidriver.JSONLEntry` for signature parity, dereferencing once at function entry.
- The hook param `postPromptHook func() error` stays on `runObserve` and `runAutoRespond` (called inside if non-nil); `runEscalate` continues to NOT accept a hook param.

Inside each runner, after the hook fires (or immediately at entry for `runEscalate`), dereference once: `eventCh := *eventChRef`. The dereferenced value is used by the receive-loop case statements below. The pointer never leaves the runner.

### Per-runner body changes

#### `runObserve` (lines 515-592)

1. **ClearInputLine** (line 530-532). Replace with `session.ClearInputLine()`. Error wrap `"clear input line"` preserved.
2. **TypePrompt** (line 533-535). Replace with `session.TypePrompt(prompt)`. Error wrap `"write prompt"` preserved.
3. **Observation-window loop** (lines 576-587). Add closed-channel guard + assistant-only filter at the receive site, mirroring spike-cancel:564-576:

   ```go
   case ev, ok := <-eventCh:
       if !ok {
           eventCh = nil
           continue
       }
       if ev.Type != "assistant" {
           continue
       }
       logObservationEvent(logger, probeN, ev)
       count++
   ```

   The filter preserves the original behaviour: the pre-migration local `tailJSONL` filtered to assistant-only before emit, so non-assistant entries never reached `logObservationEvent` (which logs `type=assistant` as a fixed substring). The filter also keeps `count` semantically equivalent to "assistant events drained during the window."

#### `runAutoRespond` (lines 597-744)

1. **ClearInputLine** (line 614-616). Replace with `session.ClearInputLine()`.
2. **TypePrompt** (line 617-619). Replace with `session.TypePrompt(prompt)`.
3. **`sendKeystroke` call** (line 638-640). Signature change only — the call becomes `sendKeystroke(session, approveKey)`. See § sendKeystroke signature change.
4. **Post-approve readiness predicate** (lines 691-710). Stays verbatim per AC #4 — including the `events []tuidriver.JSONLEntry` slice (was `[]map[string]any`), the `check()` closure, and the multi-paragraph comment block at lines 644-690 that documents the three-clause predicate, the IsIdle wedge history, the empirical 1500 ms derivation, and the cross-spike sibling references. The only type change is `events []map[string]any` → `events []tuidriver.JSONLEntry`; the predicate body and commentary do NOT change.
5. **Receive-loop case** (lines 712-733). Mirror spike-cancel's three-clause guard pattern at receive site:

   ```go
   case ev, ok := <-eventCh:
       if !ok {
           eventCh = nil
           continue
       }
       if ev.Type != "assistant" {
           continue
       }
       events = append(events, ev)
       if !modalCleared {
           modalCleared = true
           tr.RecordTransition(fmt.Sprintf("probe=%d modal-cleared", probeN))
           logger.Printf("probe=%d modal-cleared", probeN)
       }
       if tuidriver.IsEndTurn(ev) {
           if id := msgIDOf(ev); id != "" {
               latestEndTurnMsgID = id
           }
           gotEndTurn = true
       }
   ```

   The assistant-only filter is required at `modalCleared`'s gate: per the existing inline comment (lines 646-654), `modalCleared` IS "first JSONL `assistant` event after keystroke-sent" (finding #15). Without the filter, the first non-assistant entry library `TailJSONL` emits (e.g. user/attachment/permission-mode) would flip the flag wrongly. Putting the filter at the top of the case (not at each conditional) preserves pre-migration semantics exactly: `events` only accumulates assistant entries; `modalCleared` only flips on first assistant; `extractByMsgID(events, latestEndTurnMsgID)` only sees assistant entries.
6. **`isEndTurn` → `tuidriver.IsEndTurn`** (line 723). 1:1 replacement; semantics differ by the assistant-only and non-empty-text additional clauses, both no-op here (assistant filter is already at top of case; Probe 2's text-bearing reply always has non-empty assistant text). See § Behavioural notes.

#### `runEscalate` (lines 750-823)

1. **Pre-probe drain** (lines 766-773). Adapt to `<-chan tuidriver.JSONLEntry`. The current shape:

   ```go
   drain:
       for {
           select {
           case <-eventCh:
           default:
               break drain
           }
       }
   ```

   becomes (using `eventCh := *eventChRef` resolved at function entry):

   ```go
   eventCh := *eventChRef
   drain:
       for {
           select {
           case <-eventCh:
           default:
               break drain
           }
       }
   ```

   Type-only change. The latent pre-existing concern (a closed channel would infinite-loop this drain because `<-` on a closed channel always selects) is preserved as-is — same shape as spike-cancel:337-346, not new territory for this slice.
2. **ClearInputLine** (line 775-777). Replace with `session.ClearInputLine()`.
3. **TypePrompt** (line 778-780). Replace with `session.TypePrompt(prompt)`.
4. No receive-loop changes — `runEscalate` only drains; it never reads from `eventCh` post-drain.

### `sendKeystroke` signature change

`sendKeystroke(ptmx *os.File, keystroke []byte) error` → `sendKeystroke(session *tuidriver.Session, keystroke []byte) error`. Body becomes `_, err := session.Write(keystroke); return err`. Trades a direct PTY-file write for the library's exposed `Session.Write` so the spike no longer reaches into `session.PTY` for the approve keystroke. The comment at lines 1003-1006 (single-bulk-write reasoning + "renamed from spike-cancel's sendCancel" attribution) stays verbatim.

The trust-folder accept write at line 441 (`ptmx.Write([]byte("1\r"))`) is OUT OF SCOPE — spike-multi-turn, spike-cancel, and spike-long-prompt all still reach through `ptmx` for that one write, and matching their shape minimises this slice's diff. The local `ptmx := session.PTY` binding at line 393 stays for that single call site. The ticket's table mentions an "escalate keystroke" path — inspection confirms `runEscalate` does NOT send any keystroke today (just observes the modal stays open), so there is no second migration site under that label.

### Helpers staying in-file (adapted)

Four helpers stay in `cmd/spike-permission/main.go` because they encode probe-specific accumulation or formatting that's not in the library scope. All four shift from `map[string]any` to `tuidriver.JSONLEntry`:

```
func logObservationEvent(logger *log.Logger, probeN int, ev tuidriver.JSONLEntry)
func isToolUse(ev tuidriver.JSONLEntry) bool
func extractByMsgID(events []tuidriver.JSONLEntry, targetID string) string
func msgIDOf(ev tuidriver.JSONLEntry) string
```

`logObservationEvent` — preserves the three-way `<missing>` / `<nil>` / string discrimination on `stop_reason` (README finding surface — `runObserve`'s observation window logs each event's stop_reason verbatim, and the README distinguishes "field absent" from "field is JSON null" from "field is a literal string"). The typed `Message.StopReason` collapses missing vs. null vs. empty-string into "", so the helper MUST read through `ev.Message.Raw["stop_reason"]` to keep the distinction. Same shape as spike-cancel:586-607 (`logCancelEvent` is the canonical template):

```go
func logObservationEvent(logger *log.Logger, probeN int, ev tuidriver.JSONLEntry) {
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

    logger.Printf("probe=%d observation-event type=assistant stop_reason=%s msg_id=%s",
        probeN, stopRepr, msgID)
}
```

The `nil`-rawMap lookup is safe in Go (`m[k]` on nil map returns zero-value-false). Callers gate on `ev.Type == "assistant"` upstream in `runObserve`'s case body, so in practice `ev.Message` is always non-nil here; the explicit guard is defensive and matches `msgIDOf`'s posture.

`isToolUse` — `if ev.Message == nil { return false }; return ev.Message.StopReason == "tool_use"`. `EntryMessage.StopReason` is parsed zero-value-on-mismatch (empty string for absent OR null), which is exactly what the original `_, _ := msg["stop_reason"].(string)` produced — both return false on absent/null. Semantically equivalent. Same shape as spike-cancel:747-752.

`extractByMsgID` — same body shape as spike-cancel:757-776 and spike-multi-turn:434-453 (read `ev.Message.Content[i].Type == "text"`, take `ev.Message.Content[i].Raw["text"].(string)`, append). Early-return `if targetID == ""` stays. The implicit nil-Message guard via `msgIDOf` returning "" still holds — no extra explicit check needed.

`msgIDOf` — `if ev.Message == nil { return "" }; return ev.Message.ID`. Library emits non-assistant entry types whose `Message` is nil; the guard is load-bearing. Same shape as spike-cancel:781-786.

### Helpers staying in-file (unchanged)

These helpers are spike-permission-specific and do NOT migrate to library calls. They stay verbatim:

- `parseApproveKeystroke`, `parseModalPredicate` (flag parsing).
- `waitForModal` (poll-`hasModal`-until-true; spike-specific predicate).
- `hasModal`, `extractModalText`, `cleanModalLines`, `extractToolName` (modal-specific PTY parsing; no library counterpart).
- `writeTempSnapshot`, `truncateForLog` (logging utilities).
- `hasSpinnerGlyph` (lines 1087-1090) — NOT in AC #1's delete list. Currently referenced only by the compile-time `_ = hasSpinnerGlyph` line at 1280; kept for parity with the spike-suite. Out of scope.
- The compile-time stub `var ( _ = hasSpinnerGlyph; _ = isToolUse; _ = disappearedWindow )` (lines 1279-1283) stays as-is. `isToolUse` is still defined (adapted to `JSONLEntry` per above) but has no active consumer in spike-permission's three probes; the stub keeps the spike-suite-parity intent documented. `disappearedWindow` constant similarly preserved.

### Import diff

Remove (only used by the deleted helpers):

- `bufio`, `encoding/json`, `io`, `path/filepath`, `regexp` (use was `spinnerRe` + `oscRe` + `modalSepRe` — but `oscRe` and `modalSepRe` STAY, so `regexp` STAYS), `strconv`.

Re-check `regexp`: `oscRe` (line 147) and `modalSepRe` (line 181) still need it. Keep `regexp`.

Revised remove list:

- `bufio`, `encoding/json`, `io`, `path/filepath`, `strconv`.

Keep all others. `bytes` stays — `runAutoRespond`'s `check()` closure uses `bytes.Contains(stripped, tuidriver.IdleGlyph)`; `extractModalText`, `extractToolName`, `hasModal`, `hasSpinnerGlyph` all use `bytes`. `regexp` stays — `oscRe` + `modalSepRe`. `github.com/google/uuid` stays — `resolveSession` still uses it.

### Constants

| Constant                | Status after migration |
|-------------------------|------------------------|
| `statePollInterval`     | Keep — drives `runAutoRespond` receive-loop ticker. |
| `jsonlTailInterval`     | Delete — was only the local `tailJSONL` poll. |
| `sessionFileWait`       | Keep — passed to `WaitForSessionJSONL` context timeout. |
| `sessionFilePoll`       | Delete — was only the local `openSessionJSONL` poll. |
| `watchdogTick`          | Delete — library `RunWatchdog` owns its own tick. |
| `ptyQuietLimit`, `spinnerFreezeLimit` | Keep — passed to `NewTracker`. |
| `shutdownGrace`         | Keep — passed to `SpawnOpts`. |
| `disappearedWindow`     | Keep — referenced only by the `_ = disappearedWindow` compile-time stub at line 1282 (no active consumer in spike-permission). |
| `ptyQuietWindow`        | Keep verbatim with commentary at lines 100-115. AC #4. |
| `clearLineSettle`       | Delete — library `ClearInputLine` owns the settle via `ClearLineSettle`. |
| `modalDetectLimit`      | Keep — `waitForModal` deadline. |
| `observationWindow`     | Keep — `runObserve`'s drain window. |
| `escalationWindow`      | Keep — `runEscalate`'s sleep window. |

## Spike-permission deltas (vs. the #117 precedent)

Four things this spec covers that #117 did not:

1. **Two-`runSession` orchestrator** (Session A + Session B, three probes total). `run()` makes two `runSession` calls per the existing shape. Each call has its own `var eventCh <-chan tuidriver.JSONLEntry` slot and its own `openTailerHook` closure (the slot and the closure both live in `runSession`'s frame). The two-session shape is preserved verbatim per AC #5; no orchestrator changes beyond the per-call cascade described above.

2. **`sendKeystroke` migration** (Probe 2's approve keystroke path). Spike-cancel had `sendCancel(session, keystroke)` shipping its own migration; spike-permission's `sendKeystroke` is the structural twin. Direct 1:1 signature swap: `*os.File` → `*tuidriver.Session`, body becomes `session.Write(keystroke)`. The single call site (line 638) updates accordingly.

3. **Three probe-runners instead of one**. Spike-cancel had a unified `runProbe` dispatching to `runCancel` / `runRecovery`; spike-permission has three top-level runners (`runObserve`, `runAutoRespond`, `runEscalate`) each invoked directly from `runSession`'s probe loop. Each runner gets the same parameter cascade (`session *tuidriver.Session`, `eventChRef *<-chan tuidriver.JSONLEntry`); the receive-loop / drain patterns are slightly different per runner but follow the same closed-channel + assistant-only filter discipline.

4. **`logObservationEvent` adaptation** instead of `logCancelEvent`. Different log-line text (`observation-event` vs. `jsonl-cancel-event`), same three-way `<missing>`/`<nil>`/string discrimination on `stop_reason`. Body shape identical to spike-cancel:586-607.

5. **`newSessionID` simplification**. Spike-cancel's `resolveSession` already returned just the ID; spike-permission's `newSessionID` returned `(id, jsonlPath, err)` and consumed a `dir` parameter from `projectsDir()`. Post-migration: rename to `resolveSession`, drop the `dir` parameter, return `(id, err)`. The `jsonlPath` is derived at the call site via `tuidriver.SessionJSONLPath(home, cwd, sessionID)` for each of the two sessions.

## Behavioural notes

`tuidriver.IsEndTurn` is strictly stricter than the local `isEndTurn` (additionally requires `e.Type == "assistant"` AND non-empty assistant text). For spike-permission's Probe 2 payloads — `"list the files in /tmp"` produces a text-bearing assistant reply after tool execution — the two are equivalent. The stricter library predicate is safer (rejects pathological future-claude end_turn-without-text deltas) and is the documented contract. Additionally, the assistant-only filter at the top of `runAutoRespond`'s receive case (per § Per-runner body changes) means `tuidriver.IsEndTurn`'s Type guard is doubly enforced — no semantic drift either way.

Library's `TailJSONL` emits ALL entry types (`assistant`, `user`, `attachment`, `permission-mode`, …); the local `tailJSONL` filtered to assistant-only before emit. Three consumer-side filters re-establish the original behaviour:

- `runObserve`'s observation-window loop gates on `ev.Type == "assistant"` before `logObservationEvent` + `count++`.
- `runAutoRespond`'s receive-loop case gates on `ev.Type == "assistant"` before `events = append`, `modalCleared` flip, and `IsEndTurn` check.
- `runEscalate`'s pre-probe drain has no consumer side effect — it just discards values — so no filter is needed there. The drain will discard more entries per probe boundary than today (non-assistant entries accumulate during Probe 2 → 3 boundary), but the semantics are unchanged.

The `events` slice in `runAutoRespond` will accumulate the same set of entries as today (assistant-only, gated by the case-top filter). No growth differential.

The Probe 2 → Probe 3 boundary in Session B: `runAutoRespond` returns when its readiness predicate fires; `runEscalate` runs next and drains. The library tail goroutine keeps emitting between the two probes (still active on the runSession-scoped ctx). The drain at `runEscalate` entry purges anything queued. Same shape as today.

## Concurrency model

Same shape as today:

- Main goroutine: linear state machine (per-session: idle wait → trust modal → idle-predicate-check → probe loop).
- Watchdog goroutine: `RunWatchdog` (library-owned, 1 Hz, returns on ctx cancel or wedge).
- Tail goroutine: spawned by `TailJSONL` (library-owned, closes channel on ctx cancel).
- No observer goroutine (spike-permission has no `❯-disappeared` observer; this was a spike-cancel/spike-multi-turn-only pattern).

Two `runSession` invocations sequentially: each owns its own tracker, claude subprocess, watchdog goroutine, JSONL tailer goroutine, and event channel. Session A's `runSession` returns (its `defer cancelCause` fires, ctx cancels, watchdog returns, tail goroutine closes the channel, session.Close() SIGTERMs claude A); then Session B's `runSession` runs with a fresh tracker/subprocess/watchdog/tailer/channel.

Shutdown sequence per session unchanged: `defer cancelCause` → ctx cancellation → watchdog returns → tail goroutine closes its channel → `session.Close()` SIGTERMs claude → reader goroutine drains → `Wait()` returns. `wg sync.WaitGroup` stays for template parity even though `wg.Wait()` is never called (same as spike-cancel / spike-multi-turn / spike-long-prompt).

## Error handling

No new failure modes. Library functions return wrapped errors with the same `fmt.Errorf("…: %w", err)` shape the local helpers produced; preserve the existing error-wrap text at each call site (`"open session jsonl"`, `"write prompt"`, `"clear input line"`, `"send approve"`, etc.). Closed-channel handling at the two receive sites mirrors spike-cancel.

## Testing strategy

Verified by `make e2e`. Specifically:

1. **AC #1 mechanical check.** This grep must return empty:

   ```
   grep -n 'func projectsDir\|func openSessionJSONL\|func tailJSONL\|func isEndTurn\|func typePrompt\|func clearInputLine' cmd/spike-permission/main.go
   ```

   Plus `grep -n 'spinnerRe\|matchSpinner' cmd/spike-permission/main.go` must also return empty (AC #1 enumerates both). And `grep -n 'ptmx\.Write' cmd/spike-permission/main.go` should return exactly ONE hit (the trust-folder accept at line 441 / post-migration equivalent), confirming the approve keystroke no longer routes through `ptmx`.

2. **Build.** `make build-bin` succeeds — the binary compiles against the new types.

3. **e2e.** `make e2e` — `spike-permission` step prints `SUCCESS:` for Probe 2 and exits 0. The e2e-runner's only success criterion for this binary is the `SUCCESS:` marker on stdout (`cmd/e2e-runner/main.go:226-231`).

4. **Snapshot drift.** `snapshot-drift` step does NOT report new drift on `picker`/`mcp`/`agents` snapshots — this slice doesn't touch any code those snapshots cover (they live in `pkg/tuidriver/testdata/` and exercise other spike binaries). AC #6.

No new unit tests added — the e2e binary is itself the integration test. Library entrypoints already have their own unit tests under `pkg/tuidriver/*_test.go`.

## Open questions

None. The two preceding migrations (PR #119 for spike-multi-turn, PR #120 for spike-cancel) landed in the last two commits on `main` and the diff is mechanical; library entrypoints are stable; the only correctness constraint (verbatim `runAutoRespond` readiness predicate) is called out as AC #4; the two-`runSession` orchestrator is called out as AC #5.

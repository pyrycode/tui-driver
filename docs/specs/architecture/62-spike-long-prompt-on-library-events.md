# Spec: refactor `cmd/spike-long-prompt` onto the library JSONL / Events APIs

**Ticket:** [#62](https://github.com/pyrycode/tui-driver/issues/62)
**Size:** S
**Status:** ready for development

## Files to read first

Load these in order; each entry says what to extract.

- `cmd/spike-long-prompt/main.go:1-289` — the entire `main`/`run` function and the constants block. The refactor is mostly net deletion inside `run`; you need the full current shape in your head before deleting pieces.
- `cmd/spike-long-prompt/main.go:308-463` — the private helpers being deleted: `projectsDir`, `resolveSession`'s jsonl-path concatenation, `openSessionJSONL`, `tailJSONL`, `isEndTurn`, `extractAssistantText`. Every line here goes away except `resolveSession`'s UUID handling (kept; library doesn't own session-ID policy).
- `pkg/tuidriver/jsonl.go:35-78` — `SessionJSONLPath(home, cwd, sessionID)` and `WaitForSessionJSONL(ctx, path)`. Drop-in replacements for `projectsDir` + the jsonl-path concat + `openSessionJSONL`. Note `WaitForSessionJSONL` takes ctx for the deadline (no internal timeout constant).
- `pkg/tuidriver/jsonl.go:80-321` — `JSONLEntry` / `EntryMessage` / `ContentBlock` typed shape plus `IsEndTurn` and `AssistantText`. The refactored spike consumes `AssistantText(ev.Entry)` on the `EventKindJsonlEndOfTurn` event; `IsEndTurn` is called by the library and does not need to be invoked at the call site.
- `pkg/tuidriver/events.go:1-117` — the `Event` shape, the `EventKind*` constants (`EventKindPtyIdle`, `EventKindPtyThinking`, `EventKindJsonlEntry`, `EventKindJsonlEndOfTurn`, `EventKindPtyModal{Shown,Hidden}`), and `Session.Events(ctx, jsonlPath, startOffset)`'s signature + close-on-cancel contract. The new spike loop ranges over this channel.
- `pkg/tuidriver/events.go:118-246` — the merge loop's start-blind semantics ("a buffer already idle at subscription fires `EventKindPtyIdle` on tick one"). This dictates how the spike's slow-path log gating must handle the very first `EventKindPtyIdle`.
- `pkg/tuidriver/session.go:1-170` — `Spawn`, `SpawnOpts`, `Session.WritePrompt`, `Session.Buffer`. Unchanged; the refactor uses these as-is.
- `pkg/tuidriver/state.go:38-54` — `IsIdle` / `IsThinking` predicates. The pre-`Events()` idle-wait at `main.go:171-175` still uses `IsIdle` directly (the library can't tail JSONL until the file exists, and the file doesn't exist until after the first prompt — so `Events()` opens after the pre-prompt idle is already past).
- `pkg/tuidriver/trust.go:25` — `HasTrustModal`. Still used pre-`Events()` for the trust-folder dance (same reason as above).
- `pkg/tuidriver/tracker.go:69-160` — `NewTracker`, `RecordTransition`, `ObserveSpinner`, `CheckWatchdog`. Unchanged; the watchdog goroutine and its tracker calls stay in the spike.
- `cmd/spike-long-prompt/README.md:46-160` — the "What it does" steps and the "Required state log lines" list. Both need wording updates to reflect the library-API path. The substring-assertion section, fixture description, surprises/findings, and claude-version-dependency sections stay verbatim.
- `docs/specs/architecture/61-unified-events-channel.md:23-51` — the design context for `Events()` (one merge loop owned by the library, scope rationale, what's out of scope). Cited so the developer can confirm the spike's collapse matches the API's intended consumption shape.

## Context

#61 (merged 2026-05-23) shipped `Session.Events()` — the unified PTY+JSONL channel — alongside the per-entry primitives `IsEndTurn` and `AssistantText` from #60 and the deterministic session-path helpers `SessionJSONLPath` / `WaitForSessionJSONL` from #58 / #59. `cmd/spike-long-prompt/main.go` is the binary that originally inlined every piece of that — `projectsDir`, `resolveSession`, `openSessionJSONL`, `tailJSONL`, `isEndTurn`, `extractAssistantText`, and a dual-`select` watcher over a `map[string]any` channel. It was the proof of concept that drove the public-API extraction; now the same binary should consume the public APIs end-to-end, doubling as the first real integration test for that surface.

The refactor is a net deletion. The state-machine shape of `run` stays the same — wait-idle → optional trust-folder accept → write-prompt → wait-for-JSONL → wait-end-turn → substring-assertion — but each piece below the prompt boundary collapses onto library calls. The binary's behavioural contract (the substring assertion against `ALPHA_42-GAMMA_88-OMEGA_13`, the watchdog deadlines, the trust-folder policy flag, the embedded fixture) is unchanged.

**Why one ticket, not several:** the deletions only make sense together. You cannot remove `tailJSONL` without consuming `Events()`; you cannot consume `Events()` without `WaitForSessionJSONL`; the private end-of-turn discriminator and the assistant-text extractor disappear in the same edit that consumes the library's equivalents. Splitting would force interim states where the spike has half-private, half-library plumbing in the same loop — strictly worse than a single coherent collapse.

What is explicitly **out of scope** for this slice:

- **Other spike binaries.** `cmd/spike-one-turn`, `cmd/spike-cancel`, `cmd/spike-multi-turn`, `cmd/spike-permission`, `cmd/spike-ask-user`, `cmd/spike-multiselect`, `cmd/probe-first-prompt-hang` may have similar private JSONL plumbing. The ticket body restricts scope to `cmd/spike-long-prompt`; other spikes refactor in follow-ups if and when their authors choose to.
- **Verb extraction in the library.** `Session.Events()`'s `EventKindPtyThinking` exposes presence, not the spinner verb. The spike continues to extract the verb opportunistically from `Buffer.Snapshot()` at the moment of the thinking event so the `thinking-detected verb="<captured>"` log line keeps its existing shape. Promoting verb extraction into the library is a separate concern with its own design tradeoffs (the regex is documented as incomplete for class-C spinner renderings — `pkg/tuidriver/state.go:18-26`).
- **Watchdog migration.** The 1 Hz watchdog goroutine stays in the spike. The library's `Tracker` already owns the timeout-state policy; only the *driver* of the policy (the ticker, the snapshot, the spinner-seconds extraction) lives in the consumer. Promoting the watchdog driver into the library is out of scope here — no other consumer surfaced the need yet.
- **Pre-`Events()` idle wait via the library's event channel.** Conceptually `EventKindPtyIdle` could drive the pre-prompt idle wait too, but `Session.Events()` requires a `jsonlPath` that exists, and the JSONL file is created by claude only after the first prompt. The pre-prompt idle wait therefore stays on `tuidriver.WaitUntil + IsIdle`.

## Design

### Files touched

```
cmd/spike-long-prompt/main.go      (MODIFIED — net deletion of ~150 lines, ~30 new lines)
cmd/spike-long-prompt/README.md    (MODIFIED — "What it does" steps 7-11 + "Required state log lines" reworded)
```

Two files. One production source file. Well under the red lines (5 files / 150 production-source lines / 5 exported types).

### Refactor shape — `run` body

The before/after structural shape:

**Before** (current `run` body, ~200 lines): `projectsDir` → `resolveSession` → spawn → watchdog → `WaitUntil(IsIdle)` → trust-folder dance → `WritePrompt` → `openSessionJSONL` → spawn `tailJSONL` goroutine on private `chan map[string]any` → dual-`select` loop over `eventCh` + 50 ms `probe.C` ticker reading `Buffer.Snapshot()` for spinner edges → call private `isEndTurn` / `extractAssistantText` on the matching event → substring assertion.

**After** (post-refactor `run` body, ~100-120 lines): generate-or-validate UUID → `home := os.UserHomeDir(); cwd := os.Getwd(); jsonlPath := tuidriver.SessionJSONLPath(home, cwd, sessionID)` → spawn → watchdog → `WaitUntil(IsIdle)` → trust-folder dance → `WritePrompt` → `tuidriver.WaitForSessionJSONL(ctx, jsonlPath)` wrapped in a 10 s `context.WithTimeout` → `events, err := session.Events(rootCtx, jsonlPath, 0)` → single `for ev := range events` loop dispatching on `ev.Kind` → substring assertion.

Concrete contract for the loop body — the developer writes the code; the spec defines the behaviour each `case` must satisfy:

- **`case EventKindPtyThinking`** — if `thinkingObserved` is already true, ignore (the rising edge has already fired). Otherwise, set `thinkingObserved = true`, capture the verb opportunistically by calling `matchSpinner(tuidriver.StripANSI(rb.Snapshot()))` (the same helper kept in this file; see § "What stays local"), call `tr.RecordTransition("thinking-detected")`, and emit `logger.Printf("thinking-detected verb=%q", thinkingVerb)`. If the snapshot regex misses the verb (the documented class-C case), the verb stays empty — matches current behaviour, where the verb is opportunistic.
- **`case EventKindPtyIdle`** — gate on `thinkingObserved && !spinnerGone`. If both hold, set `spinnerGone = true`, call `tr.RecordTransition("spinner-gone")`, emit `logger.Printf("spinner-gone")`. **Crucially**, ignore the very first `EventKindPtyIdle` before `thinkingObserved` has flipped — the merge loop is documented as start-blind ("a buffer already idle at subscription fires `EventKindPtyIdle` on tick one"; `pkg/tuidriver/events.go:120-127`), so the spike will receive an `EventKindPtyIdle` immediately after `Events()` opens against an already-idle PTY. The `thinkingObserved` gate handles this naturally.
- **`case EventKindJsonlEndOfTurn`** — if `gotEndTurn` is already true, ignore (defensive against a second end-of-turn emission within the same turn — the library is per-entry, see § "Per-entry semantics" below). Otherwise, set `assistantText = tuidriver.AssistantText(ev.Entry)`, set `gotEndTurn = true`, call `tr.RecordTransition("end-turn-detected")`, emit `logger.Printf("end-turn-detected")`.
- **Other event kinds** (`EventKindJsonlEntry`, `EventKindPtyModalShown`, `EventKindPtyModalHidden`, `EventKindUnknown`) — fall through; the spike does not need them. The `EventKindJsonlEntry` event fires for every parsed JSONL line including the one that carries the end-of-turn, but the spike consumes the synthetic `EventKindJsonlEndOfTurn` that follows it. No double-extraction.
- **Termination condition** — after each event, check `if gotEndTurn && (!thinkingObserved || spinnerGone) { break }`. Same predicate as today; identical semantics. The fast path (spinner not observed because the spinner regex misses class C, but `end_turn` still lands) skips through.

After the loop exits, distinguish completion from cancellation: if `rootCtx.Err() != nil`, the watchdog or shutdown closed the channel — `return fmt.Errorf("wait termination: %w", context.Cause(rootCtx))`. Otherwise proceed to `tr.RecordTransition("assistant-text-extracted")`, the existing `assistant-text-extracted len=...` log, and the substring assertion.

### What stays local in `main.go`

Keep these — they are not in-scope for this refactor:

- `expectedTokens` constant.
- `promptFixture` (the `//go:embed`).
- `spinnerRe` regex + `matchSpinner` helper. Used by (a) the watchdog goroutine to feed `tr.ObserveSpinner(ok, total)` with the seconds count, and (b) the new `EventKindPtyThinking` case to capture the verb for the empirical log. Both reads target `rb.Snapshot()` directly.
- Trust-folder handling (`tuidriver.HasTrustModal(rb.Snapshot())` + the keystroke + the post-trust `WaitUntil` over `!HasTrustModal && IsIdle`).
- The pre-`Events()` `tuidriver.WaitUntil(rootCtx, func() bool { return tuidriver.IsIdle(rb.Snapshot()) })` for the initial idle gate.
- The watchdog goroutine in full (1 Hz ticker, spinner observation, `tr.CheckWatchdog`).
- `resolveSession`'s UUID generation / parsing logic — the function shrinks to UUID-only (no jsonl-path concat) and the caller composes `tuidriver.SessionJSONLPath` separately.

### What goes away

Delete entirely:

- `projectsDir` (replaced by `os.UserHomeDir()` + `os.Getwd()` + `tuidriver.SessionJSONLPath`).
- `openSessionJSONL` (replaced by `tuidriver.WaitForSessionJSONL` wrapped in `context.WithTimeout`).
- `tailJSONL` (replaced by `Session.Events`).
- `isEndTurn` (replaced by the library's `EventKindJsonlEndOfTurn` event; the predicate is called internally by `mergeEvents`).
- `extractAssistantText` (replaced by `tuidriver.AssistantText(ev.Entry)`).
- The `eventCh chan map[string]any` declaration + the goroutine that owned `tailJSONL`.
- The `probe := time.NewTicker(statePollInterval)` and its `<-probe.C` case (its work is now done by the library's merge loop).
- Constants that go unused after the deletions: `statePollInterval`, `jsonlTailInterval`, `sessionFilePoll`. Keep `sessionFileWait` (used as the `context.WithTimeout` deadline passed to `WaitForSessionJSONL`).
- Unused imports: `bufio`, `bytes`, `encoding/json`, `io`, `path/filepath`. Verify by running `go build` after the edit — Go's import-block trimming is mechanical.

`resolveSession` shrinks. The current signature `(flagValue, dir) (sessionID, jsonlPath string, err error)` becomes `(flagValue string) (sessionID string, err error)`; the jsonl-path computation moves into the caller (which already needs `home` / `cwd` to call `SessionJSONLPath` anyway).

### `Session.Events()` lifecycle wiring

Pass `rootCtx` directly to `Events()`. The library's merge goroutine returns when ctx is cancelled (closing the output channel within one poll tick) or when the internal JSONL tail closes. The spike's existing `defer cancelCause` at the top of `run` already covers shutdown — when the watchdog cancels (or the deferred shutdown fires), `Events()`'s channel closes, the `for ev := range events` loop exits naturally, and the post-loop ctx-check returns the cause.

No explicit `Events()` cleanup needed: the library owns its goroutine and closes the channel on ctx-done.

`WaitForSessionJSONL` is called with a fresh `context.WithTimeout(rootCtx, sessionFileWait)` to preserve the 10 s deadline the spike used today; cancel that derived context as soon as the call returns (defer cancel). Errors propagate up via the existing `return fmt.Errorf(...)` shape — the library wraps `context.DeadlineExceeded` so the spike's caller (`make e2e`, operators) sees a clear "session JSONL ... did not appear" message.

### Per-entry semantics (defensive read)

The library's end-of-turn signal is per-entry: every JSONL line whose `IsEndTurn(e)` holds emits one `EventKindJsonlEntry` immediately followed by one `EventKindJsonlEndOfTurn`. For the spike's prompt (a single instruction expecting a single-line response), claude emits one assistant message whose text block triggers exactly one such pair. Defensive against a future fixture / model that emits multiple lines under one `message.id`: the `if gotEndTurn { ignore }` guard accepts the first end-of-turn and ignores subsequent ones. Same behaviour as today — the original `if !gotEndTurn && isEndTurn(ev) { ... }` had the same first-wins shape.

### Concurrency model

After the refactor, the spike runs three goroutines (down from four):

1. **Main goroutine** — owns `run`'s state machine, executes the `for ev := range events` loop.
2. **PTY reader** — owned by `tuidriver.Spawn`; drains the PTY master into the rolling buffer. Unchanged.
3. **Watchdog** — 1 Hz ticker, polls `rb.Snapshot()`, calls `tr.ObserveSpinner` + `tr.CheckWatchdog`. Cancels `rootCtx` via `cancelCause` on watchdog trip. Unchanged.

Removed: the private `tailJSONL` goroutine. The library spawns its own merge goroutine inside `Session.Events()` but that's owned by the library; the consumer doesn't count it. (Internally the library spawns two goroutines under `Events()`: `tailJSONLLoop` from `TailJSONL` + `mergeEvents` from `Events`. The consumer sees one output channel.)

Shutdown order is unchanged: `defer session.Close()` runs first → SIGTERM/SIGKILL claude + drain PTY reader. `defer cancelCause(errors.New("shutdown"))` runs next → library merge goroutine returns within one poll tick → `for ev := range events` exits → watchdog goroutine returns on ctx-done. The `sync.WaitGroup` continues to gate `run`'s return on the watchdog finishing.

### Required state-log lines after refactor

The README's "Required state log lines (in order)" block updates to:

```
session-id-resolved id=<uuid> jsonl=<path>   # fires before pty.Start
idle-detected
prompt-written
session-jsonl-opened path=<path> offset=0    # fires after WaitForSessionJSONL returns
thinking-detected verb="<captured>"          # slow path only
spinner-gone                                 # slow path only
end-turn-detected
assistant-text-extracted len=<n>
shutdown-signalled
```

Differences from today:

- `prompt-loaded bytes=<n>` is removed. The original log line fired after `strings.TrimRight(promptFixture, ...)`; that line is one local variable and contributes nothing the test caller can act on. Drop it for symmetry with the post-refactor reduced surface. (If you want to keep it, fine — make sure the README's required-lines list still reflects what `main.go` actually emits.)
- *Optional*: keep `prompt-loaded bytes=<n>` if it simplifies the diff. Both shapes are acceptable; pick one and make `main.go` + README agree. The README change in this refactor is mostly about the JSONL discovery / tailing path, not this one line.

If `idle-detected-post-trust` and `trust-folder-accepted` are needed (`-trust-folder=accept` path), they still fire from the trust-folder section — unchanged. The README's existing prose mentions them under the optional-flag section; no edit needed there.

### README rewording

Sections to edit in `cmd/spike-long-prompt/README.md`:

- **§ "What it does"** — steps 7-11. Step 7 (WritePrompt) stays. Step 8 changes from "Polls `os.Stat(jsonlPath)` every 100 ms with a 10 s timeout" to "Calls `tuidriver.WaitForSessionJSONL(ctx, jsonlPath)` under a 10 s `context.WithTimeout` deadline (the library polls at `DefaultPollInterval`, 50 ms)." Step 9 changes from "Tails the JSONL from offset 0 ... filtering for `type=="assistant"` events with `message.stop_reason=="end_turn"`" to "Calls `session.Events(ctx, jsonlPath, 0)` and ranges the unified channel; the library's per-entry end-of-turn discriminator (`IsEndTurn`: `end_turn` + non-empty text) emits `EventKindJsonlEndOfTurn` carrying the matching `JSONLEntry`." Step 10 changes from "Concatenates every `content[].text` on that record" to "Reads the assistant text via `tuidriver.AssistantText(ev.Entry)`." Step 11 (substring assertion) unchanged.

- **§ "Required state log lines (in order)"** — see § "Required state-log lines after refactor" above. Update the listing in-place.

- **All other sections** — unchanged. Specifically: the "What's being tested" three-drift-vector list stays (the vectors are unchanged by the refactor — bracketed-paste is still the thing under test). The "Why a substring assertion" rationale stays. The fixture description stays. The "How to run" commands and flags stay. The "Observed timings" / "Observed thinking verbs" / "Surprises / findings" populated record stays — those are empirical, not subject to refactor. The "Claude version dependency" and "Why no automated unit tests" sections stay.

### Error handling

Three classes of error after the refactor:

1. **Library setup errors** — `SessionJSONLPath` does not error (pure function); `WaitForSessionJSONL` returns wrapped `context.DeadlineExceeded` on timeout or stat-error on filesystem failure; `Session.Events` returns the open/seek error synchronously from `TailJSONL`. All three propagate via `return fmt.Errorf("...: %w", err)` at the call site, matching the spike's existing wrap style (`"open session jsonl: %w"`, `"resolve projects dir: %w"`).
2. **Loop-during-cancellation** — `for ev := range events` exits when the merge goroutine closes the channel. Check `rootCtx.Err()` after the loop; if non-nil, return `fmt.Errorf("wait termination: %w", context.Cause(rootCtx))`. Same shape as the existing `<-rootCtx.Done()` arm.
3. **Substring assertion failure** — unchanged: `FAIL: substring %q not found in %q`.

### Testing strategy

This binary has no unit tests by design (the README documents why — the spike IS the rig). Verification is end-to-end:

- **Build & run against live claude** — `go build -o /tmp/spike-long-prompt ./cmd/spike-long-prompt && /tmp/spike-long-prompt -trust-folder=accept`. Expected outcome: `SUCCESS: <text containing ALPHA_42-GAMMA_88-OMEGA_13>` on stdout, exit 0. This is the same acceptance criterion in the ticket body.
- **`make e2e`** — the harness already enrols this spike; its outcome surfaces there.
- **`go build ./...`** — sanity-check the import-block trim and the package still compiles.
- **`go vet ./cmd/spike-long-prompt/...`** — catches the trivial mistakes (unused imports, dead helpers left behind).

The library APIs being consumed already have unit-test coverage: `pkg/tuidriver/jsonl_test.go` covers `SessionJSONLPath`, `WaitForSessionJSONL`, `TailJSONL`, `IsEndTurn`, `AssistantText`; `pkg/tuidriver/events_test.go` covers the merge loop's transition semantics. The spike is the integration test.

## Open questions

- **Keep or drop `prompt-loaded bytes=<n>`?** The line is structurally redundant with `prompt-written` (the body is loaded immediately before being written; the byte count is recoverable from the prompt write itself). Dropping it shortens the required-lines list and matches the post-refactor reduced ceremony. Keeping it preserves backward-compatibility for any external log-parser. Recommendation: keep it; the marginal cost is one `logger.Printf`, and the README's "Required state log lines" block already documents it. The current implementation is fine.

## Why no behaviour changes

The refactor changes how the spike does its work, not what it asserts. Three things stay invariant:

1. **The wire shape under test** — `Session.WritePrompt` (the bracketed-paste path) is untouched. The whole point of the binary is to regression-test that path; replacing the JSONL plumbing around it cannot change what bracketed-paste does on the wire.
2. **The substring assertion** — `expectedTokens = "ALPHA_42-GAMMA_88-OMEGA_13"` and the `strings.Contains(strings.TrimSpace(assistantText), expectedTokens)` check stay byte-identical.
3. **The watchdog deadlines and the trust-folder policy flag** — explicit acceptance criteria from the ticket; both are preserved by the design above (watchdog unchanged; trust-folder dance unchanged; `-trust-folder` flag unchanged).

# Spec: Promote `TypePrompt` + `ClearInputLine` PTY primitives to `pkg/tuidriver` (#87)

## Files to read first

- `pkg/tuidriver/session.go:122-160` — `WritePrompt` precedent (godoc shape, method-on-`*Session`, `bracketedPaste` pure-helper factoring). The new methods mirror this style exactly: a thin `*Session` method that delegates to a deterministically-testable inner function plus a clock seam.
- `pkg/tuidriver/session_test.go:60-148` — existing test patterns. `TestSpawnWriteSendsToPTY` and `TestSessionWritePromptSendsBracketedPaste` use `exec.Command("cat")` to echo PTY writes back through the rolling buffer; `TestBracketedPasteWrapping` table-tests the pure byte-shape helper. The new tests use both patterns.
- `cmd/spike-cancel/main.go:720-802` — canonical source for `typePrompt` + `clearInputLine` (the two methods to port, with the clearest godoc). The `// copied from cmd/spike-multi-turn/main.go — keep in sync until library extraction` attribution is exactly what this ticket retires.
- `cmd/spike-cancel/main.go:90-100` — `clearLineSettle = 50 * time.Millisecond` constant. The library's `ClearLineSettle` mirrors this.
- `cmd/spike-ask-user/main.go:55-80` — consumer's local `clearLineSettle` constant (line 77-79). Becomes unused after this ticket and is deleted.
- `cmd/spike-ask-user/main.go:140-210` — call sites. Line 143 binds `ptmx := session.PTY`; lines 201 and 204 call `clearInputLine(ptmx)` / `typePrompt(ptmx, prompt)`. These are the two edits.
- `cmd/spike-ask-user/main.go:439-468` — local copies to delete (30 lines).
- `docs/knowledge/architecture/system-overview.md:100` — "Prompt-submit convention" key-signal entry naming the four-consumer pattern and the empirical 10 ms / 50 ms calibration. The library godoc points at this and at #71 / PR #77.

## Context

Four spike binaries (`spike-ask-user`, `spike-multi-turn`, `spike-cancel`, `spike-permission`) carry copy-pasted `typePrompt` + `clearInputLine` helpers with explicit "keep in sync until library extraction" attribution comments. Two empirical calibrations underlie these helpers:

1. **Byte-by-byte typing with a 10 ms inter-byte delay and a 50 ms settle before `\r`** — claude 2.1.148's TUI auto-paste-detection heuristic mis-classifies fast bulk writes of short prompts. When tripped, the `\r` is absorbed into the paste body and the turn never commits. Empirical derivation: #71 / [PR #77](https://github.com/pyrycode/tui-driver/pull/77), commit `e7c3dd2`.
2. **`Ctrl-U` (`0x15`) + 50 ms settle before each new prompt** — post-cancel, claude restores the cancelled prompt as drafted input. Without `Ctrl-U`, the next `typePrompt` concatenates onto the residue. Idempotent on empty input. See `cmd/spike-cancel/README.md` surprise 2.

The `WritePrompt` method already in `pkg/tuidriver/session.go:146` is the wrong tool for short prompts — it wraps the payload in bracketed-paste markers, which is correct for long/multi-line prompts where claude's heuristic would have flipped to paste-mode anyway. Both senders survive after this ticket; they serve different prompt-size regimes.

This ticket promotes the two PTY primitives to the library so future consumers (the `pyry acp` rewrite) can reuse the calibration without re-deriving it from spike source. `cmd/spike-ask-user/main.go` is migrated as proof of usability; the three other spikes are out of scope (one follow-up ticket per spike, each trivial after this lands).

## Design

### Package placement

The new code lives in `pkg/tuidriver/session.go` alongside `WritePrompt`. Rationale: same shape (method on `*Session`, writes to `s.PTY`), same domain (prompt-submission semantics), same test file. A sibling `input.go` would split a tiny related surface across two files for no benefit. Existing precedent is one file per concern; "prompt-submission" is one concern.

### Exported constants

Add to `pkg/tuidriver/session.go` near `DefaultShutdownGrace`:

```go
// PromptInterByteDelay is the pause between bytes when TypePrompt writes a
// prompt body. Empirical value from #71 / PR #77.
const PromptInterByteDelay = 10 * time.Millisecond

// PromptCommitSettle is the pause between the last prompt byte and the
// trailing \r commit byte that TypePrompt writes. Empirical value from
// #71 / PR #77.
const PromptCommitSettle = 50 * time.Millisecond

// ClearLineSettle is the pause after the Ctrl-U byte ClearInputLine writes,
// allowing the input handler to process the line-kill before the next byte
// arrives.
const ClearLineSettle = 50 * time.Millisecond
```

These are exported (capitalised) because the AC names them as part of the contract — consumers calibrating their own retry/timeout budgets should be able to read these constants rather than re-derive them.

### `Session.TypePrompt`

Signature: `func (s *Session) TypePrompt(text string) error`

Behaviour: write each byte of `text` to `s.PTY` individually with `PromptInterByteDelay` between writes; pause `PromptCommitSettle`; write a single `\r` byte as a separate `PTY.Write` call. Returns the first non-nil PTY-write error, or nil.

Godoc must:
- Name the empirical source (#71 / PR #77) and the failure mode it averts (claude 2.1.148 paste-detection swallowing `\r`).
- Contrast with `WritePrompt`: short-prompt byte-by-byte vs long/multi-line bracketed-paste. Direct the reader to choose based on prompt length / presence of newlines.
- Note that single-byte `\r`-less keystrokes (ESC, `1\r`) do NOT go through `TypePrompt` — bulk `Write` is correct for those (no `\r` → no paste-detection misclassification risk).

### `Session.ClearInputLine`

Signature: `func (s *Session) ClearInputLine() error`

Behaviour: write `[]byte{0x15}` (Ctrl-U) to `s.PTY` as a single bulk write; sleep `ClearLineSettle`; return.

Godoc must:
- Name the empirical source (`cmd/spike-cancel/README.md` surprise 2) and the failure mode it averts (drafted-input residue after cancel concatenating with the next prompt).
- State idempotency on empty input.
- Recommend calling before every `TypePrompt` that follows a prior turn (the spike-cancel convention).

### Pure-helper factoring (mirror `bracketedPaste`)

`WritePrompt` factors its byte-payload generation into `bracketedPaste(text string) []byte`, which is table-tested in `TestBracketedPasteWrapping` without spawning a PTY. The byte stream `TypePrompt` writes IS the prompt body verbatim followed by `\r` — there's nothing meaningful to factor as a pure payload-builder. Instead, factor the **clock seam** so timing-dependent behaviour is testable without `time.Sleep`-based assertions:

```go
// Package-private. The default sleep function is time.Sleep; tests
// override it to capture call sequences without real wall-clock waits.
var sleepFn = time.Sleep
```

Both `TypePrompt` and `ClearInputLine` call `sleepFn(...)` instead of `time.Sleep(...)`. Tests swap it in `t.Cleanup`. (No public knob — internal seam only.)

### Concurrency

No new goroutines, no new channels, no new mutexes. `*os.File.Write` is goroutine-safe on POSIX, matching `Session.Write` and `Session.WritePrompt`. `TypePrompt` holds the PTY writer for the duration of its byte stream (~10 ms × N bytes + 50 ms) — concurrent callers from another goroutine writing to `s.PTY` would interleave bytes with prompt body, which is a caller bug, not a library invariant the new code needs to enforce. Same posture as `WritePrompt`.

### Error handling

Both methods return on first PTY-write error. No retry, no wrapping. Matches `WritePrompt`. The PTY being closed mid-`TypePrompt` (e.g. during shutdown) surfaces as a `write: file already closed` error from `s.PTY.Write`; callers handle the same as any other PTY-write error.

## Consumer migration: `cmd/spike-ask-user/main.go`

Edits:

1. **Line 143** — `ptmx := session.PTY` stays (still needed for `sendKeystroke`-style writes at lines 181 and 286 which are single-byte / single-bulk-write keystrokes, not prompt-submission).
2. **Line 201** — replace `clearInputLine(ptmx)` with `session.ClearInputLine()`.
3. **Line 204** — replace `typePrompt(ptmx, prompt)` with `session.TypePrompt(prompt)`.
4. **Lines 77-80** — delete the local `clearLineSettle` constant (no other references in this file after step 5).
5. **Lines 439-468** — delete the local `clearInputLine` and `typePrompt` functions and their godocs.

`go vet ./cmd/spike-ask-user` and `go build ./cmd/spike-ask-user` must pass; any remaining import that became unused gets dropped.

Out of scope: the three other spikes (`spike-multi-turn`, `spike-cancel`, `spike-permission`) keep their local copies. Their migrations are tracked as follow-ups per the ticket body.

## Testing strategy

Add tests to `pkg/tuidriver/session_test.go` in the same shape as the existing `TestSessionWritePromptSendsBracketedPaste` and `TestSpawnWriteSendsToPTY` (using `exec.Command("cat")` to echo writes back through the rolling buffer):

Wire-shape tests (the primary assertion per AC 5):

- `TestSessionClearInputLineSendsCtrlU` — call `s.ClearInputLine()`; verify `Buffer.Snapshot()` contains exactly `[]byte{0x15}` within a 2 s deadline. Mirror's TestSpawnWriteSendsToPTY's polling shape.
- `TestSessionTypePromptSendsBytesThenCommit` — call `s.TypePrompt("ping")`; verify the buffer contains `"ping\r"` (or `"ping\n"` — whatever `cat` translates `\r` to on the OS; assert by `bytes.Contains` against both candidates rather than equality, matching how the WritePrompt test asserts).
- `TestSessionTypePromptEmptyString` — call `s.TypePrompt("")`; verify only `"\r"` (single byte) is written. Documents the degenerate case.

Clock-seam tests (timing without wall-clock dependence):

- `TestSessionTypePromptInterByteTiming` — install a fake `sleepFn` that appends each sleep duration to a slice. Call `s.TypePrompt("abc")`; assert the slice equals `[PromptInterByteDelay, PromptInterByteDelay, PromptInterByteDelay, PromptCommitSettle]` (3 inter-byte delays + 1 commit settle). This is the primary timing assertion — does not depend on CI clock fidelity.
- `TestSessionClearInputLineTiming` — install a fake `sleepFn`; call `s.ClearInputLine()`; assert the slice equals `[ClearLineSettle]`.

Cleanup pattern for clock-seam tests:

- Each test installs its fake at the top: `old := sleepFn; sleepFn = fake; t.Cleanup(func() { sleepFn = old })`.
- Tests that mutate the package-level `sleepFn` are NOT safe to run in parallel with each other. None of the existing session tests call `t.Parallel()`, so this matches existing conventions; do not introduce `t.Parallel()` in the new tests either.

Constants check:

- No dedicated test. The constants are exported and named; a test would only restate the literal value, which is precisely what the source declares. The clock-seam tests above already exercise the constants via the assertion they appear in the recorded slice.

## Open questions

- **Should `ClearInputLine` accept an optional settle override?** No. The spike-cancel calibration is the only known-good value; offering an override invites consumers to plug in numbers they haven't validated. If a future consumer needs a different settle, expose a sibling constant or method then. YAGNI now.
- **Should `TypePrompt` return early when `text == ""`?** No — the bare-`\r` write is the documented degenerate case (matches the table-test entry above). An empty prompt is silly but defined behaviour.
- **Does this need to be tested against a real `claude` process?** No — the spike binaries already exercise the same byte shape through real claude in CI via `make e2e`. After migration, `cmd/spike-ask-user` is the integration witness for the library code. Adding a library-level e2e against `claude` would duplicate that without extra coverage.

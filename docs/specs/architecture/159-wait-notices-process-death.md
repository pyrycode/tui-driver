# Spec #159 — Make the wait primitives notice process death

**Size:** XS (PO sized S; architect override downward — see § Sizing).
**Security-sensitive:** no (no attacker-influenceable content classification; this is process-lifecycle plumbing).

## Files to read first

- `pkg/tuidriver/wait.go:27-43` — `WaitUntil`: the poll loop this design mirrors. Extract the ticker+select shape and the `context.Cause(ctx)` cancel contract. **Do not modify this function** — it is a standalone exported primitive with 7 tests and no Session in scope.
- `pkg/tuidriver/ready.go:33-54` — `WaitReady` + the `Readiness` struct (its public return contract). Extract the single `WaitUntil(...)` call site you will redirect, and the godoc that documents the error contract.
- `pkg/tuidriver/session.go:121-124` — the `exited chan struct{}` / `exitErr error` fields. Extract: `exitErr` is populated **before** `exited` is closed.
- `pkg/tuidriver/session.go:182-188` — the `cmd.Wait` observer goroutine: `s.exitErr = cmd.Wait(); close(s.exited)`. Extract the happens-before edge you rely on (reading `exitErr` after observing the closed channel is race-free).
- `pkg/tuidriver/session.go:403-417` — `Session.Wait()`: the existing blocking consumer of `exited`. Extract the pattern (`<-s.exited` then read `s.exitErr`); the new wait reads the same signal but must **not** block on `readerDone`.
- `pkg/tuidriver/ready_test.go:38-64` — the two existing tests. `TestWaitReady` shows the directly-constructed `&Session{buffer: NewBuffer(0)}` fixture; `TestWaitReadyContextCancelled` is the **regression guard for AC #3** (nil `exited` → keeps polling → ctx cause). Your new test mirrors this fixture but pre-closes `exited` and sets `exitErr`.
- `pkg/tuidriver/wait_test.go:35-52` — `TestWaitUntilContextCancelled` / `...PropagatesCauseFromCancelCause`: the `errors.Is` assertion idiom to reuse for the new error.
- `pkg/tuidriver/answer.go:90-104` — the package's error-message convention: `fmt.Errorf("tuidriver: <op>: ...%w", ...)`. Match the `tuidriver:` prefix. (There are no existing typed errors in the package; this spec introduces the first.)

## Context

`WaitUntil` polls its predicate at `DefaultPollInterval` and returns only on predicate-true or ctx cancel. If `claude` dies during startup, the idle predicate never becomes true, so `WaitReady` polls a dead session for the **entire context timeout** before failing — and the error it surfaces is the generic ctx cause, which hides the real reason (the process is gone).

The `Session` already carries the exit signal a session-aware wait can select on: `exited` is closed by the `cmd.Wait` observer once the process exits, and `exitErr` is populated *before* that close. `WaitReady` is a `*Session` method, so it already has direct field access to `s.exited` / `s.exitErr` — no new plumbing to the process is required. Today's `WaitUntil` simply doesn't watch that channel.

## Design

Two source files change; `WaitUntil` is left untouched.

### 1. New unexported wait method (`wait.go`)

Add a `*Session` method that mirrors `WaitUntil` but adds a third select arm watching the exit signal:

```go
// waitUntilOrExit behaves like WaitUntil but also returns promptly with a
// *ProcessExitedError if the session's process exits before predicate
// becomes true. A nil s.exited (directly-constructed Session, no live
// process) makes the exit arm block forever — identical to WaitUntil.
func (s *Session) waitUntilOrExit(ctx context.Context, predicate func() bool) error
```

Behavior contract (spell out in godoc, implement as a ticker+select loop):

- Short-circuit `if predicate()` on entry → `nil` (unchanged from `WaitUntil`; an already-idle session returns ready even if the process has also exited).
- Loop select over three arms:
  - `<-ctx.Done()` → `return context.Cause(ctx)` (unchanged).
  - `<-s.exited` → `return &ProcessExitedError{Err: s.exitErr}`.
  - `<-ticker.C` → re-check predicate; return `nil` when true.
- **Nil-channel invariant:** a directly-constructed `&Session{...}` has a nil `exited`. Receiving on a nil channel blocks forever, so the exit arm is inert and the loop degrades to exactly `WaitUntil`'s behavior — this is the correct "process still running / no exit awareness" path. No explicit nil-guard needed; the language guarantees it.
- **Race-freedom:** reading `s.exitErr` only after the `<-s.exited` receive succeeds is safe — the `close(s.exited)` in the observer goroutine (`session.go:187`) happens-after `s.exitErr = cmd.Wait()` (`session.go:186`), establishing the happens-before edge. Same guarantee `Session.Wait()` relies on.

Reuse `DefaultPollInterval` and the existing ticker/`defer ticker.Stop()` idiom from `WaitUntil`. Do **not** watch `readerDone` — the wait must fire on process exit, not on final-byte drain.

### 2. New typed error (`ready.go`)

Add alongside `Readiness` (both are `WaitReady`'s public return contract, discoverable together):

```go
// ProcessExitedError reports that the session's child process exited before
// WaitReady observed idle. Err is the process's exit status (from cmd.Wait),
// or nil on a clean exit-before-ready. Match with errors.As; the cause is
// available via the Err field or errors.Is (Unwrap).
type ProcessExitedError struct {
	Err error
}

func (e *ProcessExitedError) Error() string // "tuidriver: process exited before ready[: <Err>]"
func (e *ProcessExitedError) Unwrap() error  // returns e.Err
```

- `errors.As(err, new(*ProcessExitedError))` → matches (satisfies "typed error matchable with errors.As").
- `.Err` field → direct access to the underlying `exitErr` cause (satisfies "underlying exitErr cause available").
- `Unwrap` → `errors.Is(err, <someExitErr>)` also reaches the cause.
- `Error()`: when `Err != nil`, append `": " + e.Err.Error()`; when nil, emit the bare message. Keep the `tuidriver:` prefix per package convention.

### 3. Redirect `WaitReady` (`ready.go`)

Change the single call site:

```
- if err := WaitUntil(ctx, func() bool { return IsIdle(s.Snapshot()) }); err != nil {
+ if err := s.waitUntilOrExit(ctx, func() bool { return IsIdle(s.Snapshot()) }); err != nil {
```

The existing `return Readiness{}, err` propagates the new error unchanged (zero `Readiness` on any non-nil error — unchanged contract). Update `WaitReady`'s godoc: the error is now `context.Cause` on cancellation **or** `*ProcessExitedError` when the process exits first; still `nil` + populated `Readiness` on idle.

## Data flow

```
cmd.Wait() returns ──► s.exitErr = err ──► close(s.exited)      (observer goroutine, session.go:185-188)
                                                │
WaitReady ─► s.waitUntilOrExit(ctx, IsIdle∘Snapshot)            (ready.go:43)
                │ select {
                │   <-ctx.Done()  ─► context.Cause(ctx)         (process alive, cancelled)
                │   <-s.exited    ─► &ProcessExitedError{s.exitErr}   (process died first)
                │   <-ticker.C    ─► IsIdle? ─► nil             (reached idle)
                │ }
```

## Concurrency model

No new goroutines. The design adds one receive arm on an existing channel closed by the existing observer goroutine. The nil-channel-blocks-forever semantics and the close-before-read happens-before edge are the only two concurrency facts the implementer must preserve; both are already established patterns in `session.go`.

## Error handling

| Situation | `exited` | Return |
|---|---|---|
| Reaches idle | any | `nil` + populated `Readiness` (unchanged) |
| Process exits before idle | closed, `exitErr` set | `&ProcessExitedError{Err: exitErr}` |
| Process exits cleanly before idle | closed, `exitErr == nil` | `&ProcessExitedError{Err: nil}` |
| ctx cancelled, process alive | nil or open | `context.Cause(ctx)` (unchanged) |

## Testing strategy

Add to `ready_test.go` (bullet scenarios — write in the project idiom, reuse the `&Session{buffer: NewBuffer(0)}` fixture):

- **`TestWaitReadyProcessExited`** (the AC #4 test): construct a Session with `buffer` holding a non-idle snapshot, a **pre-closed** `exited` channel, and a set `exitErr` (e.g. a sentinel `errors.New` or a fake exit error). Call `WaitReady` with a generous ctx timeout (e.g. 2s). Assert: (a) returns well within the timeout — measure elapsed, bound it far below 2s (e.g. `< 500ms`); (b) `errors.As(err, new(*ProcessExitedError))` succeeds; (c) the matched error's `.Err` equals (or `errors.Is` the) set `exitErr`; (d) returned `Readiness` is the zero value.
- **`TestWaitReadyProcessExitedCleanExit`** (optional but cheap): same fixture with `exitErr == nil` → still returns `*ProcessExitedError`, `.Err == nil`, `errors.As` still matches.

Regression guards already in place (do not remove, note in the test file why they matter):

- **`TestWaitReadyContextCancelled`** — nil `exited` + never idle + short ctx → still returns the ctx cause. This is the AC #3 "process alive, cancelled" guard; it passes unchanged because the nil exit arm is inert.
- **`TestWaitReady`** subtests — idle snapshots still return `nil` + `Readiness`; unaffected (short-circuit path).
- **`TestWaitUntil*`** (`wait_test.go`) — unchanged because `WaitUntil` is untouched.

Optionally add a direct unit test for `waitUntilOrExit` in `wait_test.go` (pre-closed `exited` → `ProcessExitedError`; nil `exited` + short ctx → ctx cause; predicate-true-on-entry → `nil` even with `exited` closed) if the developer wants tighter coverage of the primitive independent of `WaitReady`.

## Acceptance criteria (implementer checklist)

- [ ] `WaitReady` returns on the exit event (not after ctx timeout) when the process exits before idle.
- [ ] The early return is a `*ProcessExitedError` (matchable via `errors.As`), distinct from the ctx-cancellation error; the `exitErr` cause is reachable via `.Err` and `Unwrap`.
- [ ] Idle path unchanged (`nil` + populated `Readiness`); ctx-cancel-while-alive path unchanged (`context.Cause(ctx)`).
- [ ] `TestWaitReadyProcessExited` passes: pre-closed `exited` + set `exitErr`, never idle → quick return with an identifiable process-exit error.
- [ ] `WaitUntil` and its tests are untouched; `TestWaitReadyContextCancelled` still passes.

## Sizing

Override S → XS. Two production files (`wait.go`, `ready.go`), one new exported type (`ProcessExitedError`), ~80 LOC total (≈30 production + ≈50 test). `WaitReady` has no in-repo callers (`codegraph_callers` empty — consumed externally by `pyry agent-run`); `WaitUntil`'s impact set is only its own tests + `WaitReady`, and `WaitUntil` is not modified. No consumer cascade, no red lines, no branch overlap.

## Open questions

- **Sentinel vs. typed-only:** this spec uses a typed error only (no `var ErrProcessExited`). `errors.As` + the `.Err` field satisfy both AC clauses. If the pyrycode consumer later prefers a `errors.Is(err, ErrProcessExited)` boolean check, add a sentinel and an `Is` method then — deferred (no observed need).

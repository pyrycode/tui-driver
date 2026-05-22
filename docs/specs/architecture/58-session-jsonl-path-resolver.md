# Spec: library API — deterministic session JSONL path + appearance poll

**Ticket:** [#58](https://github.com/pyrycode/tui-driver/issues/58)
**Size:** S
**Status:** ready for development

## Files to read first

Load these in order; each entry says what to extract.

- `pkg/tuidriver/cwd.go:20-35` — `EncodeCwd` contract: every non-`[a-zA-Z0-9]` byte → `-`, no run-collapse, on-disk canonicalisation before encoding. The new resolver wraps this verbatim — no separate encoder.
- `pkg/tuidriver/cwd_test.go:10-46` — table-driven shape for `EncodeCwd` tests (uses non-existent paths so the canonicalisation fallback fires and byte-transform alone is asserted). Mirror this pattern for the new path-resolver tests.
- `pkg/tuidriver/wait.go:1-44` — `WaitUntil`, `DefaultPollInterval` (50 ms). The new wait helper does NOT use `WaitUntil` (predicate-only loops can't distinguish "stat: not exist" from "stat: permission denied"), but it reuses the cancel-cause semantics: return `context.Cause(ctx)` wrapped with a path-bearing message on deadline/cancellation.
- `pkg/tuidriver/wait_test.go:1-77` — test idioms for context-driven helpers: immediate-true, eventually-true, context cancelled, deadline-exceeded. Mirror these four shapes for the new poll helper.
- `pkg/tuidriver/tuidriver.go:1-65` — package doc + scope statement. The new functions are in-scope under the existing "projects-dir name encoding" remit; the developer extends the scope sentence to mention session JSONL path resolution explicitly.
- `cmd/spike-long-prompt/main.go:310-373` — reference implementation: `projectsDir`, `resolveSession`, `openSessionJSONL`. The library functions are the path-composition and poll-loop bodies of these helpers, generalised; session-ID generation/validation is NOT promoted to the library (ticket explicitly excludes it).
- `docs/knowledge/architecture/jsonl-layout.md:9-32` — canonical statement of the layout (`~/.claude/projects/<encoded-cwd>/<session-id>.jsonl`, flat, polling-after-prompt). The functions must honour this layout exactly. **Do not edit** — documentation phase owns this file.

## Context

The encoding rule for `<encoded-cwd>` is owned by `tuidriver.EncodeCwd` (post-#57: byte-by-byte hyphen map + on-disk canonicalisation), but the surrounding path composition — `<home>/.claude/projects/<EncodeCwd(cwd)>/<sessionID>.jsonl` — and the "wait for the file to appear after first prompt" polling loop are open-coded at every call site:

- 7 in-tree spike binaries each redefine `projectsDir` + `openSessionJSONL` (`cmd/spike-{long-prompt,one-turn,multi-turn,cancel,permission,ask-user}/main.go`, `cmd/probe-first-prompt-hang/main.go`).
- Pyrycode's `agentrun.EncodeProjectDir` drifted from `EncodeCwd` (replaced only `/` and `.`, missed `_` and other non-alnum bytes) — investigation [pyrycode/pyrycode#499](https://github.com/pyrycode/pyrycode/issues/499#issuecomment-4509570303), fix tracked at [pyrycode/pyrycode#501](https://github.com/pyrycode/pyrycode/issues/501).

This ticket closes the drift at source by promoting the two stable shapes (path composition + appearance poll) into `pkg/tuidriver/`. Migrating the 7 spike binaries and pyrycode's `ptyrunner` to consume the new API is **out of scope** for this slice — those are downstream consumers and are tracked separately (see § Out of scope). This slice only ships the library API and its tests.

## Design

### New file

All new code lives in one new file:

```
pkg/tuidriver/jsonl.go         (production)
pkg/tuidriver/jsonl_test.go    (tests)
```

No edits to `cwd.go` or `wait.go`. The new functions are thematically grouped under "where claude writes session logs", separate from the byte-encoder (`cwd.go`) and the generic poll primitive (`wait.go`). The filename `jsonl.go` leaves room for future related primitives (e.g. tail helpers, if and when promoted from spike code).

### Public surface

Two free functions, no new types:

```go
// SessionJSONLPath returns the path that claude writes its per-session
// JSONL log to for a session pinned via `claude --session-id <sessionID>`
// running with working directory cwd. The home argument is typically
// os.UserHomeDir(); pass it explicitly so the function stays pure and
// testable.
//
// The path is computed as filepath.Join(home, ".claude", "projects",
// EncodeCwd(cwd), sessionID+".jsonl"). EncodeCwd is used internally —
// no separate encoder can drift at the call site.
//
// The function does not validate sessionID (consumers do, e.g. via
// uuid.Parse), does not stat the file, and does not error.
func SessionJSONLPath(home, cwd, sessionID string) string

// WaitForSessionJSONL polls path with os.Stat at DefaultPollInterval
// until the file exists or ctx is cancelled. Returns nil when the file
// appears; returns a wrapped context.Cause(ctx) on cancellation /
// deadline (so errors.Is(err, context.DeadlineExceeded) and
// errors.Is(err, context.Canceled) both work). Stat errors that are
// NOT os.IsNotExist are returned wrapped without further polling.
//
// Use after WritePrompt — interactive claude under --session-id defers
// JSONL creation until first input lands (see jsonl-layout.md §
// "Empirical surprise"). The caller owns the timeout via ctx — typical
// usage: ctx, cancel := context.WithTimeout(parent, 10*time.Second).
func WaitForSessionJSONL(ctx context.Context, path string) error
```

### Behaviour contracts

**`SessionJSONLPath`**

- Pure function. No I/O. `EncodeCwd` is called once on `cwd`; it MAY perform an `os.Stat` internally for canonicalisation (its post-#57 behaviour), but the resolver itself adds no syscalls.
- Returns `filepath.Join(home, ".claude", "projects", EncodeCwd(cwd), sessionID + ".jsonl")`.
- No nil-string defence (an empty `home` is a programmer bug; `filepath.Join` handles it sanely; not the library's problem).
- No suffix check on `sessionID` (the ticket explicitly delegates session-ID format to consumers).

**`WaitForSessionJSONL`**

Loop body (sketch — actual code is the developer's):

- Short-circuit: `os.Stat(path)` once before starting the ticker. If it succeeds, return `nil` (no allocation, no goroutine cost). Mirrors `WaitUntil`'s short-circuit pattern.
- If `os.Stat` returns a non-`IsNotExist` error, return it wrapped: `fmt.Errorf("stat session jsonl %s: %w", path, err)`. Do not retry — permission errors and ENOTDIR don't resolve by polling.
- Otherwise create `time.NewTicker(DefaultPollInterval)` (deferred `Stop`).
- Loop:
  - `select` on `ctx.Done()` (returns `fmt.Errorf("session jsonl %s did not appear: %w", path, context.Cause(ctx))`) and `ticker.C` (re-stat; same disposition as above).
- On context exit, the returned error wraps `context.Cause(ctx)`, so `errors.Is(err, context.DeadlineExceeded)` and `errors.Is(err, context.Canceled)` both work, AND the path is in the message for operator diagnostics.

### Why not extend `WaitUntil`

`WaitUntil`'s predicate returns `bool` only — there's no channel for "stat returned EACCES, don't keep polling". Writing the loop directly is ~15 lines and keeps both helpers simple. Cross-referencing in the doc comment (`// Distinct from WaitUntil because stat errors short-circuit the loop`) is preferable to overloading `WaitUntil` with an error-returning predicate variant.

### Doc-comment update in `tuidriver.go`

Extend the existing scope sentence (currently: `"Scope: PTY allocation, rolling byte buffer with quiet-time tracking, projects-dir name encoding, ANSI/OSC stripping, ..."`):

- Add `"session JSONL path resolution + appearance polling"` to the in-scope list (between "projects-dir name encoding" and "ANSI/OSC stripping").
- Leave the existing "Out of scope (consumer's job): JSONL parsing, ..." sentence unchanged — parsing is still the consumer's job; this ticket only promotes path resolution and existence polling.

This is a 1-line doc edit, not a refactor.

### Concurrency model

`WaitForSessionJSONL` runs entirely on the caller's goroutine. No internal goroutine is spawned. The ticker is local to the function and stopped on every exit path via `defer`. Safe to call concurrently with itself (different paths) — no shared state.

### Error handling

Three failure modes, all bubbled to the caller:

| Trigger                                           | Returned error shape                                                  |
|---------------------------------------------------|-----------------------------------------------------------------------|
| `os.Stat` returns a non-`IsNotExist` error        | `fmt.Errorf("stat session jsonl %s: %w", path, err)` — no retry       |
| `ctx` is cancelled / deadline expired             | `fmt.Errorf("session jsonl %s did not appear: %w", path, context.Cause(ctx))` |
| File appears before context exit                  | `nil`                                                                 |

`errors.Is` works through both wrap chains (stat error → underlying syscall error; cancellation → `context.Canceled` / `context.DeadlineExceeded`).

## Testing strategy

`pkg/tuidriver/jsonl_test.go` covers the AC bullets verbatim. Each scenario below is one (sub)test; the developer writes them in the existing `pkg/tuidriver` test idiom (table-driven where natural, otherwise discrete `Test*` funcs as in `wait_test.go`).

### Path-construction tests (`TestSessionJSONLPath_*`)

For deterministic assertions across machines, use non-existent cwds so the canonicalisation in `EncodeCwd` falls through to the byte-transform — same pattern `cwd_test.go` uses. The expected path is `filepath.Join(home, ".claude", "projects", <byte-transform of cwd>, sessionID + ".jsonl")`.

Cases (table-driven):

- cwd with `/` (e.g. `/non-existent/a/b`) — verifies path-separator transform.
- cwd with `.` (e.g. `/non-existent/v1.2.3`) — verifies dot transform.
- cwd with ` ` (space, e.g. `/non-existent/Second Brain`) — verifies space transform.
- cwd with `_` (e.g. `/non-existent/snake_case`) — drift catch (the pyrycode `EncodeProjectDir` bug). Underscore must produce `-`.
- cwd with mixed case (e.g. `/non-existent/CamelCase`) — byte-transform preserves alnum case verbatim.
- cwd with adjacent special bytes (e.g. `/non-existent/a) [b`) — verifies no run-collapse (`) [` → `---`, three hyphens).
- One real-path happy-path test: `home = t.TempDir()`, `cwd = filepath.EvalSymlinks(t.TempDir() + "/work")`, sessionID = arbitrary string. Expected path uses the canonical form. Confirms `EncodeCwd`'s canonicalisation flows through.

Session ID format is opaque to the function — pass arbitrary strings (`"abc"`, `"00000000-0000-0000-0000-000000000000"`); validation is the consumer's job.

### Stat-poll tests (`TestWaitForSessionJSONL_*`)

Use `t.TempDir()` for path isolation. Each scenario is one discrete test:

- **File already exists** — create the file with `os.WriteFile` before the call. Call with `context.Background()`. Expect `nil`, and assert the call returns in well under one tick (<5 ms — same short-circuit assertion as `TestWaitUntilNoAllocOnImmediateTrue`).
- **File appears partway through** — file does not exist at call time. Spawn a goroutine that sleeps ~150 ms then creates the file. Call with `context.WithTimeout(parent, 2*time.Second)`. Expect `nil` and elapsed ≥ ~100 ms (file appears mid-poll). Don't assert a tight upper bound on elapsed time (CI flakiness).
- **Context cancellation** — file never appears. `ctx, cancel := context.WithCancelCause(parent)`; cancel with a sentinel error from a goroutine after ~50 ms. Expect `errors.Is(err, sentinel)` (cause propagates) and `strings.Contains(err.Error(), path)` (path is in the message).
- **Deadline expiry** — file never appears. `ctx, _ := context.WithTimeout(parent, 100*time.Millisecond)`. Expect `errors.Is(err, context.DeadlineExceeded)` and `strings.Contains(err.Error(), path)`. Elapsed must be close to 100 ms (matches `TestWaitUntilContextDeadline`'s 300 ms upper bound — same shape).

`io/fs.ErrPermission` / ENOTDIR error-short-circuit is NOT in the AC and not required by callers. Skip; covered implicitly by the `os.Stat` non-`IsNotExist` arm of the contract.

### No-regression in existing tests

The new file adds tests; it does not edit existing ones. `go test ./pkg/tuidriver/...` must still pass — this is the AC's "no regression" bullet, covered by running the existing suite unchanged.

## Out of scope (reminder, mirrors ticket)

- Migrating the 7 in-tree spike/probe binaries (`cmd/spike-{long-prompt,one-turn,multi-turn,cancel,permission,ask-user}/main.go`, `cmd/probe-first-prompt-hang/main.go`) to consume the new API. Their open-coded `projectsDir` + `openSessionJSONL` stay in place. Migration is mechanical but cross-binary; it will land in a follow-up ticket so this slice stays ≤3 files.
- Migrating pyrycode's `agentrun.EncodeProjectDir` (different repo; tracked at pyrycode/pyrycode#501).
- Promoting session-ID generation/validation (`uuid.NewRandom`, `uuid.Parse`) into the library — ticket explicitly says the library does not need to parse or validate session IDs.
- Promoting `tailJSONL` — that's JSONL parsing, which ADR-0001 keeps in consumer scope.
- Adding configurable poll cadence to `WaitForSessionJSONL`. `DefaultPollInterval` (50 ms) is the only knob; callers tune timeout via context. If a future consumer needs a different cadence, a `WaitForSessionJSONLEvery(ctx, path, interval)` variant can be added then.

## Open questions

- **Should `WaitForSessionJSONL` accept a directory + sessionID rather than a pre-composed path?** Considered and rejected: the two functions compose cleanly (`p := SessionJSONLPath(...); err := WaitForSessionJSONL(ctx, p)`), and forcing the wait helper to re-compose forces callers to also pass home/cwd, which they may have already resolved. The two-function shape mirrors how the spike binaries already structure the call sequence.
- **What if `EncodeCwd` returns a path whose parent directory does not exist?** The path resolver returns the deterministic path regardless; the wait helper returns the "did not appear" error after the timeout. No special handling required — the directory not existing is just the file not existing (transitively). If a future consumer needs to distinguish "claude hasn't run yet in this cwd" from "claude has started but not written input", they can stat the parent themselves; the library doesn't need to expose this.

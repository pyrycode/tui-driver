# Spec: Serialize the PTY write path (#171)

## Files to read first

- `pkg/tuidriver/keys.go:31-38` — `writeRaw`, the single-buffer PTY-write funnel. This is where the `writeMu` acquisition goes for every keystroke path (`AcceptTrust`, `Answer`, `SendEsc`, `Navigate`, `SendKeys`, `AttachInput`).
- `pkg/tuidriver/keys.go:77-94` — `AttachInput`, the production raw-input path. It is the concurrent racer in the AC's motivating example (attach head keystrokes vs. a prompt delivery). Confirms `AttachInput → writeRaw`, so guarding `writeRaw` covers it for free.
- `pkg/tuidriver/session.go:109-140` — `Session` struct. Add the `writeMu sync.Mutex` field here (`sync` is already imported). Note the existing `sync.Mutex` uses guard read-side state; none guards the write FD today.
- `pkg/tuidriver/session.go:301-304` — `WritePrompt`. A **single** `pty.Write` of the whole `bracketedPaste(text)` buffer → reroute through `writeRaw` to pick up the lock with zero new code.
- `pkg/tuidriver/session.go:335-347` — `TypePrompt`. The **multi-write** path (one `pty.Write` per byte + inter-byte sleeps + a separate `\r` commit write). This is the genuine byte-interleaving race and the load-bearing part of the fix — the lock must be held across the *entire* loop, and it must call `s.pty.Write` **directly, not `writeRaw`** (re-entering a non-reentrant `sync.Mutex` self-deadlocks).
- `pkg/tuidriver/session.go:359-365` — `ClearInputLine`. Single Ctrl-U write + `ClearLineSettle` sleep. Hold the lock across the write **and** the settle (same direct-`pty.Write` rule as `TypePrompt`).
- `pkg/tuidriver/session.go:32-36` — `sleepFn` clock seam. Tests swap it for a no-op/`Gosched` fake so the concurrency test runs sub-second while preserving the byte-by-byte interleaving window.
- `pkg/tuidriver/deliver_test.go:69-94`, `:253-266` — established `s := &Session{...}` direct-construction test pattern (internal `package tuidriver`). The concurrency test builds on this.
- `pkg/tuidriver/session_test.go:192`, `pkg/tuidriver/keys_test.go:41` — the `stty raw -echo … exec cat` Spawn idiom (alternative harness; see Testing strategy).
- `pkg/tuidriver/deliver.go:97-121` — `DeliverPrompt` wires `WritePrompt`/`TypePrompt`/`ClearInputLine` as deps. Read to confirm that the clear-then-write *cross-call* sequence is deliberately **out of this ticket's scope** (see Open questions).

## Context

`Session` has several independent write paths onto the same PTY master FD with no synchronization between them:

- **Keystroke funnel** — `writeRaw` (`keys.go:35-38`), used by `AcceptTrust`, `Answer`, `SendEsc`, `Navigate`, `SendKeys`, and the production `AttachInput`.
- **Prompt-delivery helpers** — `WritePrompt`, `TypePrompt`, `ClearInputLine`, which write to `s.pty` **directly, bypassing `writeRaw`**.

The original review filing claimed `writeRaw` was "the single funnel" for all three concerns; that is inaccurate against current code (prompt delivery bypasses it). Guarding `writeRaw` alone would leave the actually-dangerous race unguarded. The fix serializes **all** write paths against one session-level lock.

### Where the real race is (load-bearing — drives both the design and the test)

Go's `os.File.Write` holds an internal per-FD write lock (`internal/poll.FD.writeLock`) across the **entire** call, including its partial-write retry loop. So two concurrent *single* `os.File.Write` calls on the same `*os.File` cannot split each other — `WritePrompt` vs. `AttachInput` (each one `Write`) are already non-interleaving at the byte level via the runtime.

The genuine vulnerability is **`TypePrompt`'s multi-write sequence**: `len(text)+1` separate `os.File.Write` calls. The per-FD lock serializes each individual byte-write, but **not** the sequence — any other writer's `Write` (an `AttachInput`, an `Answer`, a `WritePrompt`) can land *between* two of `TypePrompt`'s byte-writes, splitting the prompt and corrupting an escape sequence or the paste boundary.

The fix is a single shared `writeMu` held for the full duration of each logical write:
- `TypePrompt`/`ClearInputLine` hold it across their whole multi-step sequence.
- The single-write paths hold it for their one write.

Because every path contends on the *same* mutex, no writer can slip inside `TypePrompt`'s byte stream. The shared lock is what protects the multi-write sequence; the per-FD `writeLock` (which the single-write paths already benefit from) does not. Adding the lock to the single-write paths is cheap and makes the invariant uniform and explicit — but its load-bearing role is serializing them *against* `TypePrompt`.

**Test consequence:** a test that only races two single-write paths (e.g. `WritePrompt` vs. `AttachInput`) would pass **even without the fix** (the per-FD lock already serializes them) — a false green. The concurrency test **must** use `TypePrompt` as one racer and assert its multi-byte payload is never split. This is the concrete form of the AC's "`-race` alone will not prove serialization."

## Design

No new types, no signature changes, no exported surface change. One unexported field, four lock acquisitions, one one-line simplification.

### 1. Add the write mutex to `Session`

Add to the struct (`session.go:109`):

```go
// writeMu serializes every write onto the PTY master (pty). Held for the
// full duration of one logical write so a concurrent writer cannot split a
// multi-write sequence — see TypePrompt. Distinct from the read-side mutexes
// (Buffer, tracker, castRecorder): this one guards the write FD only.
writeMu sync.Mutex
```

Zero-value ready; no constructor change in `Spawn`.

### 2. `writeRaw` acquires the lock (covers all keystroke paths + `WritePrompt`)

`writeRaw` (`keys.go:35`) becomes: `Lock()`; `defer Unlock()`; `_, err := s.pty.Write(p)`; `return err`. One buffer, one write, held atomically. This covers `AcceptTrust`, `Answer`, `SendEsc`, `Navigate`, `SendKeys`, and `AttachInput` — all already funnel through `writeRaw`.

Update `writeRaw`'s doc comment: it is now the serialized single-buffer write path (guards `writeMu`); `TypePrompt`/`ClearInputLine` hold the same `writeMu` directly because they span multiple sub-writes.

### 3. `WritePrompt` reroutes through `writeRaw`

`WritePrompt` writes one buffer (`bracketedPaste(text)`) — identical shape to `writeRaw`'s contract. Replace its body with `return s.writeRaw(bracketedPaste(text))`. It inherits the lock, drops a duplicated `pty.Write`, and makes the funnel doc-comment true for `WritePrompt`. **Do not** add a second `Lock()` in `WritePrompt` — that would double-lock through `writeRaw`.

### 4. `TypePrompt` holds the lock across the whole sequence

At the top of `TypePrompt` (`session.go:335`): `s.writeMu.Lock()`; `defer s.writeMu.Unlock()`. The existing body is unchanged and continues to call **`s.pty.Write` directly** (byte loop, settle, `\r` commit). **Must not** route through `writeRaw` — `sync.Mutex` is non-reentrant, so a nested acquisition self-deadlocks. The lock spans every inter-byte `sleepFn(PromptInterByteDelay)` and the `PromptCommitSettle`; holding it across the sleeps is the intended, AC-blessed semantics (an attach keystroke injected mid-prompt would corrupt the prompt).

### 5. `ClearInputLine` holds the lock across write + settle

Same pattern: `Lock()`/`defer Unlock()` at the top of `ClearInputLine` (`session.go:359`); existing body (Ctrl-U write via `s.pty.Write` directly, then `sleepFn(ClearLineSettle)`) unchanged. The single Ctrl-U byte is atomic on its own, but holding across the settle keeps another writer from injecting into the input area during the line-kill window — the same logical-unit reasoning as `TypePrompt`.

### Data flow

```
AcceptTrust / Answer / SendEsc / Navigate / SendKeys / AttachInput
        └─> writeRaw ──[Lock writeMu]── pty.Write(p) ──[Unlock]

WritePrompt ─> writeRaw ──[Lock writeMu]── pty.Write(bracketedPaste) ──[Unlock]

TypePrompt   ──[Lock writeMu]── pty.Write(b0)…pty.Write(bN) · \r ──[Unlock]
ClearInputLine ──[Lock writeMu]── pty.Write(0x15) · settle ──[Unlock]

              (reader goroutine: ptmx.Read → Buffer/mirror — untouched, read side)
```

## Concurrency model

- **One mutex, no ordering hierarchy.** `writeMu` is a leaf lock: while held, the code calls only `s.pty.Write` and `sleepFn`, never another `writeMu`-guarded method and never a different lock. No lock-ordering deadlock is possible.
- **Re-entrancy.** The only funnel-through is `WritePrompt → writeRaw` (one acquisition). `TypePrompt`/`ClearInputLine` acquire directly and use raw `pty.Write`. No path acquires `writeMu` twice.
- **No interaction with the reader goroutine.** `writeMu` guards writes only; the reader (`ptmx.Read` → `Buffer`/mirror) is the read side and is untouched. No new cross-goroutine coupling.
- **No interaction with `Close`/`Wait`.** `Close` does not acquire `writeMu`, so a writer blocked in `pty.Write` cannot deadlock `Close` on the mutex. Write-after-`Close` ordering is explicitly out of scope (ticket).
- **Hold-duration tradeoff (confirmed acceptable).** `TypePrompt` holds `writeMu` for the whole prompt-typing duration (payload length × `PromptInterByteDelay` + `PromptCommitSettle`). Any concurrent writer — including `AttachInput` from an attach head — blocks for that span. This is the natural reading of AC #2 and the desired semantics: a keystroke injected mid-prompt would corrupt the prompt. Documented on the `writeMu` field and in `TypePrompt`'s comment.

## Error handling

- No new error modes. Each helper returns the first non-nil `pty.Write` error exactly as before.
- `defer Unlock()` guarantees release on every path, including the early `return err` inside `TypePrompt`'s byte loop and `ClearInputLine`'s write. (A bare `Unlock()` at the end would leak the lock on a mid-loop write error — use `defer`.)

## Testing strategy

Add one concurrency test (new `pkg/tuidriver/write_serialize_test.go`, or append to `keys_test.go`), internal `package tuidriver`.

**Harness — prefer `os.Pipe` + direct `Session` construction:**
- `r, w, _ := os.Pipe()`; `s := &Session{pty: w}` (the `&Session{...}` idiom from `deliver_test.go`). The write helpers touch only `s.pty` and `s.writeMu` (zero-value ready), so no other field is needed.
- A test goroutine drains `r` into an unbounded `bytes.Buffer` until EOF. A pipe is a lossless, order-preserving byte stream — no rolling-buffer cap and no `MirrorOutput` drop-newest to confound the contiguity assertion.
- Swap `sleepFn` for a fast fake (no-op, or one that calls `runtime.Gosched()` to force a scheduling point between bytes and maximize interleaving) so the test runs sub-second while keeping the multi-write window open. Restore it in a deferred cleanup.

*(Alternative harness: a `stty raw -echo … exec cat` Spawn session — `session_test.go:192` / `keys_test.go:41` — echoes writes back through the real reader path. Rejected as primary because the rolling `Buffer` cap and reader backpressure complicate a lossless full-stream assertion; the pipe is more targeted and deterministic.)*

**Scenario — must include `TypePrompt` as a racer (see Context):**
- Writer A: repeatedly `s.TypePrompt(markerA)` where `markerA` is a run from a single alphabet, e.g. `strings.Repeat("A", 16)`. On the wire each call is `AAAA…A(16)\r`.
- Writer B: repeatedly `s.AttachInput([]byte(markerB))` where `markerB` uses a **disjoint** alphabet, e.g. `strings.Repeat("B", 16)` (matches the AC's "prompt delivery racing an `AttachInput`"). Optionally a Writer C running a second `TypePrompt` with a third alphabet to also assert two multi-write sequences never interleave each other.
- Each writer loops enough times (≥ ~100) that, without the lock, the probability of zero interleaving is negligible. Run under `go test -race` with `GOMAXPROCS ≥ 2`.
- Barrier all writers, close `w`, drain `r`.

**Assertion — payload contiguity via run-length:**
- Because the alphabets are pairwise disjoint and disjoint from `\r`, "no payload split" ⟺ every maximal run of `A` bytes in the received stream has length exactly `len(markerA)`, and likewise for `B` (and `C`). A single foreign byte landing inside a run breaks this. Also assert the count of complete `markerA` tokens equals the number of A-writes (catches drops/mangling).
- **Developer self-check (do this once, don't commit it):** temporarily remove the `writeMu` acquisition from `TypePrompt` and confirm the test **fails** (a split run is observed). This proves the test exercises the invariant rather than passing vacuously. Restore the lock.

**Regression:** run the full package under `go test -race ./pkg/tuidriver/...`. Existing `DeliverPrompt`/`keys`/`mirror` tests must stay green (no signature or behavior change on the happy path).

## Open questions

- **`DeliverPrompt` clear-then-write atomicity — out of scope, do not implement.** `DeliverPrompt` (`deliver.go:97-121`) calls `ClearInputLine` and then `TypePrompt`/`WritePrompt` as two *separate* locked operations; the lock is released between them, so a concurrent writer could inject in that gap. Making the whole clear+deliver sequence atomic is a higher-level concern (a different lock scope, and a policy question about whether an attach keystroke should ever preempt an in-flight delivery). This ticket is per-call byte-interleaving only. Flagged so the developer does not widen the lock to span `DeliverPrompt`.
- **Write-after-`Close` ordering** — out of scope per ticket (separate lifecycle concern). A write racing a `Close` still returns the closed-file error; `writeMu` does not change that.

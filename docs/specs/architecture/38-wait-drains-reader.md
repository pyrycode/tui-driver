# Spec: `Session.Wait()` synchronises with the PTY reader goroutine (#38)

Ticket: [#38](https://github.com/pyrycode/tui-driver/issues/38). Size: XS. One-line behaviour change in `Session.Wait`, docstring update, two test cleanups.

## Files to read first

- `pkg/tuidriver/session.go:60-76` — `Session` struct fields. Both `exited` and `readerDone` already exist as `chan struct{}` and are closed via the same broadcast pattern; this spec only adds a second `<-` receive in `Wait`.
- `pkg/tuidriver/session.go:107-130` — the two goroutines `Spawn` launches: the `cmd.Wait` observer (closes `exited`) and the PTY reader (closes `readerDone` via `defer`). The reader exits when `ptmx.Read` returns a non-nil error, which happens naturally on subprocess exit (EIO/EOF on the master FD) without needing `Close` to intervene. This is the reason `Wait` can safely block on `readerDone` without deadlocking.
- `pkg/tuidriver/session.go:232-238` — the `Wait` method this spec edits. Three lines today (`<-s.exited`, `return s.exitErr`); becomes four (`<-s.exited`, `<-s.readerDone`, `return s.exitErr`) plus a docstring touch-up.
- `pkg/tuidriver/session.go:240-265` — `Close`'s shutdown sequence. Already drains `readerDone` after closing the PTY. Read this to confirm the same channel is used and that `Close` continues to work unchanged: `Close` closes the PTY explicitly to unblock a still-reading goroutine, while `Wait` relies on natural EOF after subprocess exit. Both paths converge on `<-s.readerDone`.
- `pkg/tuidriver/session_test.go:12-35` — `TestSpawnAndBufferReceivesOutput`. The `time.Sleep(50 * time.Millisecond)` at line 30 is the second of the two cleanups; it papers over the same race for the `Buffer` side. Remove it (and the explanatory comment immediately above it).
- `pkg/tuidriver/session_test.go:37-58` — `TestSpawnMirrorReceivesOutput`. Three edits here: drop the `t.Skip` at line 45 and the paper-trail comment at lines 41-44; drop the `time.Sleep` at line 54. The Windows skip at lines 38-40 stays. After the edits the test runs unconditionally on non-Windows.
- Ticket #34 (in PR history, no on-disk spec referenced here) — origin of the `t.Skip`. Context only; no edits required to anything from #34's landing.

## Context

`Session.Wait()` today waits only on `<-s.exited` (the `cmd.Wait` observer's broadcast channel). When the subprocess exits, `Wait` returns immediately — but the PTY reader goroutine may still be draining the final chunks from `ptmx.Read` into the rolling `Buffer` and the optional `SpawnOpts.Mirror`. Any consumer that calls `Wait()` and then reads `Buffer.Snapshot()` or `Mirror.String()` is racing the reader. The race detector flags it on every run of `go test -race ./pkg/tuidriver/ -run TestSpawnMirrorReceivesOutput`.

`TestSpawnMirrorReceivesOutput` (and its sibling `TestSpawnAndBufferReceivesOutput`) currently hide the race with `time.Sleep(50 * time.Millisecond)` after `Wait`. The Mirror test was additionally `t.Skip`-gated during #34 with a paper-trail comment so #34's `go test -race ./...` gate could land. Both tests are using the API the way any consumer naturally would, so the fix belongs in the API: `Wait` should observe the reader's completion the same way `Close` already does.

`Close` (`session.go:250-265`) already waits on `<-s.readerDone` after closing the PTY. The synchronisation primitive exists; `Wait` simply has not been using it. Adding the receive in `Wait` is safe because the reader goroutine exits naturally when the subprocess exits — `ptmx.Read` returns EIO/EOF on the master FD without anyone having to close it — so `Wait` does not need to also close the PTY to unblock the reader, and there is no deadlock risk.

## Design

### `Session.Wait` (`pkg/tuidriver/session.go:232-238`)

Add one line — `<-s.readerDone` — between the existing `<-s.exited` receive and the `return s.exitErr`. The order matters for two reasons:

1. **Reader cannot finish before the subprocess exits** — `ptmx.Read` only returns EOF after the child closes its end of the PTY, which happens when the child exits. So `<-s.exited` is reached first in practice, and waiting on it first matches the causal order.
2. **`exitErr` is populated before `close(s.exited)`** (`session.go:108-109`). Waiting on `exited` first preserves the existing read-after-close guarantee that `s.exitErr` is observable on return.

The doc comment grows from one paragraph to two. The new contract: *bytes that the reader goroutine wrote to `Buffer` or `SpawnOpts.Mirror` before the subprocess exited are guaranteed visible to the caller once `Wait` returns*. The godoc must explicitly name both `Buffer` and `Mirror` so a consumer reading the doc understands that this is the synchronisation point for any reader-side state — not just the exit status.

Specifically, the docstring must convey:

- Wait blocks until **both** the subprocess has exited AND the reader goroutine has drained all PTY bytes.
- After return, `Buffer.Snapshot()` and any `SpawnOpts.Mirror` writes from the dying subprocess are guaranteed visible (happens-before via the channel close).
- The return value is the subprocess's exit status (unchanged from today).
- Safe to call from multiple goroutines and before/after `Close` (unchanged from today — channel receives on closed channels are always safe and instant, so concurrent and post-Close Wait calls keep working).

No signature change. No new types. No new fields. No `WaitReader()` companion method (the ticket's Technical Notes explicitly rule this out).

### `Close` (`pkg/tuidriver/session.go:250-265`)

**Unchanged.** Close's flow (SIGTERM → grace timer → SIGKILL → close PTY → wait reader) is correct as-is. The Close-then-Wait pattern still works: Wait's new `<-s.readerDone` receive is instant because Close already closed that channel. The Wait-then-Close pattern also works: Wait blocks until both channels close (which is fine — Close has no contract requiring it to be called before Wait returns), then Close runs its idempotent shutdown which finds `exited` already closed and skips the SIGTERM dance.

### `TestSpawnMirrorReceivesOutput` (`pkg/tuidriver/session_test.go:37-58`)

Three deletions:

1. Lines 41-45: the paper-trail comment block and the `t.Skip("blocked on #38 — …")` call. The Windows skip at lines 38-40 stays.
2. Line 54: the `time.Sleep(50 * time.Millisecond)` between `s.Wait()` and the assertion. The new `Wait` contract makes it redundant.

After cleanup, the test reads: Windows skip → declare mirror buffer → spawn `echo mirrored` with `Mirror: &mirror` → `defer s.Close()` → `_ = s.Wait()` → assert `mirror.String()` contains `"mirrored"`. No structural changes beyond the deletions.

### `TestSpawnAndBufferReceivesOutput` (`pkg/tuidriver/session_test.go:12-35`)

Two deletions:

1. Lines 28-29: the `// Reader goroutine drains synchronously … take a beat to deliver the EOF after the child exits.` comment.
2. Line 30: the `time.Sleep(50 * time.Millisecond)`.

After cleanup, the test reads: Windows skip → spawn `echo hello` → `defer s.Close()` → `if err := s.Wait()` (with its existing failure check) → assert `Buffer.Snapshot()` contains `"hello"`. The body shrinks by three lines.

### What NOT to touch

- All other tests in `session_test.go`. None of them depend on the old "Wait does not drain reader" behaviour. The codegraph caller search confirms `Session.Wait` has only two consumers in the repo, both being modified above.
- `Close`'s shutdown sequence (lines 250-265). Already correct.
- The two goroutines `Spawn` launches (lines 107-130). The reader's natural EOF-on-subprocess-exit behaviour is exactly what makes Wait's new receive safe; do not add a PTY close or ctx cancel to the reader's exit path.
- `WaitUntil` in `pkg/tuidriver/wait.go`. Unrelated polling helper, despite the name.

## Concurrency model

The change touches one method and adds one channel receive. Goroutines are unchanged:

- **`cmd.Wait` observer** (started by `Spawn`): populates `s.exitErr`, then `close(s.exited)`. Unchanged.
- **PTY reader** (started by `Spawn`): drains `ptmx.Read` into `Buffer` (and optional `Mirror`), exits when `ptmx.Read` returns non-nil error, `defer close(s.readerDone)`. Unchanged.
- **`Wait` caller** (test or consumer): now blocks on both `<-s.exited` and `<-s.readerDone` in that order. New behaviour.
- **`Close` caller**: unchanged — `<-s.readerDone` after PTY close.

Happens-before guarantee chain after this spec:

1. PTY reader's last `Buffer.Append(chunk)` / `Mirror.Write(chunk)` happens-before `defer close(s.readerDone)` (sequenced in the goroutine).
2. `close(s.readerDone)` synchronises-with any `<-s.readerDone` receive.
3. Therefore, after `Wait` returns, any goroutine that called `Wait` is guaranteed to observe the reader's final writes.

This is the same happens-before chain `Close` relies on today; the spec just exposes it to `Wait` callers.

No new locks, no new channels, no new fields. The receive on an already-closed channel is instant and panic-safe (Go spec guarantee), so callers that invoke `Wait` long after the process has exited see no regression in latency.

## Error handling

No new failure modes. `Wait` still returns `s.exitErr` (set by `cmd.Wait()` before `close(s.exited)`, so the read is well-ordered). The reader goroutine's read errors (EOF, EIO from a closed PTY) are discarded as before — they are not surfaced through `Wait`'s return value, and this spec does not change that.

The "what if the reader is stuck forever" concern is theoretical, not observed:

- For subprocess-exit-driven shutdown, `ptmx.Read` returns naturally; reader exits within milliseconds.
- For `Close`-driven shutdown, `Close` closes the PTY explicitly, forcing `ptmx.Read` to return.
- A truly stuck reader would block `Close` today (which already does `<-s.readerDone`); it would now also block `Wait`. Same blast radius, same failure signature. No new defence is justified — evidence-based fix selection: no observed failure mode here.

## Testing strategy

The two existing tests (`TestSpawnMirrorReceivesOutput`, `TestSpawnAndBufferReceivesOutput`) become the proof-of-fix once their `t.Skip` / `time.Sleep` are removed.

Acceptance verification (developer runs all three):

- `go test -race ./pkg/tuidriver/ -run TestSpawnMirrorReceivesOutput -count=3` — passes. Was failing before the spec's behaviour change.
- `go test -race ./pkg/tuidriver/...` — passes. No other test in the package relied on the old "Wait does not drain reader" behaviour.
- `go test -race ./...` — passes. AC #6. Catches any consumer outside `pkg/tuidriver` that might have relied on the old behaviour (search confirms none exist today).

No new tests are required. Adding a third test would test the same primitive the two existing tests already cover. The shape of the existing tests (spawn → Wait → assert reader-side state without sleeping) is already the contract assertion this spec wants.

The `TestSessionCloseIsIdempotent` test (lines 316-337) covers the Wait-after-Close interaction implicitly — it calls `Close` twice and asserts identical returns; with this spec, the path through `Wait` (which Close does not call but which would also work) is structurally equivalent.

## Open questions

None. The fix is mechanical and the ticket's Technical Notes settled the design choice (fold into `Wait`, not a separate `WaitReader`). The only judgement call — order of the two receives — is resolved above (`exited` first, for both causal-order and `exitErr` read-ordering reasons).

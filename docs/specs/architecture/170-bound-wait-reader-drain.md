# Spec #170 — Bound `Wait` so a grandchild holding the PTY cannot hang it

**Size:** XS (PO sized XS; confirmed). One production file (`session.go`): a two-arm `select` in `Wait` plus a docstring re-scope. One regression test appended to the existing `//go:build !windows` test file. No new files, no new types, no signature change.
**Security-sensitive:** no. `#170` has labels `done:po`, `size:xs`, `wip:architect` — no `security-sensitive` label. This is process-lifecycle/liveness plumbing (same class as #38, #159, #164, #168, #169), no attacker-influenceable content classification. Security-review pass skipped per label gate.

## Files to read first

- `pkg/tuidriver/session.go:403-417` — `Session.Wait()`, the sole edit target. Extract the current body (`<-s.exited` → `<-s.readerDone` → `return s.exitErr`) and the two-paragraph doc comment whose happens-before promise this spec re-scopes.
- `pkg/tuidriver/session.go:164-180` — `Spawn`'s grace normalisation: `grace := opts.ShutdownGrace; if grace <= 0 { grace = DefaultShutdownGrace }`, then `shutdownGrace: grace`. **Extract the invariant this fix leans on:** every Spawned session has `s.shutdownGrace > 0` (≥ 3 s default), so `time.After(s.shutdownGrace)` is always a positive, bounded timer — no zero-grace guard is needed.
- `pkg/tuidriver/session.go:182-232` — the two goroutines `Spawn` launches. The `cmd.Wait` observer (185-188) sets `s.exitErr` **before** `close(s.exited)`. The PTY reader (193-232) `defer close(s.readerDone)` and returns only when `ptmx.Read` returns a non-nil error — which needs **every** writer of the PTY slave closed. Extract: this is precisely why a grandchild holding the slave open wedges `readerDone` while `exited` closes normally.
- `pkg/tuidriver/session.go:419-451` — `Close`. It calls `s.pty.Close()` (442) **before** `<-s.readerDone` (443), forcing the master read to return, and (via #168) SIGKILLs the whole process group. Extract: why `Close` is structurally immune to this hang and **must stay unchanged** — do not touch it.
- `pkg/tuidriver/session_signal_other_test.go:70-145` — `TestCloseReapsProcessGroupGrandchild`. Reuse verbatim: the `exec.Command("sh", "-c", …)` + `GC=$!` echo + poll-`Snapshot()`-for-`GC=(\d+)` parse + `syscall.Kill(pid, 0)` liveness probe + `t.Cleanup` reap idiom. The new test is a near-sibling with a different leader command and a different assertion (see Testing strategy). This file is already `//go:build !windows` and already imports `os/exec`, `regexp`, `strconv`, `syscall`, `testing`, `time` — add `errors` for the exit-error assertion.
- `docs/specs/architecture/38-wait-drains-reader.md` — the spec that added `<-s.readerDone` to `Wait` and wrote the happens-before/"final bytes visible" guarantee. Read §Design + §Concurrency model: this spec re-scopes exactly that guarantee to the drained path.
- `docs/specs/architecture/168-close-kills-process-group.md` §Testing strategy (lines 106-112) — the SIGHUP false-green trap. Extract *why the grandchild must ignore SIGHUP*: a session leader that owns the controlling terminal, on exit, sends SIGHUP to its foreground process group; an ordinary sleeper would die and let the reader drain, masking the bug.
- Memory lesson `pty-close-sighup-masks-group-kill-tests` (from #168) — the same false-green trap in one line. The new test's self-validating precondition check (below) is the defence.

## Context

`Wait` (`session.go:413-417`) blocks on `<-s.exited` then unconditionally on `<-s.readerDone`. `readerDone` closes only when the reader goroutine's `ptmx.Read` returns (session.go:228-229), which requires **every** holder of the PTY slave FD to have closed it. When `claude` forks a tool subprocess that inherits the slave FD and keeps it open, the master read never returns EOF: `s.exited` closes (claude is reaped) but `readerDone` never does, so `Wait` blocks forever after the process the caller was waiting on has already gone.

This is specific to the **natural-exit path** (consumer calls `Wait`, never `Close`). `Close` is immune: it closes the PTY master (`s.pty.Close()`, session.go:442) — forcing `ptmx.Read` to return regardless of who holds the slave — and #168 makes it SIGKILL the whole group, so its own `<-s.readerDone` (session.go:443) cannot wedge. Composes cleanly with #168: that handles lingering grandchildren on the `Close` path; this bounds the `Wait` path. Disjoint code, no conflict.

The fix trades away, **on the timeout path only**, `Wait`'s current unconditional happens-before promise ("after `Wait` returns, the process's final bytes are guaranteed visible via `Snapshot()`/recording"). That guarantee must be re-scoped to the reader-drained (normal) path.

## Design

One production file, one method. No new types, no new fields, no new config knob, no signature change.

### `Session.Wait()` (`session.go:403-417`) — bound the reader-drain wait

Replace the unconditional `<-s.readerDone` with a two-arm `select` against a bounded timer, keeping `<-s.exited` first and `return s.exitErr` last:

```
<-s.exited
select {
case <-s.readerDone:            // normal: reader drained
case <-time.After(s.shutdownGrace):   // grandchild holds the slave; give up the drain wait
}
return s.exitErr
```

Contract / behaviour (the developer writes the body; ~7 production lines):

- `<-s.exited` **stays first**, for the same two reasons #38 established: (a) the reader cannot finish before the process exits, so this matches causal order; (b) `s.exitErr` is written before `close(s.exited)` (session.go:186-187), so receiving on `exited` first makes the subsequent `return s.exitErr` race-free. The exit error is therefore safe to return on **both** arms — it does not depend on `readerDone` at all.
- The `select` blocks until **either** the reader drains (`<-s.readerDone`) **or** the grace elapses (`<-time.After(s.shutdownGrace)`), whichever first. On the drain arm, behaviour and latency are identical to today (the timer never fires). On the timeout arm, `Wait` returns after the grace instead of blocking forever.
- Return value is `s.exitErr` on **both** arms — the process's real exit status (or nil). **Never** a substitute or sentinel error. AC #1's "still returns the real exit error" is satisfied by construction: the timeout changes only how long `Wait` *waits*, not what it *returns*.
- **Do not tear the reader down.** The timeout leaves the reader goroutine parked on `ptmx.Read`; it exits later when `Close()` closes the master and reaps it (#168's group SIGKILL also reaps the grandchild). Adding reader teardown to `Wait` is explicitly out of scope (ticket Technical Notes).
- Match `Close`'s existing `time.After(s.shutdownGrace)` idiom (session.go:437) for consistency — no `time.NewTimer`/`Stop` bookkeeping. `Wait` is a once-per-session call; the lingering timer (≤ grace) on the drain arm is negligible and mirrors what `Close` already does.

**Grace source — decided: reuse `s.shutdownGrace`.** The ticket leaves this to the architect. Reuse over a dedicated field:

- `s.shutdownGrace` is already on the session and already normalised to `> 0` by `Spawn` (session.go:164-167). Reusing it keeps the change to the `Wait` body + doc — genuinely XS.
- A dedicated `readerDrainGrace` would add a new exported `DefaultReaderDrainGrace` const, a new `Session` field, a new `SpawnOpts.ReaderDrainGrace` public config knob, and its own `Spawn` normalisation dance — a public API surface for an unobserved need. Per *Evidence-Based Fix Selection*, deferred.
- Accepted coupling: a consumer that sets a short `ShutdownGrace` for aggressive `Close` also gets a tight `Wait` drain bound. That is coherent (both express "how long we tolerate a lingering PTY holder"), and the normal path is unaffected because the reader drains in milliseconds so the timer never fires. If a consumer ever needs to decouple the two windows, split out a dedicated grace then — no observed need today.

### Doc comment (`session.go:403-412`) — re-scope the happens-before guarantee (AC #3)

The docstring is a first-class deliverable here (AC #3), so it must be explicit. Rewrite to three parts. The developer finalises wording; the required semantics:

1. **Lead:** `Wait` blocks until the process exits, then waits **up to a bounded grace** (`ShutdownGrace`; `DefaultShutdownGrace` when unset) for the PTY reader to drain the final bytes. Returns the process's exit error (or nil) on **both** the drained and the timed-out path — never a substitute/sentinel error. Safe from multiple goroutines and before/after `Close` (unchanged).
2. **Reader-drained path (normal):** the reader drains within the grace; once `Wait` returns, bytes the reader wrote to the rolling buffer or the `RecordTo` recording before the process exited **are** guaranteed visible (happens-before via the `readerDone` close) — the synchronisation point for inspecting `Snapshot()` without racing the reader. (This is #38's guarantee, now scoped to this path.)
3. **Bounded-grace path (timeout):** if a tool grandchild inherited the PTY slave FD and holds it open after the process exits, the master read never returns EOF and the reader does not drain; `Wait` returns after the grace with the real exit error, but the "final bytes visible" guarantee **explicitly no longer holds** — `Snapshot()`/recording may be missing the process's final bytes. `Wait` does not tear the reader down; it stays parked until a later `Close()` closes the master and reaps it.

### What NOT to touch

- `Close` (session.go:419-451). Structurally immune (closes the master before `<-s.readerDone`; #168 group-kills). Leave verbatim.
- The two `Spawn` goroutines (session.go:182-232). The reader's natural-EOF exit is the mechanism; do not add a PTY close or ctx cancel to it.
- `SpawnOpts`, `DefaultShutdownGrace`, the `Session` struct fields. No new config, const, or field.
- The four existing `.Wait()` callers (see Testing strategy) — they are the AC #2 regression guards, not edit sites.

## Concurrency model

Unchanged goroutine topology. One method gains a second `select` arm on an existing channel plus a timer:

- **`cmd.Wait` observer** (Spawn): `s.exitErr = cmd.Wait(); close(s.exited)`. Unchanged.
- **PTY reader** (Spawn): drains `ptmx.Read` into buffer/mirror; `defer close(s.readerDone)` when the read returns. Unchanged — and specifically **not** reaped by `Wait`'s timeout.
- **`Wait` caller:** `<-s.exited`, then `select { <-s.readerDone | <-time.After(grace) }`, then `return s.exitErr`. New behaviour, bounded.
- **`Close` caller:** `s.pty.Close()` → `<-s.readerDone`. Unchanged; unaffected by `Wait`'s timeout because it forces the read to return itself.

Happens-before: on the drain arm, `close(s.readerDone)` synchronises-with the receive → #38's chain (reader's last append happens-before the caller's post-`Wait` reads) holds. On the timeout arm there is **no** synchronisation with the reader's in-flight writes — this is the guarantee the doc now scopes away. `s.exitErr` is race-free on both arms via the `exited`-close edge (session.go:186-187), independent of `readerDone`.

## Error handling / edge cases

- **Timeout path returns the real error, not a sentinel** — `return s.exitErr` is reached on both arms; the timer only shortens the wait. AC #1 satisfied structurally.
- **No zero-grace guard needed.** Every Spawned session has `s.shutdownGrace ≥ DefaultShutdownGrace` (Spawn normalises `<= 0` up, session.go:164-167). Hand-built `&Session{}` test fixtures have `shutdownGrace == 0`, but none call `.Wait()` (verified: the only `.Wait()` callers are Spawned sessions — see Testing strategy); such a fixture would anyway block on its nil `exited` at `<-s.exited` and never reach the timer. Do not add a `grace <= 0` guard — it would defend an unreachable state (*Evidence-Based Fix Selection*).
- **Reader stays parked after timeout** — accepted and documented, not a leak beyond `Session`'s existing "caller MUST defer `Close`" contract (Spawn godoc, session.go:146-147). `Close` (or #168's group-kill) reaps it.

## Testing strategy

### AC #2 — normal case unchanged (existing regression guards, no new test)

The four existing `.Wait()` callers already assert "final bytes visible after `Wait`" on the drain path; they stay green unchanged and are the AC #2 guards:

- `session_test.go:25` (`TestSpawnAndBufferReceivesOutput`) — `Wait` then `Snapshot()` contains `"hello"`.
- `mirror_test.go:85`, `mirror_test.go:181` — `Wait` then mirror chunks present.
- `recordto_test.go:110` — `Wait` then recording written.

The developer should run these under `-race` to confirm no regression; no new AC-#2 test is warranted (it would duplicate these).

### AC #1 + AC #4 — the lingering-PTY-holder regression test (new)

Append **`TestWaitBoundedWhenGrandchildHoldsPTY`** (name at developer's discretion) to `session_signal_other_test.go` (`//go:build !windows` — needs `sh` and `syscall.Kill`, neither Windows-safe; folding here adds no new file). Scenario, in bullets (write in the project idiom, reusing the #168 `GC=` idiom):

- **Leader command (race-free HUP-ignore):** `exec.Command("sh", "-c", "trap '' HUP; sleep 300 & echo GC=$!; exit 7")`.
  - The leader sets `trap '' HUP` (SIG_IGN) **before** forking, so the backgrounded `sleep 300` inherits the ignore disposition **from birth** (ignored signals are inherited across fork and preserved across exec) — no fork-to-trap race. This differs deliberately from #168's post-fork inner-shell trap: #168's leader stays alive via `exec cat`, so its grandchild never races the leader's exit; here the leader exits immediately, so the SIG_IGN must predate the fork.
  - `sleep 300` is the grandchild: same process group as the leader (non-interactive shell, no job control), inherits fd 0/1/2 = the PTY slave, and ignores the SIGHUP the kernel sends to the foreground group when the controlling-terminal leader exits — so it **survives** the leader's exit still holding the slave open.
  - `echo GC=$!` prints the sleeper's PID to the slave. `exit 7` makes the leader exit non-zero, so `cmd.Wait()` yields an `*exec.ExitError` (code 7) — this proves `Wait` returns the **real** status, not nil-by-default or a sentinel.
- **Spawn with a short grace** to keep the test fast and to prove the bound reads `s.shutdownGrace`: `Spawn(cmd, SpawnOpts{ShutdownGrace: 500 * time.Millisecond})`. A visible ~500 ms bound (vs. the 3 s default) also documents that the config knob flows into the `Wait` window.
- **Register cleanup first** (runs after the assertion, so it cannot mask the bug — this is the ticket's "must NOT call `Close` before asserting" requirement): `t.Cleanup(func(){ s.Close(); if gcPid > 0 { syscall.Kill(gcPid, syscall.SIGKILL) } })`.
- **Sync + capture PID:** poll `Snapshot()` for `GC=(\d+)` (2 s deadline, #168 idiom); parse `gcPid`. Then `<-s.exited` (bounded by a short deadline via `select` against `time.After`) to confirm the leader is reaped.
- **Self-validating precondition (defeats the false-green trap):** non-blocking `select { case <-s.readerDone: t.Fatalf("precondition broken: reader drained before Wait — grandchild did not hold the slave open"); default: }`. If the grandchild had failed to hold the slave (e.g. SIGHUP not ignored), the reader would have drained and `Wait` would return fast **even with the bug present** — a silent false pass. This hard-fail makes the repro prove itself.
- **The AC assertion — `Wait` is bounded and returns the real status:** run `s.Wait()` in a goroutine delivering to a buffered channel; `select` the result against an **outer** guard `time.After(5 * time.Second)` (>> grace). On outer-guard fire: `t.Fatal("Wait did not return — unbounded block on readerDone regressed")` (fail fast, never hang the suite). On result:
  - assert elapsed `≤ ShutdownGrace + slack` (e.g. `< 2s` for a 500 ms grace) — proves the bound, not merely "eventually";
  - assert the returned error is the real exit status: `errors.As(err, new(*exec.ExitError))` and `ExitCode() == 7` — proves AC #1's "real exit error, never a substitute/sentinel".
- **Optional strengthening (cheap, recommended):** after `Wait` returns, assert `syscall.Kill(gcPid, 0) == nil` — the grandchild is still alive, proving (a) the reader was blocked *because* the slave was held, and (b) `Wait`'s timeout did not tear the reader/grandchild down (the "reader stays parked" contract).

Guard the whole test against a global hang by keeping the outer `select` bound modest (5 s) — a regression fails in ~5 s, not indefinitely.

### Verification commands (developer runs)

- `go test -race ./pkg/tuidriver/ -run 'TestWaitBoundedWhenGrandchildHoldsPTY' -count=3` — new test passes deterministically.
- `go test -race ./pkg/tuidriver/...` — full package green (AC #2 guards + everything).
- `GOOS=windows go build ./pkg/tuidriver/` — still compiles (new test is `!windows`; production change is pure-Go `time`/channel, no new syscall).

## Acceptance criteria (implementer checklist)

- [ ] `Wait` returns within `ShutdownGrace` when the process has exited but the reader has not drained (grandchild holds the slave), and returns the process's real exit error (or nil) — never a substitute/sentinel.
- [ ] Normal case unchanged: reader drains promptly → `Wait` blocks until drain, final bytes remain visible via `Snapshot()`/recording. Existing `.Wait()` tests pass under `-race`.
- [ ] `Wait`'s doc comment re-scopes the "final bytes guaranteed visible"/happens-before contract to the reader-drained path and states it explicitly no longer holds after the bounded-grace timeout.
- [ ] `TestWaitBoundedWhenGrandchildHoldsPTY` (`//go:build !windows`) passes: a SIGHUP-ignoring grandchild holding the slave open does not hang `Wait`; `Wait` returns within the grace and yields the leader's exit status (ExitError, code 7). The test hard-fails its own precondition if the reader drains early, and is guarded by an outer timeout.

## Open questions

None. Grace source is decided (reuse `s.shutdownGrace`; dedicated grace deferred as unobserved-need). Receive ordering is decided (`exited` first, per #38). Test placement and the SIGHUP-ignore construction are decided against the #168 idiom and its false-green lesson.

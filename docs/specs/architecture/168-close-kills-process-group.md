# Spec: Kill the whole process group on Close (#168)

## Files to read first

- `pkg/tuidriver/session.go:419-450` — `Close` as it stands: `SIGTERM → grace → SIGKILL → wait`, idempotent via `shutdownOnce`. The single edit target. Note it signals `s.cmd.Process` (the positive leader PID) twice.
- `pkg/tuidriver/session.go:142-235` — `Spawn`: the child is started via `StartPTY(cmd)` at line 156; `s.cmd` is the leader whose PID we will negate. The `cmd.Wait` observer goroutine (185-188) closes `exited` after reaping — relevant to the reap-race note in Error handling.
- `pkg/tuidriver/pty.go:171-178` — `StartPTY` calls `pty.Start(cmd)`; no `SysProcAttr` set here. Confirms the group-leader property comes entirely from `creack/pty`, not from tui-driver.
- `github.com/creack/pty@v1.1.24/start.go:16-25` — `StartWithSize` sets `SysProcAttr.Setsid = true` (and `Setctty = true`) on every spawn. **This is the invariant the fix relies on**: `Setsid` makes the child a session/process-group leader, so `PGID == PID`, and forked tool children inherit that group. No spawn-time change is needed or wanted here.
- `pkg/tuidriver/cwd_other.go:1-3` and `pkg/tuidriver/cwd_darwin.go:1-3` — the repo's existing platform-split idiom (`//go:build !darwin` / `//go:build darwin`, explicit tag lines). **Mirror this pattern** for the new signal files.
- `pkg/tuidriver/session_test.go:12-32,99-124` — the canonical Spawn/Wait/Close test shape (`exec.Command`, `runtime.GOOS == "windows"` skip, poll-`Snapshot()`-with-deadline). Reuse the polling idiom; **do not** reuse the runtime-skip idiom for the new syscall-based tests (see Testing strategy).
- `docs/specs/architecture/11-spike-cancel.md:277` — the `pgrep -lf '^ls'` orphan-check that already proves tool subprocesses get reaped on cancel; the same technique is the optional live confirmation for this fix.

## Context

`Session.Close` today signals only the **leader** process — `s.cmd.Process.Signal(SIGTERM/SIGKILL)` at `session.go:433,437`. Tool subprocesses that `claude` forks (Bash running a long command, etc.) inherit the leader's process group but are never signalled directly. On a *hard* `Close` — where the leader is SIGKILLed before it can wind down its own children — those children orphan (reparent to init) and keep running. For a `claude` session that means live tool processes with nobody driving them, burning subscription quota.

The fix is narrow: signal the child's **process group** (negative PGID) instead of the single leader PID, for both the initial SIGTERM and the escalated SIGKILL. Because `creack/pty`'s `Setsid` already made the child a group leader (`PGID == PID`), the group is addressed by negating the leader PID — `kill(-pid, sig)` reaches the leader and every process in its group in one call. **No `SysProcAttr` / `Setpgid` change is introduced.**

### Correction to the ticket's Technical Notes (verified against current code)

The ticket asserts the group signal "needs no new build tag" because `session.go` already imports `syscall`. **This is wrong and would break the build.** Verified this run:

- `GOOS=windows go build ./pkg/tuidriver/` **succeeds today** (exit 0) — `creack/pty` ships a Windows stub, so the package is cross-platform-buildable and that property is maintained (cf. the existing `cwd_darwin.go`/`cwd_other.go` split).
- `syscall.Kill` and `syscall.Getpgid` are **Unix-only** — `GOOS=windows` compilation of a file referencing them fails with `undefined: syscall.Kill`. (`syscall.SIGTERM`/`SIGKILL`/`Signal` *do* exist on Windows, which is why today's leader-only `session.go` compiles there.)

Therefore the group-signal code and the process-group unit tests **must** live in `//go:build !windows` files, with a Windows stub preserving today's leader-only behaviour. This keeps the cross-platform build green and costs one extra tiny file — cheaper than a broken `GOOS=windows` build slipping to code-review.

## Design

Three production files. `Close` stays cross-platform and delegates the actual signalling to a small per-GOOS method.

### 1. `session.go` (modified) — `Close` delegates to `signalShutdown`

Replace the two direct `s.cmd.Process.Signal(...)` calls inside the existing `shutdownOnce.Do` block with calls to a new unexported method, leaving every other shutdown step (PTY close, `readerDone` join, recorder close, idempotency, `exitErr` return) untouched:

```
if s.cmd.Process != nil {
    s.signalShutdown(syscall.SIGTERM)
    select {
    case <-s.exited:
    case <-time.After(s.shutdownGrace):
        s.signalShutdown(syscall.SIGKILL)
        <-s.exited
    }
}
```

`session.go` keeps its `syscall` import (`syscall.SIGTERM`/`SIGKILL` are cross-platform constants; `syscall.Signal` is a cross-platform type). The method's two bodies are platform-split.

### 2. `session_signal_other.go` (new, `//go:build !windows`) — the group signal + safety guard

Two symbols:

- **`groupSignalTarget(pid int) (target int, ok bool)`** — pure, no syscalls. Maps a leader PID to the `kill(2)` target that addresses its whole process group, or reports `ok == false` when no *safe* group target exists. Contract:
  - `pid <= 1` → `(0, false)`. This is the catastrophe guard: `kill(-pid)` with `pid == 1` is `kill(-1)` (signal every process the caller may signal) and with `pid == 0` is `kill(0)` (signal the caller's own group). Negative/zero/one PIDs never yield a group target.
  - `pid > 1` → `(-pid, true)`. `-pid < -1` always, so the target addresses exactly process group `pid` and nothing else.
  - Invariant the guard test pins: whenever `ok`, `target < -1` (never `0`, never `-1`).

- **`func (s *Session) signalShutdown(sig syscall.Signal)`** — behaviour summary (precondition: `s.cmd.Process != nil`, guaranteed by `Close`'s existing nil-check):
  1. `target, ok := groupSignalTarget(s.cmd.Process.Pid)`.
  2. If `ok`, `syscall.Kill(target, sig)`; on `nil` error, return (group reached).
  3. Otherwise (guard rejected the PID, or the group signal errored) fall back to `s.cmd.Process.Signal(sig)` — the pre-fix leader-only path.

  Rationale for `-pid` over querying `syscall.Getpgid(pid)`: the group-leader invariant is guaranteed by `creack/pty`'s `Setsid` and pinned by a unit test (AC #1), and `-pid` degrades safely even if that invariant were ever broken — for `pid > 1`, `-pid` is always a specific group id `< -1`, so a stale/absent group yields `ESRCH` and the fallback fires, never a catastrophic signal. Adding `Getpgid` buys nothing here and adds a syscall.

### 3. `session_signal_windows.go` (new, `//go:build windows`) — leader-only stub

- **`func (s *Session) signalShutdown(sig syscall.Signal)`** — one line: `_ = s.cmd.Process.Signal(sig)`. Windows has no POSIX process groups and no `syscall.Kill`; this preserves exactly today's leader-only behaviour (SIGTERM is a no-op error Windows ignores; SIGKILL maps to `os.Kill`). Zero behaviour change on Windows.

### Data flow

```
Close (session.go, all platforms)
  └─ signalShutdown(SIGTERM)                      // then grace, then SIGKILL
       ├─ !windows: groupSignalTarget(pid)
       │            ├─ ok  → syscall.Kill(-pid, sig)   → whole group (leader + tool children)
       │            │        └─ err → Process.Signal(sig)   // fallback: leader only
       │            └─ !ok → Process.Signal(sig)             // fallback: leader only
       └─ windows: Process.Signal(sig)                       // leader only (unchanged)
```

### File naming

Follow the repo's existing convention (`cwd_darwin.go` / `cwd_other.go`): explicit `//go:build` tag line at the top of each file, `_other.go` for the catch-all. The `_windows.go` suffix implies its constraint but write the explicit `//go:build windows` line anyway for parity with `cwd_*.go`. Developer may pick different basenames provided the two `signalShutdown` bodies are mutually exclusive across `windows` / `!windows` — but do not put the `!windows` body in an untagged file.

## Concurrency model

Unchanged. `Close` still runs its body once under `shutdownOnce`, signals, then blocks on `<-s.exited` (closed by the `cmd.Wait` observer goroutine from `Spawn`) and `<-s.readerDone`. `signalShutdown` is a straight-line synchronous call with no new goroutines, channels, or locks. Idempotency, the exit-broadcast pattern, and the reader-join ordering are all preserved verbatim.

## Error handling

- **Guard rejects the PID (`pid <= 1`)** — cannot happen for a real spawned child (kernel PIDs of live processes are `> 1`), but the guard makes the catastrophe structurally impossible regardless. Falls back to leader-only signal.
- **Group signal returns an error** (e.g. `ESRCH` because the group already exited during the SIGTERM grace, or `EPERM`) — falls back to `s.cmd.Process.Signal(sig)`, which itself no-ops harmlessly if the leader is gone. Both errors are intentionally swallowed (matches the pre-fix `_ =` posture); shutdown is best-effort by contract.
- **PID-reuse / reap race (known, accepted; not defended)** — after the SIGTERM grace expires, `Close` signals `-pid` only when `<-s.exited` has *not* fired, i.e. the leader is still alive and its PID/PGID still valid. A sub-window TOCTOU (leader exits *and* is reaped *and* its PID is reused *as a new group leader* between the grace timer firing and the `Kill` call) is astronomically unlikely at a 3 s grace and is not observed. Per *Evidence-Based Fix Selection*, do not add machinery (e.g. re-`Getpgid` under lock) for an unobserved failure mode. The leader-only fallback path retains Go's `os.Process` reaped-PID guard (`ErrProcessDone`).
- **Out of scope (ticket-confirmed):** tool children that `setsid`/daemonize into their *own* new process group escape any single group kill — a property of the idiom, not a bug to solve here.

## Testing strategy

All tests run in `make check` (`go vet ./... && go test ./...`) with **no live `claude`**. The three PTY/syscall tests are Unix-only and **must** carry `//go:build !windows` — a `runtime.GOOS == "windows"` runtime skip (the pattern in `session_test.go`) is insufficient because `syscall.Getpgid`/`syscall.Kill` do not *compile* on Windows. Put them in a new `session_signal_other_test.go` (`//go:build !windows`).

- **`TestGroupSignalTarget` (pure, table)** — the deterministic safety net for AC #4. Cases and expectations:
  - `pid = 2` → `(-2, true)`; `pid = 1234` → `(-1234, true)`.
  - `pid = 1` → `ok == false` (would be `kill(-1)`); `pid = 0` → `ok == false` (would be `kill(0)`); `pid = -5` → `ok == false`.
  - Cross-cutting assertion over every `ok == true` row: `target < -1` — i.e. the function can never direct a signal at "own group" or "every owned process".
- **`TestSpawnChildIsProcessGroupLeader` (AC #1 — the relied-on invariant)** — `Spawn(exec.Command("cat"), SpawnOpts{})`, `defer s.Close()`, then assert `syscall.Getpgid(s.cmd.Process.Pid)` returns `s.cmd.Process.Pid` with no error. Documents that the group-leader property holds *without* any `SysProcAttr`/`Setpgid` change (it comes from `creack/pty`'s `Setsid`).
- **`TestCloseReapsProcessGroupGrandchild` (AC #3 — the synthetic tree)** — proves the group signal reaps a grandchild a leader-only kill would orphan:
  - Spawn `exec.Command("sh", "-c", "sleep 300 & echo GC=$!; exec cat")`. The backgrounded `sleep 300` is the grandchild (child of the leader shell, inherits its PGID); `exec cat` keeps the leader alive and holding the PTY so `Close` exercises the SIGTERM path.
  - Poll `s.Snapshot()` (2 s deadline, existing idiom) for `GC=<pid>`; parse the grandchild PID with a `GC=(\d+)` match.
  - Pre-assert the grandchild is alive: `syscall.Kill(gcPid, 0) == nil`.
  - Call `s.Close()`.
  - Assert the grandchild is reaped: poll `syscall.Kill(gcPid, 0)` (≤ 3 s deadline) until it returns `syscall.ESRCH`. A 300 s sleep cannot exit on its own inside the window, so its death evidences the group signal specifically — a leader-only kill would leave it running (reparented to init).
  - Note the PID-reuse caveat in a comment (same caveat the spike's `pgrep` check carries): reuse of the exact PID within the few-second window is negligible on a test host; if it ever flakes, tighten by also matching the process's start or command, but do not pre-build that.

## Open questions

- **Optional live confirmation (not a gating AC).** Mirror `11-spike-cancel.md`'s orphan check against real `claude`: drive a Bash tool running a long `sleep`, `Close`, then `pgrep` the sleeper to confirm zero orphans. Requires `make e2e` / live `claude`, so it cannot run in the developer's `make check` gate — record as a manual note if run, not as a test.
- **Coordination with #169** (parent-death `Pdeathsig`, Linux-only, build-tagged spawn-attr file). After scoping this ticket off `SysProcAttr` entirely, the two touch disjoint files (`Close` + new signal files here vs. #169's spawn attrs); no native blocker was needed and none is set. WIP=1 serialises them regardless. The branch-overlap check this run found **no** in-flight branch touching `session.go` or the new files.

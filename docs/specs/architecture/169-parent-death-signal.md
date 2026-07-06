# Spec #169 — Set a parent-death signal so Claude dies with the daemon

**Size:** XS · **Security-sensitive:** no (process-liveness signal; no auth/secrets/untrusted-input/parsing surface)

## Context

`Session.Close` (`session.go:431-449`) is the only thing that stops the driven
`claude` today: on orderly shutdown it SIGTERMs, then SIGKILLs the child's
process group (#168). That path runs **only** on an orderly shutdown. If the
host daemon crashes or is `SIGKILL`ed, no cleanup runs — the PTY child is
reparented to init and `claude` keeps rendering headless, burning subscription
quota with nobody driving it.

Linux offers a kernel-level backstop: `SysProcAttr.Pdeathsig`. Set at spawn to
`SIGKILL`, the kernel signals the child the instant its parent dies, no cleanup
code required. This ticket wires that in.

**Complementary to #168, not overlapping** — #168 covers the *controlled* path
(`Close` reaps the group on orderly shutdown); this ticket covers the
*uncontrolled* path (daemon dies without running `Close`; the kernel kills
`claude` via `Pdeathsig`). #168 was scoped to `Close`-only and does **not**
touch `SysProcAttr`; this ticket touches only `SysProcAttr.Pdeathsig`, in new
files. Different files, no logical dependency — no native blocker (dispatcher
WIP=1 serialises them regardless). Verified: #168 has already merged to `main`
(commit `5a39443`).

## Files to read first

- `pkg/tuidriver/pty.go:171-178` — `StartPTY`, the library's single spawn seam.
  Add one `setParentDeathSignal(cmd)` call *before* `pty.Start(cmd)`. This is
  the only production edit to an existing file.
- `pkg/tuidriver/session.go:148-162` — `Spawn` calls `StartPTY`, so the
  production `Session` path is covered for free. (`cmd/spike-*` call
  `pty.Start` directly and won't get the signal — expected; they're demo
  harnesses, not the `Session` path.)
- `pkg/tuidriver/session.go:431-449` — `Close`; the orderly-shutdown path the
  doc comment must reference as the non-Linux fallback.
- `pkg/tuidriver/cwd_darwin.go` + `pkg/tuidriver/cwd_other.go` — the build-tag
  split idiom to mirror: explicit `//go:build <tag>` line + a GOOS-suffix
  filename, with `_other.go` for the fallback (`_other` is not a known GOOS, so
  the explicit tag governs). Copy this shape exactly.
- `pkg/tuidriver/session_signal_other.go:1-5` + `session_signal_windows.go:1-5`
  — the freshest (#168) precedent for a platform-split spawn/signal helper;
  same `<area>_<feature>_<gooskind>.go` naming this spec adopts.
- `pkg/tuidriver/keys_test.go:36-58` — `spawnRawCat`; the raw spawn + reap
  idiom (`Spawn` / `StartPTY` then cleanup) the wiring test mirrors.
- `github.com/creack/pty@v1.1.24/start.go:18-25` (module cache, read-only) —
  `StartWithSize` allocates `SysProcAttr` if nil and only **adds**
  `Setsid`/`Setctty`; it never overwrites a caller-set struct. This is the
  load-bearing invariant: a `Pdeathsig` set before `pty.Start` survives.
  (`pty.Start(cmd)` is the thin wrapper `return StartWithSize(c, nil)`.)

## Design

Three production files, mirroring the `cwd_darwin.go` / `cwd_other.go` split.
No new exported API; `setParentDeathSignal` is package-private.

### 1. `pkg/tuidriver/pty_pdeathsig_linux.go` (new, `//go:build linux`)

Contract: `func setParentDeathSignal(cmd *exec.Cmd)` — allocates
`cmd.SysProcAttr` if nil (so `pty.Start`'s later `Setsid`/`Setctty` still
apply), then sets `cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL`. No return, no
error path.

This file carries the **platform-coverage doc comment** (AC 4): Linux gets a
hard parent-death `SIGKILL`; the kernel delivers it the moment the spawning
process dies, even with no cleanup. Also note the known caveat inline so it
isn't mistaken for a bug: Linux delivers `Pdeathsig` when the *thread* that
spawned the child exits, not necessarily the whole process (golang/go#27505);
in practice Go parks rather than destroys threads so this is rare, and a
spurious early kill fails safe (claude dies rather than survives).

### 2. `pkg/tuidriver/pty_pdeathsig_other.go` (new, `//go:build !linux`)

Contract: `func setParentDeathSignal(cmd *exec.Cmd) {}` — a no-op. Imports only
`os/exec` for the parameter type; does **not** reference `syscall.SysProcAttr`
(darwin/windows `SysProcAttr` has no `Pdeathsig` field, so touching it would
break the cross-build). Doc comment states these platforms have no kernel
parent-death signal and rely on `Close` (#168) for orderly shutdown.

### 3. `pkg/tuidriver/pty.go` — one line in `StartPTY`

Insert `setParentDeathSignal(cmd)` as the first statement of `StartPTY`, before
`pty.Start(cmd)`. Add a one-line note to `StartPTY`'s existing doc comment
pointing at the helper's platform coverage. No signature change.

```go
func StartPTY(cmd *exec.Cmd) (*os.File, error) {
	setParentDeathSignal(cmd) // linux: SIGKILL child on parent death; no-op elsewhere
	ptmx, err := pty.Start(cmd)
	// ...unchanged...
}
```

**Data flow:** `StartPTY(cmd)` → `setParentDeathSignal(cmd)` mutates
`cmd.SysProcAttr` in place → `pty.Start(cmd)` → creack `StartWithSize` sees the
non-nil `SysProcAttr`, adds `Setsid`/`Setctty`, preserves `Pdeathsig` → kernel
records the parent-death signal for the new process.

## Concurrency model

None new. `setParentDeathSignal` runs synchronously on the caller's goroutine
inside `StartPTY`, before any goroutine is spawned. No channels, no shared
state, no locking.

## Error handling

No error path. The helper mutates `cmd` in place and returns nothing; a nil
`SysProcAttr` is allocated rather than erroring. The `Pdeathsig` thread-death
caveat (above) is the only failure mode, and it fails in the safe direction
(over-eager kill, never a survivor) — no mitigation warranted at this size.

## Testing strategy

**Linux-tagged wiring test** — `pkg/tuidriver/pty_pdeathsig_linux_test.go`
(`//go:build linux`). Scenarios (write in the project's test idiom; bullets, not
pre-written bodies):

- Spawn a trivial short-lived process (e.g. `exec.Command("cat")`) via
  `StartPTY`; assert `cmd.SysProcAttr != nil` and
  `cmd.SysProcAttr.Pdeathsig == syscall.SIGKILL` after the call returns.
- In the same test, assert creack's fields co-exist: `cmd.SysProcAttr.Setsid`
  and `cmd.SysProcAttr.Setctty` are both `true`. This proves the
  "preserves-a-pre-set-struct" invariant in both directions — the helper's
  nil-allocation didn't get clobbered by creack, and creack's additions didn't
  drop `Pdeathsig`.
- Reap cleanly: close the returned `*os.File`, then `cmd.Process.Kill()` +
  `cmd.Wait()` (mirror the `keys_test.go` cleanup idiom).
- Header comment: this test is Linux/CI-only and is build-excluded on the
  darwin dev box — it is the *real* behavioural spec but does **not** gate a
  local green.

**Explicitly out of scope as a gating AC** — a real parent-death behavioural
test (kill the parent, assert the child dies). It is (a) Linux-only and (b) a
false-green trap: when the parent dies the PTY master FD closes and SIGHUPs the
whole foreground group, killing the child *regardless of* `Pdeathsig` — the
same confound #168's group-kill test hit (see memory:
`pty-close-sighup-masks-group-kill-tests`). Isolating the `Pdeathsig` mechanism
would require the child to ignore SIGHUP. If ever added, it belongs in a
separate Linux-tagged integration test documented as CI-only. Do not add it
here.

**The darwin-runnable proof (the real local gate)** — the cross-`GOOS` build
guard, because `make check` (`vet` + race `test`) runs natively on darwin and
**silently build-excludes** the `//go:build linux` file, so a broken Linux
helper would pass `make check` undetected. The developer must run and confirm:

```
GOOS=linux   go build ./...
GOOS=darwin  go build ./...
GOOS=windows go build ./...
```

All three must succeed. **Baseline: confirmed green on the current tree
(2026-07-06)** before any change — so a post-change failure is attributable to
this ticket, not a pre-existing break. These are manual verification commands,
not wired into `make check` (wiring them into CI is out of scope for this XS
ticket — it would touch `Makefile` / `.github/workflows`).

Standard `make check` (darwin `vet` + race `test`) must also stay green; it
compiles the `!linux` no-op and the unchanged `StartPTY` path.

## Open questions

None blocking. Two notes for the implementer:

- Helper name `setParentDeathSignal` is the chosen contract (platform-neutral,
  reads at the call site). Not an open question — named here so both build-tag
  files agree.
- Wiring the cross-GOOS build into `make check` / CI would strengthen the guard
  permanently, but is deliberately **not** in scope — it touches infra files
  outside this ticket's boundary. Flag as a possible follow-up if desired.

## Acceptance criteria (from the ticket)

1. On Linux, `StartPTY` sets `cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL`
   before spawning, via a `//go:build linux` helper that allocates
   `SysProcAttr` if nil. A Linux-tagged test asserts the field is set after
   spawn. *(Linux/CI-only — see Testing strategy.)*
2. On non-Linux platforms the helper is a no-op (`//go:build !linux`);
   `StartPTY`'s existing spawn path and behaviour are unchanged.
3. All three targets build clean: `GOOS=linux`, `GOOS=darwin`, `GOOS=windows`
   `go build ./...` each succeed.
4. A doc comment on the helper (and a one-line pointer on `StartPTY`) states the
   platform coverage: Linux gets a hard parent-death `SIGKILL`; other platforms
   have no kernel parent-death signal and rely on `Close` (#168) for orderly
   shutdown.

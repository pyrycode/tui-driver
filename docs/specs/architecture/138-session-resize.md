# Spec #138 — `Session.Resize`: size the hosted PTY on the sealed surface

**Ticket:** [#138](https://github.com/pyrycode/tui-driver/issues/138) · **Size:** XS (PO sized S; one additive method on one production file, no `SpawnOpts` change) · Unblocks pyrycode #593 (supervisor hosts `claude` via `tuidriver.Spawn`) without regressing shipped resize behaviour.

## Files to read first

- `pkg/tuidriver/pty.go:14-17` — `DefaultPtyRows`/`DefaultPtyCols` (40×120). The default `Resize` leaves untouched; the consts the AC names as the "unspecified" baseline.
- `pkg/tuidriver/pty.go:119-134` — `StartPTY`. **The exact call to reuse:** line 132 already does `pty.Setsize(ptmx, &pty.Winsize{Rows: …, Cols: …})`. `Resize` is the same one-line ioctl on the held file, taking caller-supplied dims instead of the defaults. Note `StartPTY` treats `Setsize` as best-effort (ignores its error); `Resize` does NOT — it returns the error (see Error handling). Do not change `StartPTY`'s signature or body — it stays the spike-facing helper.
- `pkg/tuidriver/session.go:109-140` — `Session` struct. Extract: `pty *os.File` is the held master FD (`session.go:119`), set once in `Spawn` and never reassigned. The struct doc lists the *input* seams (`AcceptTrust`/`Answer`/…/`SendKeys`); `Resize` is window-sizing, orthogonal — no edit to that comment, and no `session.go` edit at all.
- `pkg/tuidriver/session.go:419-450` — `Close`. Extract the shutdown order: `s.pty.Close()` then `<-s.readerDone`. This is *why* a post-`Close` `Resize` gets a deterministic `EBADF` (Error handling explains the fd → -1 mechanism).
- `pkg/tuidriver/keys.go:77-94` — `AttachInput`. The sibling-method precedent from #136: a thin public method over the held PTY, doc-comment register, and the after-`Close` error contract ("returns the first non-nil PTY write error … No panic"). `Resize`'s doc mirrors this register.
- `pkg/tuidriver/mirror_test.go:99-136` (`TestAttachInputReachesPTY`) + `pkg/tuidriver/mirror_test.go:34-94` — the PTY end-to-end test idiom this spec reuses verbatim: `exec.Command("sh","-c","stty …; …")`, `runtime.GOOS == "windows"` skip, `Spawn(…, SpawnOpts{MirrorOutput: true})`, `recvUntil(want []byte)` drain-with-deadline. The resize tests are this shape with a different child program — do not invent a new harness.
- `pkg/tuidriver/pty_test.go` — existing `StartPTY`/`EnsureClaudeEnv` tests; confirms the file-level test conventions if the developer co-locates the resize tests here instead of a new file.
- Consumer context (read-only, for the error contract): pyrycode ADR 008 `docs/knowledge/decisions/008-bridge-resize-seam.md` — "resize errors never fail the attach; `pty.Setsize` `EBADF` on a closed fd in the narrow race window is logged at Warn and the attach proceeds; geometry is best-effort." This is the consumer that calls `Session.Resize`; it expects an *honest* non-nil error and does its own best-effort swallowing. tui-driver must not silently no-op.

## Context

The substrate seal (v1.0.0 / v1.1.0) removed `Session.PTY`, so a consumer holding only a `*Session` has no way to change the live PTY's window size; `Spawn` locks it at `DefaultPtyRows`×`DefaultPtyCols` (40×120) via `StartPTY`. pyrycode #593 migrates the supervisor to host `claude` through `tuidriver.Spawn` — after which the supervisor no longer holds the `*os.File` and cannot resize the PTY itself. Without a sanctioned resize method, foreground `pyry attach` renders at the fixed 40×120 regardless of the operator's real terminal, regressing shipped, test-pinned behaviour (pyrycode #126/#131/#132/#133/#137).

`Resize` is the sanctioned replacement for the removed raw `Session.PTY` accessor — the same additive, opt-in shape #136 used to add `MirrorOutput`/`AttachInput` without reopening a raw seam.

**Why the window size actually changes for the child.** The `creack/pty` `Setsize` is the `TIOCSWINSZ` ioctl on the master FD. The kernel both updates the slave's stored winsize (so the child's own `TIOCGWINSZ` / `stty size` reports the new dims) *and* delivers `SIGWINCH` to the slave's foreground process group (so `claude`'s TUI redraws). SIGWINCH delivery is a property of the ioctl — not separate code. tui-driver writes no signal-handling.

## Design

One additive public method. No new `SpawnOpts` field, no `Session` struct field, no `session.go` edit. Existing exported symbols are byte-for-byte unchanged → the v1.0.0/v1.1.0 seal is preserved (AC 5).

| Symbol | Kind | Contract |
|---|---|---|
| `(*Session).Resize(rows, cols uint16) error` | new method (lives in `pty.go`) | Sets the held PTY master's window size to `rows`×`cols` via the same `pty.Setsize`/`pty.Winsize` call `StartPTY` uses (`pty.go:132`). The ioctl triggers `SIGWINCH` to the hosted process (kernel behaviour, not added code). Returns a non-nil error if the size cannot be applied (e.g. after `Close`); never panics. Pass-through values — `rows`/`cols` are not interpreted, clamped, or defaulted. |

Placement rationale: `Resize` goes in `pty.go`, cohesive with `StartPTY`, `DefaultPtyRows`/`DefaultPtyCols`, and the `creack/pty` import already present there — and it reuses `StartPTY:132`'s exact call. This keeps `session.go` untouched and the whole change in one production file. (Methods are spread across the package by concern: input in `keys.go`, prompt delivery in `deliver.go`, window-sizing in `pty.go`.)

Method shape (signature + behaviour summary, **not** a body to paste):

```
// Resize sets the hosted PTY's window size to rows×cols (TIOCSWINSZ on the
// held master), which the kernel turns into SIGWINCH on the hosted process so
// its TUI redraws. Returns a non-nil error if the size cannot be applied
// (e.g. after Close); never panics. rows/cols are passed through verbatim.
func (s *Session) Resize(rows, cols uint16) error
```

Implementation is a single `pty.Setsize(s.pty, &pty.Winsize{Rows: rows, Cols: cols})` call. Return the underlying error, **optionally** wrapped with `tuidriver: resize to %dx%d:` context (recommended — ADR 008 logs these at Warn, so dims in the message aid the operator; adds a `fmt` import to `pty.go`). Raw-error return (matching `AttachInput`'s `keys.go:92-94` style) is acceptable if the developer prefers no new import. Either way: non-nil on failure, no panic.

**Why no `SpawnOpts.InitialRows/Cols` field** (AC 3 says "either a `SpawnOpts` field *or* allowing `Resize` immediately after `Spawn`"): the real consumer (#593) spawns the session headless (no attach yet, 40×120 is correct) and resizes *live* when `pyry attach` connects and learns the operator's terminal size. Initial-size-at-spawn is dead code for that consumer. AC 3 is satisfied by the cheaper branch:
- **Settable before first render:** `Resize` is callable the instant `Spawn` returns (the master FD is open). For a consumer that knows the size at spawn, calling `Resize` immediately is the pre-render path; any single 40×120 frame `claude` paints before the SIGWINCH lands is healed by the redraw.
- **Default preserved when unspecified:** a consumer that never calls `Resize` gets `StartPTY`'s untouched 40×120 — existing callers exercise zero new code, byte-for-byte unaffected.

This keeps the surface to one method and matches the consumer's actual call pattern (`Bridge.Resize` → `Session.Resize`, live, on attach). The `SpawnOpts`-field alternative is deferred (Open questions) until a foreground spawn-and-attach consumer needs a guaranteed flicker-free first frame.

## Concurrency model

No new goroutine, no new shared state, no lock.

- **Resize vs the reader goroutine.** `Resize` issues `TIOCSWINSZ` (an ioctl) on `s.pty` while the reader goroutine concurrently `Read`s from it. ioctl and read are independent syscalls on the same FD, kernel-serialised — the same safety class as `AttachInput`'s concurrent `Write`-vs-reader-`Read` (established acceptable in #136). No mutex.
- **`s.pty` is immutable after `Spawn`.** It is assigned once in `Spawn` before any goroutine starts and never reassigned, so reading it in `Resize` needs no synchronisation (same posture as `AttachInput`/`writeRaw`).
- **Resize vs `Close`.** A `Resize` racing `Close` either runs before the fd is invalidated (ioctl succeeds) or after (deterministic `EBADF` — Error handling). No panic; no wrong-FD operation (the post-`Close` fd reads back as `-1`, see below). Same acceptable race class as `AttachInput`-vs-`Close`.

## Error handling

- **Live session:** `pty.Setsize` returns `nil`; `Resize` returns `nil`.
- **After `Close` (AC 4 — deterministic non-nil error, no panic).** `Close` calls `s.pty.Close()` then blocks on `<-s.readerDone`; the reader closes `readerDone` only after its final `Read` releases the FD's last reference, at which point Go invalidates the `*os.File`'s descriptor (`Fd()` returns `-1`). So by the time `Close` returns, a subsequent `Resize` issues the ioctl on fd `-1` → `EBADF` → non-nil. This is deterministic for the sequential `Close`→`Resize` ordering the AC-4 test exercises. It is also *safe under fd reuse*: `Fd()` returns `-1` (not the recycled number), so `Resize` can never resize an unrelated terminal that reused the closed fd number. No explicit `closed` guard is needed — the failure mode is structurally prevented, and per Evidence-Based Fix Selection we do not add a mutex+flag for an unobserved, already-prevented failure. (If a future audit wants belt-and-suspenders, a deterministic `closed bool` set under `shutdownOnce` is the option — noted, not built.)
- **Honest error, consumer swallows.** tui-driver returns the error; it does not silently no-op. The consumer (pyrycode ADR 008) wraps `Session.Resize` best-effort — logs `EBADF` at Warn and proceeds, since a stale window size self-heals on the next keystroke/redraw. Keeping the tui-driver side honest lets the consumer own that policy.
- **Degenerate dims.** `rows`/`cols` are passed through verbatim. `0` is the caller's choice (sets a zero-dimension terminal, the conventional "unknown size" value) — not reinterpreted as "default". No validation: no validation-failure mode has been observed, and the consumer supplies real terminal dims.

## Testing strategy

New test file `pkg/tuidriver/resize_test.go` (co-locating in `pty_test.go` is acceptable; new file matches the per-feature `mirror_test.go` precedent and is more discoverable). Reuse `mirror_test.go`'s idiom exactly: `runtime.GOOS == "windows"` skip, `exec.Command("sh","-c", …)`, `Spawn(…, SpawnOpts{MirrorOutput: true})` to read the child's report, the `recvUntil(want []byte)` drain-with-2s-deadline helper. Scenarios (developer writes bodies in-idiom — these are descriptions, not code):

- **Child observes the resized dimensions (AC 1 + AC 2 — the key test).** Child = a shell that re-emits its own terminal size when it receives `SIGWINCH`:
  `sh -c "stty -echo 2>/dev/null; trap 'stty size' WINCH; sleep 5"`.
  Spawn with `MirrorOutput: true`; `Resize(r, c)` to dims **different from** 40×120 (e.g. `50, 100`) and assert it returns `nil`; then `recvUntil([]byte("50 100"))` on the mirror stream (`stty size` prints `rows cols`). The trap firing at all is positive evidence of `SIGWINCH` delivery (AC 1); its payload being `50 100` is the child observing the new size (AC 2). Use a generous deadline (≥2 s) and `bytes.Contains` matching (raw-mode output may carry `\r\n`), per the existing harness. **Fallback if the signal-interrupts-`sleep` timing proves flaky in CI:** child = `sh -c "stty -echo 2>/dev/null; IFS= read -r _; stty size"`, then `Resize(r,c)` → `AttachInput([]byte("\n"))` → `recvUntil("50 100")`. This reads the post-resize winsize deterministically via the same ioctl path (`stty size` reports current dims regardless of signal timing); it asserts AC 2 robustly while AC 1's SIGWINCH remains the kernel-guaranteed ioctl property documented above.
- **Default preserved when `Resize` is never called (AC 3).** Same child shape, but **no** `Resize`; nudge it to report once (the `read`-then-`stty size` fallback child is the cleaner fit here) and assert it observes `40 120` (`DefaultPtyRows DefaultPtyCols`). Guards "existing callers byte-for-byte unaffected." (The broader guard for AC 5 is that every existing test — `session_test.go`, `mirror_test.go`, `recordto_test.go`, `pty_test.go` — stays green with zero edits.)
- **`Resize` after `Close` returns non-nil error, no panic (AC 4).** `Spawn` a short-lived child; `Close`; then `Resize(50, 100)`; assert the returned error is non-nil and the call did not panic. Deterministic `EBADF` per Error handling.

Gate: `make check` + `make e2e` green; all pre-existing tests unchanged and passing.

## Open questions

- **`SpawnOpts.InitialRows/InitialCols` (deferred).** Not added — the #593 consumer resizes live on attach and never needs a pre-render size, and "settable before first render" is met by calling `Resize` immediately after `Spawn`. If a future *foreground* spawn-and-attach consumer needs a guaranteed flicker-free first frame (no 40×120 → target redraw), add the two `uint16` fields and have `Spawn` apply them between `StartPTY` and return (best-effort, zero-value → keep the 40×120 default). Cheap to add additively later; YAGNI now.
- **Error wrap vs raw.** Light `tuidriver: resize to %dx%d:` wrap is recommended for the operator-facing Warn log (ADR 008); raw `pty.Setsize` error matches `AttachInput`'s style. Local to this PR (no external consumer parses the error string — the consumer logs it), so the developer/reviewer may pick either.

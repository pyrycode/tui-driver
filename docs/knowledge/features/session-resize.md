# Session.Resize — size the hosted PTY on the sealed surface

One additive, opt-in `Session` method that lets a consumer holding **only a `*Session`** change the live hosted PTY's window size, so a foreground `pyry attach` head renders at the operator's real terminal size instead of the fixed 40×120. `Resize` is the **sanctioned replacement for the removed raw `Session.PTY` accessor** — the same additive shape [#136](../codebase/136.md) used for [`MirrorOutput`/`AttachInput`](attach-mirror-surface.md), reopening no raw seam. Introduced by [#138](../codebase/138.md). Lives in `pkg/tuidriver/pty.go`.

## Why

The substrate seal (v1.0.0 / v1.1.0) removed every raw seam — `Session.PTY`, `Session.Buffer`, `SpawnOpts.Mirror`, the public `Write`. `Spawn` locks the PTY at `DefaultPtyRows`×`DefaultPtyCols` (40×120) via `StartPTY`, and nothing on the sealed surface could change it afterwards.

pyrycode #593 migrates the supervisor to host `claude` through `tuidriver.Spawn`. After that migration the supervisor no longer holds the `*os.File`, so it cannot resize the PTY itself — and without a sanctioned method, foreground `pyry attach` would render at the fixed 40×120 regardless of the operator's terminal, **regressing shipped, test-pinned resize behaviour** (pyrycode #126 / #131 / #132 / #133 / #137). This is the gap v1.1.0 skipped: [#136](../codebase/136.md) cleared the mirror surface but not the resize seam. `Resize` closes it additively.

## API

```go
func (s *Session) Resize(rows, cols uint16) error
```

Sets the held PTY master's window size to `rows`×`cols`. **Additive** — no existing exported symbol changed signature, so the v1.0.0 / v1.1.0 seal is preserved. `rows`/`cols` are passed through **verbatim** — not clamped, defaulted, or interpreted (`0` is the caller's choice, the conventional "unknown size" value, not reinterpreted as "default").

## How it works — one ioctl, SIGWINCH for free

`Resize` is a single `pty.Setsize(s.pty, &pty.Winsize{Rows: rows, Cols: cols})` call — the **same `creack/pty` call `StartPTY` uses** to set the 40×120 default at spawn (`pty.go`), just on the held master with caller-supplied dims. `Setsize` is the `TIOCSWINSZ` ioctl on the master FD, and the kernel does **two** things in response:

1. updates the slave's stored winsize — so the child's own `TIOCGWINSZ` / `stty size` now reports the new dims (this is what the AC-2 test observes); and
2. delivers `SIGWINCH` to the slave's foreground process group — so `claude`'s TUI redraws at the new size.

**SIGWINCH delivery is a property of the ioctl, not separate code.** tui-driver writes no signal handling — the same one-line ioctl that resizes the terminal is what makes `claude` repaint.

Unlike `StartPTY` (which treats its spawn-time `Setsize` as best-effort and ignores the error), `Resize` **returns** the error, lightly wrapped (`tuidriver: resize to %dx%d: …`).

## Settable before first render; default preserved when unspecified

The AC ("first window size settable before the child first renders") is met **without** a `SpawnOpts` field: `Resize` is callable the instant `Spawn` returns (the master FD is already open). A consumer that knows the size up front calls `Resize` immediately; any single 40×120 frame `claude` paints before the SIGWINCH lands is healed by the redraw.

A consumer that **never** calls `Resize` keeps `StartPTY`'s untouched 40×120 — exercising zero new code, byte-for-byte unaffected. (No `SpawnOpts.InitialRows/Cols` field was added: the #593 consumer spawns headless and resizes *live* on attach, so an initial-size-at-spawn field would be dead code. Deferred until a foreground spawn-and-attach consumer needs a guaranteed flicker-free first frame — cheap to add additively later. See [#138](../codebase/138.md) and the [spec's Open questions](../../specs/architecture/138-session-resize.md).)

## Concurrency

No new goroutine, no new shared state, no lock — a deliberately lighter footprint than the [reader-goroutine tap](attach-mirror-surface.md) `MirrorOutput` needed.

- **`s.pty` is immutable after `Spawn`** (assigned once before any goroutine starts, never reassigned), so reading it in `Resize` needs no synchronisation — same posture as `AttachInput` / `writeRaw`.
- **`Resize` vs the reader goroutine:** the `TIOCSWINSZ` ioctl and the reader's `Read` are independent syscalls on the same FD, kernel-serialised — the same safety class as `AttachInput`'s concurrent `Write`-vs-reader-`Read`, established acceptable in [#136](../codebase/136.md). No mutex.
- **`Resize` vs `Close`:** either the ioctl runs before the FD is invalidated (succeeds) or after (deterministic `EBADF`, below). No panic, and no wrong-FD operation — see Error handling.

## Error handling

- **Live session:** `pty.Setsize` returns `nil`; `Resize` returns `nil`.
- **After `Close`:** `Close` runs `s.pty.Close()` then blocks on `<-s.readerDone`; once the reader releases the FD's last reference Go invalidates the `*os.File`'s descriptor (`Fd()` returns `-1`). A subsequent `Resize` therefore issues the ioctl on fd `-1` → **`EBADF` → non-nil error, deterministically, no panic.** This is also **safe under fd reuse**: `Fd()` returns `-1` (not the recycled number), so `Resize` can never resize an unrelated terminal that reused the closed fd number. No explicit `closed` guard is needed — the failure mode is structurally prevented (per [Evidence-Based Fix Selection](../codebase/138.md#lessons-learned), no mutex+flag for an already-prevented, unobserved failure).
- **Honest error — the consumer swallows.** tui-driver returns the error and does **not** silently no-op. The real consumer (pyrycode **ADR 008 — `Bridge.Resize`**) wraps `Session.Resize` best-effort: it logs an `EBADF` from the narrow `Close` race at Warn and proceeds, because a stale window size self-heals on the next keystroke/redraw. Keeping the tui-driver side honest lets the consumer own that best-effort policy.

## Usage

```go
sess, _ := tuidriver.Spawn(cmd, tuidriver.SpawnOpts{})

// On attach, size the hosted PTY to the operator's real terminal.
if err := sess.Resize(rows, cols); err != nil {
    // e.g. EBADF after Close — best-effort: log and proceed (consumer ADR 008).
}
```

## Limitations

- **Mechanism only — no geometry policy.** `Resize` exposes the size-setting mechanism. *Which* size wins when two heads attach at different sizes (a desktop head and a phone head) is a consumer decision (pyrycode #595), not tui-driver's.
- **Best-effort after `Close`.** A `Resize` racing `Close` returns `EBADF`; the contract is "non-nil error, no panic," and the consumer is expected to swallow it (ADR 008).
- **No validation.** Degenerate dims (`0`, or larger-than-screen) are passed through verbatim — the consumer supplies real terminal dims.

## Related

- [Local-attach mirror surface](attach-mirror-surface.md) — the sibling sealed-surface seams (`MirrorOutput` out, `AttachInput` in); `Resize` completes the attach surface with geometry. Same additive-no-raw-seam shape.
- [System overview § Concurrency model](../architecture/system-overview.md#concurrency-model) — where the sealed `Session` attach surface is enumerated.
- [#138 — per-ticket notes](../codebase/138.md) — implementation summary, patterns, and lessons.
- Consumer-side **ADR 008 — `Bridge.Resize`** (`pyrycode/docs/knowledge/decisions/008-bridge-resize-seam.md`) — the supervisor-side seam that calls `Session.Resize` and owns the best-effort Warn-and-proceed policy. Wire shape: ADR 009 + pyrycode #137; SIGWINCH producer: pyrycode #133.

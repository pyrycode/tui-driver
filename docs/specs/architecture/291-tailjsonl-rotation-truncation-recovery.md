# Spec #291 — TailJSONL detects rotation & truncation mid-tail and recovers

**Ticket:** [#291](https://github.com/pyrycode/tui-driver/issues/291) · **Size:** S · **Security-sensitive:** no (unlabelled) · Split from [#289](https://github.com/pyrycode/tui-driver/issues/289) · Blocked-by [#290](https://github.com/pyrycode/tui-driver/issues/290) (**shipped**, PR #292 / `78db474`)

## Files to read first

- `pkg/tuidriver/jsonl.go:197-227` — `TailJSONL`: opens the fd, resolves the start offset against its **own** fd (#290), spawns the loop. The recovery reopen will mirror this `os.Open(path)` shape; `f.Name()` is how the loop recovers the path.
- `pkg/tuidriver/jsonl.go:229-277` — `tailJSONLLoop`: the tail loop. `defer r.Close()` at :236 must become a closure (recovery reassigns `r`). The **EOF branch at :263-268** is where the rotation/truncation check goes. `r` is typed `io.ReadCloser` — the seam this ticket threads stat access through **without a signature change**.
- `pkg/tuidriver/jsonl_test.go:381-429` — `faultReader` + `TestTailJSONLLoop_ReadErrorClosesChannelCtxLive`: the fault-injection seam. `faultReader` implements only `Read`+`Close`. **It must NOT satisfy the new `statReader` interface** — that is what keeps recovery skipped under fault injection. This test must stay green **unchanged**.
- `pkg/tuidriver/events_test.go:489-520` — `TestMergeEvents_ReadFaultSurfacesTerminalError`: the second `tailJSONLLoop` fault call site. Regression guard; stays green unchanged.
- `pkg/tuidriver/jsonl_test.go:189-224` (`TestTailJSONL_AppendDuringTail`) + `:431-462` (`TestTailJSONL_EOFAppendCycles`) + the `mustAppend` helper — the normal-append pattern the new tests mirror. **Landmine:** `mustAppend` closes its fd per append (Close flushes), dodging APFS cross-handle deferral; the new rotation/truncation tests hold a handle open across the write→reopen boundary, so they need an explicit `f.Sync()` after writes.
- `pkg/tuidriver/session_signal_other.go` + `pkg/tuidriver/session_signal_windows.go` — the **exact naming + build-tag convention** to mirror for the new platform pair: `_other.go` carries `//go:build !windows`, `_windows.go` carries `//go:build windows`. Copy this shape, do **not** invent a `_unix.go` name.
- `pkg/tuidriver/cwd_darwin.go:1-35` — in-package example of a build-tagged file using `syscall` against an `*os.File`. (It uses `F_GETPATH`, not `Sys()`, so it is a template for the *file shape*, not the extraction — there is no existing `Sys().(*syscall.Stat_t)` call in the package; the developer writes the first one.)
- `docs/knowledge/codebase/290.md` — the "Sibling #291" boundary note: #290 deliberately did **not** touch inode identity, `//go:build` platform files, or re-read-from-0 recovery. Those three are exactly this ticket's scope.

## Context

`TailJSONL` now owns its fd and resolves its baseline offset against that fd (#290, shipped). But `tailJSONLLoop` (`pkg/tuidriver/jsonl.go:234`) still holds that one open fd forever and never re-checks the file behind the path. Two mid-tail failure modes leave the subscription stranded:

- **Rotation** — the path is replaced by a new inode (log rotation, or a consumer's `/clear`-style session-file swap). The fd still reads the old, possibly-unlinked generation; the new generation's content is never delivered. Size-based detection (all #290 could provide) cannot see this — a same-byte-length replacement is invisible to size. Only **inode identity** distinguishes it.
- **Truncation** — the file is rewritten shorter (same inode). The fd is now positioned past EOF; it parks forever, and if the file regrows past the stale offset it reads from the wrong byte position, silently skipping the rewritten prefix.

This is the mid-tail half of the pyrycode #929/#930 tail-offset class (#290 closed the cold-start/baseline half). The consumer motivation is the `/clear` rotation scenario tracked in pyrycode #671 — a mid-session session-file swap the tail must follow.

## Design

### The seam: a type-assertion, not a signature change

The design constraint (#290's shipped shape): `tailJSONLLoop(ctx, r io.ReadCloser, ch)` is typed `io.ReadCloser` so tests can inject `faultReader`. An `io.ReadCloser` exposes no `Stat`/`Name`/`Seek`. The recovery check needs all three.

**Resolution — do not change the loop signature.** Define a minimal interface that `*os.File` satisfies and `faultReader` does not, and type-assert `r` to it inside the EOF branch:

```go
// statReader is the subset of *os.File the rotation/truncation guard
// needs. The production tail reader (*os.File) satisfies it; the
// fault-injection seam (faultReader — Read+Close only) does not, so the
// guard is a no-op under injection. No call site of tailJSONLLoop changes.
type statReader interface {
	io.ReadCloser
	Name() string
	Stat() (os.FileInfo, error)
	Seek(offset int64, whence int) (int64, error)
}
```

`*os.File` satisfies all four methods; `faultReader` (Read+Close) does not. `tailJSONLLoop`'s signature, `TailJSONL`, and both fault-injection call sites are **untouched** — this is what keeps the ticket at S with zero call-site fan-out (`codegraph_callers tailJSONLLoop` = `TailJSONL` + 2 tests; signature unchanged → 0 edits).

### Generation identity (the platform pair)

Rotation detection compares the **path's current inode** against the **open fd's inode**. Inode identity is Unix-only via `FileInfo.Sys().(*syscall.Stat_t)`; `pkg/tuidriver` compiles on Windows, so this needs the platform split (project-memory landmine: **no `runtime.GOOS` runtime skip** — build tags).

`fileIdentity` (the shared type) lives in `jsonl.go` (untagged). Only the extractor is platform-tagged:

```go
// fileIdentity is a file's (device, inode) generation identity. known
// is false on platforms (Windows) that cannot supply one — two unknown
// identities never compare equal, so rotation detection self-disables.
type fileIdentity struct {
	dev, ino uint64
	known    bool
}
```

- **`pkg/tuidriver/jsonl_ident_other.go`** (`//go:build !windows`, mirrors `session_signal_other.go`): `func statIdentity(fi os.FileInfo) fileIdentity` reads `fi.Sys().(*syscall.Stat_t)` and returns `{dev: uint64(st.Dev), ino: uint64(st.Ino), known: true}`. The `uint64(...)` casts are load-bearing: `Stat_t.Dev` is `int32` on darwin and `uint64` on linux — the `!windows` tag covers both, the cast normalises the field-type difference. If the `Sys()` assertion fails, return `{known: false}`.
- **`pkg/tuidriver/jsonl_ident_windows.go`** (`//go:build windows`): `func statIdentity(fi os.FileInfo) fileIdentity { return fileIdentity{} }` (`known: false`). Rotation detection is thus a documented no-op on Windows (AC3 permits this); truncation detection, which needs only size, still works.

Rotation compares two identities as equal **only when both are `known`** (a helper `func (a fileIdentity) sameGeneration(b fileIdentity) bool { return a.known && b.known && a.dev == b.dev && a.ino == b.ino }`). On Windows both are unknown → never "same" and never "different-and-known" → the rotation branch simply never fires.

### The recovery check (EOF branch only)

The check lives in the EOF branch (`jsonl.go:263`), the point where the tail is stranded — actively-progressing reads (`rerr == nil`) need no check. A single helper, contract only:

```go
type recoveryKind int
const (
	recoveryNone recoveryKind = iota
	recoveryRotated
	recoveryTruncated
)

// checkTailGeneration inspects the path behind sr (sr.Name()) for a
// generation change and, if found, prepares recovery per the documented
// policy. Returns the reader to continue with and the kind of recovery.
//   - recoveryNone: no change (or a stat/open race — see Error handling);
//     next == sr, caller parks.
//   - recoveryRotated: next is a FRESH *os.File opened at offset 0; caller
//     closes the old sr, then resets its bufio reader + partial buffer.
//   - recoveryTruncated: next == sr, already reseeked to offset 0; caller
//     resets its bufio reader + partial buffer (fd unchanged, no close).
func checkTailGeneration(sr statReader) (next statReader, kind recoveryKind)
```

Detection logic inside the helper (contract, ~35 lines):

1. `pathInfo, err := os.Stat(sr.Name())`. On error → `recoveryNone` (see Error handling). Do **not** fail the tail.
2. `fdInfo, err := sr.Stat()` — the fd's identity (immutable for the fd's life). On error → `recoveryNone`.
3. **Rotation:** if `statIdentity(pathInfo).sameGeneration(statIdentity(fdInfo))` is **false** *and* both are `known` → the path points to a new inode. `os.Open(sr.Name())`; on success return `(newFile, recoveryRotated)`; on open error → `recoveryNone` (park, retry next tick).
4. **Truncation** (same generation): `readOffset, _ := sr.Seek(0, io.SeekCurrent)` — at EOF the fd position equals the high-water mark of bytes pulled off disk. If `pathInfo.Size() < readOffset` → the file shrank below where we've read. `sr.Seek(0, io.SeekStart)`; return `(sr, recoveryTruncated)`.
5. Otherwise `(sr, recoveryNone)`.

Why the fd position is the correct `readOffset`: at the EOF branch `bufio.Reader` has drained (it returned `io.EOF`), so the underlying fd sits at the last byte it read — the exact count of bytes consumed from disk, including any buffered partial line. `Seek(0, io.SeekCurrent)` reports it without moving the fd. Comparison is strict `<` so a fully-read-to-end file (`size == readOffset`) is never a false truncation.

### Loop integration (contract sketch, <20 lines)

```go
// defer r.Close() at :236 becomes a closure so the FINAL r (post-recovery
// reopen) is the one closed:
defer func() { _ = r.Close() }()
...
case rerr == io.EOF:
	if sr, ok := r.(statReader); ok {
		if next, kind := checkTailGeneration(sr); kind != recoveryNone {
			if kind == recoveryRotated {
				_ = r.Close() // close the old generation's fd
			}
			r = next
			reader.Reset(r)
			partial = partial[:0]
			continue // read the recovered file immediately; skip the poll-sleep
		}
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(DefaultPollInterval):
	}
```

The `defer` change from method-value (`defer r.Close()`) to closure (`defer func(){ r.Close() }()`) is **required and behaviour-preserving** for the fault path (still closes `faultReader` exactly once) and **necessary** for the reassignment case (the method-value form would bind the original `r` and leak the reopened fd on rotation). Old fds are closed explicitly at each rotation; the final fd is closed by the deferred closure.

### Recovery policy (AC4 — goes in the doc comments)

Both branches re-sync to **offset 0** of the current generation — symmetric and simple:

- **Rotation** → reopen the path fresh and read the **entire new generation from 0**. A rotated log is a brand-new file; all of its content is unseen. (This is the "re-read-from-0" #290 explicitly deferred to here.)
- **Truncation** → reseek the same fd to **0** and re-read the rewritten content. The tail's position was beyond EOF; re-syncing from the start recovers the current bytes with no silent wrong-offset read.

A consumer of a rotated/truncated log may therefore re-observe that generation's entries from the top — these are genuinely new bytes (the old generation is gone), not duplicates of already-consumed live content.

## Concurrency model

Unchanged. One tail goroutine per `TailJSONL` call; recovery happens inline on that goroutine during the EOF poll — no new goroutines, no shared state, no locks. `ctx` cancellation is still honoured: recovery either `continue`s (re-checks `ctx.Err()` at the loop top) or parks in the existing `select { <-ctx.Done(); <-time.After(poll) }`.

## Error handling

Recovery is **best-effort and never fails the tail** — it must not introduce a new terminal-error path, or it would spuriously trip `mergeEvents`' ctx-live-close → `EventKindError` contract (`events.go`; guarded by `TestMergeEvents_ReadFaultSurfacesTerminalError`). All degradations resolve to `recoveryNone` (park and retry the next EOF poll):

- **`os.Stat(path)` ENOENT** (rename window: old unlinked, new not yet at path; or a plain delete) → park. When the replacement appears, the next poll detects the inode change and recovers. A permanently-deleted log parks until `ctx` cancels — correct (no bytes to read).
- **`os.Stat(path)` other error** (permission, ENOTDIR) → park.
- **`os.Open(path)` fails during rotation reopen** (stat saw the new inode but open raced/failed) → park, keep the old fd, retry.
- **`sr.Stat()` / `sr.Seek()` failure** → park.

The only terminal error path remains the existing non-EOF read error (the loop's `default:` branch), unchanged. Recovery never reaches it.

**Known limitation (documented, not fixed — Evidence-Based Fix Selection).** Truncation is detected at the EOF branch. A truncate-then-immediately-regrow that rewrites *past* the stale offset within one poll window — so the tail never hits EOF at the stale position — would read from the wrong offset undetected. This mid-read regrow is unobserved (the observed class is EOF-stranding), the ticket scopes the check to the EOF branch, and a per-read check would cost a stat on every iteration. Record it in the doc comment as a known gap; do not defend against it.

## Testing strategy

All tests are claude-free (`t.TempDir()` + file ops) and run in `make check` (AC5). Scenarios as bullets — write them in the package's testing idiom, not from these words:

- **Rotation, differing length** — tail a file; mid-tail replace the path with a new file (create-and-rename-over, or remove+create) holding new content; assert the tail delivers the new generation's entries from offset 0 and no stale old-generation bytes leak.
- **Rotation, same byte length (the AC's critical case)** — the replacement is byte-length-identical to the original; only the inode differs. Assert recovery still fires — proving inode identity, not size, drives rotation detection.
- **Truncation** — tail past offset N; `f.Truncate(0)`, `f.Sync()`, append new (shorter) content; assert the tail detects `size < readOffset`, reseeks to 0, and delivers the new content with no wrong-offset garbage.
- **Truncate-to-empty then regrow** — truncate to 0, sync, then append lines; assert delivery from 0.
- **No false positive on plain append** — a normal append (size grows, inode unchanged) must **not** trigger recovery or duplicate delivery. (`TestTailJSONL_AppendDuringTail` already exercises the happy path; add an assertion that no entry is re-delivered.)
- **Rotation-window ENOENT is graceful** — remove the path with no replacement; assert the tail parks (no crash, no garbage delivery); then create a new file at the path; assert recovery onto the new generation.
- **`statIdentity` unit (`//go:build !windows` test)** — two distinct files yield different identities; the same file stat'd twice yields equal, `known == true`.
- **Fault-injection seam unregressed (regression guard, no new code)** — `TestTailJSONLLoop_ReadErrorClosesChannelCtxLive` and `TestMergeEvents_ReadFaultSurfacesTerminalError` must stay green **unchanged**: `faultReader` does not satisfy `statReader`, so the type-assertion fails and recovery is skipped. If either needs an edit, the seam has been broken — stop and rethink.

**Landmine — cross-handle visibility.** Tests that write then re-read across handles need `f.Sync()` after writes (macOS APFS defers cross-handle visibility). `mustAppend` dodges this by closing per append; the new tests hold a handle open across write→reopen, so sync explicitly.

## Files changed (scope check)

Production source files (new or modified, excluding tests): **3** — under the ≥5 red line.

- `pkg/tuidriver/jsonl.go` (modified): `statReader` interface, `fileIdentity` type + `sameGeneration`, `recoveryKind` + constants, `checkTailGeneration` helper, EOF-branch integration, `defer` closure change, recovery-policy doc comments (AC4).
- `pkg/tuidriver/jsonl_ident_other.go` (new, `//go:build !windows`): `statIdentity`.
- `pkg/tuidriver/jsonl_ident_windows.go` (new, `//go:build windows`): `statIdentity` stub.
- `pkg/tuidriver/jsonl_test.go` (modified, test file): rotation/truncation/identity tests.

No new **exported** symbols (all of `statReader`, `fileIdentity`, `recoveryKind`, `checkTailGeneration`, `statIdentity` are unexported). Zero `tailJSONLLoop` call-site edits. ~120-150 production LOC + ~250-350 test LOC ≈ 400-500 total — within S.

## Open questions

- **Rotation reopen offset — 0 vs end.** Spec picks **0** (read the whole new generation). If a downstream consumer surfaces a "flood on rotation" problem, revisit toward `TailFromEnd` — but no such consumer exists today, so 0 is the documented default. Defer any knob until a real backpressure problem appears (mirrors `defaultJSONLTailBuffer`'s "not a knob until a real consumer needs it").
- **jsonl-layout.md update.** The canonical `TailJSONL` API prose in `docs/knowledge/architecture/jsonl-layout.md` (line 33) already flags rotation/truncation recovery as "sibling #291." Updating that section is **documentation-phase work** (owned post-merge), **not** a developer AC — AC4 is satisfied by the code doc comments in `jsonl.go` alone.

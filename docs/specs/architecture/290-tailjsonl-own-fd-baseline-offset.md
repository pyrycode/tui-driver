# #290 — TailJSONL resolves its baseline offset against its own fd

**Ticket:** [#290](https://github.com/pyrycode/tui-driver/issues/290) · **Size:** S · **Security-sensitive:** no · Split from #289 (sibling #291 is blocked-by this ticket)

## Files to read first

- `pkg/tuidriver/jsonl.go:137-183` — `TailJSONL` doc comment + body. **This is the only production file the ticket edits.** Extract the current open→seek→spawn flow; the change replaces the seek half with stat-own-fd→validate→seek.
- `pkg/tuidriver/jsonl.go:185-233` — `tailJSONLLoop`. **Do not touch.** Confirms the goroutine contract (buffered cap 32, ctx-driven close, EOF-poll, malformed-drop, ctx-live close on read error) that AC3 says must not change. The change is entirely in the synchronous prologue *before* the goroutine spawns.
- `pkg/tuidriver/jsonl_test.go:189-511` — the eight existing `TestTailJSONL_*` tests. Four are named in AC3 as must-stay-green (`_AppendDuringTail:189`, `_EOFAppendCycles:431`, `_StartOffsetSkipsPrefix:464`, `_OpenFailsWhenFileMissing:496`). `_StartOffsetSkipsPrefix` is the load-bearing one: it passes a raw `int64` offset and expects prefix-skip — this is why the **signature stays `int64`** (see Design).
- `pkg/tuidriver/jsonl_test.go:159-187` — `mustReceive` / `mustAppend` helpers the new tests reuse. `mustAppend` opens `O_APPEND`, writes, and `defer f.Close()` (flush-on-close is why existing append tests need no explicit `Sync`).
- `pkg/tuidriver/events.go:188-196` — `Events`, the **only in-repo production caller**. It threads `startOffset int64` straight through to `TailJSONL`. Confirms that preserving the signature means zero call-site edits (no fan-out).
- `docs/knowledge/architecture/jsonl-layout.md` § "Discovering the session file" (the canonical-library-API blockquote, lines 33–34) — the `TailJSONL` contract prose. The returned-channel half of this must remain verbatim-true after the change.

## Context

`TailJSONL(ctx, path, startOffset int64)` today does `os.Open(path)` then `f.Seek(startOffset, io.SeekStart)` (`pkg/tuidriver/jsonl.go:171-179`). The `startOffset` is measured by the **caller**, in a different place, against a **different fd** (typically an earlier `os.Stat`). Two problems follow:

1. **TOCTOU across fds.** Between the caller's `os.Stat` and `TailJSONL`'s `os.Open`, the file can rotate or truncate. The offset was true against the caller's now-dead fd; it binds the tail's fresh fd to the wrong bytes. This is the root of the pyrycode #929/#930 tail-offset class.
2. **`Seek` past EOF is silent.** `(*os.File).Seek(n, io.SeekStart)` does **not** error when `n` exceeds the file size on a regular file. An oversized offset lands the reader past all content, where it sits in EOF-poll — no error, no signal, wrong position.

This slice inverts ownership of the **baseline offset only**: the tail stats its **own** fd and validates the requested start against *that* size before seeking. Mid-tail rotation/truncation *recovery* (inode identity, re-read decisions) is the sibling #291, which is natively blocked by this ticket. Keep the two apart — see § "Boundary with #291".

## Design

### Decision: keep the signature; change the internal prologue

The signature stays `TailJSONL(ctx context.Context, path string, startOffset int64) (<-chan JSONLEntry, error)`. Rationale:

- AC3 requires `TestTailJSONL_StartOffsetSkipsPrefix` (passes a raw `int64` offset) to stay green — the `int64` offset path must survive.
- `Events` (the only production caller) and all eight tests then compile and pass unchanged → **zero edit fan-out** (this is what keeps the ticket S; `codegraph_impact` shows 9 callers, 1 production + 8 test, all preserved).
- "Successor API" was considered and rejected: it would fan out to `Events` + 8 tests + the sibling's future consumer, and buys nothing the sentinel below doesn't.

### The offset contract (three cases, resolved against the tail's own fd)

Introduce one exported constant:

```go
// TailFromEnd, passed as TailJSONL's startOffset, starts the tail at the
// current end of the tail's OWN fd — the fd-agnostic way to say "skip
// existing content, stream only future appends," without a caller
// measuring size against a foreign os.Stat.
const TailFromEnd int64 = -1
```

`TailJSONL`'s new synchronous prologue, after `os.Open` succeeds:

1. `fi, err := f.Stat()` — the size guard's source of truth is the **tail's own fd**. On error: `f.Close()`, return `fmt.Errorf("stat session jsonl %s: %w", path, err)`.
2. Resolve `seekTo int64` from `startOffset` and `size := fi.Size()`:
   - `startOffset == TailFromEnd` → `seekTo = size` (start at own-fd end).
   - `startOffset == 0` → `seekTo = 0` (from the beginning; unchanged).
   - `startOffset > 0` → `seekTo = min(startOffset, size)` (**clamp**; oversized requests land at the own-fd end, never past content).
   - `startOffset < TailFromEnd` (i.e. `< -1`) → `f.Close()`, return an "invalid negative offset" error (only the `TailFromEnd` sentinel is a legal negative).
3. `f.Seek(seekTo, io.SeekStart)` — `seekTo` is now provably in `[0, size]`, so this cannot land past content. Keep the existing error check + `f.Close()` on failure (defensive; regular-file seek to an in-range position won't fail, but non-regular fds could).
4. Spawn `go tailJSONLLoop(ctx, f, ch)` — unchanged.

### Clamp, not reject — and clamp to `size`, not `0`

- **Clamp over reject:** this is a resilience primitive. Rejecting an oversized resume would fail the consumer's tail exactly when the file shifted under it — the failure mode the inversion exists to absorb. Clamp keeps the tail alive.
- **Clamp to `size` (own-fd end), not to `0`:** the documented warm-file consumer intent (pyrycode `internal/turnbridge`: pass `size` = "skip existing content," spec #671) is preserved — an oversized/stale offset still means "skip everything that exists, stream appends." Clamping to `0` would *re-read* existing content, contradicting that intent. Re-reading a genuinely-rotated file from `0` is a *recovery* decision keyed on inode identity — that belongs to #291, not here.

### Note the stat↔seek window is NOT a reintroduced TOCTOU

`f.Stat()` and `f.Seek()` operate on the **same open fd** (same inode). Between them the file can only grow (append-only within one fd); it cannot shrink or be replaced under a held fd. So `seekTo ≤ size` remains true-or-conservative at seek time (a grown file just means we resume a few bytes earlier and re-read the appended lines — correct for an append-only log). The cross-fd race is gone because there is now exactly one fd. Call this out in the doc comment so a future reader doesn't "fix" the window.

## Concurrency model

Unchanged. The entire change lives in the synchronous prologue that runs on the **caller's** goroutine before `tailJSONLLoop` spawns. `f.Stat()` on the freshly-opened, not-yet-shared fd has no concurrency exposure. The goroutine, channel (buffered cap 32), ctx-driven close, EOF-poll, malformed-drop, and ctx-live-close-on-read-error semantics are all untouched.

## Error handling

| Failure | Behaviour | Test |
|---|---|---|
| `os.Open` fails (missing/permission) | return wrapped error mentioning path; `ch == nil` (unchanged) | existing `_OpenFailsWhenFileMissing` |
| `f.Stat` fails | `f.Close()`; return `fmt.Errorf("stat session jsonl %s: %w", …)` | covered incidentally; no dedicated test required (hard to force on an open fd) |
| `startOffset < -1` | `f.Close()`; return invalid-negative-offset error mentioning path + offset | new `_InvalidNegativeOffset` |
| `f.Seek` fails | `f.Close()`; return wrapped seek error (unchanged shape) | existing behaviour |
| oversized `startOffset > size` | clamp to `size`, no error, start at end | new `_OversizedOffsetClampsToEnd` |

Error message idiom matches the file: `fmt.Errorf("<verb> session jsonl %s…: %w", path, …)` (see the existing open/seek wraps at `jsonl.go:174,178`).

## Testing strategy

Reuse `mustReceive` / `mustAppend` (both already flush via `defer f.Close()`; no explicit `Sync` needed for the append pattern the existing tests already rely on). Each new test follows the existing `_StartOffsetSkipsPrefix` shape (write body → `TailJSONL` → assert deliveries → `cancel()` → assert channel closes).

Three new test functions in `pkg/tuidriver/jsonl_test.go`:

- **`TestTailJSONL_OversizedOffsetClampsToEnd`** (AC4 — the primary new test) — write `{"type":"a"}\n{"type":"b"}\n`; call `TailJSONL(ctx, path, <fileSize+9999>)`; `mustAppend` `{"type":"c"}\n`. Assert: first received entry is `"c"` (existing `a`/`b` are before the clamp point and are skipped), and no `a`/`b` ever arrives. Proves both halves of AC2/AC4: no read past content, and the clamp lands at the own-fd end.
- **`TestTailJSONL_FromEndSentinelSkipsExisting`** (AC2) — write two lines; call `TailJSONL(ctx, path, TailFromEnd)`; append a third; assert only the third is delivered. Proves `TailFromEnd` resolves to the own-fd end without a caller measuring size.
- **`TestTailJSONL_InvalidNegativeOffset`** (documented-policy edge) — call `TailJSONL(ctx, path, -2)`; assert `err != nil`, `ch == nil`, and the error mentions the path. Pins that only the sentinel is a legal negative.

Do **not** modify the eight existing tests. Run the four AC3-named tests explicitly to confirm green (`go test ./pkg/tuidriver/ -run 'TestTailJSONL_(AppendDuringTail|EOFAppendCycles|StartOffsetSkipsPrefix|OpenFailsWhenFileMissing)'`). Then `make check`.

## Documentation (AC3)

Update the `TailJSONL` doc comment (`jsonl.go:137-170`) to state the resume contract explicitly:

- The tail opens the file and validates the start position against **its own fd's** size — a caller-precomputed offset is no longer trusted blind.
- `0` = from the beginning; `TailFromEnd` = from the current end of the tail's own fd; a positive offset is **clamped** to the own-fd size (oversized/stale requests start at the end, never past content); `startOffset < -1` is an error.
- One line noting the stat↔seek window is not a TOCTOU (same fd, append-only), so it should not be "hardened" away.

No shared-doc edits — `docs/knowledge/architecture/jsonl-layout.md` is documentation-phase-owned and its channel-contract prose stays true. Do not add a `docs/knowledge/codebase/290.md` AC (documentation phase writes it post-merge).

## Boundary with #291 (keep them apart)

#290 guards **reads-past-content via a size bound** on a single fd. #291 adds **rotation/truncation detection via inode identity** (`dev`/`ino` of path vs open fd, needing the package's first `//go:build` platform files) and the *recovery* policy (when a genuine inode change is detected mid-tail, decide whether to re-open and re-read from `0`). This ticket deliberately does **not**:

- introduce any platform-specific (`//go:build`) file — it uses only `fi.Size()` (cross-platform stdlib);
- compare inode identity;
- re-read a rotated file from `0` on an oversized offset (clamp-to-`size` is the #290 behaviour; #291 owns the smarter re-read trigger).

If implementation reveals the size guard alone can't satisfy an AC without inode identity, stop and flag — that's a sign scope has leaked into #291.

## Open questions

- None blocking. The clamp-to-`size` vs clamp-to-`0` choice is decided above (clamp to `size`, aligned with warm-consumer "skip existing" intent); it is documented so #291's architect inherits the rationale rather than re-litigating it.

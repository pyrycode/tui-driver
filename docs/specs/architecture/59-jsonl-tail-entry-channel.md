# Spec: library API — JSONL tail with parsed-entry channel

**Ticket:** [#59](https://github.com/pyrycode/tui-driver/issues/59)
**Size:** S
**Status:** ready for development

## Files to read first

Load these in order; each entry says what to extract.

- `cmd/spike-long-prompt/main.go:375-439` — `tailJSONL` reference implementation. The library version is "this loop, minus the assistant-only filter (lines 413-422), with a typed `JSONLEntry` instead of `map[string]any` on the channel, and with parse errors silently dropped instead of `logger.Printf`'d." Read the full body — the partial-line accumulation idiom (`partial = append(partial, chunk...)` then `partial[:0]` after emit), the `select` on `ctx.Done()` for both backpressure and EOF-sleep, and the `rerr` triage are all reused verbatim.
- `cmd/spike-long-prompt/main.go:48-56` — `jsonlTailInterval = 50 * time.Millisecond`. The library reuses `tuidriver.DefaultPollInterval` (also 50 ms) instead of introducing a new constant; document the equivalence in the doc comment.
- `pkg/tuidriver/jsonl.go:1-66` — existing file. The new `TailJSONL` + types are appended to this file; package and imports are already in place. The two existing functions establish the "wraps `EncodeCwd`, returns wrapped `context.Cause(ctx)` on cancellation, no goroutines, doc comments cite jsonl-layout.md" idiom — match it.
- `pkg/tuidriver/jsonl_test.go:97-172` — existing `WaitForSessionJSONL` tests. Test idiom for context-driven helpers in this package: `t.TempDir()` for path isolation, `os.WriteFile` to materialise the file, `context.WithCancelCause` + sentinel + `errors.Is` for cancel-cause assertions, `time.Sleep` between phases is acceptable (no fake-clock dependency in this package). Mirror these shapes for the new tests.
- `pkg/tuidriver/wait.go:1-44` — `DefaultPollInterval` declaration and the "short-circuit when predicate is already true" comment. The new tail also reuses `DefaultPollInterval` for its EOF-sleep cadence; reference it directly, do not redefine.
- `pkg/tuidriver/tuidriver.go:1-65` — package doc comment. Two phrases need to be updated (see § "Doc-comment update in `tuidriver.go`" below). Read the current scope sentence (line ~7-11) and the in-scope/out-of-scope split.
- `docs/knowledge/architecture/jsonl-layout.md:53-133` — turn lifecycle, envelope shapes, content-block kinds, parser rule consumers apply ("`type == "assistant"` AND `message` is a map"). The library tail does NOT apply this filter; consumers do. **Do not edit** — documentation phase owns this file.
- `cmd/spike-cancel/main.go:762-784` and `cmd/spike-multi-turn/main.go:445-467` — `extractByMsgID` shows how downstream code reaches into a parsed entry today: `ev["message"].(map[string]any)`, then `msg["content"].([]any)`, then walk blocks looking at `c["type"]`. The new typed `JSONLEntry` shape mirrors this access pattern with named fields; consumers needing fields outside the typed shape reach through `Raw` (the post-extraction equivalent of today's `map[string]any`).

## Context

Every consumer of claude's per-session JSONL re-implements the same tail loop: open at offset, `bufio.Reader.ReadBytes('\n')`, accumulate partial bytes across short reads, JSON-decode each completed line, push onto an outbound channel with context-cancellation backpressure, and on EOF sleep one tick then retry. That logic exists in seven in-tree files today:

```
cmd/spike-one-turn/main.go    tailJSONL
cmd/spike-multi-turn/main.go  tailJSONL
cmd/spike-cancel/main.go      tailJSONL
cmd/spike-permission/main.go  tailJSONL
cmd/spike-ask-user/main.go    tailJSONL
cmd/spike-long-prompt/main.go tailJSONL
cmd/probe-first-prompt-hang/main.go tailJSONLAll
```

Six of the seven (every `spike-*` binary) are byte-identical apart from the import block; the seventh (the probe) is a callback-shaped variant of the same loop. Pyrycode's `agentrun` will need the same primitive once it stops shelling out to a spike binary. Reimplementing the loop is busywork and a drift risk.

This ticket promotes the loop into the library as a public API alongside the existing `SessionJSONLPath` + `WaitForSessionJSONL` (#58). The same justification as #58 applies: the shape is stable, the duplication is real, and a single canonical implementation in `pkg/tuidriver/` closes the drift at source.

**Scope delta from the spike loop.** Two behavioural changes from `cmd/spike-long-prompt/main.go:tailJSONL`:

1. The assistant-only filter (`type == "assistant"` AND `message` is a map) is **removed**. The library emits every parsed line. Filtering moves up the stack — different consumers want different filters (the cancellation marker is `type == "user"`, the deferred-tools-delta marker is `type == "attachment"`, end-of-turn detection is `type == "assistant"` only). See [jsonl-layout.md § Cancellation signal](../../knowledge/architecture/jsonl-layout.md) and § Tool-use shapes.
2. Parse errors are **silently dropped** instead of `logger.Printf`'d. The library has no logger to call into; surfacing parse errors is left to a follow-up if needed (see § Open questions).

What is explicitly **out of scope** for this slice:

- End-of-turn discriminator (`isEndTurn`-shaped) — separate concern layered on top, tracked separately.
- msg_id grouping / content extraction (`extractByMsgID`-shaped) — separate concern, tracked separately.
- Merging the JSONL stream with PTY-side state — separate concern.
- Migrating the seven in-tree spike/probe binaries to consume the new API — mechanical follow-up, lands separately.

## Design

### Files touched

```
pkg/tuidriver/jsonl.go         (MODIFIED — append TailJSONL + types)
pkg/tuidriver/jsonl_test.go    (MODIFIED — append TestTailJSONL_*)
pkg/tuidriver/tuidriver.go     (MODIFIED — 2-line doc-comment edit)
```

No new files. The new function and types are thematically grouped under the existing `jsonl.go` (which #58 created precisely to leave room for "future related primitives e.g. tail helpers, if and when promoted from spike code" — see #58's spec § "New file").

### Public surface

One function and three exported types, no constructor / no handle struct.

```go
// JSONLEntry is one parsed line from a claude session JSONL log.
//
// Type is the envelope kind (assistant, user, attachment,
// permission-mode, file-history-snapshot, ai-title, system,
// last-prompt — see docs/knowledge/architecture/jsonl-layout.md §
// "Observed top-level type values"). Message is non-nil iff the
// envelope carried a "message" object (i.e. only on assistant and
// user). Raw holds the full parsed JSON for fields outside the typed
// shape — envelope-specific fields like "attachment", "sessionId",
// future additions, and the message object itself.
//
// Fields are populated best-effort. Missing or type-mismatched JSON
// yields zero values (Type == "", Message == nil, Content == nil);
// the entry is still emitted. Consumers requiring presence-vs-absence
// semantics check `_, ok := e.Raw["message"]` directly.
type JSONLEntry struct {
    Type    string
    Message *EntryMessage
    Raw     map[string]any
}

// EntryMessage is the nested `message` object on assistant and user
// envelopes. The library populates ID, StopReason, and Content
// best-effort from the JSON; consumers reach through Raw (the full
// message map) for fields outside this typed view (model, role, usage,
// stop_sequence, …).
type EntryMessage struct {
    ID         string
    StopReason string
    Content    []ContentBlock
    Raw        map[string]any
}

// ContentBlock is one element of `message.content[]`. Type is the
// block's kind (text, thinking, tool_use, tool_result, …); Raw holds
// the full block JSON so consumers can extract type-specific fields
// (text, input, name, tool_use_id, …) without the library committing
// to a type-tagged union over claude's content-block schema.
type ContentBlock struct {
    Type string
    Raw  map[string]any
}

// TailJSONL opens path, seeks to startOffset, and spawns a goroutine
// that reads lines, parses each as JSON into a JSONLEntry, and emits
// them on the returned channel. Use 0 for startOffset to start at the
// beginning; use a previously recorded offset (e.g. f.Tell()-ish) to
// resume mid-file.
//
// Returns an error synchronously if the file cannot be opened or the
// seek fails — both indicate a programmer error or permission issue
// that polling will not resolve. Use WaitForSessionJSONL first to
// guarantee the file exists.
//
// The channel is closed when ctx is cancelled or when an
// unrecoverable read error occurs (rare on an append-only file —
// see § Error handling). The goroutine returns within one poll tick
// (DefaultPollInterval, 50 ms) after ctx is cancelled.
//
// Malformed JSON lines are silently dropped — the goroutine continues
// reading. Partial lines split across reads are reassembled before
// parsing, so each completed JSON line is delivered as exactly one
// channel value, never two. EOF on a still-active log is treated as
// "wait for more bytes" (sleep one poll tick, retry).
//
// The channel is buffered (capacity 32 — matches the spike-side
// observed-stable size). Consumers should drain promptly; if the
// buffer fills, the tail goroutine blocks on the next send until
// either a receiver makes space or ctx is cancelled.
//
// Compose with SessionJSONLPath and WaitForSessionJSONL:
//
//     path := tuidriver.SessionJSONLPath(home, cwd, sessionID)
//     if err := tuidriver.WaitForSessionJSONL(ctx, path); err != nil { … }
//     entries, err := tuidriver.TailJSONL(ctx, path, 0)
//     if err != nil { … }
//     for ev := range entries { /* … */ }
func TailJSONL(ctx context.Context, path string, startOffset int64) (<-chan JSONLEntry, error)
```

### Behaviour contracts

**`TailJSONL` — open + seek phase (synchronous, runs on caller's goroutine)**

- `os.Open(path)` — returns `fmt.Errorf("open session jsonl %s: %w", path, err)` on failure. No retry.
- `f.Seek(startOffset, io.SeekStart)` — returns `fmt.Errorf("seek session jsonl %s to %d: %w", path, startOffset, err)` on failure. The file is closed before returning (so the caller does not need to). `defer f.Close()` is NOT used at this point because the goroutine takes ownership on success.
- On success, create the buffered channel (capacity `defaultJSONLTailBuffer = 32`, package-level unexported constant in `jsonl.go`), spawn the goroutine, return `(ch, nil)`. The goroutine owns `f` from this point and closes it on exit.

**`TailJSONL` — goroutine loop (runs to completion or ctx cancellation)**

Loop body sketch (the developer writes the code; this lists the invariants):

- `bufio.NewReader(f)` once before the loop. `var partial []byte` for cross-read accumulation.
- Top of loop: `if ctx.Err() != nil { return }` short-circuit before any I/O.
- `chunk, rerr := reader.ReadBytes('\n')`.
  - If `len(chunk) > 0`, `partial = append(partial, chunk...)`.
  - If `rerr == nil` (newline found):
    - `line := bytes.TrimRight(partial, "\r\n")`.
    - `partial = partial[:0]` (reset, keep capacity).
    - If `len(line) == 0`, `continue` (blank line — claude doesn't emit these but be defensive).
    - Parse with `parseEntry(line) (JSONLEntry, bool)` (private helper; see below). On parse failure (`ok == false`), `continue` — the malformed-line case.
    - On parse success, `select { case ch <- entry: case <-ctx.Done(): return }` — backpressure-aware send.
  - If `rerr == io.EOF`:
    - The trailing partial (if any) stays in `partial` for next read.
    - `select { case <-ctx.Done(): return; case <-time.After(DefaultPollInterval): }` — sleep one tick, then continue loop.
  - Else (`rerr != nil && rerr != io.EOF`): unrecoverable I/O error. Return — channel is closed via deferred `close(ch)`.
- Defer order: `defer close(ch)` and `defer f.Close()` are both set up before the loop, so both fire on every exit path.

**`parseEntry` — private helper in `jsonl.go`**

Signature and contract (no code body in this spec):

```go
// parseEntry parses one already-trimmed JSONL line into a JSONLEntry.
// Returns (zero-value, false) if the bytes are not valid JSON or do
// not decode into a top-level object. Caller is responsible for
// filtering out empty/whitespace-only lines.
func parseEntry(line []byte) (JSONLEntry, bool)
```

Body invariants:

- `json.Unmarshal(line, &raw)` where `raw` is `map[string]any`. On error, return `(JSONLEntry{}, false)`.
- `entry.Raw = raw`.
- `entry.Type, _ = raw["type"].(string)` (zero-value-on-mismatch type assertion — same idiom as the spike).
- `if m, ok := raw["message"].(map[string]any); ok { entry.Message = parseMessage(m) }` (private helper, similar shape).
- Return `(entry, true)`.

`parseMessage` does the analogous walk for `id` (string), `stop_reason` (string), and `content` ([]any → []ContentBlock). `parseContentBlock` extracts `type` (string) and stores the rest in `Raw`. All three helpers are ~5-8 lines each — straight type assertions with zero-value fallbacks. **Inline as the developer sees fit; the parse layer should add no more than ~25 lines total** (parse + parseMessage + parseContentBlock combined).

### Why not the assistant-only filter

The spike's `tailJSONL` filters to `type == "assistant"` with a `message` map. This is wrong at the library boundary for three reasons documented in jsonl-layout.md:

1. **Cancellation marker is `user`-role.** ESC cancel produces `{type:"user", message:{content:[{type:"text", text:"[Request interrupted by user]"}]}}`. The assistant-only filter drops it; consumers wanting JSONL-side cancel acknowledgment must observe `user` events too. See [jsonl-layout.md § Cancellation signal](../../knowledge/architecture/jsonl-layout.md).
2. **Tool-use produces `user(tool_result)` interleavings.** The spike filters these out because spike-one-turn-style content extraction only needs the final `assistant(end_turn)` text block. Library consumers writing richer logic may want the full sequence (e.g. an "observe a tool run" feature).
3. **Attachment / deferred-tools-delta markers are `attachment`-type.** The `probe-first-prompt-hang/tailJSONLAll` callback variant exists precisely because the assistant-only filter dropped the signal it needed. The library version emits everything; filtering is a one-line consumer-side branch.

Empirically, filtering moves up zero net lines (consumers add a `for ev := range entries { if ev.Type != "assistant" { continue } … }` they were going to write anyway against the typed shape) and unlocks two filter variants the library cannot anticipate.

### Why a free function, not a tail-handle type

The ticket allows "a function (or method on a small tail-handle type)". Free function chosen because:

- No state outside the channel — the goroutine owns the file handle, the partial buffer, and the bufio reader. None of these need consumer-side observability.
- Terminal-error reporting (e.g. an `Err()` method on a handle) adds API surface for the marginal benefit of distinguishing "ctx cancelled" (the expected exit) from "non-EOF read error mid-tail" (extremely rare on an append-only file claude is writing to). Consumers check `ctx.Err()` after the channel closes if they need to know.
- The shape matches the existing `WaitForSessionJSONL` (free function, error return, no handle). Promoting one to a method would introduce inconsistency in `jsonl.go`.

If a future consumer needs terminal-error observability or pause/resume control, a follow-up can introduce `NewJSONLTail`-shaped API alongside `TailJSONL`. The function version stays as the simple default.

### Why reuse `DefaultPollInterval` (50 ms)

The seven existing call sites all use `jsonlTailInterval = 50 * time.Millisecond` (literally the same value as `DefaultPollInterval`). No empirical evidence has surfaced for differentiating these. The library uses `DefaultPollInterval` directly; if a future ticket finds JSONL polling needs a different cadence than state polling, a `TailJSONLEvery(ctx, path, offset, interval)` variant can be added then.

### Doc-comment update in `tuidriver.go`

Two phrases need an update (lines around 7-11):

- In-scope list: change `"session JSONL path resolution + appearance polling"` → `"session JSONL path resolution, appearance polling, and tail-with-parsed-entries"`.
- Out-of-scope list: change `"JSONL parsing"` → `"JSONL semantic interpretation (end-of-turn detection, msg_id grouping, content extraction)"`.

This codifies the new boundary: the library owns the tail loop and the typed shape; consumers own the semantic interpretation on top of that shape. 2-line edit, no refactor.

### Concurrency model

One goroutine per `TailJSONL` call. The goroutine:

- Owns the `*os.File` (closed via `defer` on exit).
- Owns the partial-byte accumulator (`partial []byte`, never shared).
- Owns the bufio reader.
- Writes to the channel; the caller reads.

No mutexes. No shared state. Safe to call concurrently with itself (multiple paths, different goroutines).

Shutdown ordering:

1. Caller cancels `ctx`.
2. Goroutine observes either `ctx.Err() != nil` at loop top, or `<-ctx.Done()` in the EOF-sleep `select`, or `<-ctx.Done()` in the channel-send `select`.
3. Goroutine returns; deferred `close(ch)` fires; deferred `f.Close()` fires.
4. The caller's `for ev := range entries` loop terminates.

The maximum delay between ctx cancellation and channel close is bounded by `DefaultPollInterval` (50 ms) — the time the goroutine might be parked in the EOF-sleep `time.After`. There is no upper bound on a blocked send (the goroutine waits until the caller drains or ctx is cancelled — the `select` handles both).

### Error handling

Three exit paths for the tail goroutine, all bubbled to the caller via channel close (not via a returned error — the function already returned synchronously by the time the goroutine starts):

| Trigger                                           | Channel-close cause                       | Caller observes                          |
|---------------------------------------------------|-------------------------------------------|------------------------------------------|
| `ctx` cancelled / deadline expired                | Goroutine returns via `<-ctx.Done()`      | `ctx.Err() != nil`                       |
| `ReadBytes` returns non-EOF I/O error             | Goroutine returns from the `else` arm     | `ctx.Err() == nil` (rare; not reported separately in this slice) |
| File appears fully tailed forever                 | Not an exit — the loop sleeps in EOF retry indefinitely | n/a (the loop never reaches this) |

The `Tell()`-from-prior-run resume semantics: a `startOffset` beyond EOF is valid — `f.Seek` succeeds and `ReadBytes` returns `io.EOF` on the first read; the loop enters the EOF-sleep cycle and waits for the file to grow. This matches consumer intent for resume.

The synchronous open/seek errors:

| Trigger                                           | Returned error shape                                                  |
|---------------------------------------------------|-----------------------------------------------------------------------|
| `os.Open` fails (ENOENT, EACCES, …)               | `fmt.Errorf("open session jsonl %s: %w", path, err)`                  |
| `f.Seek` fails (offset out of range on a non-seekable file, etc.) | `fmt.Errorf("seek session jsonl %s to %d: %w", path, startOffset, err)` |

`errors.Is` works through both wrap chains. The caller chains naturally with `WaitForSessionJSONL`:

```go
if err := tuidriver.WaitForSessionJSONL(ctx, path); err != nil { return err }
entries, err := tuidriver.TailJSONL(ctx, path, 0)
if err != nil { return err }
```

After `WaitForSessionJSONL` returns nil, `os.Open` failing in `TailJSONL` is a programmer error or a TOCTOU race the library does not try to recover from.

### Backpressure

The channel is buffered (capacity 32). The send arm is `select { case ch <- entry: case <-ctx.Done(): return }`. Three cases:

- **Receiver drains promptly.** Sends succeed immediately, buffer hovers near zero. Common case.
- **Receiver lags but ctx is live.** Sends block on `ch <- entry`. The goroutine cannot read more bytes from the file while blocked, so the file's bufio buffer fills first (4 KB default), then claude's stdout pipe to the JSONL file fills, then claude's write `syscall.Write` blocks. In practice this is unlikely — claude emits JSONL lines at human-perceptible speeds.
- **Receiver lags AND ctx is cancelled.** The `<-ctx.Done()` arm fires; goroutine returns immediately; channel closes. The pending entry is dropped.

The capacity-32 value matches the spike-side `eventCh := make(chan map[string]any, 32)` that survived all six observed tickets without buffer-overflow. No empirical evidence to adjust it; tunability is a follow-up if a real consumer surfaces a real problem.

## Testing strategy

`pkg/tuidriver/jsonl_test.go` covers the five AC bullets. Each scenario is one (or two) discrete `TestTailJSONL_*` functions; the developer writes them in the existing `pkg/tuidriver` idiom (discrete `Test*` funcs, `t.TempDir()` for isolation, `time.Sleep` between phases is acceptable — same as `TestWaitForSessionJSONL_FileAppearsPartwayThrough`).

For every test, the file is created with `os.WriteFile` initially, then opened with `os.OpenFile(..., os.O_APPEND|os.O_WRONLY)` for in-test appends. `time.Sleep(150 * time.Millisecond)` between phases is reliable (3× the 50 ms poll interval); CI flakiness margin is the same as the `WaitForSessionJSONL` tests.

### `TestTailJSONL_AppendDuringTail`

Verifies AC bullet "append-during-tail with multiple multi-line entries".

- Pre-write: two valid JSON lines in the file (e.g. `{"type":"a"}\n{"type":"b"}\n`).
- Start `TailJSONL(ctx, path, 0)`.
- Receive two entries; assert `Type == "a"` and `Type == "b"` in order.
- Append two more lines (`{"type":"c"}\n{"type":"d"}\n`) to the file.
- Receive two more entries; assert `Type == "c"` and `Type == "d"`.
- Cancel ctx; assert channel closes.

Test scaffolding shape:

- A helper `mustReceive(t, ch <-chan JSONLEntry, timeout time.Duration) JSONLEntry` that fails the test if no value arrives within timeout (e.g. 500 ms). Use for every receive in this file to bound test runtime under deadlock.
- A helper `mustAppend(t, path, line string)` that opens with `O_APPEND|O_WRONLY` and writes one line. Closes the file before returning.

### `TestTailJSONL_PartialLineAcrossReads`

Verifies AC bullet "mid-line truncation across reads".

- Pre-write the first half of a JSON line, no newline: `{"type":"split"`.
- Start `TailJSONL(ctx, path, 0)`.
- `time.Sleep(150ms)` — tail consumes the partial bytes, hits EOF, parks in retry.
- Append the rest: `,"id":"x"}\n`.
- Receive one entry; assert `Type == "split"` and `Raw["id"] == "x"`.
- Assert no second entry arrives (no spurious split delivery): `select { case e := <-ch: t.Fatalf("unexpected second entry: %+v", e); case <-time.After(150ms): }`.
- Cancel ctx; assert channel closes.

This is the most behaviour-sensitive test — its job is to lock in "exactly one channel value per completed line, never two".

### `TestTailJSONL_MalformedLineNonFatal`

Verifies AC bullet "malformed JSON line followed by valid line".

- Pre-write: `not json at all\n{"type":"recovers"}\n`.
- Start `TailJSONL(ctx, path, 0)`.
- Receive one entry; assert `Type == "recovers"` (the bad line was silently dropped, the goroutine kept reading).
- Cancel ctx; assert channel closes.

A second sub-test asserts that the channel is NOT closed by a malformed line: between receiving the bad line and the valid one, the channel must still be live. The shape "receive a valid entry after a bad one" is sufficient evidence — if the goroutine had died, the valid line would never arrive.

### `TestTailJSONL_ContextCancellationMidRead`

Verifies AC bullet "context cancellation causes the tail goroutine to return promptly without panicking, and the output channel is closed on shutdown".

- Pre-write nothing (or one line — doesn't matter). The interesting case is cancellation while the goroutine is parked in the EOF-sleep `select`.
- Start `TailJSONL(ctx, path, 0)` with a `ctx, cancel := context.WithCancelCause(parent)`.
- Drain any pre-written entries.
- `time.Sleep(150ms)` to ensure the goroutine has parked in the EOF-sleep.
- `cancel(sentinel)` where `sentinel := errors.New("test stop")`.
- Assert the channel closes within ~200 ms (well above one tick, well below CI timeout): `select { case _, ok := <-ch: if ok { t.Fatal("got entry instead of close") }; case <-time.After(200ms): t.Fatal("channel did not close") }`.
- Assert `errors.Is(context.Cause(ctx), sentinel)` (sanity check on the test, not on the library — the library does not return the cause; the caller already has it).

### `TestTailJSONL_EOFAppendCycles`

Verifies AC bullet "EOF/append cycles".

- Pre-write one line.
- Start `TailJSONL(ctx, path, 0)`.
- Receive one entry.
- `time.Sleep(150ms)` (tail is now in EOF-sleep).
- Append a line. Receive an entry.
- `time.Sleep(150ms)`.
- Append another line. Receive an entry.
- Repeat once more for good measure (4 cycles total).
- Cancel ctx; assert channel closes.

This is the "the loop doesn't get stuck after the first EOF" test. The repeated cycles catch any one-shot bug in the partial-buffer reset or the EOF-sleep wakeup.

### `TestTailJSONL_StartOffsetSkipsPrefix`

Verifies AC bullet "Caller controls the start offset — the API supports both starting at 0 (new session) and resuming mid-file at a caller-supplied byte offset".

- Pre-write three lines: `{"type":"a"}\n{"type":"b"}\n{"type":"c"}\n`.
- Compute the offset of the second line's start: `int64(len("{\"type\":\"a\"}\n"))`.
- Start `TailJSONL(ctx, path, offset)`.
- Receive entries; assert exactly `b` and `c` arrive, in order, with no `a`.
- Cancel ctx; assert channel closes.

### `TestTailJSONL_OpenFailsWhenFileMissing`

Spec-only test (not in AC bullets, but the synchronous-error contract is worth asserting):

- Call `TailJSONL(ctx, "/nonexistent/path/x.jsonl", 0)`.
- Assert `(ch, err)` — `ch == nil` and `err != nil`.
- Assert `strings.Contains(err.Error(), "/nonexistent/path/x.jsonl")` — path is in the message.
- Assert `errors.Is(err, fs.ErrNotExist)` (the wrapped syscall error).

### No-regression on existing tests

The new file adds tests; the existing `TestSessionJSONLPath_*` and `TestWaitForSessionJSONL_*` are unchanged. `go test ./pkg/tuidriver/...` must still pass; that's covered by running the existing suite unchanged.

## Out of scope (reminder, mirrors ticket)

- **End-of-turn discriminator.** `isEndTurn(ev)` — `ev.Message != nil && ev.Message.StopReason == "end_turn"` — stays in consumer code for this slice. Promotion is a follow-up.
- **msg_id grouping / content extraction.** The `extractByMsgID` shape (collect every assistant event for a given `msg_id`, walk `Content` looking for `text` blocks, concatenate) stays in consumer code. Same follow-up candidate.
- **Cancellation marker detection.** `ev.Type == "user" && len(ev.Message.Content) == 1 && ev.Message.Content[0].Type == "text" && ev.Message.Content[0].Raw["text"] == "[Request interrupted by user]"` is one line in the consumer; not worth a library helper today.
- **Migrating the seven in-tree spike/probe `tailJSONL`s.** Mechanical follow-up, lands separately. The new library function is purely additive in this slice.
- **Migrating pyrycode's downstream consumers.** Different repo; tracked over there.
- **Parse-error observability.** Silent-drop is the default; logger-injection or error-channel variants are follow-up candidates if a consumer surfaces real need.
- **Configurable poll interval.** `DefaultPollInterval` (50 ms) is the only knob; consumers tune timeout via context.
- **Configurable channel buffer.** Hardcoded at 32; expose as an Option only if a consumer surfaces real backpressure.
- **Partial-line buffer cap.** `partial []byte` grows via `append` without an upper bound. Claude's JSONL lines are typically <1 KB but can be many KB (base64-encoded images). No cap in this slice; document as an open question.

## Open questions

- **Should `TailJSONL` return a `JSONLTail` handle with an `Err()` method instead of a bare channel?** Considered and deferred. A bare channel covers every AC bullet with less API surface; terminal-error reporting for non-EOF I/O errors is the only feature lost, and those errors are extremely rare on an append-only file. If a future ticket surfaces a real need, the handle variant can land alongside `TailJSONL` (additive).
- **Should parse errors be logged or surfaced?** Deferred to follow-up. The spike's `logger.Printf("jsonl-parse-warning err=%v", jerr)` was useful for debugging the spike but adds no value at the library boundary — consumers either don't care or have a richer log story. If observability becomes load-bearing, an `Option func(*tailOpts)` shape can be added.
- **Should the partial-line accumulator have a cap?** Skipped. Claude's longest observed JSONL line is the base64-encoded image envelope (`attachment` with a `~500-line` body), well under the GB regime where the cap would matter. A pathological adversary writing a 10 GB single-line "JSONL" file is not in scope. Document as a future-hardening candidate.
- **Should the channel-buffer capacity be exposed as an Option?** Skipped. Hardcoded at 32 (matches the spike's observed-stable size). Same "follow-up if real consumer surfaces real problem" answer.
- **Does `TailJSONL` need a `WithLogger`-style option for parse warnings?** Same as above — deferred. The two-line consumer-side wrap (`for ev := range entries { … }` + a manual log on a known-bad envelope shape) covers any near-term need.

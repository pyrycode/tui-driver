# Spec: `CastRecorder` — asciinema v2 flight recorder (`io.Writer` mirror)

Ticket: [#125](https://github.com/pyrycode/tui-driver/issues/125). From-spec build (the `spike-screen-recorder` spike was lost — same as pyrycode#553). Size: S.

## Files to read first

- `pkg/tuidriver/session.go:37-49` — `SpawnOpts.Mirror io.Writer`: the seam the recorder plugs into. `*CastRecorder` must satisfy `io.Writer` so it can be passed here directly.
- `pkg/tuidriver/session.go:112-130` — the PTY reader goroutine: `chunk := buf[:n]; opts.Mirror.Write(chunk)`. Two facts the design depends on: (a) **one `Mirror.Write` per PTY read** → one cast event per `Write`, no coalescing; (b) `chunk` aliases the reused 4 KB `buf`, so `Write` MUST consume `p` synchronously before returning (it does — `string(p)` copies during marshal, and the reader does not issue the next `Read` until `Write` returns). The reader **ignores** `Write`'s return values.
- `pkg/tuidriver/buffer.go:1-50` — house style for a small stateful primitive in this package: doc comment with "Construct with NewX; the zero value is not usable", `sync.Mutex` guarding mutable state, `time.Now()` used directly (no clock seam). Mirror this shape.
- `pkg/tuidriver/jsonl.go` — the only other `encoding/json` user in the package; confirms stdlib `encoding/json` is the in-package idiom (no third-party JSON).
- `pkg/tuidriver/buffer_test.go` — closest test shape (construct primitive, drive it, assert on observable output); model `castrecorder_test.go` on it. Real-clock timing assertions with tolerance are acceptable here (the package does not inject a clock).
- Source spec (vault): `📋 Projects/2026-04-10 - Pyrycode/ptyrunner-session-recorder-debug-flag-prompt.md` § "Encoding notes" — the authoritative resolution of the data-string encoding question (see Context below). Read it before writing the encoder.

## Context

ptyrunner drives `claude` blind: when a session misbehaves (hang, wrong state detection, modal mis-handling) the only record is a 4 KB rolling buffer that vanishes when the process exits. A **flight recorder** closes that gap — every PTY byte is mirrored to an [asciinema v2](https://docs.asciinema.org/manual/asciicast/v2/) `.cast` file, replayable (`asciinema play`) or parseable offline.

The `Session` reader already exposes a passive `io.Writer` mirror (`SpawnOpts.Mirror`). This ticket adds the recorder that frames each mirrored chunk as one asciinema v2 output event. **Library-only** — there is no `cmd/` consumer in this repo to wire; the `PYRY_RECORD_DIR` wiring (file creation, session-id naming, permissions, rotation) lives in pyrycode#552.

### asciinema v2 format (what the recorder emits)

Line-delimited JSON:

1. **Header** (line 1): one JSON object. Required `version` (always `2`), `width`, `height`. `timestamp`/`title`/`env` optional.
   `{"version":2,"width":80,"height":24}`
2. **Events** (line 2+): one JSON array per line, `[elapsed, code, data]`:
   - `elapsed` — float seconds since the **first `Write`** (not construction, not header).
   - `code` — always `"o"` (output). No other stream recorded.
   - `data` — the raw PTY bytes as a JSON string.
   `[0.248, "o", "[2J[Hhello"]`

### The one subtle decision: data-string encoding (resolved)

The AC phrase *"non-UTF8 escaped correctly"* + *"decodes back to the exact bytes of `p`"* could be read as "byte-exact for arbitrary binary." It is **not** that, and the source spec's "Encoding notes" section settles it:

- **Marshal `string(p)` with `encoding/json`.** It escapes quotes, backslashes, and control bytes (`` for ESC, `\n`, `\t`, …) correctly and produces a valid JSON string. This is the simplest correct path and the in-package idiom.
- **Invalid UTF-8 is coerced to U+FFFD by `json.Marshal`** (Go's documented `string`-encoding behaviour). For a faithful recorder this is **acceptable**: asciinema assumes a UTF-8 text stream and real `claude` output is UTF-8. **Do not hand-roll a byte-exact escaper** — byte-exact round-trip of arbitrary bytes through a JSON string is not achievable by standard JSON decode and is not a requirement.
- The AC's round-trip guarantee is **for valid UTF-8** (quotes, control/escape bytes, multibyte runes), which `encoding/json` round-trips exactly. *"non-UTF8 escaped correctly"* means "stays valid JSON, doesn't crash" — not "byte-exact binary."

This is the highest-risk implementation choice; the developer must follow it rather than re-derive it, or they will burn turns building an unnecessary custom escaper.

## Design

One new file: `pkg/tuidriver/castrecorder.go`. No changes to any existing file.

### Type

```go
type CastRecorder struct {
    // w, cols, rows set at construction; start lazily on first Write;
    // mu guards start + the underlying write sequence.
}
```

Unexported fields: `w io.Writer`, `cols int`, `rows int`, `start time.Time` (zero until the first `Write`), `mu sync.Mutex`. The zero value is not usable — construct with `NewCastRecorder`. Add a compile-time assertion `var _ io.Writer = (*CastRecorder)(nil)`.

### Constructor (fixed by downstream contract — do not vary)

```go
func NewCastRecorder(w io.Writer, cols, rows int) *CastRecorder
```

Stores `w`, `cols`, `rows`. **Does not** touch the clock and **does not** write anything. Returns `&CastRecorder{...}` with `start` left zero.

### `WriteHeader() error`

Signature decision (the AC delegates this to the architect): **returns `error`.** Rationale — the consumer (#552) calls `WriteHeader()` exactly once right after creating the file, a natural place to check/log a failure; returning the error is the idiomatic Go surface and the minimal way to satisfy "a failed underlying write surfaces to the caller rather than being swallowed." A stored-error / deferred-`Err()` design would add state for no benefit. The source spec writes it `WriteHeader() error`.

Behaviour: marshal a header object, append `'\n'`, do **one** `w.Write`, return its error (nil on success). Use a small unexported struct for stable key order:

```go
type castHeader struct {
    Version int `json:"version"`
    Width   int `json:"width"`
    Height  int `json:"height"`
}
```

Emit `{"version":2,"width":<cols>,"height":<rows>}`. **Omit `timestamp`** — the AC explicitly tolerates its absence, omitting it keeps the header deterministic (no `time.Now()` at header time) and trivially testable. (Including it is allowed but adds nothing here.) Contract: call once, before the first `Write`; not enforced in code (no observed misuse — evidence-based, don't add a guard for a failure that hasn't happened).

### `Write(p []byte) (int, error)`

Frames `p` as one event line and returns `(len(p), nil)` on success. Steps (under `mu`):

1. If `start.IsZero()`, set `start = time.Now()` — **lazy clock start on the first `Write`**. This is the mechanism that keeps the first event's `elapsed ≈ 0` regardless of the construction→header→first-byte gap.
2. `elapsed := time.Since(start).Seconds()` (≈ 0 on the first call).
3. Marshal the event and append `'\n'`. Simplest correct form: `json.Marshal([]any{elapsed, "o", string(p)})`. This handles the float, the `"o"` literal, and the data-string escaping in one call. `string(p)` copies `p` (satisfies the aliasing constraint from `session.go`).
4. One `w.Write` of the line.
   - On error: return `(0, err)` — recording is all-or-nothing per chunk, so `0` is the honest count of `p`-bytes durably recorded (satisfies `io.Writer`'s "n < len(p) ⇒ non-nil err"). The Mirror caller ignores the return anyway.
   - On success: return `(len(p), nil)`.

Notes on the encoded forms:
- `elapsed` serialises as a JSON number (`0` for the first event, `0.248…` later); decoded into `[]any` it is a `float64`, satisfying "`<elapsed float>`". The AC does not pin decimal precision, so default `json.Marshal` float formatting is fine.
- Default `json.Marshal` HTML-escapes `<`, `>`, `&` inside the data string (`<`, …). Harmless: still valid JSON and round-trips to the same bytes. Disabling it (via a `json.Encoder` with `SetEscapeHTML(false)`) is optional and not required.

## Concurrency model

Documented use is a **single writer** (the PTY mirror goroutine), so strict locking is not required by the consumer. The design still includes a `sync.Mutex` guarding the lazy `start` init and the underlying `w.Write` in both `WriteHeader` and `Write` — it matches `Buffer`'s house style, costs nothing on the single-writer path, and makes `*CastRecorder` safe if a future caller writes concurrently. No goroutines, channels, or shutdown sequence are introduced; the recorder is purely synchronous and owns no lifecycle (the consumer owns the file handle `w` and closes it).

## Error handling

- `WriteHeader` and `Write` surface the underlying `w.Write` error to the caller; neither swallows it.
- No retry, no buffering, no partial-line recovery: a failed `w.Write` returns the error and the caller decides (the consumer in #552 will typically log and continue or stop recording). A partially-written line on a failing writer is the writer's problem, not the recorder's to repair.
- Malformed input is impossible — `p` is arbitrary bytes and `json.Marshal` of `[]any{float64, string, string}` cannot fail for these types, so `Write`'s only error source is `w.Write`. (No need to handle a `json.Marshal` error specially; if defensiveness is wanted, return it as `(0, err)` identically.)

## Testing strategy

Test-first, in `pkg/tuidriver/castrecorder_test.go`. Drive the recorder against a `*bytes.Buffer` (success) or a sentinel-failing `io.Writer` (error path). Scenarios (developer writes them in the package's testing idiom):

- **Header shape** — construct with `cols=80, rows=24`, `WriteHeader()`. Assert: output is exactly one `'\n'`-terminated line; it parses as a JSON object; `version==2`, `width==80`, `height==24`. (Tolerate extra keys like `timestamp` so the test doesn't pin the omission.)
- **Header propagates write error** — `w` returns a sentinel error; `WriteHeader()` returns that error (not nil). Pins "don't swallow."
- **Event shape + valid-UTF8 round-trip** — `Write` a payload mixing the tricky-but-valid cases: a double-quote, a backslash, a newline, an ESC (`\x1b`), and a multibyte rune (e.g. `"café"` or an emoji). Assert: one `'\n'`-terminated line; it parses as a 3-element JSON array; `arr[1]=="o"`; `[]byte(arr[2].(string))` equals the original payload byte-for-byte; the `Write` call returned `(len(p), nil)`.
- **First elapsed ≈ 0, clock starts on first Write (not construction/header)** — construct, `WriteHeader()`, then sleep a measurable delay (e.g. 20 ms), then first `Write`. Parse `elapsed`; assert it is ≈ 0 (generous tolerance, e.g. `< 0.01` s). This proves neither construction nor `WriteHeader` started the clock.
- **Later event has strictly larger elapsed** — after the first `Write`, sleep a measurable delay (e.g. ≥ 5 ms), second `Write`; assert `elapsed₂ > elapsed₁`.
- **`io.Writer` satisfaction** — the compile-time `var _ io.Writer = (*CastRecorder)(nil)` assertion in the source file covers this; no runtime test needed.

Then run the repo's standard check — `make e2e` builds every binary and runs the runner; the new unit test runs under `go test ./...` (the per-package gate). Paste the `ok  github.com/pyrycode/tui-driver/pkg/tuidriver` line into the PR.

### What is NOT tested

- Byte-exact round-trip of **invalid** UTF-8 — explicitly out of contract (U+FFFD coercion is accepted). Do not write a test asserting `[]byte{0xff}` survives; it cannot and should not.
- Concurrent writers — the documented contract is single-writer; the mutex is defence-in-depth, not a tested guarantee.
- The consumer wiring (`PYRY_RECORD_DIR`, file creation, replay) — pyrycode#552.

## Open questions

None blocking. Two notes for the developer/PR, not design decisions:

- **Clock seam:** the timing assertions use the real clock with tolerance (matching how `Buffer` tests treat `time.Now()`). A `now func() time.Time` field would make timing fully deterministic but adds an unexported seam the package otherwise doesn't use — **not recommended** unless the real-clock test proves flaky in CI.
- **Downstream pseudo-version:** after this lands, note the new `github.com/pyrycode/tui-driver` pseudo-version for the pyrycode `go.mod` bump (currently pinned `v0.0.0-20260523181457-c2dcd1e49992`). This is a pyrycode#552 concern; mention the new version in the PR description so the consumer ticket can pick it up.

## Out of scope

- Any `cmd/` consumer or `PYRY_RECORD_DIR` handling (pyrycode#552).
- Input-stream (`"i"`) events, resize (`"r"`) events, or any asciinema code other than `"o"`.
- Redaction / secret-scrubbing — the recorder copies raw bytes verbatim; a `.cast` may contain whatever the terminal showed. Placement and permissions are the consumer's call. (The ticket confirms this is not a `security-sensitive` surface.)
- Coalescing or buffering multiple PTY reads into one event.
- The offline analyzer (`cmd/analyze-cast`) — deferred, separate decision.

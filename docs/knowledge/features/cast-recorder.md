# CastRecorder — asciinema v2 flight recorder

`tuidriver.CastRecorder` is an `io.Writer` that mirrors a PTY byte stream to an [asciinema v2](https://docs.asciinema.org/manual/asciicast/v2/) `.cast` file — one event per mirrored chunk. Drop it into [`SpawnOpts.Mirror`](#how-it-plugs-in) and every byte the spawned `claude` shows is recorded, so a misbehaving session (hang, wrong state detection, modal mis-handling) can be replayed (`asciinema play`) or parsed offline frame-by-frame with real timing. Library-only — introduced by [#125](../codebase/125.md); the file-creation / naming / placement wiring lives in the downstream consumer (pyrycode#552, behind `PYRY_RECORD_DIR`). Lives in `pkg/tuidriver/castrecorder.go`.

## Why

ptyrunner drives `claude` blind: when a session misbehaves the only forensic record is the 4 KB rolling buffer, which vanishes when the process exits. The flight recorder closes that gap by mirroring the full byte stream to a durable, replayable `.cast`. The `Session` reader already exposed a passive `io.Writer` mirror (`SpawnOpts.Mirror`, previously used only to tee the TUI to `os.Stderr` for spike operators); the recorder just frames those bytes as asciinema v2 events.

## API

```go
func NewCastRecorder(w io.Writer, cols, rows int) *CastRecorder  // constructor pinned by the downstream contract
func (r *CastRecorder) WriteHeader() error                       // emits the version-2 header line; call once, before the first Write
func (r *CastRecorder) Write(p []byte) (int, error)              // appends one event line; satisfies io.Writer
```

- `NewCastRecorder` stores `w` / `cols` / `rows`. It writes nothing and does not start the clock. The zero value is not usable — always construct via the constructor.
- `*CastRecorder` satisfies `io.Writer` (compile-time `var _ io.Writer = (*CastRecorder)(nil)`), so it passes straight into `SpawnOpts.Mirror`.
- `cols` / `rows` are the recorded terminal dimensions written into the header; the recorder does not query or enforce them against the real PTY.

## Wire format (what it emits)

Line-delimited JSON:

1. **Header** (line 1, from `WriteHeader`): one JSON object — `{"version":2,"width":<cols>,"height":<rows>}`. `timestamp` / `title` / `env` are optional and intentionally omitted, which keeps the header deterministic and free of `time.Now()` (the asciinema spec and the AC both tolerate their absence). Emitted from an unexported `castHeader` struct (not a map) so key order is stable.
2. **Events** (line 2+, from each `Write`): one JSON array per line — `[<elapsed>, "o", <data>]`:
   - `elapsed` — float seconds since the **first `Write`** (see [Lazy clock](#lazy-clock-start)).
   - code — always `"o"` (output). No other asciinema stream (`"i"` input, `"r"` resize) is recorded.
   - `data` — the raw PTY chunk as a JSON string.

Example:

```
{"version":2,"width":80,"height":24}
[0.248,"o","[2J[Hhello"]
```

## How it plugs in

```go
f, _ := os.Create(castPath)          // consumer owns the file handle and its lifecycle
rec := tuidriver.NewCastRecorder(f, 80, 24)
rec.WriteHeader()                    // once, right after creating the file
sess, _ := tuidriver.Spawn(ctx, argv, tuidriver.SpawnOpts{Mirror: rec})
// ... the reader goroutine now calls rec.Write(chunk) once per PTY read ...
f.Close()                            // consumer closes when done; the recorder owns no lifecycle
```

The `Session` reader calls `Mirror.Write` **once per PTY read** (`session.go:112-130`), so each chunk becomes exactly one event — no coalescing or buffering across reads. The reader ignores `Write`'s return values.

## Lazy clock start

The elapsed clock starts on the **first `Write`**, not at construction and not at `WriteHeader`. The first event's `elapsed` is therefore ≈ 0 regardless of how much wall-clock passed during setup or before the first byte arrived; later events advance monotonically from that origin. This is the behaviour the recorder exists to guarantee — a replay's timeline tracks the session, not the harness's startup latency.

## Data encoding (the one subtle decision)

Each chunk is recorded as `json.Marshal(string(p))`. This is the in-package idiom (matches `jsonl.go`) and the simplest correct path:

- **Valid UTF-8 round-trips byte-for-byte** — quotes, backslashes, control/escape bytes (ESC → ``, `\n`, `\t`, …), and multibyte runes all survive a JSON decode back to the exact bytes. The AC's round-trip guarantee is for valid UTF-8.
- **Invalid UTF-8 is coerced to U+FFFD** by `json.Marshal` (Go's documented `string`-encoding behaviour). This is **accepted** for a recorder: asciinema assumes a UTF-8 text stream and real `claude` output is UTF-8. *"non-UTF8 escaped correctly"* in the AC means "stays valid JSON, doesn't crash" — **not** byte-exact binary round-trip, which standard JSON decode cannot provide. **Do not hand-roll a byte-exact escaper.** (Spec § "The one subtle decision"; this was the implementation's highest-risk caveat.)
- `string(p)` copies `p` synchronously during marshal, so the caller may reuse its buffer the moment `Write` returns — which the `Session` reader relies on, since the mirrored chunk aliases its reusable 4 KB read buffer.
- Default `json.Marshal` HTML-escapes `<` `>` `&` inside the data string. Harmless — still valid JSON and decodes back to the same bytes.

## Error handling

- Both `WriteHeader` and `Write` surface the underlying `w.Write` error to the caller — never swallowed. `WriteHeader` returning `error` is the architect's signature decision (the AC delegated it): the consumer calls it once right after file creation, a natural place to check/log a failure.
- `Write` returns `(len(p), nil)` on success and `(0, err)` on a failed underlying write — recording is all-or-nothing per chunk, so `0` is the honest count of bytes durably recorded (and satisfies `io.Writer`'s "n < len(p) ⇒ non-nil err").
- No retry, no buffering, no partial-line recovery: a failed write returns and the consumer decides (log-and-continue or stop recording). `json.Marshal` of `[]any{float64, "o", string}` cannot fail, so `w.Write` is the only error source.

## Concurrency

Documented use is a **single writer** (the PTY mirror goroutine). A `sync.Mutex` guards the lazy `start` init and the underlying `w.Write` in both methods — this matches `Buffer`'s house style, costs nothing on the single-writer path, and keeps `*CastRecorder` safe if a future caller writes concurrently. It is defence-in-depth, not a tested guarantee. The recorder introduces no goroutines, channels, or shutdown sequence — it is purely synchronous and owns no lifecycle (the consumer owns and closes `w`).

## Limitations / out of scope

- **No redaction.** The recorder copies raw PTY bytes verbatim; a `.cast` may contain anything the terminal showed (including secrets). Directory placement and permissions are the consumer's call (pyrycode#552 writes under `PYRY_RECORD_DIR`). Not a `security-sensitive` surface in this repo.
- **Output-only.** Only the `"o"` stream is recorded; input (`"i"`) and resize (`"r"`) events are not.
- **No byte-exact binary** — invalid UTF-8 becomes U+FFFD (see above).
- **No `cmd/` consumer in this repo** — the `PYRY_RECORD_DIR` wiring (file creation, session-id naming, permissions, rotation) and the deferred offline analyzer (`cmd/analyze-cast`) live downstream / are separate decisions.

## Related

- [`SpawnOpts.Mirror`](../architecture/system-overview.md) — the `io.Writer` seam the recorder plugs into (`session.go:37-49`); previously teed only to `os.Stderr`.
- [Per-ticket notes #125](../codebase/125.md) — implementation summary, patterns, lessons.
- Spec: [docs/specs/architecture/125-castrecorder.md](../../specs/architecture/125-castrecorder.md).
- Downstream consumer: pyrycode#552 (TUI session flight-recorder behind `PYRY_RECORD_DIR`).

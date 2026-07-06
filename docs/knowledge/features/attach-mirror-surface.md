# Local-attach mirror surface — `MirrorOutput` stream + `AttachInput`

Two additive, opt-in `Session` surfaces that give a consumer a **live stream of the hosted process's raw PTY output bytes** out, and a **production raw-input path** for an attached terminal's keystrokes in — so a local `pyry attach` head can mirror `claude`'s live terminal byte-for-byte without reopening a parse-able seam. Introduced by [#136](../codebase/136.md). This is the load-bearing seam for the whole mobile-remote-head approach (consumer-side ADR 025, open risk #1). Lives in `pkg/tuidriver/session.go` (output) and `pkg/tuidriver/keys.go` (input).

## Why

The daemon supervisor must stop hosting `claude` with a bare PTY (`pty.Start` + `io.Copy`) and host it through a tui-driver `Session`, to unlock the sealed `DeliverPrompt` reliability path and the [`Events()`](../architecture/system-overview.md#key-signals) stream. But the local attach head still needs the hosted process's **raw screen bytes** to mirror the terminal, and a **production raw-input path** to forward the attached terminal's keystrokes back in.

The v1.0.0 seal removed every raw seam — `Session.Buffer`, `Session.PTY`, `SpawnOpts.Mirror`, and the public `Write` are all gone. What remained gave **no live raw-byte stream** and **no production raw-input path**:

| Existing surface | Why it doesn't fit attach |
|---|---|
| `Snapshot()` | point-in-time, parse-oriented buffer copy — not a stream |
| `RecordTo` | writes an asciinema `.cast` file tui-driver owns end to end — no `io.Writer` crosses the boundary (see [CastRecorder](cast-recorder.md)) |
| `MirrorStderr` | tees PTY bytes to the *process's own* stderr — not a per-session sink the consumer controls |
| `SendKeys` | writes raw bytes to the PTY but is documented "not intended for production drivers" |

The hard constraint: **no `claude` screen literal may appear in consuming pyrycode code** as a result of this surface — that is exactly what would break the consumer's `cmd/substrate-guard`. The API emits **opaque bytes** and offers nothing that invites parsing; all screen knowledge (anchors, spinners, modal text) stays inside tui-driver.

## API

```go
// Spawn-time opt-in (default false → zero behaviour change for non-attach sessions)
type SpawnOpts struct {
    // ...
    MirrorOutput bool
}

func (s *Session) MirrorOutput() <-chan []byte  // live receive-only raw-output stream; nil when not enabled
func (s *Session) AttachInput(p []byte) error   // production raw-input path; writes p verbatim to the PTY
```

Both are **additive**: no existing symbol changed signature. `SendKeys`, `RecordTo`, `MirrorStderr`, and the rolling buffer are untouched.

## Output direction — a third tap on the reader goroutine

`MirrorOutput()` returns a receive-only channel of opaque byte slices. Each received `[]byte` is an **independent copy of one PTY read chunk**, delivered verbatim. It is a tap on the **live PTY read** — *not* a reader over the rolling buffer — so no parse-able `io.Reader` over the rolling buffer crosses the boundary, and no removed seam is reintroduced.

The reader goroutine (`session.go`) already reads each PTY chunk into a reused 4 KB buffer, appends it to the rolling buffer, and tees it to the optional mirror sink (`RecordTo` / `MirrorStderr`). The raw-output stream is a **third tap on that same chunk**. Because `chunk` aliases the reused read buffer, the async consumer needs its own backing array — so this tap (and only this tap) copies the chunk before sending:

```go
if s.mirrorOut != nil {
    cp := make([]byte, n)
    copy(cp, chunk)
    select {
    case s.mirrorOut <- cp:
    default: // drop-newest — see Slow-consumer policy
    }
}
```

- **Opt-in at Spawn:** the channel is allocated in `Spawn` only when `SpawnOpts.MirrorOutput` is true. The hot loop's `nil` check is a plain read of an immutable field — no behaviour change and no per-chunk cost for every non-attach session.
- **`nil` when disabled:** `MirrorOutput()` returns `nil` when `SpawnOpts.MirrorOutput` was false. Ranging a `nil` channel blocks forever, so the contract is: **only call this when you set `SpawnOpts.MirrorOutput`.** Standard Go nil-channel convention.
- **One producer, one closer:** the reader goroutine is the sole sender and sole closer. No mutex, no atomics; send-on-closed is impossible (the sender closes on its own way out).

### Slow-consumer policy — drop-newest, non-blocking

The send is `select { case ch <- cp: default: }` — **non-blocking drop-newest** at a fixed channel capacity (`defaultMirrorOutputBuffer = 256`, ≤ ~1 MB queued at the 4 KB read size). This **deliberately diverges** from the block-on-full precedent of [`Events()`](../architecture/system-overview.md#key-signals) / `TailJSONL` (buffered cap 32):

Those channels are fed by *dedicated* goroutines; blocking one stalls only that single stream. Here the send is in the **shared core reader goroutine** — the one that feeds the rolling buffer that state detection depends on. A blocking send would:

1. stall `Buffer.Append` for every subsequent chunk → freeze idle/thinking/modal detection (the library's primary job), and
2. risk a **`Close` deadlock**: `Close` does `<-s.readerDone`, but a reader blocked forever on a send to an absent/slow attach consumer never reaches its `defer close(s.readerDone)`.

Dropping is the correct policy for a *mirror*: a dropped chunk is a transient screen glitch that `claude`'s next full repaint heals; it never stalls `claude`'s PTY writes, never freezes state detection, and never wedges shutdown. Because the policy is *drop* (not block), buffer depth maps to **dropped-frame frequency**, not producer-stall duration — a genuinely different tradeoff from the lossless `Events()` channel, which is why 256 (not 32) is the capacity. The const is internal (not a public knob, same posture as `defaultEventBuffer`); retune on evidence from the downstream `pyry attach` spike.

> **"Verbatim" means content-identical, not lossless.** When a chunk *is* delivered it is byte-for-byte the PTY read, untransformed. It does **not** promise zero drops under backpressure.

### Shutdown / lifecycle

The reader goroutine closes the channel on shutdown — either `Close` or natural process exit (which unblocks `ptmx.Read`); both route through the same reader-goroutine exit, so the channel closes exactly once. No separate `ctx` is introduced; the session lifecycle is the single shutdown authority.

The close is registered as `defer close(s.mirrorOut)` **after** `defer close(s.readerDone)`, so LIFO ordering closes `mirrorOut` *before* `readerDone`. Consequence (asserted by a test): a consumer that calls `Wait()` (which blocks on `readerDone`) and then drains the channel observes it already closed **on the reader-drained path**. Since [#170](../codebase/170.md), `Wait` bounds its reader-drain wait by `ShutdownGrace` (so a tool grandchild holding the PTY slave FD open after the leader exits can't hang it); on that **timeout** path `Wait` returns *before* `readerDone`/`mirrorOut` close, so a `Wait()`-then-drain may find the channel still open until `Close`. (`MirrorOutput`'s exported godoc still states this unconditionally — a tracked one-line follow-up from the #170 review; see [codebase/170.md](../codebase/170.md).) A consumer that enabled the stream but never drains it leaks nothing — sends drop once the buffer fills, and the channel is GC-eligible after `Close`.

## Input direction — `AttachInput`

`AttachInput(p []byte) error` writes `p` verbatim to the PTY via the same internal `writeRaw` funnel the typed keystroke methods use, and returns the first non-nil write error (e.g. the closed-file error after `Close`). No panic. It is a thin sibling of the typed-keystroke methods with a **production attach contract** that `SendKeys` lacks.

`SendKeys` is left exactly as-is — its "not intended for production drivers" doc and all its tests are unchanged. The two coexist for different consumers; `writeRaw` was **not** re-scoped or renamed (that would create edit fan-out for zero benefit).

Concurrent `AttachInput` + the reader goroutine are opposite directions on the PTY master (independently safe). Concurrent *writers* are serialized since [#171](../codebase/171.md): `writeRaw` — and the direct-writing prompt helpers (`WritePrompt`/`TypePrompt`/`ClearInputLine`) — all hold one session-level `writeMu` for the full duration of each logical write, so an `AttachInput` from the attach head can no longer land between two of a concurrent `TypePrompt`'s byte-writes and split the prompt. The tradeoff: `AttachInput` **blocks** for the whole span of a concurrent `TypePrompt` (payload × `PromptInterByteDelay` + `PromptCommitSettle`) — intended, since an attach keystroke injected mid-prompt would corrupt it. See [system-overview § Concurrency model](../architecture/system-overview.md#concurrency-model).

## The opaque-bytes-no-parse contract

Both doc comments state, in the library's existing register, that the bytes are **for byte-mirroring only** — the consumer must never inspect, match, or branch on their content. The API *shape* itself avoids inviting parsing: output is `<-chan []byte` (opaque chunks, deliberately not an `io.Reader`/`bufio.Scanner` that begs to be tokenized); input is `[]byte` (opaque, not a string or a typed keystroke).

### SECURITY

- **`MirrorOutput` carries secret-bearing content.** The stream carries *every byte `claude`'s terminal renders* — the prompt, `claude`'s output, and all tool output, which can include file contents and secrets (the same content `RecordTo`'s comment flags). The consumer owns the confidentiality of whatever transport it forwards these bytes to (local TTY, socket, or — per ADR 025 — a network link to a remote head). **Do not pipe them over an untrusted channel.**
- **The drop path is silent by design — never log chunk bytes.** A debug `log.Printf("dropped %q", cp)` would leak the secret-bearing content above into logs. The drop branch carries a code comment forbidding this.
- **`AttachInput` governs `claude`'s agency.** The bytes are interpreted by `claude`'s TUI under `claude`'s own permission model, not by a shell. Authenticating *who* may attach is the **consumer's** responsibility (ADR 025 mobile-head auth), not tui-driver's.

## How it plugs in

```go
sess, _ := tuidriver.Spawn(cmd, tuidriver.SpawnOpts{MirrorOutput: true})

// Output: forward raw screen bytes to the attached terminal, verbatim.
go func() {
    for chunk := range sess.MirrorOutput() { // closes on shutdown; range terminates
        _, _ = attachedTTY.Write(chunk)      // never inspect chunk — opaque bytes
    }
}()

// Input: forward the attached terminal's raw keystrokes back in.
if err := sess.AttachInput(rawKeystrokes); err != nil {
    // ... PTY write error (e.g. after Close)
}
```

## Limitations

- **Best-effort, not lossless.** Under backpressure the stream drops (drop-newest). Fine for a mirror; not a transcript. If you need a complete record, use [`RecordTo`](cast-recorder.md) (which is lossless to a file) in parallel — `MirrorOutput` composes with `RecordTo` / `MirrorStderr`; all sinks see the same bytes.
- **`nil` channel when disabled** — ranging it blocks forever. Only call `MirrorOutput()` when you set the opt-in.
- **No transport, no auth, no encryption.** tui-driver emits opaque bytes; confidentiality, attach authentication, and any remote-head transport crypto are the consumer's responsibility (ADR 025).
- **Buffer depth is a guess (256).** Sized for repaint bursts under a drop policy; expect to retune from the `pyry attach` spike. Internal const, so it changes without an API break.
- **Drop-newest, not drop-oldest.** Discards the *incoming* chunk when full (simple `select`/`default`). Drop-oldest (evict the stalest queued chunk to keep the mirror maximally live) is arguably better attach UX but is deferred until the spike shows freshness matters.

## Related

- [Session.Resize](session-resize.md) — the sibling sealed-surface seam that completes the attach surface with **geometry** (`(*Session).Resize(rows, cols)` sizes the hosted PTY). Same additive-no-raw-seam shape; together `MirrorOutput` (out) + `AttachInput` (in) + `Resize` (geometry) are everything a local `pyry attach` head needs from the sealed `Session`. Introduced by [#138](../codebase/138.md).
- [CastRecorder](cast-recorder.md) — the lossless, file-owned sibling sink; composes with `MirrorOutput`.
- [System overview § Concurrency model](../architecture/system-overview.md#concurrency-model) — the reader goroutine and its taps; the `Events()` block-on-full precedent this diverges from.
- [#136 — per-ticket notes](../codebase/136.md) — implementation summary, patterns, and lessons.
- Consumer-side **ADR 025 — mobile-remote-head interactive session** (`pyrycode/docs/knowledge/decisions/025-mobile-remote-head-interactive-session.md`, open risk #1) — the threat source and the attach-auth / transport owner for this seam.

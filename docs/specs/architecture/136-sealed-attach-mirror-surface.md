# Spec #136 — Sealed local-attach mirror surface: raw output channel + attach raw-input on `Session`

**Ticket:** [#136](https://github.com/pyrycode/tui-driver/issues/136) · **Size:** S · **Phase 5 / Phase 1 foundation** — mobile-remote-head (ADR 025, open risk #1).

## Files to read first

- `pkg/tuidriver/session.go:38-100` — `SpawnOpts` (where the opt-in field is added) + the `Session` struct (where the new internal channel field lives). Note the existing `RecordTo` / `MirrorStderr` doc-comment style — the new field's doc must match its register.
- `pkg/tuidriver/session.go:108-168` — `Spawn` + the **reader goroutine** (lines 149-165). This is the single hot loop the new tap hooks into, beside the existing `s.buffer.Append(chunk)` and `mirror.Write(chunk)`. Extract: the `chunk := buf[:n]` aliasing of the reused 4 KB `buf`, and the `defer close(s.readerDone)` pattern the channel close mirrors.
- `pkg/tuidriver/session.go:170-208` — `buildMirror`. Confirms the existing tee is a by-value sink (path/bool) that never escapes the package. The new stream is a **third tap on the same chunk**, but delivered as a channel rather than folded into this `io.Writer` (rationale in Design).
- `pkg/tuidriver/session.go:331-362` — `Close` (and `Wait` at 315-329). Extract: the shutdown order (SIGTERM → grace → SIGKILL → `s.pty.Close()` → `<-s.readerDone` → close `recCloser`). The reader-goroutine exit is what drives the new channel's close.
- `pkg/tuidriver/buffer.go:38-50` — `Buffer.Append`. **Confirms `Append` copies its argument** (`append(b.buf, p...)`). This is why the existing `chunk` aliasing is safe today, and why the new async channel path is the *only* consumer that needs its own copy before send.
- `pkg/tuidriver/keys.go:31-75` — `writeRaw` (the single internal PTY-write funnel) + `SendKeys` (the spike-only escape hatch). The new `AttachInput` is a sibling thin method over `writeRaw` with a *production* contract; `SendKeys` stays byte-for-byte unchanged.
- `pkg/tuidriver/events.go:7-13,142-150` — `defaultEventBuffer = 32` and the `Events()` buffered-channel + `mergeEvents` precedent. Read for the channel-buffer/backpressure shape — but note (Design § Concurrency) the slow-consumer policy here is deliberately *different* because the producer is the shared reader goroutine, not a dedicated merge goroutine.
- `pkg/tuidriver/recordto_test.go` — the end-to-end test idiom this spec's tests mirror: `exec.Command("echo", …)` / `cat` under a PTY, `runtime.GOOS == "windows"` skip, `Spawn` → `Wait`/`Close`, table-driven subtests. Reuse this shape; do not invent a new harness.

## Context

The daemon supervisor must stop hosting `claude` with a bare PTY (`pty.Start` + `io.Copy`) and host it through a tui-driver `Session`, to unlock the sealed `DeliverPrompt` reliability path and the `Events()` stream. But the local `pyry attach` head needs the hosted process's **raw screen bytes** to mirror the terminal, and a **production raw-input path** to forward the attached terminal's keystrokes back in.

The v1.0.0 seal removed every raw seam (`Session.Buffer`, `Session.PTY`, `SpawnOpts.Mirror`, the public `Write`). What remains gives no live raw-byte *stream* (`Snapshot` is point-in-time and parse-oriented; `RecordTo` writes a file tui-driver owns; `MirrorStderr` tees to the process's own stderr) and no production raw-input path (`SendKeys` is documented "not for production drivers").

This is the load-bearing seam for the whole mobile-remote-head approach. The hard constraint: **no claude screen literal may appear in consuming pyrycode code** as a result of this surface. The API must emit opaque bytes and offer nothing that invites parsing. All screen knowledge (anchors, spinners, modal text) stays inside tui-driver.

## Design

Two additive, opt-in surfaces. No existing symbol changes signature; `SendKeys`, `RecordTo`, `MirrorStderr`, and the rolling buffer are untouched.

### Output direction — a third tap on the reader goroutine

The reader goroutine (`session.go:149-165`) already reads each PTY chunk into a reused 4 KB `buf`, appends it to the rolling buffer, and tees it to the optional `mirror` sink. The raw-output stream is a **third tap on that same `chunk`**, delivered as a receive-only channel of opaque byte slices.

Crucially, the stream is a tap on the **live PTY read**, *not* a reader over the rolling buffer. The bytes are copied straight from the read buffer before they enter the parse-oriented `Buffer`. This satisfies the AC literally: no parse-able `io.Reader` over the rolling buffer crosses the boundary, and no removed seam (`Buffer`/`PTY`/`SpawnOpts.Mirror`) is reintroduced.

**Opt-in at Spawn time** (mirrors the by-value `buildMirror` toggle pattern; keeps the hot path identical for every non-attach session):

| Symbol | Kind | Contract |
|---|---|---|
| `SpawnOpts.MirrorOutput bool` | new field | When true, `Spawn` allocates the buffered stream channel before the reader goroutine starts. Default false → zero behaviour change, hot loop unchanged. |
| `(*Session).MirrorOutput() <-chan []byte` | new method | Returns the receive-only opaque-byte stream. Returns **`nil`** when `SpawnOpts.MirrorOutput` was false. Each received `[]byte` is an independent copy of one PTY read chunk, verbatim. |
| internal `s.mirrorOut chan []byte` | new field | The channel. Producer = reader goroutine (sole sender + sole closer). |
| internal `defaultMirrorOutputBuffer` | new const | Channel capacity. **256** (see Concurrency for why this diverges from `Events()`' 32). |

Reader-goroutine change (sketch — additive, after the existing `mirror.Write`):

```
if s.mirrorOut != nil {
    cp := make([]byte, n)   // chunk aliases the reused buf; the async
    copy(cp, chunk)         // consumer needs its own backing array
    select {
    case s.mirrorOut <- cp: // delivered
    default:                // drop: consumer slower than the PTY (see policy)
    }
}
```

Channel lifecycle is owned entirely by the reader goroutine — the canonical "producer closes its own channel" pattern. Registered as `defer close(s.mirrorOut)` (guarded by the nil check) **after** `defer close(s.readerDone)`, so LIFO ordering closes `mirrorOut` *before* `readerDone`. Consequence (asserted by a test): a consumer that calls `Wait()` and then drains the channel always observes it closed. No separate `ctx` is introduced — the session lifecycle (`Close`, or natural process exit unblocking `ptmx.Read`) is the single shutdown authority. Both paths route through the same reader-goroutine exit, which closes the channel exactly once.

### Input direction — a production raw-input method

| Symbol | Kind | Contract |
|---|---|---|
| `(*Session).AttachInput(p []byte) error` | new method | Writes `p` verbatim to the PTY via the existing internal `writeRaw`. Returns the first non-nil write error (e.g. after `Close`, when the PTY is closed). |

`AttachInput` is a thin sibling of the existing typed-keystroke methods over `writeRaw` — functionally a single PTY write, but with a **production attach contract** that `SendKeys` lacks. `SendKeys` stays exactly as-is (its "not for production drivers" doc and all its tests unchanged); the two coexist for different consumers. No re-scoping or rename of `writeRaw`/`SendKeys` — that would create edit fan-out for zero benefit.

### The opaque-bytes-no-parse contract (both directions)

Both doc comments must state, in the library's existing register, that the bytes are **for byte-mirroring only** — the consumer must never inspect, match, or branch on their content; all screen knowledge stays inside tui-driver. The API shape itself avoids inviting parsing: output is `<-chan []byte` (opaque chunks, not an `io.Reader`/`bufio.Scanner` that begs to be tokenized), input is `[]byte` (opaque, not a string or a typed keystroke). Cross-reference the seal rationale the way `RecordTo`'s comment does.

**Required `SECURITY:` note on `MirrorOutput` (mandatory, not optional).** The stream carries *every byte claude's terminal renders* — the prompt, claude's output, and all tool output, which can include file contents and secrets — exactly the content `RecordTo`'s comment already flags (`session.go:48-52`). `MirrorOutput`'s doc comment MUST carry an equivalent `SECURITY:` paragraph: these are sensitive bytes; the consumer owns the confidentiality of whatever transport it forwards them to (local TTY, socket, or — per ADR 025 — a network link to a remote head). Without this note a consumer may treat "screen bytes" as innocuous and pipe them over an untrusted channel. The same warning is not needed on `AttachInput` (it carries keystrokes *into* the session, not secrets out), but its doc should note that input governs claude's agency and the consumer is responsible for authenticating *who* may attach.

**Chunk bytes must never be logged.** The drop path is silent by design. A developer must not add a debug `log.Printf("dropped %q", cp)` (or similar) anywhere on the output path — it would leak the secret-bearing content above into logs. State this in the implementation as a code comment on the drop branch.

## Concurrency model

- **One new channel, one producer, one closer.** The reader goroutine is the only sender to `s.mirrorOut` and the only thing that closes it. No mutex, no atomics — the channel is allocated in `Spawn` before any goroutine starts, and the nil check in the hot loop is a plain field read on an immutable field. No send-on-closed is possible (the sender closes on its own way out).
- **Slow-consumer policy: non-blocking drop-newest** (`select { case ch <- cp: default: }`). This is the central design decision and it **deliberately diverges from the `Events()`/`TailJSONL()` block-on-full precedent.** Those channels are fed by *dedicated* goroutines (`mergeEvents`, `tailJSONLLoop`); blocking them stalls only that one stream. Here the send is in the **shared core reader goroutine** — the goroutine that feeds the rolling buffer that state detection depends on. A blocking send would:
  1. stall `Buffer.Append` for every subsequent chunk → freeze idle/thinking/modal detection (the library's primary job), and
  2. risk a **`Close` deadlock**: `Close` does `<-s.readerDone`, but the reader, blocked forever on a send to an absent/slow attach consumer, never reaches its `defer close(s.readerDone)`.

  Dropping is the correct policy for a *mirror*: a dropped chunk is a transient screen glitch that claude's next full repaint heals; it never stalls claude's PTY writes, never freezes state detection, and never wedges shutdown. The byte mirror is best-effort by nature.
- **Buffer size 256** (not `Events()`' 32). Because the policy is *drop* (not block), buffer depth maps directly to dropped-frame frequency, not to producer-stall duration — a genuinely different tradeoff from the lossless `Events()` channel. 256 chunks (≤ ~1 MB at the 4 KB read size) absorbs multi-frame repaint bursts before any drop. Internal const, not a public knob (same posture as `defaultEventBuffer`); retune on evidence from the downstream attach spike.
- **"Verbatim" means content-identical, not lossless.** The AC's "output bytes arrive verbatim" is satisfied by the copy: when a chunk *is* delivered it is byte-for-byte the PTY read, untransformed. It does not promise zero drops under backpressure — the slow-consumer policy is the explicitly separate AC.

## Error handling

- **Output:** no error channel. The stream's only failure mode is drop-under-backpressure (silent by policy) and close-on-shutdown (signalled by channel close). A consumer that enabled `MirrorOutput` but never drains the channel leaks nothing: sends drop once the buffer fills, and the channel is GC-eligible after `Close`.
- **`MirrorOutput()` returning `nil`:** documented. Ranging a nil channel blocks forever — so the contract is "only call this when you set `SpawnOpts.MirrorOutput`." This is the standard Go nil-channel convention; the doc comment states it plainly.
- **Input:** `AttachInput` returns the underlying write error verbatim (e.g. writing after `Close` returns the closed-file error). No panic. Idempotent-safe to call concurrently with the reader goroutine (writes go to the PTY master; reads come from it — independent directions).

## Testing strategy

New file `pkg/tuidriver/mirror_test.go`, table/subtest shape mirroring `recordto_test.go` (`exec.Command`, `runtime.GOOS == "windows"` skip). Test-first: RED before GREEN. Scenarios (developer writes bodies in-idiom):

- **Opt-in seal:** `Spawn(cmd, SpawnOpts{})` → `s.MirrorOutput()` returns `nil`. Guards "no behaviour change unless enabled."
- **Verbatim output delivery:** `Spawn(echo "<known-bytes>", SpawnOpts{MirrorOutput: true})`; drain the channel concatenating chunks until it closes; assert the concatenation contains the exact emitted bytes, untransformed. (Corruption from a missing copy / aliasing would surface here as torn content.)
- **Multi-chunk ordering + independence:** a process emitting several distinct chunks; assert all content arrives, in order, across multiple receives (exercises the per-chunk copy across reused-`buf` iterations).
- **`AttachInput` reaches the PTY:** `Spawn(cat, SpawnOpts{MirrorOutput: true})`; `AttachInput([]byte("ping\r"))`; read the mirror channel until the echoed `ping` appears; assert; then `Close`.
- **Clean shutdown / drain:** `Spawn(…, MirrorOutput: true)`; `Close`; assert the channel closes (range terminates) with no panic; assert a `Wait()`-then-drain observes the channel already closed (the LIFO close-ordering contract).
- **Slow/absent consumer does not wedge shutdown:** `Spawn(…, MirrorOutput: true)`, never read the channel, let the process emit well past `defaultMirrorOutputBuffer` chunks, then `Close` — assert `Close` returns promptly (proves drop-policy prevents the reader-blocked-on-send → `Close` deadlock). This is the key safety test.

Gate: `make e2e` + `make check` green. Existing `recordto_test.go`, `session_test.go`, `keys_test.go` unchanged and still passing.

## Open questions

- **Buffer depth (256).** A guess sized for repaint bursts under a drop policy. The downstream `pyry attach` spike is the real workload; expect to retune the const there. Left internal precisely so this can change without an API break.
- **Drop-newest vs drop-oldest.** This spec specifies drop-*newest* (discard the incoming chunk when full) for implementation simplicity (`select`/`default`). Drop-*oldest* (evict the staleest queued chunk to keep the mirror maximally live) is arguably better attach UX but needs a non-blocking drain-then-send and is not justified before the spike shows freshness matters. Deferred.
- **Naming.** `MirrorOutput` (field + accessor) pairs with the existing `MirrorStderr`/`RecordTo` mirror family; `AttachInput` names the input direction by use case. If the developer/reviewer finds a cleaner pair, the names are local to this PR (no external consumer yet) — but the *contracts* above are fixed.

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] **Addressed in spec.** Two boundaries. (1) *Output* — the `MirrorOutput` channel carries secret-bearing screen content out of the seal; the design now mandates a `SECURITY:` doc note mirroring `RecordTo`'s (`session.go:48-52`) and assigns transport confidentiality to the consumer (Design § opaque-bytes-no-parse contract). (2) *Input* — `AttachInput` forwards attached-terminal keystrokes verbatim into claude's PTY; bytes are interpreted by claude's TUI under claude's own permission model, not by a shell. Authenticating *who* may attach is OUT OF SCOPE for tui-driver — it belongs to the consumer's attach-auth layer (ADR 025 mobile-head auth). The spec names this rather than silently inheriting it.
- [Tokens, secrets, credentials] No tokens/credentials generated or stored by this design. Secrets only *transit* the output channel (covered under Trust boundaries). No lifecycle to address.
- [File operations] N/A — the design opens no files and constructs no paths. `RecordTo`'s file handling is untouched.
- [Subprocess / external command execution] N/A — no new `exec`, no `sh -c`. `AttachInput` writes to the *already-spawned* PTY; it does not execute anything. claude's agency over those bytes is governed by claude's permission model + consumer attach-auth (out of scope, as above).
- [Cryptographic primitives] N/A — no RNG, no secret comparison, no crypto introduced. Transport crypto for a remote head is the consumer's responsibility (ADR 025).
- [Network & I/O] **No findings — design is the mitigation.** Resource exhaustion by a slow/absent/hostile output consumer is bounded by construction: a fixed-capacity channel (`defaultMirrorOutputBuffer = 256`, ≤ ~1 MB queued) with a non-blocking drop-newest send. No unbounded growth, no reader stall, no `Close` deadlock. `AttachInput` takes no size cap by design — `p` is the caller's own bytes written synchronously by the caller's goroutine into the kernel-bounded PTY; a huge `p` blocks only the caller, not the library.
- [Error messages, logs, telemetry] **Addressed in spec.** The drop path is silent and the spec now forbids logging chunk bytes (a debug log would leak the secret-bearing content). `AttachInput`'s returned error is the underlying PTY write error (e.g. closed-file) — no secret content, no sensitive path.
- [Concurrency] No findings. **No new goroutine** — the output stream reuses the existing reader goroutine, whose exit (PTY close / process exit) is unchanged. The reader is the *sole sender and sole closer* of `mirrorOut` (close in its own `defer`, after the loop exits) → send-on-closed is impossible. The non-blocking send guarantees the reader never blocks → `Close`'s `<-s.readerDone` always progresses (no deadlock). `s.mirrorOut` is set once in `Spawn` before any goroutine starts and only read thereafter → no data race, no lock needed in the accessor or hot loop. Concurrent `AttachInput` + reader are opposite directions on the `*os.File` (independently safe); concurrent *writers* through `writeRaw` could interleave bytes, but that is a pre-existing property of every typed keystroke method, unchanged here, and an attach head has a single input source — OUT OF SCOPE.
- [Threat model alignment] Aligned with ADR 025 (`pyrycode/docs/knowledge/decisions/025-mobile-remote-head-interactive-session.md`) open risk #1 — this *is* that seam. In-scope mitigations: secret-content exposure (SECURITY doc note + consumer-owned transport), resource exhaustion (bounded drop policy). Out-of-scope, named with owner: attach authentication/authorization (consumer), transport encryption (consumer). No `docs/threat-model.md` exists in this repo; ADR 025 is the governing threat source.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-06-07

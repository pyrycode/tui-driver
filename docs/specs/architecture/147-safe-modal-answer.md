# Spec: safe modal-answer — abstract choice → per-class keystroke + confirm dismissal

**Ticket:** [#147](https://github.com/pyrycode/tui-driver/issues/147)
**Size:** S
**Status:** ready for development
**Labels:** `security-sensitive` (security-review pass appended below — verdict PASS)

Phase 3 (epic pyrycode#597) screen-side modal **writer** — the counterpart to
the modal **reader** shipped in [#146](146-parse-permission-trust-modal.md)
(`ParseModalContent` → `ModalContent` / `ModalOption`). The reader projects a
permission / trust-folder modal into a neutral typed value (class + ordered
options, each carrying claude's rendered 1-based `Index`); this child sends the
keystroke that selects a chosen option **by its `Index`** and verifies the modal
dismissed. Purely additive — one new production file, no existing file touched.

## Files to read first

Turn-1 reading list. Load these before writing code; the design below mirrors
`deliver.go`'s deliver→re-read→confirm structure and its injectable seams almost
line-for-line.

- `pkg/tuidriver/deliver.go:94-171` — `DeliverPrompt` (the `Session` method that
  wires real session methods into a `deps` struct) + `deliverPrompt` (the
  unexported pure driver the tests call with fakes) + `deliverDeps`. **This is
  the structural template to mirror exactly**: method wires seams → unexported
  driver loops over seams → tests inject fakes, no PTY.
- `pkg/tuidriver/deliver.go:179-201` — `promptDidCommit`: the
  `time.NewTimer` + `time.NewTicker` + `select { ctx.Done / deadline / tick }`
  poll loop. The dismissal re-read poll (`modalDismissed`) mirrors this exactly,
  swapping the commit predicate for `DetectModalClass(snap) != class`.
- `pkg/tuidriver/keys.go:31-75` — the **sealed typed-keystroke surface**:
  `writeRaw` (the single private PTY-write path), `Answer(choice string)`
  (sends `choice + "\r"`), `AcceptTrust` (hardcoded `1\r`), `Navigate`,
  `SendEsc`, and `SendKeys` (the spike-only escape hatch the dispatch MUST NOT
  use). Note: the public `Write` seam is **gone** — there is no raw-byte write to
  call. The number-select dispatch uses `Answer(strconv.Itoa(index))`; for
  index 1 this is byte-identical to `AcceptTrust`'s `1\r`.
- `pkg/tuidriver/modal.go:84-131` — `DetectModalClass` + the `ModalClass` enum
  (`ModalClassPermission`, `ModalClassTrustFolder`, …). The confirm re-read calls
  `DetectModalClass`; the dispatch switches on `ModalClass`. The two supported
  classes are exactly the two `ParseModalContent` returns today.
- `pkg/tuidriver/permission.go:11-88` — `ModalContent` / `ModalOption` /
  `ParseModalContent`: the reader this writes the counterpart to. **`ModalOption.Index`
  is the security-relevant selector the answer routes on; `Label` is advisory,
  prompt-/tool-influenceable, never routed on** (the doc-comments state this).
- `pkg/tuidriver/deliver_test.go:69-216` — the injectable-seam table-test idiom:
  fake `deps` closures recording calls + scripting per-attempt outcomes, the
  `captureLogger` helper, and `TestPromptDidCommit:217-257` (the poll tested
  against a real `Session` buffer with a short timeout). The answer tests mirror
  both shapes.
- `pkg/tuidriver/session.go:371` — `func (s *Session) Snapshot() []byte`: the
  re-read source the confirm poll reads.
- `pkg/tuidriver/testdata/permission-snapshot.bin`,
  `pkg/tuidriver/testdata/trust-folder-snapshot.bin` — #146's committed fixtures.
  **Reuse them as the "modal still present" re-read snapshot — no new fixtures
  are needed.** A dismissed re-read is plain idle bytes.
- `docs/knowledge/codebase/146.md` § Patterns / Follow-ups — the
  **Index-not-Label** security contract and the **"keep a per-class switch seam;
  do NOT pre-build handlers for classes that have no fixtured reader yet"**
  directive this spec implements.

## Context

Permission and trust-folder prompts have **zero JSONL footprint** — they live
only on the rendered PTY screen ([#146](146-parse-permission-trust-modal.md)
§ Context) — so the substrate seal makes tui-driver the sole owner of claude's
per-class keystroke knowledge. The daemon (mobile-remote-head) parses a modal
via `ParseModalContent`, serializes the neutral struct to the phone, and the
operator picks an option. The daemon then holds **the modal's class + the chosen
`Index`** — nothing more — and needs tui-driver to (a) send the keystroke that
selects that option for that class, using only the sealed keystroke surface, and
(b) confirm the modal dismissed so the daemon learns immediately whether the
answer landed (retry/degrade on failure rather than assume success).

The permission *decision* (which option, who may answer, timeout/audit) stays in
the consumer (ADR 025 / epic pyrycode#597). tui-driver owns only the
screen↔keystroke mechanics: abstract choice in → correct keystroke out →
dismissal confirmed.

## Design

### Package structure

One new production file, `pkg/tuidriver/answer.go`, plus its test
`pkg/tuidriver/answer_test.go`. **No new fixtures** (reuses #146's two `.bin`
fixtures). Purely additive — no existing file changes, no exported symbol
renamed, no consumer call site touched. `answer.go` sits beside `deliver.go`
(its closest structural sibling) and `permission.go` (the reader it mirrors).

### Public surface (the daemon's call shape)

```go
// AnswerModalOpts configures Session.AnswerModal.
type AnswerModalOpts struct {
    // Class is the modal's class as returned by ParseModalContent /
    // DetectModalClass. It selects the per-class keystroke dispatch and is the
    // class the dismissal re-read confirms gone. Only ModalClassPermission and
    // ModalClassTrustFolder are supported today.
    Class ModalClass

    // Choice is the chosen option's Index — claude's rendered 1-based number
    // (ModalOption.Index). The keystroke selects this option. Routing on Index,
    // never the prompt-/tool-influenceable Label, is the #146 security contract.
    Choice int

    // ConfirmTimeout bounds the dismissal re-read poll. 0 picks
    // DefaultAnswerConfirmTimeout.
    ConfirmTimeout time.Duration
}

// AnswerModal sends the keystroke that selects opts.Choice for opts.Class's
// modal, then re-reads the screen to confirm the modal dismissed. Returns nil
// once the modal is no longer detected; returns an error if the keystroke
// could not be sent, the class is unsupported, or the modal is still present
// after ConfirmTimeout (so the caller can retry or degrade rather than assume
// success). Mirrors DeliverPrompt's deliver→re-read→confirm structure.
func (s *Session) AnswerModal(ctx context.Context, opts AnswerModalOpts) error
```

`AnswerModalOpts` mirrors `DeliverOpts`: a struct (not positional args) for the
package idiom and forward-compat (a future class may add a field). It carries
**Class + Choice only** — not the full `*ModalContent`: the daemon sends a
neutral JSON to the phone and gets back an index, so by answer time it holds the
class it parsed plus the returned index, nothing more. No `Label`, no command
detail, no screen text crosses this API.

### Timing constants

```go
const (
    // DefaultAnswerConfirmTimeout bounds the dismissal re-read when
    // AnswerModalOpts.ConfirmTimeout is 0. claude re-renders within a few
    // hundred ms of a committing keystroke; 2s is generous headroom.
    DefaultAnswerConfirmTimeout = 2 * time.Second
    // answerConfirmPoll is the re-read cadence (unexported; cf. promptCommitPoll).
    answerConfirmPoll = 150 * time.Millisecond
)
```

### Injectable seams (mirrors `deliverDeps`)

```go
// answerDeps are the seams answerModal drives. AnswerModal wires the real
// session methods; tests inject fakes to script the keystroke + dismissal
// outcome without a live PTY. Mirrors deliverDeps.
type answerDeps struct {
    answer    func(choice string) error      // → s.Answer (number-select keystroke)
    dismissed func(timeout time.Duration) bool // → modalDismissed over s.Snapshot
}
```

`AnswerModal` wires `answer: s.Answer` and
`dismissed: func(t) bool { return modalDismissed(ctx, opts.Class, s.Snapshot, t) }`
— byte-for-byte the `DeliverPrompt` → `deliverPrompt` wiring shape (`didCommit`
is likewise a closure over a poll helper). `ctx` is captured in the `dismissed`
closure, so the unexported driver takes no `ctx` (same as `deliverPrompt`).

Today only `answer` is needed (both supported classes are number-select). A
future non-number-select class adds its primitive seam (e.g. `navigate`,
`sendEsc`) **when it is fixtured** — not before (see § Per-class dispatch).

### Unexported driver — contract

```go
// answerModal dispatches the per-class keystroke for opts then confirms the
// modal dismissed via deps.dismissed. The pure driver the tests call directly.
func answerModal(opts AnswerModalOpts, deps answerDeps) error
```

Behaviour (the binding contract):
1. **Dispatch the keystroke per class** (the per-class switch, § below). On an
   unsupported class, return an error **without sending anything**. On a
   supported class with `Choice < 1`, return an error without sending (Index is
   1-based; never send a `"0\r"` / `"-1\r"` to a live claude). On a `deps.answer`
   failure, wrap and return.
2. **Confirm by re-read.** `if deps.dismissed(timeout) { return nil }`.
3. **Still present → error.** Return a `… modal still present after answering
   choice N` error so the caller retries or degrades.

`ConfirmTimeout` defaulting (`<= 0 → DefaultAnswerConfirmTimeout`) happens once
at the top, same as `deliverPrompt`'s timeout/attempts defaulting.

### Per-class dispatch (the seam #146 mandates)

A single switch on `opts.Class`. Both classes `ParseModalContent` returns today
are **number-select**, so both map to the number-select keystroke
`Answer(strconv.Itoa(Choice))` — for `Choice == 1` this is byte-identical to
`AcceptTrust`'s `1\r`, and it honours `Choice == 2` (decline) which the hardcoded
`AcceptTrust` cannot. The switch IS the extension seam:

```go
switch opts.Class {
case ModalClassPermission, ModalClassTrustFolder:
    // Number-select strategy: the chosen Index IS the keystroke. A future
    // non-number-select class (y/n, arrow+enter) adds its own case here with a
    // different keystroke strategy (and its seam to answerDeps) — but only once
    // it has a fixtured reader. Do NOT pre-build unfixtured classes (#146
    // follow-up: destructive / plan-confirm deferred until fixtured).
    if opts.Choice < 1 { /* invalid-choice error */ }
    if err := deps.answer(strconv.Itoa(opts.Choice)); err != nil { /* wrap */ }
default:
    // unsupported-class error — send nothing.
}
```

Combining the two supported classes in one `case` is the elegant expression of
"both use the number-select strategy"; the seam (add a `case`) is preserved and
duplicating two identical arms would be needless. The comment names the
extension point so the next class slots in without rediscovery.

### Confirm-by-re-read poll (mirrors `promptDidCommit`)

```go
// modalDismissed polls the screen until the answered modal class is no longer
// detected (the dismissal signal), the timeout elapses, or ctx is cancelled.
// It only observes — never writes. snapshot is the injectable re-read source.
func modalDismissed(ctx context.Context, class ModalClass, snapshot func() []byte, timeout time.Duration) bool
```

Loop body mirrors `promptDidCommit` exactly: at the top, if
`DetectModalClass(snapshot()) != class` → return `true` (dismissed); else
`select` on `ctx.Done()` / `deadline.C` / `tick`. The dismissal signal is
**class-level, not content-level**: `DetectModalClass` reads claude's structural
anchors (cheap, no `Render`), not the prompt-/tool-influenceable option text, so
a hostile screen cannot spoof "dismissed." Using `DetectModalClass` (not
`ParseModalContent`) is sufficient and cheaper — we only need "is this class
still the active modal," not its parsed content.

`DetectModalClass(snap) != class` treats **any** non-matching screen as
dismissed — idle, or a *different* modal that popped after the answer landed.
That is correct: the class the operator answered is gone. The daemon re-reads
and handles any new modal as a fresh parse→answer cycle. The one residual gap
(two consecutive *same-class* modals) is documented under § Error handling and
§ Open questions.

## Concurrency model

None of its own. `AnswerModal` runs on the caller's goroutine: it issues one
keystroke write then polls `Snapshot()` (a lock-guarded buffer read, like
`DeliverPrompt`) on a timer until dismissal or timeout. No goroutines spawned,
no channels owned, no shared state introduced. `ctx` cancellation ends the
confirm poll promptly (the `select` honours `ctx.Done()`), identical to
`promptDidCommit`. The keystroke funnels through the existing `writeRaw` path
(`Answer` → `writeRaw` → `s.pty.Write`), whose concurrency posture is unchanged.

## Error handling

Every failure is a returned `error` (the daemon's retry/degrade signal); there
is no panic path. `fmt.Errorf("tuidriver: AnswerModal: …")` prefix, matching the
package idiom:

- **Unsupported class** — `Class` not permission/trust-folder → error,
  **nothing sent**. Guards against a number-select keystroke reaching a future
  arrow-select modal.
- **Invalid choice** — supported class but `Choice < 1` → error, **nothing
  sent** (Index is 1-based; a `0`/negative selection is meaningless).
- **Keystroke send failure** — `deps.answer` returns a PTY write error (e.g.
  `os.ErrClosed` after `Close`) → wrapped and returned.
- **Modal still present** — keystroke sent but `DetectModalClass` still reports
  `opts.Class` after `ConfirmTimeout` → error naming the class + choice. This is
  the AC2 contract: a still-present modal is an error, not silent success. Covers
  a rejected/ignored keystroke (wrong index claude declines, transient
  un-commit) deterministically.
- **`ctx` cancelled during confirm** — `modalDismissed` returns `false` →
  surfaces as the "still present" error (the keystroke may have landed; the
  caller re-reads). Matches `promptDidCommit`'s false-negative-is-cheap posture.

**Out of scope (consumer-owned, ADR 025):** answer-keystroke *staleness* — if
the on-screen modal changed between the daemon's parse and the answer, the
keystroke targets whatever is on screen now. tui-driver's contract is honest and
narrow: "given the class you tell me, I send that class's keystroke and confirm
that class is gone." Freshness/decision integrity (the operator answered the
modal they actually saw) is the daemon's permission-flow concern, not the
screen↔keystroke mechanic. Named in the security review below.

## Testing strategy

New `answer_test.go`, no new fixtures (reuses #146's two `.bin` files for the
"still present" re-read; idle bytes for "dismissed"). Two test surfaces mirror
`deliver_test.go`'s split (`TestDeliverPrompt_*` boolean seams +
`TestPromptDidCommit` real-buffer poll). Scenarios (developer writes them in the
package's table/subtest idiom; the assertions below are the binding contract):

**`answerModal` driver — fake `answer` + `dismissed` seams (no PTY):**
- **Permission, dismissed (success).** `{Class: permission, Choice: 2}`,
  `dismissed → true`: returns nil; `answer` was called once with `"2"`.
- **Permission, still present (error).** same opts, `dismissed → false`:
  returns an error containing "still present"; `answer` called once with `"2"`.
- **Trust-folder, dismissed (success).** `{Class: trust-folder, Choice: 1}`,
  `dismissed → true`: returns nil; `answer` called with `"1"` (== `AcceptTrust`).
- **Trust-folder, still present (error).** `dismissed → false`: error.
- **Unsupported class.** `{Class: mcp (or slash-picker), Choice: 1}`: returns an
  error containing "unsupported"; **`answer` was NOT called** (a fake that
  `t.Fatal`s if invoked).
- **Invalid choice.** `{Class: permission, Choice: 0}`: returns an error
  containing "1-based"; **`answer` NOT called**.
- **Send failure.** `answer → os.ErrClosed`: returns an error wrapping the write
  failure (contains "send keystroke"); `dismissed` NOT consulted.

**`modalDismissed` poll — inject the snapshot sequence (AC3 "no live PTY"):**
- **Dismissed immediately.** `snapshot → idle bytes` (`[]byte("idle TUI")`),
  `class = permission`: returns `true` fast (no wait) — `DetectModalClass` =
  Unknown ≠ permission.
- **Still present → times out.** `snapshot →` the `permission-snapshot.bin`
  fixture every call, short `timeout` (~60ms, < `answerConfirmPoll`): returns
  `false`, and honours the timeout (elapsed ≈ timeout), like
  `TestPromptDidCommit`'s timeout subtest.
- **Different modal counts as dismissed.** `snapshot →` the
  `trust-folder-snapshot.bin` fixture, `class = permission`: returns `true`
  (trust ≠ permission — the answered class is gone).
- **`ctx` cancelled.** pre-cancelled `ctx`, `snapshot →` the permission fixture:
  returns `false` promptly.

These cover each supported class's answer-and-confirm path in both the dismissed
(success) and still-present (error) outcomes (AC3), plus the unsupported-class /
invalid-choice / send-error reject branches, with no live PTY.

## Open questions

1. **Two consecutive same-class modals.** If answering permission modal A
   immediately surfaces permission modal B, `DetectModalClass` still reports
   `permission`, so `modalDismissed` reads "still present" and `AnswerModal`
   returns the still-present error even though A was dismissed. This is a
   conservative false-negative: the daemon retries/degrades, re-reads, sees the
   (new) modal, and the operator answers again — never a silent wrong-grant.
   Content-diffing A vs B to distinguish them would route on the
   prompt-/tool-influenceable screen text the security model forbids trusting,
   so it is deliberately not done. Tighten only if observed (evidence-based).
2. **Number-select is the only strategy shipped.** Both fixtured classes are
   number-select. The y/n and arrow+enter strategies (and their `answerDeps`
   seams) are deferred until a class that needs them is fixtured (#146 follow-up
   defers destructive / plan-confirm). The switch's `default` arm makes that
   extension a single added `case`, not a refactor.
3. **`AcceptTrust` is now subsumed.** `AnswerModal({trust-folder, 1})` sends the
   same `1\r` as `AcceptTrust` while also honouring `Choice == 2`. `AcceptTrust`
   stays as the spike convenience; no consumer call site is migrated by this
   ticket (out of scope — additive only).

## Security review

**Verdict:** PASS

This ticket **sends keystrokes that select a permission/trust grant** on a live
claude — a more security-relevant operation than #146's read-only parse — so the
audit weighed grant-redirection, keystroke injection, and screen-literal leakage
most heavily.

**Findings:**

- **[Trust boundaries]** SHOULD FIX (mitigation built into the design). One
  explicit boundary: the operator's `Choice` (relayed by the daemon) → the
  keystroke `Answer(strconv.Itoa(Choice))` → claude's TUI, which applies the
  grant. It is a single named function (`AnswerModal` / `answerModal`), not
  scattered. The grant-redirection threat (a hostile prompt makes claude render
  a misleading option label) is mitigated **structurally**, exactly as #146: the
  answer routes on `Choice` = `ModalOption.Index` = claude's own rendered number,
  **never** the prompt-/tool-influenceable `Label`. A spoofed label can mislead a
  human but cannot change which grant a given number selects. The `AnswerModalOpts`
  doc-comment marks `Choice` as the Index and forbids `Label` routing.
- **[Trust boundaries — staleness]** OUT OF SCOPE (consumer-owned, ADR 025). If
  the on-screen modal changed between the daemon's parse and the answer, the
  keystroke targets the current screen. tui-driver's contract is deliberately
  narrow ("given the class you name, I send that class's keystroke and confirm
  that class is gone"); the confirm-by-re-read is the deterministic backstop for
  the common "answer didn't land" case. Ensuring the operator answered the modal
  they actually saw (freshness / replay / who-may-answer) is the daemon's
  permission-flow concern — epic pyrycode#597 / ADR 025 — not the
  screen↔keystroke mechanic. Named in the ticket body, § Error handling, and here.
- **[Tokens / secrets]** No findings — and a deliberate hardening. No token, key,
  or credential is generated, stored, or compared. Error messages carry **only**
  the `ModalClass` enum value and the integer `Choice` — never the option
  `Label`, the prompt text, the command detail, or any screen literal. Nothing
  secret crosses the error boundary (mirrors #146's "action-detail lines not
  captured" posture).
- **[File operations]** N/A — the production path reads no files; `modalDismissed`
  reads the in-memory `Snapshot()` buffer. The test loads #146's committed
  fixtures via `os.ReadFile(filepath.Join("testdata", …))` with fixed in-repo
  literals — no caller-controlled path, no traversal, no TOCTOU.
- **[Subprocess / external command]** N/A — spawns and executes nothing; claude
  is spawned elsewhere. Keystroke-injection check: `Choice` is an `int`,
  `strconv.Itoa` emits only `[-0-9]`, and the `Choice < 1` guard rejects
  non-positive, so the bytes written are `[1-9][0-9]*` + `\r` — no control or
  escape bytes, and they reach claude's TUI as a menu keystroke, never a shell.
  No injection surface.
- **[Cryptographic primitives]** N/A — no randomness, no crypto; the only
  comparison is `DetectModalClass(snap) != class`, a non-secret enum compare (no
  constant-time requirement).
- **[Network & I/O]** No new finding. `AnswerModal` opens no sockets and runs no
  server (the daemon's network + auth are consumer-side, ADR 025). `Snapshot()`
  is bounded by the existing 4K rolling buffer — same posture as every sibling
  screen reader; adding a cap here alone would be inconsistent and defend an
  unobserved DoS against a buffer-bounded input. The confirm poll is bounded by
  `ConfirmTimeout` and `ctx` cancellation — no unbounded spin.
- **[Error messages / logs / telemetry]** No findings — errors carry only the
  enum + integer (above); the method emits no logs or telemetry (it returns the
  error; the consumer logs it if it chooses). No screen text, path, or stack
  trace leaks.
- **[Concurrency]** N/A / no new contract — spawns no goroutines, takes no locks
  of its own, introduces no shared state. The single keystroke reuses the
  existing `writeRaw` → `pty.Write` path (no new concurrency contract — one more
  caller of the established single write seam; not answering while delivering a
  prompt is the caller's mutual-exclusion concern, unchanged by this ticket).
  `ctx` cancellation ends the poll cleanly; the keystroke write is atomic, so
  there is no partial state on cancel.
- **[Threat model alignment]** No repo-local `docs/threat-model.md`; the
  remote-permission threat model lives in pyrycode (epic #597, ADR 025,
  `keys.go`'s `AttachInput` SECURITY note). OUT OF SCOPE and owned downstream:
  authentication of *who* may answer, keystroke *integrity / replay*, and modal
  *staleness* (above). This ticket's only obligation — the **substrate seal** —
  is met: the public surface is `ModalClass` (enum) + `int` + `time.Duration`,
  the keystroke is a computed digit string, no option `Label` or screen literal
  reaches the keystroke / error / public API, and **no new claude wording is
  introduced** (classification reuses the shipped `DetectModalClass`). The
  consumer's `cmd/substrate-guard` stays green (it runs in pyrycode CI, not this
  repo's `make check`).

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-06-22

# #172 — Screen short prompts for control bytes, not just newlines

## Files to read first

- `pkg/tuidriver/deliver.go:245-255` — `shouldTypePrompt` and its doc comment; **the only production edit site**. The `!strings.Contains(text, "\n")` clause is what generalises.
- `pkg/tuidriver/deliver.go:97-127` — `DeliverPrompt`'s method selection: `shouldTypePrompt(opts.Prompt)` picks `TypePrompt` (raw) else `WritePrompt` (paste). The `else` already routes disqualified prompts to paste — **no new routing code is needed**, the strategy falls out of the predicate flip.
- `pkg/tuidriver/session.go:341-362` — `TypePrompt`, the raw seam that writes every byte verbatim then a `\r` commit. This is why a stray control byte reaches the PTY today.
- `pkg/tuidriver/session.go:307-321` — `WritePrompt` / `bracketedPaste`, the destination path. Body goes verbatim *inside* `\x1b[200~ … \x1b[201~`, so control bytes are treated as literal pasted text.
- `pkg/tuidriver/session_test.go:139-143` — `TestBracketedPasteWrapping` "embedded CR survives unchanged inside body": `"before\rafter"` → `"\x1b[200~before\rafter\x1b[201~\r"`. This green test already backs the "route-to-paste neutralises `\r`" claim; reference it, do not duplicate it.
- `pkg/tuidriver/deliver_test.go:44-67` — `TestShouldTypePrompt`, the table this ticket extends.
- `pkg/tuidriver/deliver.go:238-243` — `typePromptMaxLen` (256) doc comment; note it already calls the single-line shape "the load-bearing condition." This change reframes that as "control-byte-free," of which single-line is a subset.

## Context

`shouldTypePrompt` selects the raw byte-by-byte `TypePrompt` seam for prompts that are `<= typePromptMaxLen` **and** contain no `\n`. `TypePrompt` writes each prompt byte to the PTY verbatim, then a separate `\r` commit. The `\n` exclusion exists because a multi-line prompt must paste — but `\n` is the *only* control byte screened.

A short single-line prompt carrying a carriage return (`\r`, 0x0D), ESC (0x1B), or another C0 control byte therefore reaches the PTY unscreened through the seam the library advertises as "safe": an embedded `\r` is read as Enter and commits the turn early; an embedded ESC can steer claude's TUI (interrupt, dismiss/navigate a modal). The consumer's prompt content is an explicit trust boundary (pyrycode spec #749, `mapPromptContent` → `WriteUserTurn`) — downstream holds opaque bytes and never re-parses — so control-byte screening belongs here, at the delivery seam.

The `\n` check is a special case of the general rule "a control byte disqualifies the typed path." This spec generalises the rule and documents where the disqualified prompt goes.

## Design

**One production edit site: the pure predicate `shouldTypePrompt`.** No changes to `TypePrompt`, `WritePrompt`, `DeliverPrompt`, or any write seam.

### Disqualifying byte set — the whole C0 range (`0x00`–`0x1F`)

Screen any byte `< 0x20`. Rationale:

- It **subsumes** the existing `\n` (0x0A) check — the two clear turn-steering bytes the ticket names, `\r` (0x0D) and ESC (0x1B), are both C0, as are `\t` (0x09), NUL, and the rest.
- **No false positives on real content.** Space is 0x20, so every printable ASCII byte is `>= 0x20`. No byte of a multi-byte UTF-8 rune is ever `< 0x80` (lead bytes `>= 0xC0`, continuation bytes `>= 0x80`), so screening `< 0x20` never trips on Unicode. The only borderline-legitimate C0 byte is `\t`; a tab in a *short single-line* prompt is unusual and routing it to paste (which preserves it verbatim) is lossless.
- Per evidence-based fix selection the demonstrated bytes are only `\r` and ESC, but screening the whole C0 range is **zero marginal cost** over screening two bytes (one comparison either way) and is belt-and-suspenders against an undiscovered steering byte. This is the "screen the whole range for safety, set documented" branch the AC explicitly permits. DEL (0x7F) is deliberately **excluded** to keep the predicate a single `< 0x20` comparison and because it is neither C0 nor demonstrated; widening to include it later is a one-token change if ever warranted.

### Predicate contract

- Introduce a small unexported pure helper — suggested `hasControlByte(text string) bool` — that reports whether `text` contains any byte `< 0x20`. A plain byte-index loop is the clearest idiom and matches the byte-oriented style already in `TypePrompt`; `strings.IndexFunc(text, func(r rune) bool { return r < 0x20 }) >= 0` is an equally-acceptable stdlib alternative (any byte `< 0x20` is always a standalone single-byte rune, so rune- and byte-iteration are equivalent here). Developer picks the idiom; keep it total over the empty string (returns `false`).
- `shouldTypePrompt` becomes: `len(text) <= typePromptMaxLen && !hasControlByte(text)`. The `strings.Contains(text, "\n")` clause is deleted (subsumed); drop the now-unused `strings` import only if nothing else in the file needs it (it does — `deliver.go` uses `strings` elsewhere, so leave the import).

### Handling strategy — route to the bracketed-paste path (single, documented)

The disqualified prompt is handled by exactly one strategy: **it takes the existing non-typed path (`WritePrompt` / bracketed paste).** This requires *no new code* — `DeliverPrompt` already selects `s.WritePrompt` whenever `shouldTypePrompt` returns `false`. Flipping the predicate is the entire behavioural change.

Record the choice and its rationale in the `shouldTypePrompt` doc comment (this satisfies AC #2's "decision documented in a code comment"):

- **Why paste, not reject:** the paste path embeds the body verbatim inside `\x1b[200~ … \x1b[201~`, so claude treats an embedded `\r`/ESC/`\t` as literal pasted text rather than a commit or a TUI command — already unit-tested for `\r` (`session_test.go:139`). It is the existing destination for *every* non-typed prompt, so after this change a short control-byte-bearing prompt behaves identically to a long or multi-line one: one code path, consistent, lossless, and it never drops a legitimate turn.
- **Why not reject/sanitise:** rejecting would drop turns whose content legitimately contains a control byte, and sanitising (stripping/replacing bytes) would silently mutate user content. Both are heavier and change observable behaviour. More importantly, either would be **asymmetric** — it would harden only the short-prompt path while the long/multi-line path (a *larger*, more attacker-influenceable payload) keeps pasting the same bytes. Uniform route-to-paste is the coherent, minimal choice.

### Data flow (unchanged except the predicate branch)

```
DeliverPrompt(opts)
  └─ shouldTypePrompt(opts.Prompt)?
       ├─ true  (short AND no C0 byte) ─→ write = TypePrompt   (byte-by-byte + \r)
       └─ false (long OR any C0 byte)  ─→ write = WritePrompt  (bracketed paste)   ← control-byte prompts land here now
```

## Concurrency model

None introduced. `shouldTypePrompt` is a pure synchronous predicate with no shared state. The write-path serialization on `writeMu` (#171) is untouched — this change only alters *which* already-serialized write method `DeliverPrompt` selects.

## Error handling

No new error path. Because the strategy is route-to-paste (not reject), there is no new failure mode to surface: a disqualified prompt is delivered, not refused. `WritePrompt`'s existing error contract (first non-nil PTY write error, propagated by `DeliverPrompt` as `tuidriver: write prompt: %w`) covers it unchanged.

## Testing strategy

All assertable in the claude-free `make check` gate — no live-claude/e2e run required. Extend the `TestShouldTypePrompt` table (`deliver_test.go:44`):

- **Core AC** — short single-line prompt with embedded `\r` (e.g. `"before\rafter"`) → `want: false` (does not take the typed path).
- Short single-line prompt with embedded ESC (`"a\x1bb"`) → `want: false`.
- Short single-line prompt with embedded `\t` (`"a\tb"`) → `want: false` (documents the whole-C0 decision).
- Short single-line prompt with embedded NUL (`"a\x00b"`) → `want: false` (pins that the set is the range, not an enumerated shortlist).
- **No-regression cases stay green unchanged:** all existing `want: true` rows ("What is 2+2?", empty, at-cap, the two runner prompts) contain no C0 byte and still type; the two existing `want: false` newline rows now return `false` via the general C0 check rather than the special `\n` clause — same result.

No reject/sanitise test is needed: the chosen strategy adds no reject behaviour to pin. The "route-to-paste is safe for control bytes" property is already covered by `TestBracketedPasteWrapping`'s "embedded CR survives unchanged inside body" case (`session_test.go:139`) — reference it in the spec/PR rather than re-asserting it.

## Open questions / follow-up

- **Residual — paste-close injection (out of scope here).** Routing to paste neutralises control bytes as literal text *except* the exact paste-close sequence `\x1b[201~`: a prompt body containing it can break out of the bracketed paste early, after which trailing bytes are interpreted by the TUI. This residual is **not introduced by this ticket** — it already affects every long/multi-line prompt delivered via `WritePrompt` today. Routing short control-byte prompts to paste brings them to *parity* with long prompts (a strict improvement: from "every byte reaches the PTY verbatim" to "only the exact `\x1b[201~` sequence can escape"), so it opens no new exposure. Closing the residual means hardening `bracketedPaste` against a body containing `\x1b[201~`, uniformly for all prompt lengths — a separate, well-scoped follow-up (a natural next item from the same Cross-Repo Code Review 2026-07-03). Recommend PO file it; do not expand this ticket to cover it.

## Security review

**Verdict:** PASS

**Findings:**

- **[Trust boundaries]** No MUST-FIX. The trust boundary is explicit and single: the consumer's prompt content (`opts.Prompt`) is untrusted attacker-influenceable data (pyrycode spec #749 `mapPromptContent` → `WriteUserTurn`, opaque downstream), and `shouldTypePrompt` is the single point that decides how those bytes reach the PTY. This change *tightens* the boundary — the raw `TypePrompt` seam previously passed arbitrary control bytes through verbatim; after the change only control-byte-free prompts take it. The decision and its byte set are documented at the boundary function.
- **[Subprocess / external command execution]** No MUST-FIX. The threat is *steering the `claude` subprocess's TUI via injected control bytes* — the vector this ticket closes. Post-change, `\r` (early commit) and a lone ESC (interrupt / modal steer) are both delivered inside a bracketed paste as literal text, not interpreted as terminal input. Verified against the mechanism: bracketed-paste mode instructs the terminal not to interpret escape sequences within the paste body (`session_test.go:139` pins the `\r` case). AC #1's named bytes (`\r`, ESC) plus the rest of C0 all return `false` from the predicate.
- **[Error messages, logs, telemetry]** No findings. No new error/log surface; route-to-paste refuses nothing, so no prompt content is echoed into an error message. `DeliverPrompt`'s existing logs carry no prompt bytes.
- **[Concurrency]** No findings. Pure predicate, no shared state, no new goroutine or lock; `writeMu` (#171) semantics unchanged.
- **[Cryptographic primitives / Tokens / File ops / Network]** N/A — this change is a pure in-memory byte-classification predicate; it touches no filesystem path, no token, no crypto, no socket.
- **[Threat model alignment]** OUT OF SCOPE (named): the **paste-close-injection residual** — a paste body containing the literal `\x1b[201~` can break out of the bracketed paste early and steer the TUI. This is *pre-existing* for every long/multi-line prompt delivered via `WritePrompt` and is **not** introduced or widened by this ticket; routing short control-byte prompts to paste brings them to parity with long prompts (strict improvement, no new exposure). Fixing it means hardening `bracketedPaste` uniformly for all prompt lengths, which is a separate follow-up (see Open questions). Classifying it as a MUST-FIX here would force an *asymmetric* fix (short prompts hardened, the larger long-prompt payload path left open) — incoherent and out of this ticket's XS scope.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-07-07

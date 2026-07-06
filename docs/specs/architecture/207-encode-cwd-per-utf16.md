# #207 — Encode the cwd per UTF-16 code unit to match `claude`

- Issue: https://github.com/pyrycode/tui-driver/issues/207
- Size: **XS** (architect-confirmed — 1 production source file, ~100 LOC total, no consumer cascade, no split)
- Split from #166; unblocked by #206 (CLOSED — golden recorded)
- Not `security-sensitive` (no label) — the cwd is operator/consumer-supplied, not attacker-influenceable `claude` output; this is not the grid-classification chain. Security-review gate does **not** apply.

## Files to read first

- `pkg/tuidriver/cwd.go:5-35` — `EncodeCwd`: the doc comment (5-19, describes the **byte** rule, must be rewritten) and the per-byte loop (24-33, the one behavioural change). This is the only production file that changes.
- `cmd/probe-cwd-encoding/main.go:374-389` — `perUTF16`: **the reference implementation of the target rule.** Range by rune; ASCII-alnum → write the byte; else emit `len(utf16.Encode([]rune{r}))` hyphens. `EncodeCwd`'s new loop mirrors this shape exactly. Also read `perByte:347-358` and `perRune:362-372` (the two contrast transforms the new `main_test.go` pins) and `deriveRule:332-342` + `candidates:319-323` (leaf-suffix match the table test asserts).
- `pkg/tuidriver/cwd_test.go:31` — the self-referential `café → caf--` case to replace; `cwd_test.go:48-64` — the `byteTransform` helper (flagged "Keep in sync with EncodeCwd's loop") + its two consumers `TestEncodeCwd_RealpathHappyPath:66-84` and `TestEncodeCwd_NonexistentPath:86-94`.
- `cmd/spike-queued-modals/main_test.go:1-45` — the repo convention for a claude-free table test in a `cmd/*` binary (`package main`, `struct{name; …; want}` slice, one row per branch). The new `cmd/probe-cwd-encoding/main_test.go` follows this exact shape.
- `cmd/probe-cwd-encoding/README.md` § "Derived rule (for #207)" (128-149) and § "Candidate rules" table (53-59) — the recorded golden `Työ😀 → Ty---` and the per-char breakdown the fixtures assert.
- `docs/knowledge/codebase/206.md` § "The observed result" + § Follow-ups — the resolved finding and the explicit fold-in of the deterministic `main_test.go` safety net into this ticket.
- `pkg/tuidriver/jsonl.go:34-37` (`SessionJSONLPath`) and `pkg/tuidriver/jsonl_test.go:17-98` — **read only to confirm no edit is needed.** `SessionJSONLPath` is a straight `EncodeCwd` pass-through; its tests use ASCII/self-consistent paths, so the behaviour change is invisible to them. Do not touch these files.

## Context

`tuidriver.EncodeCwd` maps every non-`[a-zA-Z0-9]` **byte** to one `-`, so a multi-byte char like `ö` (2 UTF-8 bytes) yields two hyphens. Ticket #206 drove the `probe-cwd-encoding` rig against real `claude` **2.1.199** (2026-07-06) and recorded the ground truth: `claude` encodes **per UTF-16 code unit** — JS `.replace(/[^a-zA-Z0-9]/g,'-')` semantics. A cwd leaf `Työ😀` produced the projects-dir suffix `…-Ty---`, not the per-byte `…-Ty------`.

The consequence is a real, latent bug: `SessionJSONLPath` / `WaitForSessionJSONL` route through `EncodeCwd`, so for **any** cwd containing a multi-byte character they compute a projects-dir path `claude` never writes → the JSONL wait times out against a real non-ASCII working directory. This ticket flips the transform to match the recorded golden and closes that bug. Every prior derivation (#47, #57) exercised only ASCII specials, which is why the defect was latent and why the `café → caf--` unit case was the byte loop describing its own output rather than an observation.

The rule is **not** merely "per rune": for BMP characters (`ö`, `é`) per-rune and per-UTF-16 agree (1 hyphen), but for astral characters (`😀`, a surrogate pair) they diverge — per-rune gives 1 hyphen, per-UTF-16 gives 2. The implementation must match per-UTF-16.

## Design

Single-function behavioural change. `EncodeCwd`'s signature, canonicalisation prefix, and no-error fallback contract are all unchanged; only the character→hyphen mapping in the loop changes.

### The new loop (contract, not body)

Replace the byte-indexed loop (`cwd.go:26-33`) with a rune-ranged loop that mirrors `probe-cwd-encoding`'s `perUTF16`:

- `for _, r := range cwd` — ranging a Go string yields runes; invalid UTF-8 yields `utf8.RuneError` (1 byte consumed), which is non-alnum → one hyphen. No special handling needed.
- If `r < 128 && isASCIIAlnum(byte(r))` → `b.WriteByte(byte(r))` (ASCII fast path — identical to today for every ASCII byte).
- Else → write `len(utf16.Encode([]rune{r}))` hyphens (1 for BMP, 2 for astral). A `for range utf16.Encode([]rune{r}) { b.WriteByte('-') }` loop is the idiom the probe uses; match it.

New import: `unicode/utf16`. Add a small `isASCIIAlnum(c byte) bool` helper (or inline the existing `(c >= 'a' …)` predicate) — the probe already defines one at `main.go:391-393`; keep the two definitions independent (see below), don't cross-import.

`b.Grow(len(cwd))` stays: output byte-length is always ≤ input byte-length (each non-alnum rune emits at most as many hyphens as it has UTF-8 bytes — BMP ≤ 3 bytes → 1 hyphen; astral 4 bytes → 2 hyphens), so the hint remains a valid upper bound.

### Doc comment

Rewrite `cwd.go:5-19`. Replace "every byte outside `[a-zA-Z0-9]` is mapped to exactly one `-`" with the UTF-16 rule: **each non-`[a-zA-Z0-9]` UTF-16 code unit maps to one `-` (BMP rune → 1 hyphen, astral rune → 2), matching `claude`'s `/[^a-zA-Z0-9]/g` encoding.** Keep the canonicalisation and fallback paragraphs. Replace the stale "Byte-transform empirically derived 2026-05-18 (loop 2 B-4)" provenance note with the #206 provenance: observed against real `claude` 2.1.199 on 2026-07-06 (`Työ😀 → Ty---`); cite the probe and `docs/knowledge/codebase/206.md`.

### Do NOT deduplicate against the probe's `perUTF16`

`EncodeCwd` and `probe-cwd-encoding`'s `perUTF16` are intentionally **two independent copies** of the same rule. The probe is a verification rig for `EncodeCwd`; #206 established that a rig must discover ground truth by a path that does not run through the artifact it verifies. Importing `EncodeCwd` into the probe (or extracting a shared helper) would collapse that independence and defeat the rig's ability to observe a future divergence. Leave both copies in place. This is a deliberate, documented exception to DRY — state it in nothing new; just don't refactor.

### Data flow (unchanged)

```
cwd → canonicalisePath (symlinks, case-fold) → per-UTF-16-unit hyphen map → projects-dir name
                                                        ↑ only this step changes
SessionJSONLPath(home, cwd, id) = home/.claude/projects/EncodeCwd(cwd)/id.jsonl   (transparent)
```

## Concurrency model

None. `EncodeCwd` is a pure synchronous string transform with a filesystem stat in `canonicalisePath` (unchanged). No goroutines, no shared state.

## Error handling

No new failure modes. `EncodeCwd` has no error path by contract — a non-existent or unresolvable path falls through to encoding the input as-passed (unchanged). Invalid-UTF-8 input is handled implicitly by `range` (each bad byte → `RuneError` → 1 hyphen), which is at least as sensible as the old per-byte behaviour and needs no guard.

## Testing strategy

### `pkg/tuidriver/cwd_test.go`

- **Replace** the `café → caf--` case (`cwd_test.go:31`) with `café → caf-` (`é` = U+00E9, BMP, 1 UTF-16 unit → 1 hyphen). Rename the case label away from "per-byte".
- **Add** the discriminating astral fixture: `Työ😀 → Ty---` (T, y, then `ö`→`-`, then `😀`→`--`). Call out in the case name that this is the #206 golden and that a naive per-*rune* implementation (`Ty--`) must fail it.
- **Update `byteTransform`** (`cwd_test.go:52-63`) in lockstep: reimplement its body to the per-UTF-16 rule so it still mirrors the production loop. Both `TestEncodeCwd_RealpathHappyPath` and `TestEncodeCwd_NonexistentPath` compute expected values through it against **ASCII-only** temp paths, so their outputs are unchanged — but the helper must stay honest to the new rule. Correct its "byte-by-byte" doc comment; renaming the helper to reflect the rule (e.g. `utf16Transform`) is encouraged for honesty but optional (if renamed, update its two call sites).
- **Leave unchanged** every other `TestEncodeCwd` case (empty, pure-alnum, single-slash, non-existent-fallback, adjacent-specials, dot/space, underscore, loop-2-B-4 reference) and the symlink/case-canonicalisation tests — all ASCII, all still pass.

### `cmd/probe-cwd-encoding/main_test.go` (new)

The deterministic `make check` safety net #206's code-review called for, folded here per `docs/knowledge/codebase/206.md` § Follow-ups. `package main`, table-driven, one struct-slice test in the `spike-queued-modals/main_test.go` idiom:

- `perByte("Työ😀") == "Ty------"`, `perUTF16("Työ😀") == "Ty---"`, `perRune("Työ😀") == "Ty--"` — pins all three reference transforms to the observed golden, proving the astral fixture discriminates them.
- (optional but cheap) an ASCII passthrough row for each transform, e.g. `"abc" → "abc"`, to lock the fast path.
- `deriveRule(dir, "Työ😀")` returns rule `"per-utf16-code-unit"` where `dir` is a **synthetic** projects-dir ending in `-Ty---` (e.g. `"-tmp-probe-Ty---"`). Do **not** hardcode the recorded dir's `…2549730227` random temp suffix — `deriveRule` matches on the leaf suffix only, so a portable synthetic dir is correct and machine-independent. Optionally assert the returned `matched` slice is exactly `["per-utf16-code-unit"]` (the astral leaf makes per-rune and per-byte non-matching, so no ambiguity).

Run `make check` — it must build/vet and pass all of the above with no live `claude`.

## Open questions

- **Helper rename vs. minimal churn.** Whether to rename `byteTransform → utf16Transform` is a cosmetic call left to the developer; the binding requirement is that the helper's body and comment mirror the new rule. Either choice is in scope for XS.

None of these block implementation.

## Out of scope

- pyrycode's `agentrun.EncodeProjectDir` carries the same per-byte bug (pyrycode#633 divergence map). It is the **consumer's** copy — a cross-repo follow-up, not this ticket.
- The other #206 follow-ups (deterministic golden capture into a committed artifact, widening the CI `probe-recordings` glob, emoji-dir accumulation cleanup) are separately tracked and not part of #207.

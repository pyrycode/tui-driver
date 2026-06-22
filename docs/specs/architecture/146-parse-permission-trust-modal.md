# Spec: parse claude permission/trust modals into a typed struct + serializer

**Ticket:** [#146](https://github.com/pyrycode/tui-driver/issues/146)
**Size:** S
**Status:** ready for development
**Labels:** `security-sensitive` (security-review pass appended below — verdict PASS)

Phase 3 (epic pyrycode#597) foundation — the screen-side modal reader. This
ticket adds **structured extraction + serialization** on top of the already-
shipped classification (`DetectModalClass` / `ModalClass`). It does NOT
re-introduce class detection, and it owns no permission *decision* — that lives
in the pyrycode children. Screen-side extraction only.

## Files to read first

Turn-1 reading list. Load these before writing code; the design below assumes
you have read the empirical modal byte shapes in the spike README.

- `pkg/tuidriver/modal.go:21-131` — `ModalClass` enum + `DetectModalClass` +
  the **unexported anchors to reuse directly** (`anchorPermissionStripped`,
  `anchorPermissionSpaced`, `anchorTrustFolder`). The extractor calls
  `DetectModalClass` for classification; it does NOT re-detect.
- `pkg/tuidriver/agents.go:41-77` — `ParseAgentList`: the
  `Render` → split → space-normalize → line-scan skeleton to **mirror exactly**.
  This is the parser's structural template.
- `pkg/tuidriver/mcp.go:41-110` — `ParseMcpStatus`: pre-compiled package-level
  regex vars, `Render`-based row parsing, and the "skip hint-bar / footer
  lines" filtering pattern. The numbered-option regex mirrors the
  `mcpTotalServersRe` style.
- `pkg/tuidriver/ask_user.go:1-105` — `AskUserQuestion` / `*Option` struct shape:
  the **JSON-tag convention** this ticket's "neutral push shape" copies, and the
  **"return nil if it would be empty"** no-false-positive posture (AC1).
- `pkg/tuidriver/grid.go:36-46` — `Render(snap, 0, 0)` contract: pass `0, 0` for
  default grid dims; why `Render` (not `StripANSI`) is required — claude paints
  inter-word spacing with `\x1b[1C` cursor-forwards, which `StripANSI` collapses
  ("Do you want to proceed" → "Doyouwanttoproceed", "1. Yes" → "1.Yes").
- `pkg/tuidriver/trust.go` — `TrustModalAnchor` ("Quicksafetycheck") + the
  post-accept-confirmation false-positive trap (do NOT match "Yes, I trust this
  folder ✔" residue — anchor on the header only).
- `pkg/tuidriver/keys.go:40-54` — `AcceptTrust` / `Answer`: the **downstream
  keystroke contract** the option `Index` must feed. The next child
  (choice → keystroke) calls `Answer(strconv.Itoa(opt.Index))`, so `Index` MUST
  be claude's rendered 1-based number ("1.", "2.", "3."), not a 0-based slot.
- `cmd/spike-permission/README.md:187-298` — **the empirical modal byte shapes.**
  Bash modal raw bytes (195-207) → transcribe into
  `testdata/permission-snapshot.bin`. Rendered layout (246-257) → the exact
  title / prompt / option / default the test asserts. The Read-modal divergence
  (259-298) is the documented out-of-scope inline-packed variant (see Open
  Questions).
- `pkg/tuidriver/modal_test.go:106-128` — `TestDetectModalClassRealFixtures`:
  the `os.ReadFile(filepath.Join("testdata", …))` fixture-load + table-assert
  idiom the new test mirrors.
- `pkg/tuidriver/testdata/picker-snapshot.bin` — an existing committed raw-PTY
  `.bin` fixture; shows the byte format (`\x1b[…m` SGR, `\x1b[1C` cursor-forward)
  the new fixtures match.

## Context

Permission and trust-folder prompts never reach `Events()` — the spike proved
they have **zero JSONL footprint** (`cmd/spike-permission/README.md` § Status;
claude defers `assistant(stop_reason=tool_use)` until the modal is
approved/cancelled). They exist only on the rendered PTY screen. The substrate
seal makes tui-driver the sole owner of claude's screen knowledge, so the daemon
(mobile-remote-head) must receive a modal as a **neutral typed value**, never by
reading screen text.

`DetectModalClass` already classifies the `permission` and `trust-folder`
screens; `HasTrustModal` / `TrustModalAnchor` already anchor the trust prompt.
This ticket adds the structured extraction (title / prompt / ordered options /
default into a struct) and confirms the struct serializes to a neutral,
ANSI-free, screen-literal-free JSON shape. The next child (safe modal-answer:
choice → keystroke + re-read confirmation) builds on the `ModalContent` struct.

## Design

### Package structure

One new production file, `pkg/tuidriver/permission.go`, plus its test
`pkg/tuidriver/permission_test.go` and two fixtures under `testdata/`. Purely
additive — no existing file changes, no exported symbol renamed, no consumer
call site touched.

### Types (the neutral push shape)

```go
// ModalContent is the neutral, screen-sourced value for a claude
// permission / trust-folder modal. JSON tags ARE the push shape (no separate
// marshaling framework — same convention as AskUserQuestion / Agent).
type ModalContent struct {
    Class   ModalClass    `json:"class"`   // reuses the shipped enum
    Title   string        `json:"title"`   // modal header (see per-class extraction)
    Prompt  string        `json:"prompt"`  // the question line
    Options []ModalOption `json:"options"` // render order, top-to-bottom
    Default int           `json:"default"` // 1-based index of the ❯-marked option; 0 = none
}

// ModalOption is one numbered choice. Index is claude's rendered 1-based
// number — the keystroke the downstream answer child sends; it is the
// security-relevant selector (the actual grant is chosen by the number, which
// claude renders). Label is advisory display text (with the "N." prefix and ❯
// marker stripped) — it is prompt-/tool-influenceable and MUST NOT be parsed
// or routed on by the consumer; route the answer on Index. The struct
// doc-comment must state this so a downstream caller cannot drift into
// treating a label as authoritative.
type ModalOption struct {
    Index int    `json:"index"`
    Label string `json:"label"`
}
```

Field-level decisions:
- **`Default int`, 0 = none, always present (no `omitempty`).** claude's option
  numbers are 1-based, so 0 is an unambiguous sentinel and a stable contract for
  the consumer (field always there). The captured modals all mark option 1 with
  ❯, so both fixtures assert `Default == 1`.
- **`Index` is the rendered number, not a slice position.** It is the literal
  keystroke `Answer` will send downstream — keep them identical.

### Extractor — contract

```go
// ParseModalContent extracts claude's permission / trust-folder modal from a
// PTY snapshot into a neutral typed value. Returns nil when the snapshot is
// not one of those two modals (idle screen, a different modal, or garbage) —
// no false positive. Screen-sourced sibling of ParsePicker, NOT of the JSONL
// extractors (modals never reach Events()).
func ParseModalContent(snap []byte) *ModalContent
```

Naming note: the ticket tentatively called this `ParsePermissionModal`, but the
extractor is class-generic (handles `permission` AND `trust-folder`, and extends
to future classes via the shared `Class` field). `ParseModalContent` →
`*ModalContent` follows the package's `Parse<Thing>` → `*<Thing>` convention
(`ParseAgentList`, `ParseMcpStatus`, `ParseAskUserQuestion`) and matches the
ticket's own "additional classes extend naturally" framing.

Behaviour (the binding contract):
1. `class := DetectModalClass(snap)`. Return `nil` unless `class` is
   `ModalClassPermission` or `ModalClassTrustFolder`. (Do not re-detect — reuse
   the shipped classifier.)
2. `text := Render(snap, 0, 0)`; split into lines; normalize each line by
   collapsing runs of spaces to one and trimming — identical prelude to
   `ParseAgentList` / `ParseMcpStatus`. `Render` (not `StripANSI`) is mandatory:
   it expands `\x1b[1C` cursor-forwards back to spaces so options read
   "2. Yes, allow reading from tmp/ from this project", not "2.Yes,allow…".
3. Extract `Title`, `Prompt`, `Options`, `Default` per the class branch below.
4. **Return `nil` if zero option rows parsed.** A detected-but-unparseable modal
   yields nil, not a half-empty struct — mirrors `ParseAskUserQuestion`'s
   "nil if it would be empty" posture (AC1's no-false-positive requirement).

### Per-class extraction

Both classes share the **option-row** and **default** mechanics; only
title/prompt differ. An option row is a normalized line matching an unexported
regex of shape `^(❯\s*)?(\d+)\.\s*(.+)$`:
- group 2 → `Index` (parsed int)
- group 3 → `Label` (trimmed; the `N.` prefix and any ❯ are outside the capture)
- presence of group 1 (the ❯ marker) on a row → `Default = that row's Index`

**`permission`** (multi-line render — see README 246-257):
```
─────────…─────────        ← separator (long run of ─, U+2500); modal top anchor
Bash command               ← Title  (first content line after the separator)
ls /tmp                     (action detail — NOT captured; see Open Questions)
List files in /tmp          (action detail — NOT captured)
Do you want to proceed?    ← Prompt (line containing anchorPermission*)
❯ 1. Yes                   ← option, ❯ → Default=1
  2. Yes, allow reading from tmp/ from this project
  3. No
(Esc to cancel · Tab to amend)   ← hint bar, skipped
```
- `Title` = first non-empty line after the separator that is neither an option
  row nor the prompt line. Detect the separator as a line consisting solely of
  ─ (e.g. `strings.Trim(line, "─") == "" && len(line) > 8`).
- `Prompt` = the trimmed line containing `anchorPermissionStripped` or
  `anchorPermissionSpaced` (reused from `modal.go`); after `Render` it reads
  "Do you want to proceed?".

**`trust-folder`** (header + question + numbered options):
```
Quick safety check: Is this a project you created or one you trust?
❯ 1. Yes, I trust this folder
  2. No, …
```
- `Title` = "Quick safety check" — the text up to the first ":" on the line
  containing `anchorTrustFolder` ("Quicksafetycheck"), or that whole line when
  no ":" is present.
- `Prompt` = the remainder after the ":" ("Is this a project you created or one
  you trust?"), or a separate line if claude wraps it. An unexported
  `anchorTrustQuestion` (e.g. `[]byte("Is this a project")`) is acceptable to
  locate it; keep it unexported.

### Serializer (the neutral push shape)

Per the ticket: **the struct's JSON tags ARE the serializer.** No wrapper method
— `AskUserQuestion` / `Agent` have none either; the consumer calls
`encoding/json.Marshal(modal)`. The deliverable that *proves* AC2 is a test
(see Testing strategy) that marshals a parsed struct and asserts the bytes carry
no ANSI / terminal control bytes and no screen-marker glyphs — only neutral
field values. Do not invent a redundant `Marshal*` method.

### Unexported-literal discipline (AC3)

Every claude screen literal stays an **unexported** package var/const, matching
`modal.go` / `trust.go`:
- Reuse the existing unexported anchors (`anchorPermissionStripped`,
  `anchorPermissionSpaced`, `anchorTrustFolder`).
- Any new literals (the option-row regex, the separator predicate, an optional
  `anchorTrustQuestion`) are unexported package-level vars/consts.
- No exported const, type name, or struct field bakes in claude wording. Field
  *values* are runtime-extracted neutral data (allowed — same as
  `ParseAskUserQuestion` carrying claude's question text). This is what keeps the
  consumer's `cmd/substrate-guard` green downstream (that guard runs in pyrycode
  CI, not in this repo's `make check`).

## Concurrency model

None. `ParseModalContent` is a pure, stateless function: snapshot bytes in,
value out. No goroutines, channels, shared state, or locks — same shape as every
other `Parse*` / `Detect*` screen function in the package. The caller owns
snapshot timing (`Session.Snapshot()`); the library does not poll.

## Error handling

All failure modes collapse to a `nil` return (no error value, matching the
sibling parsers — a snapshot either is a parseable modal or is not):
- `DetectModalClass` returns anything other than permission/trust-folder → nil.
- Detected class but zero option rows parsed → nil (no half-struct).
- Empty / nil / garbage input → `DetectModalClass` returns `ModalClassUnknown`
  → nil.
- No ❯ marker found → not an error; `Default = 0` (none).

There is no panic path: `Render` tolerates arbitrary bytes, regex match on a
non-matching line simply yields no option, and an out-of-range or malformed
option number is skipped rather than parsed.

## Testing strategy

New `permission_test.go`, plus two committed fixtures. Scenarios (developer
writes them in the table-test idiom of `modal_test.go`; assertions below are the
binding contract):

- **No-modal → nil (AC1 no-false-positive).** `nil`, idle bytes
  (`[]byte("idle TUI")`), and a non-target modal fixture (reuse
  `testdata/picker-snapshot.bin` or `mcp-snapshot.bin`) each return `nil`.
- **Permission fixture.** Load `testdata/permission-snapshot.bin`; assert:
  - `Class == ModalClassPermission`
  - `Title == "Bash command"`
  - `Prompt == "Do you want to proceed?"`
  - `Options == [{1,"Yes"}, {2,"Yes, allow reading from tmp/ from this project"}, {3,"No"}]`
  - `Default == 1`
- **Trust-folder fixture.** Load `testdata/trust-folder-snapshot.bin`; assert:
  - `Class == ModalClassTrustFolder`
  - `Title == "Quick safety check"`
  - `Prompt == "Is this a project you created or one you trust?"`
  - `Options[0] == {1,"Yes, I trust this folder"}`, `len(Options) >= 2`
  - `Default == 1`
- **Label cleanliness.** For both fixtures, no option `Label` contains a leading
  "N.", a ❯, or a box-drawing rune.
- **Serialization neutrality (AC2).** `json.Marshal` a parsed struct; assert the
  bytes contain **no** `0x1b` (ESC), **no** `\x07` (OSC terminator), and none of
  the glyphs `❯` (U+276F), `─` (U+2500), `⎿` (U+23BF); assert the known neutral
  field values round-trip (e.g. JSON contains `"class":"permission"` and
  `"default":1`).

### Fixture provenance

- `permission-snapshot.bin` — **transcribe faithfully** from the raw byte
  excerpt in `cmd/spike-permission/README.md:195-207` (the Bash modal, a real
  capture the spike persisted to a now-deleted temp file; the README is the
  surviving record). Include the surrounding separator + option lines so
  `DetectModalClass` and the parser both fire. Keep the `\x1b[1C` cursor-forwards
  and SGR codes — they are what `Render` resolves.
- `trust-folder-snapshot.bin` — construct from the known anchor
  ("Quick safety check"), the documented question, and the option labels above,
  using the same `\x1b[…m` / `\x1b[1C` idiom as the permission fixture. No raw
  trust-modal capture survives (see Open Questions).

## Open questions

1. **Trust-folder fixture is constructed, not captured.** No raw trust-modal
   snapshot was ever persisted (the spikes pre-mark the workdir trusted or send
   `1\r` via `AcceptTrust`). The permission fixture is a faithful transcription
   of real captured bytes; the trust fixture is reconstructed from the known
   anchor + `AcceptTrust`'s "Yes, I trust this folder" contract + documented
   wording. **Option-2 wording is unconfirmed** — assert option-1, the option
   count (`>= 2`), and `Default`, not an option-2 literal. Recommended follow-up:
   replace the constructed trust fixture with a live capture via
   `cmd/spike-permission` against a fresh untrusted cwd (or the e2e harness), and
   tighten the assertions then.
2. **Read-modal inline-packed variant is out of scope.** The Read permission
   modal (README 259-298) packs separator + header + action + question onto one
   line and preserves literal spaces — structurally different from the Bash
   render. AC requires *one* permission fixture (Bash) + *one* trust fixture; the
   separator-anchored title extraction targets the Bash layout. On the Read
   render the parser must degrade to nil/partial (never panic), not produce a
   wrong title. Hardening the Read variant is a follow-up once it is fixtured —
   building it now would defend an un-fixtured shape (evidence-based fix
   selection).
3. **Action-detail lines are not captured.** The permission modal's command
   lines ("ls /tmp", "List files in /tmp") are not among the ticket's named
   fields (class/title/prompt/options/default), so the struct omits them; the
   resource scope still reaches the daemon via the option labels (option 2 names
   the resource). If the daemon later needs the command detail as a distinct
   field, that is an additive struct extension in a follow-up.

## Security review

**Verdict:** PASS

**Findings:**

- **[Trust boundaries]** SHOULD FIX (applied inline). The one real boundary:
  claude's rendered PTY screen → parsed struct → JSON → daemon → phone. The
  modal *content* is prompt-/tool-influenceable (a hostile prompt can make
  claude render arbitrary option label text). The mitigation is structural: the
  parser extracts `Index` from claude's rendered number, and `Index` — not the
  label — is the keystroke that selects the grant. A spoofed label can mislead a
  *human*, but cannot change which grant a given keystroke applies. Strengthened
  the `ModalOption` doc-comment to mark `Label` as advisory, non-authoritative
  display text and `Index` as the security-relevant selector the consumer routes
  on. The single boundary is one named function (`ParseModalContent`), not
  scattered.
- **[Tokens / secrets]** No findings — and a deliberate hardening: the parser
  never generates, stores, or compares secrets. The permission modal's
  action-detail lines ("ls /tmp", a full Bash command line that could echo a
  secret) are **intentionally not captured** (see Design / Open Question 3), so
  no command body is serialized into the push shape or anywhere else. `Title`
  for a permission modal is the generic header ("Bash command"), not the
  command. Option labels carry only the coarse resource scope claude already
  shows the user (e.g. "tmp/") — needed for the permission decision, no finer.
- **[File operations]** N/A — `ParseModalContent` reads no files. The test reads
  fixtures via `os.ReadFile(filepath.Join("testdata", …))` with fixed in-repo
  literals; no caller-controlled path, no traversal, no TOCTOU.
- **[Subprocess / external command]** N/A — the function spawns and executes
  nothing; it parses bytes. claude is spawned elsewhere, outside this ticket's
  surface.
- **[Cryptographic primitives]** N/A — no randomness or crypto; nothing
  security-relevant is compared.
- **[Network & I/O]** No new finding. `ParseModalContent` takes `snap []byte`
  and caps nothing itself — but input is bounded upstream by the 4K rolling
  buffer (`Session.Snapshot()`), and this matches the established posture of the
  sibling screen parsers (`ParsePicker`, `ParseAgentList`, `ParseMcpStatus` —
  none cap input). `Render` allocates a fixed `cols×rows` grid; the regex scan
  is linear. Adding a cap here (and not on the siblings) would be inconsistent
  and would defend an unobserved DoS against a buffer-bounded input — deferred
  per evidence-based fix selection.
- **[Error messages / logs / telemetry]** No findings — failure collapses to a
  `nil` return (no error string to leak), and the function performs no logging
  or telemetry. The serialized field values are the modal's user-facing text,
  which is the *intended* push payload; the neutrality test asserts no ANSI /
  OSC / box-drawing glyphs cross the boundary.
- **[Concurrency]** N/A — pure, stateless function: no goroutines, shared state,
  or locks (documented in § Concurrency model). No lifecycle to leak, no lock
  order to invert.
- **[Threat model alignment]** No repo-local `docs/threat-model.md`; the
  remote-permission threat model lives in pyrycode (epic pyrycode#597, Phase 3,
  and `keys.go`'s `AttachInput` SECURITY note / ADR 025). OUT OF SCOPE for this
  ticket and named as such in the ticket body and Context: the permission
  *decision*, authentication of *who* may answer, and integrity/replay of the
  answer keystroke are owned by the pyrycode children. This ticket's only
  security obligation — the substrate seal (no claude screen literal escapes the
  package's public surface or the serialized output) — is enforced by AC3 +
  the unexported-literal discipline + the serialization-neutrality test.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-06-22

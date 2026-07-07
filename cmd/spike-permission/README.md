# spike-permission

End-to-end PTY drive of three permission-prompt modal probes across two
interactive `claude` sessions: observe the modal in PTY + JSONL, auto-
respond + complete the turn, simulated escalation (extract modal text
and don't respond). Establishes the empirical foundation for the
TUI Driver architecture's modal-handling **Layer 2** (detect known
modals + auto-respond or escalate via consumer callback).

See [ticket #13](https://github.com/pyrycode/tui-driver/issues/13) and
`docs/specs/architecture/13-spike-permission.md`.

## Status

**Spike complete (2026-05-17).** Three green end-to-end runs with the
default `-approve-keystroke 1-enter` and `-modal-predicate literal-text`
flags. Total wall ~24–27 s per run. Headline findings:

- **The approve keystroke is `1\r` (`31 0d`)** — single-byte digit +
  carriage return. Matches the on-screen `1.Yes` numbered option as the
  first / default selection. `y\r`, bare `\r`, and `\x1b[B\r` (down +
  enter) all also work, validated in supplementary runs.
- **The permission modal has ZERO JSONL footprint.** Probe 1's 5 s
  observation window captures `events=0`. Claude does not write
  `assistant(stop_reason=tool_use)` until AFTER the modal is approved.
  The cancelled / not-yet-approved tool request leaves no envelope
  trace at all. Consequence: **modal detection must be PTY-side**, not
  JSONL-side. Architecturally consistent with cancellation (spike #11
  finding #12), where JSONL signaling is also a separate path from
  the canonical state signal.
- **Modal text renders with two distinct space-stripping patterns**,
  depending on the tool being requested. The Bash modal uses CSI
  cursor-forward sequences (`\x1b[1C`) between words (same root cause
  as spike #1 finding #8 for the spinner verb), so the stripped buffer
  reads `Doyouwanttoproceed` with NO interword spaces. The Read modal
  uses literal spaces, so the stripped buffer reads `Do you want to
  proceed`. **Detection + extraction must handle BOTH variants.**
- **Box-drawing predicate has known false positives at idle** (the
  input box border uses `╭ ╰ │` chars); the `idle-predicate-check`
  diagnostic line documents `has_modal=true` at idle when using
  `-modal-predicate box-drawing`. The default `literal-text` predicate
  is false-positive-free at idle (`has_modal=false`).
- **Session is fully usable after auto-respond.** Probe 2 produces a
  normal `assistant(end_turn)` and a `SUCCESS:` extraction in ~5–8 s
  after approve. Same shape as a non-modal turn. (This was the 2.1.158
  behaviour; see the 2.1.199 note below for what changed.)

## 2.1.199 observation rig (#180)

On claude 2.1.199, Probe 2's post-approve turn no longer reaches the
readiness predicate (`gotEndTurn ∧ ❯-present ∧ PTY-quiet`) that the same
`1\r` keystroke reached on 2.1.158, so the old `for !check()` loop hung to
the runner's 60 s cap and reddened `make e2e`. The README's 2.1.158
`SUCCESS:` behaviour above is now historical.

Probe 2 is therefore an **observation rig**, the same posture as
`spike-multiselect` / `probe-cwd-encoding` (#206). After the approve
keystroke it drains a **bounded settle window** (`autoRespondSettleWindow`,
8 s), records the state it reached, and exits 0. It does **not** change the
modal matcher, the approve keystroke, or any readiness predicate; it records
what it sees and never asserts a completion it did not reach.

**Output.** One `^OBSERVED:` line (the e2e-runner gates `spike-permission`
on `^OBSERVED`, not `^SUCCESS`, since #180):

```
OBSERVED: spike-permission post-approve modal_cleared=<bool> end_turn=<bool> idle_present=<bool> pty_quiet=<bool> approve_keystroke=<hex> snapshot=<path>
```

The fields distinguish the failure modes from the artifact alone:
`modal_cleared=false` → the keystroke was not accepted; `modal_cleared=true,
end_turn=false` → no `end_turn` signal within the window; `end_turn=true`
with `idle_present`/`pty_quiet` false → completed but the readiness predicate
was unmet. The post-approve PTY snapshot and the OBSERVED block are persisted
durably under an `outDir` (printed as `probe=2 outDir=…`), not a bare
ephemeral tempfile (#206 lesson).

**Re-gate condition.** Restore `^SUCCESS` gating (revert the e2e-runner
`SuccessMarker` to `successSuccess` and the probe to the readiness-loop) only
once the post-approve turn reaches `end_turn` again against the pinned claude.
If the recorded observation reveals a genuine library/consumer-facing
regression in the approve-keystroke or `end_turn` detection, file a separate
`blocked-by` follow-up to apply the warranted fix (the #206 → #207 split), and
do NOT force this spike back to `^SUCCESS` to paper over it.

## What it does

Two sequential `claude` invocations (Session A then Session B), three
probes total:

1. **Session A → Probe 1 (observe)** — submit `list the files in /tmp`,
   wait for the modal, capture raw byte snapshot + extracted text,
   drain JSONL events for 5 s, exit. Shutdown SIGTERMs claude with the
   modal still open.
2. **Session B → Probe 2 (auto-respond)** — same prompt as Probe 1,
   wait for modal, send the approve keystroke, wait for the modal to
   clear AND `end_turn` to arrive, extract assistant text by msg_id
   (same `msg_id`-grouped extraction as spike #11), print `SUCCESS:`.
3. **Session B → Probe 3 (escalate)** — submit `read /etc/hostname` to
   force a fresh permission prompt for a different tool (Read instead
   of Bash), wait for modal, extract text + tool name, log what an ACP
   escalation callback would receive, sleep 3 s, verify the modal is
   still open, exit.

| Probe | Prompt | Tool | Behavior |
|---|---|---|---|
| 1 | `list the files in /tmp` | Bash | observe; don't respond; exit with modal open |
| 2 | `list the files in /tmp` | Bash | trigger → approve (`1\r`) → end_turn → SUCCESS |
| 3 | `read /etc/hostname` | Read | trigger → extract → simulated escalation; modal stays open at exit |

Probe 3 uses a different tool (Read) from Probe 2 (Bash) because claude
may grant "yes, allow X from this project" for repeated Bash invocations
within a session — switching to Read guarantees a fresh permission
prompt.

## How to run

Requirements: `claude` 2.1.x on `$PATH`, authenticated against a Claude
subscription; Go 1.26+.

```sh
go build -o /tmp/spike-permission ./cmd/spike-permission
/tmp/spike-permission
```

On success, stdout is one line (from Probe 2's recovery):

```
SUCCESS: Listed /tmp — <claude's actual response describing the directory>
```

Stderr contains the raw claude UI bytes interleaved with the state log.
Recommended: `tee` stderr to a file for post-hoc inspection
(`/tmp/spike-permission ./cmd/spike-permission 2> spike-permission.log`).

### Optional flags

- `-approve-keystroke {1-enter|y-enter|enter|down-enter}` (default
  `1-enter`) — the keystroke to send after `modal-detected` in Probe 2.
  All four work; `1-enter` is canonical because it matches the on-
  screen `1.Yes` numbered default. See *Approve keystroke that worked*.
- `-modal-predicate {literal-text|box-drawing}` (default `literal-text`)
  — the predicate `hasModal` uses to detect the modal. `literal-text`
  is false-positive-free at idle; `box-drawing` has the known
  false-positive risk noted above.
- `-session-id <uuid>` (default: generate) — pin Session A's session
  ID. Session B always generates a fresh UUID (two `claude` invocations
  cannot share a `--session-id`).

### Required state log line sequence (one green run)

Session A (probe 1, observe):

```
projects-dir path=<…>
session-id-resolved id=<uuid-a> jsonl=<…> tag=a
idle-detected tag=a
idle-predicate-check tag=a predicate=literal-text has_modal=false
probe=1 probe-start kind="observe" prompt="list the files in /tmp"
probe=1 prompt-written
session-jsonl-opened path=<…> offset=0 tag=a
probe=1 modal-detected pattern="literal-text"
probe=1 modal-bytes-snapshot len=4096
probe=1 modal-bytes-snapshot-path=/tmp/spike-permission-probe1-bytes-<ts>.bin
probe=1 modal-text-extracted text="Bash command\nls/tmp\n…"
probe=1 observation-window-start window=5s
probe=1 observation-window-end events=0
shutdown-signalled tag=a
```

Session B (probes 2 + 3):

```
session-id-resolved id=<uuid-b> jsonl=<…> tag=b
idle-detected tag=b
idle-predicate-check tag=b predicate=literal-text has_modal=false
probe=2 probe-start kind="auto-respond" prompt="list the files in /tmp"
probe=2 prompt-written
session-jsonl-opened path=<…> offset=0 tag=b
probe=2 modal-detected pattern="literal-text"
probe=2 modal-text-extracted text="Bash command\nls/tmp\n…"
probe=2 response-keystroke-sent bytes=31 0d
probe=2 modal-cleared
probe=2 end-turn-detected msg_id=<id>
probe=2 assistant-text-extracted len=<n>
probe=3 probe-start kind="escalate" prompt="read /etc/hostname"
probe=3 prompt-written
probe=3 modal-detected pattern="literal-text"
probe=3 modal-bytes-snapshot len=4096 path=/tmp/spike-permission-probe3-bytes-<ts>.bin
probe=3 modal-text-extracted text="Read file\r\r  Read(/etc/hostname)\r\rDo you want to proceed?\n…"
probe=3 modal-escalation-callback-would-receive text="…" tool=Read
probe=3 escalation-simulated
probe=3 modal-still-open=true
shutdown-signalled tag=b
complete elapsed=<duration>
```

### Verifying clean exit

```sh
pgrep -lf 'claude --session-id' || echo 'no claude orphans'
pgrep -lf '^ls /tmp' || echo 'no ls orphans'
```

Both clean across all observed green runs.

## Approve keystroke that worked

**`1\r` (`31 0d`), 2 bytes, single bulk write.** Matches the on-screen
`❯1.Yes` numbered default option as the first choice. Single bulk
`pty.Write([]byte{0x31, 0x0d})`; no inter-byte delay; no follow-on
keystrokes.

| Flag value | Bytes | Hex | Outcome |
|---|---|---|---|
| `1-enter` (default) | `1\r` | `31 0d` | **Works.** Modal clears, turn proceeds, `SUCCESS:` extracted in ~5–8 s. |
| `y-enter` | `y\r` | `79 0d` | Works. Same outcome as `1-enter`; no observable timing difference. |
| `enter` | `\r` | `0d` | Works. Bare Enter accepts the highlighted default (option 1). |
| `down-enter` | `\x1b[B` `\r` | `1b 5b 42 0d` | Works but slower by ~1 s — the down-arrow navigates to option 2 ("Yes, allow X from this project"), which is a different (broader) grant than option 1. Use only for testing the navigation path. |

**Implication for the library:** `1\r` is the simplest, narrowest grant
("Yes" / "once"). For an ACP host UI's "approve once" button, the
library's auto-respond default should send `1\r`. For "approve always
for this project", the library would send `\x1b[B\r` (down then enter).

## Modal PTY shape

### Bash modal (Probe 1 + Probe 2)

**Raw byte excerpt** (~500 bytes around `modal-detected`, ANSI sequences
visible — see `modal-bytes-snapshot-path=<tempfile>` for the full
snapshot):

```
\x1b]0;✳ List files in /tmp directory\x07\x1b[39m⏺\x1b[1C\x1b[1mListing 1 directory…(ctrl+o to expand)\x1b[22m
\x1b[2C⎿  $ ls /tmp
─────────────────────────────────────────────────────────────────────────────────────────
\x1b[1mBash command\x1b[22m
ls\x1b[1C/tmp
\x1b[1mList\x1b[1Cfiles\x1b[1Cin\x1b[1C/tmp\x1b[22m
\x1b[33mDo\x1b[1Cyou\x1b[1Cwant\x1b[1Cto\x1b[1Cproceed?\x1b[39m
\x1b[36m❯\x1b[1C1.\x1b[1CYes\x1b[39m
\x1b[2C2.\x1b[1CYes,\x1b[1Callow\x1b[1Creading\x1b[1Cfrom\x1b[1Ctmp/\x1b[1Cfrom\x1b[1Cthis\x1b[1Cproject
\x1b[2C3.\x1b[1CNo
\x1b[2m(Esc\x1b[1Cto\x1b[1Ccancel\x1b[1C·\x1b[1CTab\x1b[1Cto\x1b[1Camend)\x1b[22m
```

**Stripped text** (after `ansiRe.ReplaceAll(snap, nil)` plus `oscRe`):

```
Listing 1 directory…(ctrl+o to expand)
  ⎿  $ ls /tmp
─────────────────────────────────────────────────────────────────────────────────────────
Bash command
ls/tmp
Listfilesin/tmp
Doyouwanttoproceed?
❯1.Yes
2.Yes,allowreadingfromtmp/fromthisproject
3.No
Esctocancel·Tabtoamend
```

Notice the no-space form (`ls/tmp`, `Listfilesin/tmp`,
`Doyouwanttoproceed`) — Bash modal uses `\x1b[1C` (CSI cursor-forward-1)
between every word, and the ANSI strip removes them along with color
codes.

**Box-drawing characters used:** `─` (horizontal dash, U+2500, in the
separator line), `⎿` (curly bottom-right, U+23BF, in the "ls /tmp"
preview line), `❯` (rightwards black arrowhead, U+276F, in the option
selection cursor).

**ANSI color codes used:** `\x1b[33m` (yellow, for the proceed
question), `\x1b[36m` (cyan, for the selection cursor row), `\x1b[39m`
(default foreground), `\x1b[1m` / `\x1b[22m` (bold on/off), `\x1b[2m`
(dim, for the bottom hint), `\x1b[1C` (cursor-forward-1, used between
words — load-bearing for the no-space stripping issue above).

**Cursor positioning:** the modal paints within the rolling buffer's
window; no out-of-buffer cursor moves observed. The block is preceded
by a long horizontal-dash separator (`─` × ~80) that the extractor uses
as the modal anchor.

**Layout:**

```
<header — tool name + bold>           "Bash command"
<action description>                  "ls /tmp"
<action description, bold>            "List files in /tmp"
<proceed question, yellow>            "Do you want to proceed?"
<option 1, cyan + ❯>                  "❯ 1. Yes"
<option 2>                            "  2. Yes, allow reading from tmp/ from this project"
<option 3>                            "  3. No"
<dim bottom hint>                     "(Esc to cancel · Tab to amend)"
```

### Read modal (Probe 3)

**Stripped text** (after `oscRe` + `ansiRe`):

```
⎿  /etc/hostname
─────────────────────────────────────────────────────────────────────────────────────────Read file  Read(/etc/hostname)Do you want to proceed?
❯1.Yes
2.Yes,allowreadingfrometc/duringthissession
3.No

Esctocancel·Tabtoamend
```

Notice the divergence from the Bash modal: **Read uses literal spaces
in "Do you want to proceed"** and packs "Read file Read(/etc/hostname)
Do you want to proceed?" on a single line with the dash separator
inline. This is structurally different enough that the detection
predicate must match both `"Doyouwanttoproceed"` (Bash form) AND
`"Do you want to proceed"` (Read form).

**Box-drawing characters used:** same as Bash modal plus the `⎿` for
the file-list display.

**Layout:**

```
<file-list display>                  "⎿  /etc/hostname"
<inline modal: dashes + header + action + question>
                                     "─────…─────Read file  Read(/etc/hostname)Do you want to proceed?"
<option 1>                           "❯1.Yes"
<option 2>                           " 2.Yes, allow reading from etc/ during this session"
<option 3>                           " 3.No"
<dim bottom hint>                    "Esctocancel · Tab to amend"
```

The "during this session" wording (vs Bash's "from this project") is a
content-level difference that the README documents but the spike
doesn't exploit — both options are option-2 from the spike's
keystroke-injection perspective.

## JSONL events recorded during/around each modal

### Probe 1 (Session A, observation window 5 s) — ZERO events

`events=0` across all green runs. Post-hoc `jq -c '.type' <session-a-jsonl>`
shows envelope types that DID arrive (startup envelopes only — none
during the modal window):

| Envelope type | When |
|---|---|
| `permission-mode` | session boot |
| `file-history-snapshot` | session boot |
| `user` | spike's prompt write |
| `attachment` | × 2 (claude metadata) |

**No `assistant` envelope arrives while the modal is up.** Claude
defers `assistant(stop_reason=tool_use)` until the modal is approved
or cancelled. This is the load-bearing finding for the layer-2 design:
**modal detection must be PTY-side.**

### Probe 2 (Session B, auto-respond) — normal tool-use sequence after approve

Post-approve the JSONL has the same shape as spike #11 Probe 2's
tool-use turn:

| idx | type | role | stop_reason | content | arrived |
|---|---|---|---|---|---|
| n | `assistant` | assistant | `tool_use` | `thinking` block | after approve, before tool executes |
| n+1 | `assistant` | assistant | `tool_use` | `tool_use` block | after thinking |
| n+2 | `user` | user | — | `tool_result` block | after tool subprocess finishes |
| n+3 | `assistant` | assistant | `end_turn` | `text` block | claude's final response describing /tmp |

No new envelope types. No new `stop_reason` values. The modal is
purely a PTY-side affordance with no JSONL trace.

### Probe 3 (Session B, escalate window 3 s) — zero events while modal open

Same as Probe 1 — the modal is up; claude has not written
`assistant(tool_use)` yet; `escalationWindow` of 3 s expires with no
events. `modal-still-open=true` confirms the modal didn't auto-clear.

## Pattern-detection regex shape

Two predicate variants implemented; `literal-text` is the default and
production-recommended.

### Cheap predicate (`-modal-predicate box-drawing`)

```go
var boxDrawingBytes = []byte("╭╮╰╯│─┌┐└┘├┤")
func hasModal(snap []byte) bool {
    stripped := ansiRe.ReplaceAll(snap, nil)
    return bytes.ContainsAny(stripped, string(boxDrawingBytes))
}
```

**False-positive baseline:** `idle-predicate-check has_modal=true` at
idle, before any prompt. Claude's input box uses `╭ ╰ │` characters as
its border. The cheap predicate is unusable as-is; it would fire
immediately on every session start.

### Recommended predicate (`-modal-predicate literal-text`, default)

```go
var modalLiteralTexts = [][]byte{
    []byte("Esctocancel"),
    []byte("Doyouwanttoproceed"),
    []byte("Do you want to proceed"),
}
func hasModal(snap []byte) bool {
    stripped := ansiRe.ReplaceAll(snap, nil)
    for _, lit := range modalLiteralTexts {
        if bytes.Contains(stripped, lit) {
            return true
        }
    }
    return false
}
```

**False-positive baseline:** `idle-predicate-check has_modal=false` at
idle. None of the three literal-text markers appear in the idle TUI.
**Production recommendation: ship the literal-text predicate.**

Note the dual-form `Doyouwanttoproceed` / `Do you want to proceed`:
Bash modal strips spaces via CSI cursor-forward (same as spike #1
finding #8 for the spinner verb), Read modal keeps spaces literal. Both
forms must be in the literal set.

## Modal text extraction strategy

Anchored on the last horizontal-dash run (`─{20,}`) BEFORE the
proceed-marker. Everything from the position after that run to the end
of the `Esctocancel`-containing line is the modal block. Per-line
cleanup strips leading/trailing box-drawing border characters and
whitespace; empty lines drop; lines join with `\n`.

This anchor strategy beats two alternatives that failed during
implementation iteration:

1. **Split on `modalSepRe`** (`─{20,}`) — fails for Read modal, where
   the dash separator is inline with the modal content on a single
   line; the split returns one part (the whole buffer including
   spinner-animation noise).
2. **Walk back N newlines from the marker** — fails because N must be
   different for the two modal shapes (Bash has tool header on a
   separate line above; Read has it inline with the marker). No single
   N works for both.

The last-dash-run anchor works robustly because both modal variants
prefix the modal block with a long horizontal-dash run, even though
the surrounding layout differs.

## Post-response state machine timing (Probe 2)

Observed across 5 green runs:

| Run | prompt-written → modal-detected | modal-detected → keystroke-sent | keystroke-sent → modal-cleared | keystroke-sent → end-turn-detected | total wall |
|---|---|---|---|---|---|
| 1 | 3.7 s | 0 ms | 5.3 s | 5.7 s | 24.7 s |
| 2 | 3.4 s | 0 ms | 5.4 s | 5.7 s | 27.2 s |
| 3 | 3.2 s | 0 ms | 4.8 s | 5.0 s | 24.7 s |
| 4 | 3.3 s | 0 ms | 5.0 s | 5.3 s | 25.2 s |
| 5 | 3.0 s | 0 ms | 4.4 s | 4.6 s | 24.2 s |

Observations:

- `modal-detected → keystroke-sent` is effectively instantaneous (the
  spike sends the keystroke immediately after the predicate fires).
- `keystroke-sent → modal-cleared` is dominated by claude's PTY
  quiescence requirement (1500 ms) plus tool execution + redraw. The
  Bash `ls /tmp` tool execution is fast (~50 ms) but claude's UI redraw
  + post-tool wind-down extends the window.
- `modal-cleared → end-turn-detected` is small (300–400 ms); the
  `assistant(end_turn)` envelope typically arrives within ms of the
  modal clearing.

## Escalation contract sketch

What a consumer callback would need to forward a permission modal to a
host UI (e.g. `pyry acp` → mobile app's "approve / deny" prompt). Each
field's extraction confidence noted; the spike validates the
extractable surface, the actual library API is post-spike work.

```go
type ModalEvent struct {
    // Text: the cleaned-up modal text, suitable for direct display in
    // a host UI's body field. Extraction is reliable across both Bash
    // and Read modals (the spike's run-by-run output is identical
    // shape). Confidence: HIGH.
    Text string

    // RawBytes: the full rolling-buffer snapshot at the moment the
    // modal was detected. ANSI + OSC sequences intact. Provided as
    // fallback for consumers that want to do their own parsing (e.g.
    // for color extraction or alternative-text fallback). Confidence:
    // HIGH (always available).
    RawBytes []byte

    // Tool: the tool name claude wants permission for (Bash, Read,
    // Write, Edit, …). Extracted via a known-token list (Bash, Read,
    // Write, Edit, Glob, Grep, WebFetch, WebSearch, Task). Confidence:
    // HIGH for known tools, EMPTY string for tools not in the known
    // list.
    Tool string

    // Options: the option list as extracted. Currently NOT populated
    // by the spike — would be a per-modal sub-parse. The 1/2/3
    // numbered list is consistent across both Bash and Read modals
    // observed, but the OPTION TEXT differs ("from this project" vs
    // "during this session"). Confidence: MEDIUM if added.
    Options []string

    // SuggestedResponse: the bytes that auto-respond WOULD have
    // written for the default "approve once" choice. For permission
    // modals this is always "1\r" (option 1 = Yes / once); for other
    // modal classes (if added later) this may differ. Confidence:
    // HIGH for permission modals; the layer 2 design needs to
    // propagate the modal class to the callback so this field can be
    // populated correctly per class.
    SuggestedResponse []byte
}
```

**Dangerous tools to flag explicitly** (per spec security-review
addendum): `Bash`, `Write`, `Edit`. The consumer's auto-approve policy
should NOT auto-approve these without explicit user gesture; the
permission modal exists precisely so that grant happens at a known
gesture point. The library API should make "auto-approve this tool by
default" hard to misconfigure for dangerous tools — likely an opt-in
allowlist rather than a default-deny block list.

## Surprises / findings

### 1. The modal has ZERO JSONL footprint

The pre-spike expectation was either (a) a new envelope type, (b) a new
`stop_reason` value, or (c) embedded data in an existing `assistant`
event. None of those — the modal is purely PTY-side. The first
`assistant` envelope of the turn arrives only AFTER the modal is
approved. Consequence: modal detection MUST be PTY-side (rolling-buffer
pattern matching); the assistant-only JSONL tailer filter from spike #11
catches nothing related to the modal. Architecturally consistent with
cancellation (spike #11 finding #12, where cancellation also lives on
a separate signal path from the canonical state).

### 2. Modal text uses TWO different space-stripping patterns

The Bash modal uses CSI cursor-forward (`\x1b[1C`) between words —
stripped buffer has NO interword spaces (`Doyouwanttoproceed`). The
Read modal uses literal spaces — stripped buffer has normal spacing
(`Do you want to proceed`). Same root cause as spike #1 finding #8 for
the spinner verb, but applied selectively. Why claude picks one
rendering vs the other per tool is unknown; the detection +
extraction logic must handle both variants.

**Implication for the library API:** the literal-text predicate's
substring list must include both spaced and unspaced forms of the
canonical marker phrases. The library's modal-class detector cannot
rely on a single regex shape.

### 3. Box-drawing predicate has unworkable false-positive rate at idle

`hasModal(snap, modalPredBoxDrawing)` returns `true` at idle (before
any prompt) because claude's input box uses box-drawing characters
(`╭ ╰ │`) for its border. The cheap predicate is structurally
unusable as the production library's modal-class detector. Document
+ ship the literal-text predicate as the default.

A future enhancement could use a region-restricted box-drawing check
(e.g. "any box-drawing char NOT inside the input box's known position")
but that requires tracking the input box's rolling-buffer position,
which adds complexity. Not worth pursuing unless the literal-text
predicate fails for some claude version we haven't yet seen.

### 4. Three approve keystrokes are equivalent for the "Yes / once" grant

`1\r`, `y\r`, and bare `\r` all approve the modal with the same grant
("Yes, once" — option 1). The down-arrow variant (`\x1b[B\r`) selects
option 2 ("Yes, allow X from this project / during this session"), a
broader grant. **Choice matters for the library's auto-approve policy:**
- `1\r` / `y\r` / `\r` = single-grant, narrowest scope
- `\x1b[B\r` = session/project-scope grant

The library should expose this explicitly; the consumer policy chooses
which to send based on what the host UI's gesture means.

### 5. Modal text extraction via last-dash-run anchor is robust

Initial extraction strategy (`modalSepRe.Split` then take the segment
containing the proceed-marker) fails for the Read modal because the
dash separator is inline with the content. Walk-back-from-marker
strategies fail because the right walk-back distance depends on the
modal variant (Bash has multi-line header; Read has inline header).

The strategy that works: find the LAST `─{20,}` run BEFORE the
proceed-marker; start extraction from the position immediately after
that run; end at the line containing `Esctocancel`. Robust across both
Bash and Read modal variants observed. Documented in
`extractModalText`'s comment block.

### 6. Session A's shutdown with modal open is clean

Probe 1 SIGTERMs claude while the modal is still rendered on screen.
No claude orphans across all observed runs (`pgrep -lf 'claude
--session-id'` returns nothing post-exit). Claude handles SIGTERM
during a permission prompt the same way it handles SIGTERM in any
other state — clean exit. No "stuck modal" failure mode observed.

### 7. `❯-disappeared` log line was never emitted

Same as spike #11 — under the spike's "wait for state, then act"
access pattern, claude's `❯` doesn't visibly disappear from the
rolling buffer between input acceptance and the modal rendering. The
optional log line that was reserved for this event never fires across
any observed run. The state machine works correctly without it; just
noted for completeness. (The spike-permission binary doesn't include
this observer — the spec didn't require it for this class of probe.)

### 8. `modal-detected → response-keystroke-sent` is effectively zero

The spike's poll interval is 50 ms, so the moment `hasModal` first
returns true, the next ticker iteration sends the keystroke. The
observed gap is well under 1 ms in the timing logs (Go's `log` library
truncates to microsecond resolution, so 0 ms is the floor). This is
worth noting because it means the modal doesn't need to be
"stable for N ms" before responding — the first detection is reliable
(no observed flake where the predicate fires then unfires before the
keystroke can be sent).

### 9. Claude's modal "second option" wording varies by tool

The Bash modal's option 2 says "Yes, allow reading from tmp/ from this
project" — a project-scoped grant. The Read modal's option 2 says
"Yes, allow reading from etc/ during this session" — a session-scoped
grant. Different scope semantics per tool. The library's auto-approve
policy should be aware that "option 2" is not consistent across tools;
the SuggestedResponse field above for "approve once" (option 1) is the
universally safe choice.

## Follow-up tickets the spike's findings justify

The spike validates layer 2 of the architecture's three-layer modal
handling. Concrete follow-ups (none filed):

1. **`pkg/tuidriver/` library API extraction** — primary post-spike
   next move. The escalation contract sketch above plus the predicate
   + extractor surface from this binary feed directly into the library
   API design.
2. **Multiselect modal spike** (`/doctor`, slash-command pickers) —
   different structural shape (multiple options with checkbox state).
   Sibling spike per the original spec.
3. **Trust-workdir prompt** — fires once per workdir; operator runbook
   concern more than library concern, but if `pyry acp` needs to
   handle it for first-time spawns, file a small spike.
4. **Layer 3 — bail-safely on unknown modal shape** — trivial once
   layer 2's detection primitives exist; a no-modal-shape-matches
   case in the library returns "unknown modal" to the consumer with
   the raw bytes for diagnosis.
5. **Parallel tool-use stress test** — still outstanding from spike #2.
   Could fold into a Read+Write or Read+Bash combined prompt that
   triggers parallel permission modals (would also exercise modal-
   sequencing semantics).

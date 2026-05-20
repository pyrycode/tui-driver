# Spec: spike — `WritePrompt` regression rig against real claude

**Ticket:** [#47](https://github.com/pyrycode/tui-driver/issues/47)
**Size:** S
**Status:** ready for development

## Files to read first

Turn-1 reading list — load these before writing any code. Paths are relative to repo root.

- `cmd/spike-one-turn/main.go` (entire file, 436 LOC) — direct template. The new binary copies this end-to-end and swaps two things only: (a) `promptText` constant becomes a fixture read at runtime, (b) the `ptmx.Write` call becomes `session.WritePrompt`, (c) success path adds a substring assertion. Everything else (UUID resolution, projects-dir, JSONL discovery, JSONL tailer, watchdog, shutdown, log line set) is reused verbatim.
- `cmd/spike-one-turn/README.md` — README shape to mirror.
- `pkg/tuidriver/session.go:146-160` — `Session.WritePrompt` contract and `bracketedPaste` wire shape. Read this so you understand WHY the spike must call `WritePrompt` and NOT `session.Write` / `ptmx.Write` (the whole point of the regression rig).
- `cmd/e2e-runner/main.go:38-42` — `successSuccess` vs `observedSuccess` constants. New entry uses `successSuccess` (the spike emits `SUCCESS: <text>` on match).
- `cmd/e2e-runner/main.go:205-283` — `buildChecks` table; the new entry goes at the end of the spike block (after `spike-ask-user`, before `probe-first-prompt-hang`).
- Vault `📋 Projects/2026-05-16 - tui-driver/Session Logs/2026-05-19.md` § *Fourth pivot — Session.WritePrompt for long-prompt support (PR #43)* — empirical record of the 3.5 KB prompt that PR #43 was validated against. Reuse the content shape for the fixture if convenient.
- Vault `📋 Projects/2026-04-10 - Pyrycode/PTY-Drive Recon.md` § *Open uncertainties* — long-prompt finding + ptyrunner (#468) consumer relationship.

## Context

`Session.WritePrompt` (PR #43, merged 2026-05-19) wraps the input in bracketed-paste markers (`\x1b[200~ … \x1b[201~\r`) so claude's auto-paste-detection does not swallow the trailing `\r` and leave the input pending. It was validated once empirically against claude 2.1.144 with a 3.5 KB multi-line prompt. None of the existing six spike binaries call it — they all use short single-line prompts via raw `session.Write` / `ptmx.Write`. There is no regression rig.

This ticket adds `spike-long-prompt` to the e2e check list. It runs against real claude under `make e2e` and fails fast if any of three drift vectors trips:

1. tui-driver's bracketed-paste wire shape diverges from what claude expects.
2. claude's auto-paste-detection heuristic changes (threshold / handling).
3. claude truncates or mis-receives a multi-KB multi-line prompt in a way single-line spikes cannot surface.

The downstream consumer that motivates this is pyrycode/pyrycode#468 (`pyry agent-run` via ptyrunner). Catching drift here is much cheaper than letting it surface in that consumer.

## Design

### Layout

```
cmd/spike-long-prompt/
  main.go                       # binary; mirrors cmd/spike-one-turn/main.go
  testdata/long-prompt.txt      # committed fixture, 3-5 KB, three tokens embedded
  README.md                     # purpose, fixture description, expected output, version dep
```

No new packages, no `pkg/tuidriver/` changes, no helper extraction. Per ticket "Out of scope": the scaffolding duplication across the seven spikes is a known refactor, but is its own ticket — #47 follows the established per-spike pattern.

### Binary (`cmd/spike-long-prompt/main.go`)

Start from a verbatim copy of `cmd/spike-one-turn/main.go`. Apply three localized changes; everything else (UUID resolution, JSONL discovery, tailer, watchdog, shutdown, state log lines, fast-path/slow-path spinner handling) is untouched.

**Change 1 — fixture-driven prompt.** Replace the `promptText` const with a runtime read.

- Drop `const promptText = "What is 2+2?\r"`.
- After flag parsing, locate the fixture relative to the running binary's package. Use `embed.FS` (`//go:embed testdata/long-prompt.txt`) — preferred over `os.ReadFile`, because `go test` and `go run ./cmd/spike-long-prompt` from any cwd both resolve embed paths identically. The fixture becomes part of the binary, which is what we want for a regression rig: the embedded bytes ARE the asserted-against contract.
- Strip a single optional trailing newline before sending (UNIX text files conventionally end with `\n`; we do not want to send that into the paste body if it would confuse claude). Concretely: `promptBody := strings.TrimRight(rawPrompt, "\n")`. Do NOT trim other whitespace — interior newlines, leading whitespace, indentation are part of the test surface.
- Log `prompt-loaded bytes=<n>` before `prompt-written`.

**Change 2 — `WritePrompt` not `Write`.** Replace the existing `ptmx.Write([]byte(promptText))` call with `session.WritePrompt(promptBody)`. Use the `Session` value returned by `tuidriver.Spawn` (the variable is already named `session` in `spike-one-turn/main.go`). Do NOT pass `\r` through the fixture or append one in the binary — `WritePrompt` adds the commit `\r` outside the bracketed-paste close marker itself (see `pkg/tuidriver/session.go:154-160`).

**Change 3 — substring assertion at success path.** Where `spike-one-turn` prints `SUCCESS: <assistantText>` unconditionally, this spike:

1. Computes `trimmed := strings.TrimSpace(assistantText)`.
2. Defines `const expectedTokens = "ALPHA_42-GAMMA_88-OMEGA_13"`.
3. If `strings.Contains(trimmed, expectedTokens)`: print `SUCCESS: <trimmed>` to stdout, return nil (exit 0). The `SUCCESS:` prefix matches `successSuccess` in the e2e-runner — do not change it.
4. Else: return an error with shape `fmt.Errorf("FAIL: substring %q not found in %q", expectedTokens, trimmed)`. The existing `main` already prints `spike failed: %v` and exits 1 — that produces a `FAIL:` substring in stderr matching the ticket's failure-message contract. (Exit code is what the harness actually keys off; the `FAIL:` text is for human triage.)

Substring (not exact-equality) is deliberate. The regression target is "claude received all three tokens", NOT "claude obeyed the formatting instruction perfectly". Exact-equality would produce noisy false-negatives whenever claude prepends `OK:`, adds a trailing newline, or wraps the answer in backticks. The fixture's instructions ask for tokens-joined-by-hyphens-on-a-single-line, and the assertion verifies the joined form appears somewhere in the response.

**Watchdog timeout headroom.** The existing 60 s per-state inactivity watchdog in `spike-one-turn` is also fine here — claude responded to the 3.5 KB validation prompt in well under 30 s during PR #43's empirical run. Do not raise the watchdog limits speculatively. If the spike trips on real claude in CI because the prompt is heavier than `"What is 2+2?"`, that is an empirical signal worth surfacing — note it in the README and file a follow-up; do not pre-emptively widen.

### Fixture (`cmd/spike-long-prompt/testdata/long-prompt.txt`)

Size 3-5 KB, multi-line, structured to exercise bracketed-paste handling against varied content:

- Opens with a one-line instruction: `Respond with all three tokens, separated by hyphens, on a single line. Nothing else.`
- Three token markers embedded in distinct positions:
  - Near the start: `[TOKEN_START: ALPHA_42]`
  - Roughly the middle: `[TOKEN_MIDDLE: GAMMA_88]`
  - Near the end: `[TOKEN_END: OMEGA_13]`
- Mixed structural content between markers:
  - At least one prose paragraph wrapping past 80 columns.
  - At least one bullet list (3-5 items).
  - At least one fenced code-block-shaped region (` ``` ` markers around indented sample text) — exercises pasted content that LOOKS like markdown to claude's renderer.
  - At least one indented block (4-space leading indent) and one tab-indented line — both are common in real prompts.
  - At least one very long single line (>200 chars, no spaces past the first 100) — exercises long unbroken runs.
- Closes with a one-line reminder: `End of input. Token reminder: respond ONLY with the three tokens joined by hyphens.`
- File ends with a single `\n`. The binary strips it before sending (see Change 1).

Vault `📋 Projects/2026-04-10 - Pyrycode/PTY-Drive Recon.md` § *Open uncertainties* documents the exact content shape used in the 2026-05-19 empirical probe — reuse it verbatim if it satisfies the constraints above. Otherwise compose freely; the constraints, not the prose, are what matter.

The fixture is a regression-rig contract. Do not edit it casually post-merge. If a future change to it is needed (e.g. new fixture shape for a different drift case), file a new ticket — do not silently widen this one.

### Harness wiring (`cmd/e2e-runner/main.go`)

Add a single `Check` entry at the end of the spike block, **after** `spike-ask-user` (line ~259) and **before** `probe-first-prompt-hang` (line ~261). Shape:

```go
{
    Name:          "spike-long-prompt",
    Kind:          "spike",
    Binary:        "spike-long-prompt",
    Args:          commonArgs,
    SuccessMarker: successSuccess,
},
```

No `Timeout` field — the default per-check timeout (60 s, the constant the existing spikes inherit) applies. No `OnFailure` / `OnComplete` callbacks — same as `spike-one-turn`. No version-lock changes — bracketed-paste is a terminal feature, not a claude CLI flag.

Confirm the build target picks up the new binary automatically. The Makefile's `e2e` target enumerates `cmd/spike-*` (verify by reading the `Makefile` target itself — do not guess); if it does not, add `spike-long-prompt` to whatever explicit list controls compilation. Adding a binary that does not compile under `make e2e` would silently skip the check.

### README (`cmd/spike-long-prompt/README.md`)

Mirror the structure of `cmd/spike-one-turn/README.md` (status, what-it-does, how-to-run, observed-timings, surprises). One spike-specific section to add:

- **What's being tested** — the three drift vectors enumerated in the ticket § *Context*. Link to PR #43 and the 2026-05-19 vault session log.
- **Claude version dependency** — note that bracketed-paste is a terminal feature, not a CLI flag, so this spike does NOT contribute to `claude-version.lock`. If claude's TUI changes its paste-detection threshold or markers, this spike trips; the version-lock check stays clean. That separation is intentional.

## Concurrency model

Unchanged from `spike-one-turn`. Three goroutines coordinated by a single `context.Context`: orchestrator (state machine + 1 Hz watchdog tick), PTY reader (managed inside `tuidriver.Spawn`), JSONL tailer. Shutdown sequence and `wg.Wait` semantics also unchanged.

## Error handling

Two named failure surfaces, both already in place in `spike-one-turn`'s scaffolding — the spike just adds a third:

1. Watchdog timeouts (60 s per-state inactivity, 30 s spinner freeze). Surface via `cancelCause` and propagate out of `run`. No change.
2. PTY / JSONL / spawn errors. Surface via `fmt.Errorf` from `run`. No change.
3. **New: substring-assertion failure.** Returned as `fmt.Errorf("FAIL: substring %q not found in %q", expectedTokens, trimmed)`. This is a hard failure (exit 1) — claude responded, the turn terminated cleanly, but the response did not contain the expected tokens. The harness records the check as failed; the human reads stderr to see the actual `trimmed` text.

No retries. The whole point of a regression rig is that one failure trips the alarm.

## Testing strategy

This spike is verified by execution against real claude under `make e2e`, not by unit tests. No `*_test.go` companion (matches the six existing spikes).

Acceptance scenarios:

- **Happy path against current claude.** `go build ./cmd/spike-long-prompt && ./spike-long-prompt -trust-folder=accept` from a directory with a valid claude subscription session. Stdout shows `SUCCESS: ALPHA_42-GAMMA_88-OMEGA_13` (possibly with surrounding text). Exit 0. Stderr shows the full state log line set from `spike-one-turn`, plus `prompt-loaded bytes=<n>` before `prompt-written`.
- **Failure visibility.** Temporarily corrupt the fixture (delete the middle token line) and re-run. Confirm: exit 1, stderr contains `FAIL: substring "ALPHA_42-GAMMA_88-OMEGA_13" not found in "..."`. Revert. (Do not commit the corrupted fixture; this is a manual smoke test the dev runs once to confirm the assert wires up correctly.)
- **Harness integration.** `make e2e` includes `spike-long-prompt` in its run, marks it pass, and the resulting `e2e-report.json` contains an entry for it with `ok=true`.

Negative-path validation already covered by the `spike-one-turn` scaffolding (kill claude externally → spike exits cleanly within seconds). No new negative path here.

## Out of scope (reminder, from ticket)

- Stress-testing with multi-MB prompts.
- Bracketed-paste edge cases (empty, control-chars-only, paste-during-modal) — `pkg/tuidriver/session_test.go` covers the wire-shape side; consumer-side cases are not this ticket.
- Extracting shared spike scaffolding into a helper package — separate refactor ticket.
- Updating `claude-version.lock` — bracketed-paste is terminal-layer, not CLI-layer.

## Open questions for the dev

These are left for empirical answer in the README, not pre-decided here:

- **How long does claude take to respond to the 3-5 KB prompt under `make e2e` conditions?** Record wall-clock in the README. If it consistently exceeds 30 s, surface as a finding — do not silently raise the watchdog.
- **Does the verb on the spinner differ noticeably for a long structured prompt vs `"What is 2+2?"`?** Log it; the README accumulates the observed set.
- **Does claude ever emit intermediate `assistant` events with `tool_use` for a prompt this size?** Unlikely for the fixture shape, but if observed, document — the substring-on-final-end-turn-text assert may need broadening if the assistant streams the tokens across multiple records. Today's `extractAssistantText` concatenates only the final `end_turn` event's content; if that becomes insufficient, file a follow-up.

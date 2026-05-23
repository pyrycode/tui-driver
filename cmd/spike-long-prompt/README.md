# spike-long-prompt

Regression rig for `Session.WritePrompt` against real `claude`. Drives one
interactive turn end-to-end through a PTY, sending a multi-KB multi-line
prompt body via the bracketed-paste path shipped in
[PR #43](https://github.com/pyrycode/tui-driver/pull/43). See
[ticket #47](https://github.com/pyrycode/tui-driver/issues/47) and
`docs/specs/architecture/47-spike-long-prompt.md`.

## Status

New (2026-05-19). `make e2e` enrolls this spike alongside the existing six.
The 3.5 KB content shape was validated once during PR #43 against
`claude 2.1.144`; this binary turns that one-off probe into a standing check.
Empirical observations (timings, thinking-verb set, response shape) accumulate
in the *Observed timings* and *Surprises* sections as the rig runs.

## What's being tested

Three drift vectors. Any one of them tripping fails the check:

1. **tui-driver's bracketed-paste wire shape diverges from what claude expects.**
   `Session.WritePrompt` wraps input in `\x1b[200~ … \x1b[201~\r`; if either
   marker or the terminating `\r` changes shape, claude no longer commits the
   turn and the spike's 60 s inactivity watchdog fires.
2. **Claude's auto-paste-detection heuristic changes.** The motivating bug
   (input rate-based paste auto-detection swallowing the trailing `\r` and
   parking the prompt in the input area as `[Pasted text +N lines]` chips) is
   what `WritePrompt` exists to defeat. If claude's threshold or marker
   handling changes such that the explicit markers no longer suppress the
   heuristic, the same wedge re-appears here.
3. **Claude truncates / mis-receives a multi-KB multi-line prompt.** The
   existing six spike binaries all use single-line prompts via raw
   `session.Write`; none would surface this drift. The fixture's three token
   markers (start/middle/end positions) make truncation observable: any
   dropped token fails the substring assertion.

The downstream consumer this protects is
[pyrycode/pyrycode#468](https://github.com/pyrycode/pyrycode/issues/468)
(`pyry agent-run` via ptyrunner). Catching drift here is much cheaper than
letting it surface in that consumer.

Empirical record of the original validation:
[vault: 📋 Projects/2026-05-16 - tui-driver/Session Logs/2026-05-19.md § Fourth pivot](obsidian://open?file=📋%20Projects%2F2026-05-16%20-%20tui-driver%2FSession%20Logs%2F2026-05-19.md).

## What it does

1. Resolves a session ID — generates a fresh UUIDv4 by default, or accepts
   one via `-session-id <uuid>` (validated; bad UUID exits 2 before claude
   spawns). Computes the deterministic JSONL path and logs
   `session-id-resolved` *before* spawn.
2. Allocates a 120×40 PTY via `github.com/creack/pty`.
3. Spawns `claude --session-id <uuid>` attached to the slave side.
4. Reads the PTY master into a 4 KB rolling buffer, mirroring raw bytes to
   stderr so a human watching the spike sees claude's UI live.
5. Waits for the idle prompt (`❯` glyph present and no `✻` spinner).
6. Loads the embedded fixture `testdata/long-prompt.txt` (compiled into the
   binary via `//go:embed`) and strips a single optional trailing `\n`.
7. Sends the body via `session.WritePrompt(promptBody)`. **Not** `session.Write`
   / `ptmx.Write` — the whole point is to exercise the bracketed-paste path.
8. Calls `tuidriver.WaitForSessionJSONL(ctx, jsonlPath)` under a 10 s
   `context.WithTimeout` deadline (the library polls at `DefaultPollInterval`,
   50 ms); under `--session-id` claude defers JSONL creation until first input
   arrives.
9. Calls `session.Events(ctx, jsonlPath, 0)` and ranges the unified PTY+JSONL
   channel; the library's per-entry end-of-turn discriminator (`IsEndTurn`:
   `end_turn` + non-empty text) emits `EventKindJsonlEndOfTurn` carrying the
   matching `JSONLEntry`. PTY `EventKindPtyThinking` / `EventKindPtyIdle` events
   drive the opportunistic `thinking-detected` / `spinner-gone` log pair on the
   slow path.
10. Reads the assistant text via `tuidriver.AssistantText(ev.Entry)`.
11. Asserts that `strings.TrimSpace(assistantText)` **contains** the substring
    `ALPHA_42-GAMMA_88-OMEGA_13`. On match: prints `SUCCESS: <trimmed>` to
    stdout, exits 0. On mismatch: returns an error of shape
    `FAIL: substring "ALPHA_42-GAMMA_88-OMEGA_13" not found in "<trimmed>"`,
    which `main` prints as `spike failed: <err>` to stderr and exits 1.
12. SIGTERMs `claude`, races a 3 s timer against `cmd.Wait()`, SIGKILLs if
    timer wins, closes the PTY, drains goroutines.

A watchdog ticks at 1 Hz throughout, enforcing the same two deadlines as
`spike-one-turn`: 60 s since the last state transition; 30 s since the
spinner counter last incremented (while the spinner regex still matches).

### Why a substring assertion (not exact-equality)

The regression target is "claude received all three tokens", **not** "claude
obeyed the formatting instruction perfectly". Exact-match would conflate the
two — every time claude prepends `OK:`, wraps the response in backticks, or
adds a trailing newline, the spike would trip with a noisy false-negative
even though the bracketed-paste path worked correctly. Substring containment
checks the wire-shape contract; the formatting instruction in the fixture is
there to nudge claude toward a tight response, not as part of the assertion.

## The fixture

`testdata/long-prompt.txt` is a ~3.5 KB body structured to exercise
bracketed-paste handling against varied content:

- Opens with `Respond with all three tokens, separated by hyphens, on a single line. Nothing else.`
- Three token markers at start / middle / end:
  `[TOKEN_START: ALPHA_42]`, `[TOKEN_MIDDLE: GAMMA_88]`, `[TOKEN_END: OMEGA_13]`.
- A prose paragraph wrapping past 80 columns.
- A short bullet list (4 items).
- A fenced code-block-shaped region (` ``` ` markers around indented sample text).
- A 4-space-indented log-shaped block.
- One tab-indented line.
- One unbroken single line >250 chars with no spaces past the first ~40 chars.
- Closes with `End of input. Token reminder: respond ONLY with the three tokens joined by hyphens.`

The fixture is compiled into the binary via `//go:embed` (preferred over
`os.ReadFile`, because `go test`, `go run`, and built-binary execution from
any cwd all resolve embed paths identically — the embedded bytes ARE the
asserted-against contract).

The fixture is a regression-rig contract. **Do not edit it casually
post-merge.** If a future change is needed (e.g. a different drift case), file
a new ticket — do not silently widen this one.

## How to run

Requirements:

- `claude` 2.1.x on `$PATH`, authenticated against a Claude subscription.
- Go 1.26+ (`go.mod` pins `go 1.26.2`).

```sh
go build -o /tmp/spike-long-prompt ./cmd/spike-long-prompt
/tmp/spike-long-prompt -trust-folder=accept
```

On success, stdout is one line — the trimmed assistant text, beginning with
`SUCCESS:` and containing the joined token string somewhere within:

```
SUCCESS: ALPHA_42-GAMMA_88-OMEGA_13
```

(Claude may decorate it — e.g. `SUCCESS: OK: ALPHA_42-GAMMA_88-OMEGA_13` —
which still passes; only the substring contract matters.)

Stderr contains the raw claude UI bytes interleaved with the state log lines.

### Optional flags

- `-session-id <uuid>` — pin claude's session ID (and therefore the JSONL
  filename). Default: empty value triggers a fresh UUIDv4 at startup.
- `-trust-folder=accept` — auto-trust the current cwd if claude prompts for
  it (sends `1\r` to the trust-folder modal). Default `fail` returns a clear
  error if the modal appears.

### Required state log lines (in order)

```
session-id-resolved id=<uuid> jsonl=<path>   # fires before pty.Start
idle-detected
prompt-written
session-jsonl-opened path=<path> offset=0    # fires after WaitForSessionJSONL returns
thinking-detected verb="<captured>"          # slow path only
spinner-gone                                 # slow path only
end-turn-detected
assistant-text-extracted len=<n>
shutdown-signalled
```

### Failure visibility

If the substring assertion trips, stderr contains
`spike failed: FAIL: substring "ALPHA_42-GAMMA_88-OMEGA_13" not found in "<trimmed>"`
followed by `shutdown-signalled`. Exit code is 1. The harness records the
check as failed; the human triages by reading the `<trimmed>` text — that
shows what claude actually returned, which usually reveals whether a token
was dropped (paste truncation) or claude responded with something
unrelated (paste-detection wedge swallowed the body).

To smoke-test the failure path manually: temporarily delete the
`[TOKEN_MIDDLE: GAMMA_88]` line from the fixture, rebuild, run. Restore
before committing.

## Observed timings

Populated empirically as the rig runs. Columns mirror `spike-one-turn`'s
table.

| Run | spawn → idle | idle → prompt-loaded | prompt-loaded → jsonl-opened | jsonl-opened → end-turn | total | outcome |
|-----|--------------|----------------------|------------------------------|--------------------------|-------|---------|
|  1  | 354 ms       | 0.041 ms             | 266 ms                       | 2.606 s                  | 3.23 s | **SUCCESS** — `SUCCESS: ALPHA_42-GAMMA_88-OMEGA_13` against installed `claude 2.1.145`; fast path (`thinking-detected` never logged — spinner appeared briefly as `✻ Brewed for 2s` but rendered via interleaved per-character glyph updates so the regex didn't match, same shape as `spike-one-turn` finding #8). Prompt body 3454 bytes. |

The single recorded run sits well under the watchdog deadlines (60 s state inactivity / 30 s spinner freeze) so no widening is justified at this point.

## Observed thinking verbs

| Verb (captured) | Run(s) | Notes |
|-----------------|--------|-------|
| (none captured) | 1      | `✻ Brewed for 2s` rendered in the raw PTY bytes but the spinner regex never matched — the glyph and verb are emitted via interleaved CSI cursor-positioning + per-character writes (same shape as `spike-one-turn` finding #8). The fast path triggered because the JSONL `end_turn` event arrived ~2.6 s after `session-jsonl-opened`, and the state machine accepts that even when no spinner has been recognised. |

Known-good verbs from sibling spikes: `Baked`, `Brewing`, `Brewed`, `Skedaddling`, `Whipped up`, `Cooking`.

## Surprises / findings

### Run 1 (2026-05-20, claude 2.1.145)

- **Total wall-clock 3.23 s** with a 3454-byte fixture. The architect's open question ("If it consistently exceeds 30 s, surface as a finding") is answered: well under. No watchdog widening warranted.
- **Fast path taken** — same shape as `spike-one-turn` Run 5. The spinner regex never matched even though `✻ Brewed for 2s` appeared in the PTY bytes; the JSONL `end_turn` event arrived first. Confirms that the substring-on-final-`end_turn`-text assertion is sufficient for the fixture shape (no intermediate `assistant` events with `tool_use`).

## Claude version dependency

**This spike does NOT contribute to `claude-version.lock`.**

Bracketed-paste is a terminal-layer feature, not a claude CLI flag. The
sequences `\x1b[200~` (paste-on) and `\x1b[201~` (paste-off) are part of the
xterm bracketed-paste protocol; claude's TUI consumes them, but they do not
appear in `claude --help`. So `claude-version-lock`'s flag-verification
machinery has nothing to assert about this path.

If claude's TUI ever changes its paste-detection threshold, or the markers
it accepts, or the timing relationship between marker arrival and `\r`
commit, **this spike trips while the version-lock check stays clean**. That
separation is intentional — version-lock guards CLI-surface compatibility;
this spike guards TUI-behavioural compatibility. Two different drift
surfaces, two different alarms.

## Why no automated unit tests

Same rationale as the six sibling spike binaries (see
`cmd/spike-one-turn/README.md § Why no automated tests`): the deliverable is
empirical knowledge against real `claude`, not reusable code. The reusable
primitives live in `pkg/tuidriver/` and have their own unit-test suite
(`pkg/tuidriver/session_test.go` covers `bracketedPaste` wire-shape). This
binary IS the test rig.

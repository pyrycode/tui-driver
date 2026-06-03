# spike-short-prompt

End-to-end PTY drive of one interactive `claude` turn using a **short,
single-line** prompt delivered through `Session.DeliverPrompt`. Regression rig
for the short-prompt stall: a short single-line prompt renders inline with no
`[Pasted text]` chip, so before the fix `DeliverPrompt`'s chip-based recovery
could not tell a genuinely-uncommitted bracketed paste from a committed-but-slow
one, and the run went idle until the watchdog killed it.

## What it does

1. Resolves a session ID (fresh UUIDv4 by default, or `-session-id <uuid>`) and
   computes the deterministic JSONL path.
2. Spawns `claude`, waits for the `❯` idle prompt, optionally accepts the
   trust-folder dialog (`-trust-folder accept`).
3. Delivers the short prompt `"What is 2+2?"` via `Session.DeliverPrompt` — the
   deliver-confirm-recover loop under test. This is the whole point: `SendKeys`
   (used by spike-one-turn) bypasses the bug; `DeliverPrompt` exercises it.
4. Bounds the wait for a real assistant `end_turn` with a 25s turn-deadline. If
   no turn commits in time, the prompt wedged and the spike fails with a clear
   `STALL:` message instead of waiting for the 60s PTY-quiet watchdog.
5. On a committed turn, prints `SUCCESS: <assistant text>`.

## The fix it guards

`DeliverPrompt` now routes short single-line prompts (no newline, `<=
typePromptMaxLen` bytes) through `TypePrompt` (byte-spaced body + isolated `\r`,
the #71 / PR #77 primitive) instead of a bracketed paste. Typing keeps the byte
stream under claude's paste-detection threshold so the trailing Enter is not
absorbed and the turn commits reliably. Long or multi-line prompts keep the
bracketed-paste path, whose chip drives the corrupted-paste recovery.

## Empirical result (live claude 2.1.158, haiku/low, ambient MCP churn)

Driving the **same spike** through `DeliverPrompt`, 12 runs each:

| build | commit | stall |
| --- | --- | --- |
| before the fix (bracketed paste) | 3 | 9 |
| after the fix (TypePrompt for short prompts) | 12 | 0 |

The stalls all showed `committed=true attempts=1` (the no-chip branch wrongly
declaring commit) followed by the session JSONL never appearing — the prompt
never actually committed. Run under `make e2e` it executes with
`TUIDRIVER_STRICT_MCP_CONFIG=1`, where the cold-start churn is removed and the
typed path commits cleanly.

# Spec #178 — snapshot-drift: re-record fixtures against claude 2.1.199 + bump lock `version=`

**Ticket:** [#178](https://github.com/pyrycode/tui-driver/issues/178) — snapshot-drift: reconcile fixtures with claude 2.1.199 (Option C re-record) + bump `claude-version.lock` `version=`. Split from #176. Size **S** (`size:s`).

**One line:** Re-record the two surviving snapshot-drift JSON fixtures (`mcp`, `agents`) from a real claude **2.1.199** session via `make rerecord-snapshots`, and bump `claude-version.lock` `version=` `2.1.158`→`2.1.199`. **No `pkg/tuidriver/` parser change** — this is the "Option C" atomic bump-and-re-record (#66, #111, #128 precedent). Zero production `.go` files change.

---

## Files to read first

Everything the developer needs on turn 1. This is a data/config re-record, not a code change — the reading list is the *decision surface* (what makes a re-record "pure" vs. "needs a parser change and must be split").

- `cmd/e2e-snapshot-check/main.go:72-75` — the fixture table: exactly two cases, `{name:"mcp", trigger:"/mcp\r", settle:5s}` and `{name:"agents", trigger:"/agents\r", settle:0}`. **No picker case** (dropped in #129). These are the only two fixtures `make rerecord-snapshots` iterates.
- `cmd/e2e-snapshot-check/main.go:132-139` — the `-record` write path: on success prints `SNAPSHOT <name> recorded` and overwrites `pkg/tuidriver/testdata/<name>-snapshot.json` with the freshly-captured sidecar. This is what `make rerecord-snapshots` triggers per case.
- `cmd/e2e-snapshot-check/main.go:125-130` + `:178-188` (`enrichMissingSidecar`) — **the split-signal path.** A missing `.parsed.json` sidecar means the spike's `DetectModalClass` didn't return MCP/Agents and its `default` branch wrote nothing → the error is enriched with `spike classified modal as "<class>" and wrote no sidecar — classifier drift, not a content diff`. If the re-record hits this, the parser no longer recognises the 2.1.199 render → **STOP and split** (see § Decision gate).
- `cmd/e2e-snapshot-check/main.go:102-105` — the **unconditional** `TUIDRIVER_STRICT_MCP_CONFIG=1` env injection onto every spike child. This is the mcp fixture's host-portability guard: it forces the strict-mcp empty state (`total_servers: 0`) regardless of operator MCP config. Read to understand why a re-record on *this* host is still CI-portable.
- `cmd/spike-multiselect/main.go:287-326` — the `ModalClassMCP` and `ModalClassAgents` dispatch branches. **Key structural fact:** both write the sidecar **unconditionally** (unlike the picker branch at `:279-286`, gated on `len(items) > 0`). So as long as `DetectModalClass` classifies the render correctly, the sidecar is always produced and a pure re-record needs zero spike/parser change. Conversely, a *missing* sidecar can only be a classifier miss — the clean signal that a parser change (out of scope) is required.
- `pkg/tuidriver/testdata/mcp-snapshot.json` — current committed shape `{"total_servers": 0, "categories": null}`. The `total_servers: 0` guard must survive the re-record.
- `pkg/tuidriver/testdata/agents-snapshot.json` — current committed shape `{"tabs":["Running","Library"], "current_tab":"Running", "items":null, "empty_text":"No subagents are currently running."}`. The `items: null` + empty-text guard must survive the re-record.
- `Makefile:44-48` — the `rerecord-snapshots` target (`e2e-snapshot-check -record -bin-dir $(BIN_DIR)`) and its comment: *"Review with `git diff` before commit; bump `claude-version.lock` `version=` to match `claude --version` in the same commit."* The bump lives in this ticket, coupled to this sweep.
- `claude-version.lock` — the `version=2.1.158` line to bump (line 11). The header documents the informational-vs-enforced split: `version=` is informational; `flag=`/`value=` are substring-enforced. **Only `version=` changes** (see § Lock bump).
- `cmd/e2e-runner/main.go:427-479` (`runClaudeVersionLockCheck`) + `:493` (`evaluateClaudeVersionLock`) — the `claude-version-lock` check. Confirms `version=` is **not** compared against the installed binary (patch drift tolerated, #64); only the five `flag=`/`value=` entries are `strings.Contains`-checked against `claude --help`. Read to confirm the `version=` bump cannot break the check, and to know exactly what would (a missing flag/value).
- `docs/knowledge/features/e2e-harness.md` §"Snapshot re-record workflow" (the row at ~line 181: *"If the change is legitimate, `make rerecord-snapshots` to refresh the JSON fixtures, bump `claude-version.lock` `version=`, commit both together"*) and §"version-lock" (~line 99). **Read-only — do not edit this doc.** The canonical operator procedure this ticket executes.
- `docs/specs/architecture/128-mcp-empty-state-classifier.md` §Context + §"Slash-picker precedence" — the strict-mcp `/mcp` empty-state model being re-recorded, and the **per-case split precedent** the last Technical Note invokes.
- `docs/specs/architecture/129-drop-picker-snapshot-case.md` — why there are only two cases; **do not re-add or re-record a picker fixture.**
- `docs/specs/architecture/81-snapshot-drift-parsed-shape.md` — the parsed-shape `reflect.DeepEqual` comparison model (supersedes #35's raw-byte compare). Why re-recording the parsed sidecar — not patching bytes — is the only correct refresh.

---

## Context

`snapshot-drift` (spec #81) re-derives two committed fixtures and `reflect.DeepEqual`-compares the freshly-parsed shape against each committed `.parsed.json`. Both fixtures were last recorded against claude **2.1.158**; the runner now has **2.1.199** installed, and the parsed shape has drifted, so the check reds (~8.8 s) on every PR — a permanent false signal.

This is a **parsed-shape** drift (spec #81), not the first-prompt-deadline hang that reds the sibling child #177 (split from the same #176). `spike-multiselect` currently **passes**, which is strong evidence both parsers still classify the 2.1.199 render correctly and the drift is purely in committed fixture *values* — the pure-re-record happy path. The spec still makes that a *verified gate*, not an assumption (§ Decision gate), because the ticket framing is a hypothesis the developer must confirm empirically.

The `/picker` case was removed in #129 (irreducibly host-dependent — operator plugin/skill commands leak in). **There is no picker fixture to re-record.** Picker parser/modal-class coverage is retained separately via the `picker-snapshot.bin` / `picker-truecolor-snapshot.bin` unit fixtures.

Per the #47/#57 lesson, a `claude-version.lock` `version=` bump must coincide with a fixture re-record sweep. This ticket is that sweep, so the bump lives here (and only here).

---

## Design

There is no new type, interface, or code path. The deliverable is (a) two re-recorded data fixtures, (b) one lock line, produced by an existing operator workflow, gated by a deterministic pass/split decision. Treat the sections below as the procedure + its decision tree.

### Step 1 — Confirm the drift is a genuine 2.1.199 render change (AC #2, pre-record)

Before re-recording, run the check in **read-only** mode against installed claude and inspect the diff:

- `make e2e` (or `bin/e2e-snapshot-check -bin-dir ./bin` directly after `make build-bin`) and observe the `SNAPSHOT mcp …` / `SNAPSHOT agents …` lines plus the stderr `drift in <path>` + `captured at <parsed.json>` forensic anchors.
- For each case that reports `diff`, diff the committed fixture against the captured sidecar (the e2e-harness doc's idiom): `diff <(jq . pkg/tuidriver/testdata/<name>-snapshot.json) <(jq . <captured .parsed.json>)`.
- **Confirm the delta is a real render/wording change**, not host-injected content. Legitimate: field-value/wording shifts intrinsic to 2.1.199 (e.g. changed empty-state prose, tab labels). Illegitimate (must NOT be baked in): operator MCP servers in the mcp case (`total_servers > 0`), or operator subagents in the agents case (`items` populated / non-empty `Running` tab).

If a case reports `diff` with only legitimate deltas → proceed to Step 2. If a case reports the **missing-sidecar / classifier-drift** error instead of a plain `diff` → jump to § Decision gate (split).

### Step 2 — Re-record from a real 2.1.199 session (AC #1, #4)

Run the existing operator target — **do not hand-edit fixtures** (AC #4):

```
make rerecord-snapshots
```

This runs `e2e-snapshot-check -record`, which forces `TUIDRIVER_STRICT_MCP_CONFIG=1` on every spike child and overwrites the two surviving fixtures with freshly-captured parsed JSON. Success is `SNAPSHOT mcp recorded` **and** `SNAPSHOT agents recorded` on stdout — both sidecars were produced, i.e. both parsers still classify → the re-record is pure.

### Step 3 — Verify the host-portability guards survived (AC #2)

`git diff` the two fixtures and assert the guards still hold:

- `mcp-snapshot.json` — `total_servers` is `0` (strict-mcp empty state; no operator MCP-config path leaked in).
- `agents-snapshot.json` — `items` is `null` and `empty_text` is the subagent-free empty state (e.g. `"No subagents are currently running."`; no operator-specific running subagents embedded). Exact wording is whatever 2.1.199 renders — the guard is *empty-state*, not a specific string.

A re-record that bakes in host-specific content re-reds on a clean CI runner (the #129 lesson) and is **not acceptable** — if the guards fail, that is a strict-mcp/isolation problem, not a fixture to commit; stop and escalate rather than commit a host-tainted fixture.

### Step 4 — Bump `claude-version.lock` `version=` (AC #3)

- Change the single line `version=2.1.158` → `version=2.1.199`.
- Re-verify the five enforced entries against `claude --help` on 2.1.199 — `--session-id`, `--permission-mode`, `bypassPermissions`, `--model`, `--effort`. **Pre-checked during refinement: all five still appear** in `claude --help` on 2.1.199, so **no `flag=`/`value=` edit is expected.** Confirm independently; edit a `flag=`/`value=` line only if a listed flag/value actually changed. Because `version=` is informational (not compared against the installed binary — #64), this line cannot itself fail the `claude-version-lock` check.

### Step 5 — Verify the whole gate is green (AC #1, #5)

Re-run `make e2e` (read-only) and confirm:

- `snapshot-drift` PASSES — both `mcp` and `agents` report `match`.
- `claude-version-lock` PASSES (five flag/value substrings present; `version=` informational).
- `spike-multiselect` PASSES (unchanged; the re-record touches no spike/parser code).

> Note on #177 independence: there is **no ordering dependency** on sibling #177 (the first-prompt hang). `spike-multiselect` and the two snapshot triggers are unaffected by that hang, so this ticket can land regardless of #177's state. If unrelated first-prompt-hang spikes are red in the same `make e2e` run, that is #177's surface — out of scope here; scope the verification to the three checks named above.

---

## Decision gate — pure re-record vs. classifier drift (the split escape)

This is the one branch the developer must not rationalize past. It is deterministic and keyed on whether the sidecar is produced:

| Observation | Meaning | Action |
|---|---|---|
| Both cases `diff` (or `match`) with sidecars written; `make rerecord-snapshots` prints `recorded` for both | Parsers still classify the 2.1.199 render; drift is in values only | **Pure re-record.** Proceed. In scope for #178. |
| A case errors with `read parsed-json … no such file` / `classifier drift, not a content diff` (no sidecar) | `DetectModalClass` no longer returns MCP/Agents for the 2.1.199 render; the parser/classifier needs a change | **Split.** A pure re-record must not touch `pkg/tuidriver/` parser code (last Technical Note; #128 per-case precedent). |

**If the split branch fires** (for either or both cases): do **not** edit any `pkg/tuidriver/` parser/classifier file to force a pass, and do **not** commit a partial fixture. Instead:

1. Post a comment on #178 naming which case(s) hit classifier drift and the `modal-class detected=<class>` value scraped from stderr.
2. Propose the split — one child per affected case, mirroring #128 (`/mcp` classifier reconcile) — e.g. *"Split: `mcp` re-records cleanly (in #178); `agents` render shifted enough that `ParseAgentList`/`DetectModalClass` no longer classifies — carve a `128`-shaped classifier child for it."* If the clean case can still ship, note it stays in #178.
3. Add label `needs-rework:po`. Do not add `done:architect`. Leave the worktree otherwise untouched for the clean-case work if PO reslices.

Evidence so far points to the pure-re-record path (`spike-multiselect` passes today), but the gate is mandatory — the ticket's "no parser change expected" is a hypothesis, not a license to edit parser code if it turns out false.

---

## Error handling / failure modes

- **Missing sidecar on re-record** → classifier drift → § Decision gate (split). Never fixed by hand-patching the fixture.
- **Guard violated** (`total_servers > 0`, or agents `items` non-null) → host content leaked into the capture. Do not commit. This indicates strict-mcp wasn't applied or a subagent was running during capture; re-run cleanly (no active subagents; confirm `TUIDRIVER_STRICT_MCP_CONFIG=1` reached the child) before committing.
- **A `flag=`/`value=` genuinely dropped from `claude --help`** on 2.1.199 → the `claude-version-lock` check would fail on the missing substring. Pre-checked as not the case; if it were, updating that lock line is in scope (AC #3), but a *removed* CLI flag is a compatibility break bigger than a re-record — surface it to PO rather than silently deleting the enforced line.

---

## Testing strategy

No unit tests change — this ticket ships data + one config line, no `.go` production or `_test.go` change. Verification is the `make e2e` gate itself (Step 5): `snapshot-drift`, `claude-version-lock`, and `spike-multiselect` all PASS against installed claude 2.1.199. `make check` (vet + race test) remains green trivially since no Go source changed. The `git diff` of the two fixtures + the one lock line is the reviewable artifact; the guards in Step 3 are the human-verifiable invariants.

---

## Scope / non-goals

- **No `pkg/tuidriver/` parser or classifier change.** If one proves necessary, it is out of scope → split (§ Decision gate).
- **No picker fixture** — do not re-add or re-record it (#129). `picker-snapshot.bin` / `picker-truecolor-snapshot.bin` retain parser/modal-class coverage.
- **No `flag=`/`value=` lock edits expected** — only `version=`; touch an enforced line only if a real CLI-surface change forces it.
- **No knowledge-doc edit.** `docs/knowledge/features/e2e-harness.md` and `docs/knowledge/codebase/178.md` are owned by the documentation phase; the developer's worktree mutates only the two fixtures, `claude-version.lock`, and this spec file.

## Open questions

- **Which case(s) currently diff?** The ticket asserts both are stale against 2.1.199 but one may already `match`. Step 1 resolves this empirically; either way `make rerecord-snapshots` is idempotent for a case that already matches (re-writes identical bytes), so the procedure is unchanged.
- **Exact 2.1.199 empty-state wording** for the agents fixture — unknown until captured; the guard is *empty-state shape* (`items: null`), not a hardcoded string, so the re-record defines it.

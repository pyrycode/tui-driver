# Spec #252 — snapshot-drift: reconcile `/mcp` fixture with claude's new built-in `computer-use` MCP

**Ticket:** [#252](https://github.com/pyrycode/tui-driver/issues/252) · **Size:** S (mechanically XS — data-only re-record) · **Not security-sensitive** (fixture data + informational lock; no classifier/answer-path change)

**Split from #250. Recurrence of #178** (same check, same fixture). This spec follows #178's decision-gate shape: confirm the cause empirically, then apply the in-scope remediation (re-record) or escape to PO.

---

## Files to read first

- `pkg/tuidriver/testdata/mcp-snapshot.json` — the committed fixture, currently `{total_servers:0, categories:null}`. This is what drifted.
- `cmd/e2e-snapshot-check/main.go:87-165` (`runFixture`) — how the check re-derives + compares; note it **forces** `TUIDRIVER_STRICT_MCP_CONFIG=1` on the spike child (line 103) and the `-record` write path (lines 130-137).
- `pkg/tuidriver/mcp.go:41-179` (`ParseMcpStatus` + its three anchor regexes) — the parser under suspicion. Read it to confirm it is **not** at fault (it isn't — see Diagnosis).
- `pkg/tuidriver/pty.go:122-133` (`EnsureClaudeEnv` strict-mcp block) — the **only** place `--strict-mcp-config` is appended to `cmd.Args`. This is the cause-(c) fix surface *if* the guard trips; otherwise untouched.
- `docs/knowledge/features/e2e-harness.md:181` (the drift-triage runbook line) and `:227` (re-record + lock-bump discipline, #47/#57).
- `Makefile:50-54` (`rerecord-snapshots` → `bin/e2e-snapshot-check -record -bin-dir bin`) — the re-record command.
- `claude-version.lock` — `version=2.1.199` today; read the header comment on when to bump.
- Issue **#178** (closed) — the direct precedent: read the developer/architect decision-gate comments for the dev-host MCP-approval-picker gotcha.

---

## Context

`make e2e`'s `snapshot-drift` check re-derives the `/mcp` picker (`spike-multiselect -trigger=/mcp\r` → `ParseMcpStatus` → `.parsed.json` sidecar) and `reflect.DeepEqual`s the **parsed shape** against the committed `mcp-snapshot.json`. It now reports `diff`, identically on PR #249 and its merge-base — pre-existing, unrelated to the #243/#249 busy-predicate change.

This is a recurrence: #178 (Jul 3) confirmed `/mcp` matched `{total_servers:0, categories:null}` on claude 2.1.199. It has since drifted, and `claude --version` still reports **2.1.199** — so the version string did not change even though the render did.

## Diagnosis — cause (a): a claude **content change** (new built-in `computer-use`)

**Attributed to cause (a) of the ticket's three, with evidence.** The architect captured the drifting sidecar on this host (`/tmp/spike-multiselect-bytes-*.parsed.json`, two identical strict-mcp runs). It is well-formed:

```json
{
  "total_servers": 1,
  "categories": [
    { "name": "Built-in MCPs", "path": "(always available)",
      "servers": [ { "name": "computer-use", "status": "disabled", "highlighted": true } ] }
  ]
}
```

This rules the causes out/in decisively:

- **NOT cause (b) parse/detector regression.** `ParseMcpStatus` produced a *correct, well-formed* `McpStatus` — every anchor regex matched (`1 server[s]` → `total_servers:1`; `Built-in MCPs (always available)` → the category; the item line → `computer-use`/`disabled`). A broken anchor yields a *missing* sidecar / `Unknown` class, not a clean parse. `mcp.go` has no code change since #21. The parser is fine.
- **NOT cause (c) strict-mcp leak.** A leak means **operator** servers (`qmd`, `smart-connections`) appearing under **"User MCPs"** / **"Project MCPs"** because `--strict-mcp-config` didn't take effect. That is not what the capture shows: the sole server sits under **"Built-in MCPs (always available)"**, and `computer-use` is **not** in this host's `~/.claude.json` (verified). `--strict-mcp-config` only restricts `--mcp-config`-sourced servers; it does **not** (and is not meant to) filter claude's own built-ins. The strict-mcp plumbing (`pty.go:122`) is working.
- **IS cause (a) content change.** claude Code (current 2.1.199 build) now ships a **built-in `computer-use` MCP server, disabled by default**, which renders in `/mcp` as `total_servers:1` regardless of strict-mcp. Because it is a built-in — not operator config — it is **host-independent**: it appears on any host running this claude build, including a clean runner. The committed `{total_servers:0}` fixture predates this built-in.

**Therefore the remediation is a re-record, not a code fix and not a fixture-bless-of-a-leak.** The check is doing its job: it caught a genuine claude content change. The response per the #47/#57 discipline is a deliberate re-record.

## Design — remediation (the expected path)

Re-record the single `mcp` fixture from a fresh strict-mcp capture, then verify host-independence and re-run the compare. No `.go` changes on this path.

1. **Build + re-record** via `make rerecord-snapshots` (which runs `bin/e2e-snapshot-check -record -bin-dir bin`; the `-record` path forces `TUIDRIVER_STRICT_MCP_CONFIG=1`, so the capture is strict-mcp and host-portable). This overwrites `pkg/tuidriver/testdata/mcp-snapshot.json` with the freshly-parsed shape. **Do not hand-edit the fixture** — regenerate it, then confirm it matches the diagnosed shape above (the JSON block is the evidence anchor, not a value to type in).

2. **`claude-version.lock` `version=` — check, don't assume a bump.** `claude --version` currently reports `2.1.199`, which the lock already pins. Since the version string is unchanged, **no `version=` edit is possible or needed** — record in the PR that this drift is a *within-version* claude built-in change (the informational `version=` field cannot distinguish it; that's a known limitation). **If** `claude --version` reports a *newer* string at implementation time, bump `version=` to it per the re-record convention. The five enforced `flag=`/`value=` lines are unaffected either way (re-confirm they still appear in `claude --help`).

3. **Verify** (AC-gating) — see Testing strategy.

### Decision gate — confirm cause (a) **before** recording (mirrors #178)

The architect's diagnosis was taken on this host; the developer re-confirms on the live gate before blessing. Inspect the fresh capture:

| Observation on the fresh strict-mcp capture | Cause | Action |
|---|---|---|
| Only a **"Built-in MCPs"** category (`computer-use`, disabled); **no** "User MCPs" / "Project MCPs" categories; strict-mcp confirmed active (flag on argv / `total_servers` excludes operator servers) | **(a) content change** | **Re-record** (this ticket) |
| **"User MCPs" / "Project MCPs"** categories present (operator servers like `qmd`/`smart-connections` leaked in) | **(c) strict-mcp leak** — `EnsureClaudeEnv` (`pty.go:122`) not appending the flag, or claude renamed/dropped `--strict-mcp-config` | **STOP. Do not bless.** Escalate to PO (`needs-rework:po`) with the capture — a plumbing fix needs its own scoping |
| **Missing sidecar** / `modal-class detected=` empty (`Unknown`) / garbled parse | **(b) parse regression** *or* the dev-host approval-picker artifact (see Error handling) | Rule out the approval-picker artifact first; if a genuine parse break, **STOP** and escalate to PO — a parser fix needs its own scoping |

The (b)/(c) rows are **escape hatches**, not the expected path: the evidence points to (a). They exist so a live-gate result that contradicts the diagnosis routes correctly instead of silently ballooning this data-only ticket into a code-debug. This is the #178 decision-gate-split precedent; the ticket's "fix the code for (b)/(c)" AC is honoured by handing PO a scoped, evidence-backed re-slice rather than an unbounded in-place fix.

## Error handling — dev-host artifacts to distinguish from real drift

- **First-run MCP-approval picker (dev-host only, from #178).** On a host with unapproved user-scope MCP servers, claude's startup *"N new MCP servers found — approve?"* picker can render before idle and swallow `/mcp\r` → `modal-class=Unknown`, no sidecar. This is **not** a fixture edit: approve the servers once (or use a clean profile) and re-run. It does not occur on a clean runner. (Current captures show the picker is not blocking now — the servers are approved — but re-runs can hit it.)
- **Capture flakiness.** A single strict-mcp capture can transiently miss the settled state. The AC requires re-running the compare **≥2× consecutively**; treat a lone red among greens as flake, not drift (per the QA baseline-hygiene lesson — deterministic re-sampling overrides single-run reasoning).

## Testing strategy

- **Re-record guard (host-independence):** after `-record`, assert the fixture contains only the "Built-in MCPs" category with `computer-use` and **no operator ("User"/"Project") category**. An operator category means a leak → decision-gate row (c), not a bless.
- **Compare, ≥2× consecutively:** run `bin/e2e-snapshot-check -bin-dir bin` directly at least twice; both must print exactly `SNAPSHOT mcp match` and exit 0. This is the fast deterministic re-sample the AC mandates (defeats capture flakiness without a full `make e2e` each time).
- **Full gate:** `make e2e` reports `snapshot-drift -> pass` with one `SNAPSHOT mcp match` line and no other fixture.
- **Unit tests unchanged:** `go test ./pkg/tuidriver/ ./cmd/...` stays green — no parser/classifier code is touched, and the fixture is read only by the e2e check, not by unit tests (the unit fixtures are the retained `.bin` files).

## Open questions

- **Long-term fixture fragility.** Coupling the `mcp` fixture to claude's built-in set means it will drift again whenever claude adds/removes/renames a built-in — potentially *within* a version string, as here, so the `version=` annotation can't flag it. A future ticket could make the snapshot check filter built-in MCPs (keeping the fixture the operator-empty `{total_servers:0}` state and re-scoping the check to "no operator leak"). That is a code change with its own tests and a genuine design call (it also *removes* the check's ability to catch built-in changes) — **out of scope here**; note it as a follow-up if the developer agrees it's worth filing.
- **`highlighted`/`status` stability.** `highlighted:true` holds because `computer-use` is the only (thus cursor-selected) server, and `disabled` is its default under the harness's fresh bypassPermissions spawn. Both are deterministic today; if a future build adds more built-ins the highlight moves to the first — re-record handles it then.

## Acceptance criteria

- [ ] **Diagnosed** — the drift is attributed to **cause (a): a claude content change** (claude's new built-in `computer-use` MCP server renders in `/mcp` as `total_servers:1`, host-independent, unaffected by strict-mcp). Evidence (the drifting `.parsed.json` vs. the committed fixture) is recorded in the PR. *(If the live-gate re-confirmation instead shows cause (b) or (c) per the decision gate, do not re-record — escalate to PO with the capture.)*
- [ ] **Fixture re-recorded** from a fresh strict-mcp capture via `make rerecord-snapshots` / `bin/e2e-snapshot-check -record` (not hand-edited), and the guard confirms host-independence: only the "Built-in MCPs" category, no operator ("User"/"Project") category, `total_servers:1`.
- [ ] **`claude-version.lock`** handled per § Design step 2: no `version=` edit if `claude --version` is still `2.1.199` (documented as a within-version built-in change in the PR); bumped only if claude reports a newer version. The five enforced `flag=`/`value=` lines re-confirmed present in `claude --help`.
- [ ] **`make e2e` reports `snapshot-drift -> pass`**, confirmed by running `bin/e2e-snapshot-check` directly **≥2× consecutively** (both `SNAPSHOT mcp match`, exit 0).
- [ ] **No `pkg/tuidriver/` parser/classifier or plumbing `.go` touched** on the (a) path — this is a data-only re-record.

# Spec: code-review agent invokes `make e2e` for library PRs (path-filtered)

Ticket: [#33](https://github.com/pyrycode/tui-driver/issues/33) (size: s, security-sensitive). Modifies a **sibling repo** — `pyrycode/tui-driver-agents/code-review/CLAUDE.md` — not this repo's source. Closes the PR-time e2e-coverage gap that #32/#40 deliberately left open (push-to-main only, for cost reasons). Sibling to #40 (merged); the GitHub Actions workflow is the deterministic backstop, the code-review gate is the stochastic-but-pre-merge layer.

## Files to read first

1. `/Users/juhanailmoniemi/Workspace/Projects/tui-driver-agents/code-review/CLAUDE.md` (whole file, 168 lines) — the agent prompt to edit. The new section inserts between "## Severity Levels" (line 103) and "## Workflow" (line 109); the Workflow section itself gets one new step at the top (steps 1–7 become 2–8); the "## Mechanical contract" section (lines 143–159) gets a one-sentence addendum on red-e2e label routing. **No other section is touched.**
2. `docs/knowledge/features/e2e-harness.md` — the runner's exit-code contract (line 10: "exits `0` iff every check passed; `1` otherwise"), the report-emission invariant (line 179: "the report always emits when feasible"), and the failure-mode table at "How it handles failure" (lines 161–179). These are the source of truth for the failure-mode distinguisher in this spec.
3. `docs/knowledge/features/e2e-harness.md:117-144` — the report schema (`{claude_version, total_duration_ms, checks: [{name, status, duration_ms, ...}]}`). The new gate extracts the failing check name(s) from `checks[]` where `status` ∈ {`fail`, `timeout`} and the lead `claude-version-lock` entry exists.
4. `docs/specs/architecture/40-ci-e2e-workflow.md` (whole file, esp. the trigger-surface and cost rationale sections) — same `make e2e` invocation, different trigger. The code-review gate must match the workflow's invocation shape so a runner-side change lands in both places without re-architecting either.
5. `/Users/juhanailmoniemi/Workspace/Projects/tui-driver-agents/code-review/CLAUDE.md:84-101` — the existing "Security-sensitive PRs (label-gated)" section. The new gate composes with this — both must run on a security-sensitive PR — but the gate runs *before* the security section's spec-verification step, so the gate's verdict is independent of the security-section's verdict.
6. `/Users/juhanailmoniemi/Workspace/Projects/tui-driver-agents/code-review/CLAUDE.md:143-159` — the existing "Mechanical contract — labels are the truth" section. The new gate must extend this rule (red e2e → `needs-rework:developer`), not replace it.

## Context

The code-review agent reviews every tui-driver PR but currently relies on either (a) the push-to-main workflow (`.github/workflows/e2e.yml`, #40) — which fires AFTER merge, so a broken PR sits in `main` until the workflow runs — or (b) the developer's own local `make e2e` run, which the developer is not currently required to perform. Both gaps converge into the same failure mode: a library-touching PR can land in `main` without anyone having run the e2e harness against the exact commit being merged.

This ticket adds a third coverage layer: the code-review agent itself runs `make e2e` against the PR's checked-out worktree when the diff touches library, spike/probe, harness-runner, Makefile, or version-lock files. The new layer is intentionally redundant with the push-to-main workflow (the workflow remains the deterministic backstop — per the agents-repo CLAUDE.md "Belt-and-Suspenders Means Different Fabric" rule, you do not replace a deterministic check with a stochastic one). The code-review layer trades determinism for pre-merge visibility: the failure surfaces on the PR, not on `main`.

The include list is a **path-prefix INCLUDE** (not exclude). PO chose include over exclude per the AC because new files at unfamiliar paths default to "no e2e" rather than "run e2e" — safer for cost (a new top-level file like `SECURITY.md` shouldn't drag a $X harness run) and consistent with the bias toward false negatives over false positives (a missed e2e run is recovered by the push-to-main workflow; a spurious e2e run wastes API credits with no signal).

## Cross-repo boundary (developer: read this before doing anything)

**The file you are editing lives in a sibling repo, not in your worktree.** Your worktree is a checkout of `pyrycode/tui-driver` at `/Users/juhanailmoniemi/Workspace/Projects/.pyrycode-worktrees/developer-33/`. The file you must edit is at `/Users/juhanailmoniemi/Workspace/Projects/tui-driver-agents/code-review/CLAUDE.md` — a sibling checkout of `pyrycode/tui-driver-agents`. The two repos are sibling clones, not nested.

Concrete consequences for your workflow:

- **Edit by absolute path.** Use `Read` and `Edit` against `/Users/juhanailmoniemi/Workspace/Projects/tui-driver-agents/code-review/CLAUDE.md`. Do not copy the file into your worktree; do not write any version of it inside `/Users/juhanailmoniemi/Workspace/Projects/.pyrycode-worktrees/developer-33/`.
- **The dispatcher's auto-commit safety net only sees YOUR worktree.** Edits made outside your worktree are NOT auto-committed for you. You must explicitly `git -C /Users/juhanailmoniemi/Workspace/Projects/tui-driver-agents` against the agents repo for the staging, commit, push, and PR-open steps.
- **Commit + push happen in the agents repo, on a new branch there.** Branch name: `feature/tui-driver-33-e2e-gate`. Commit message: `feat(code-review): e2e harness gate for library PRs (closes pyrycode/tui-driver#33)`. Push: `git -C /Users/juhanailmoniemi/Workspace/Projects/tui-driver-agents push -u origin feature/tui-driver-33-e2e-gate`.
- **Open the PR against the agents repo, not against tui-driver.** Use `gh pr create --repo pyrycode/tui-driver-agents --base main --head feature/tui-driver-33-e2e-gate ...`. The PR title: `feat(code-review): e2e harness gate for library PRs`. The PR body must link back to `pyrycode/tui-driver#33` (e.g. `Closes pyrycode/tui-driver#33` — cross-repo close keyword; GitHub respects it).
- **Do NOT add commits to your tui-driver worktree.** The architect already committed this spec to the `feature/33` branch in tui-driver; that is the only artifact this ticket produces in this repo. The dispatcher's empty-branch guard checks `commitsAhead != 0`; the architect's spec commit satisfies it, so a zero-developer-commit run does not trigger the guard. (Verified against `shouldFlagEmptyBranch` in `dispatcher/src/blockers.ts`: returns false when commitsAhead is non-zero, regardless of which agent produced the commits.)
- **The tui-driver PR auto-opens with the spec only.** That PR will pass code-review with no findings (it's a doc-only spec) and proceed through documentation → merge normally. The functional change lands in the agents-repo PR, reviewed by Juhana manually. The two PRs are coupled by issue reference; merge order doesn't matter for correctness because the agents-repo prompt change has no runtime effect on the tui-driver repo until the next code-review run.

The `gh` token in your environment is authorized against the `pyrycode` org; both repos are owned by that org, so `--repo pyrycode/tui-driver-agents` resolves with the same credentials as `--repo pyrycode/tui-driver`. No new auth setup is required.

## Design

Three edits to `/Users/juhanailmoniemi/Workspace/Projects/tui-driver-agents/code-review/CLAUDE.md`. No other files anywhere are touched (verified: nothing under `tui-driver/`, nothing under `tui-driver-agents/` except this one file).

### Edit 1 — insert a new "## e2e harness gate (path-filtered, additive)" section

**Position:** between the existing "## Severity Levels" section (ends at line 107) and the existing "## Workflow" section (starts at line 109). The new section is approximately 35–45 lines of prose + one code block for the path-filter command.

**Required content** (the developer writes this prose; the structure and the load-bearing details are mandatory):

1. **One-paragraph rationale.** Explain: this gate runs `make e2e` against the PR's worktree when the diff touches library/spike/probe/harness/Makefile/version-lock files. Note that it is redundant with the push-to-main workflow on purpose — the workflow is the deterministic backstop, the gate is the pre-merge visibility layer. Cross-reference the push-to-main workflow at `.github/workflows/e2e.yml` and the feature doc at `docs/knowledge/features/e2e-harness.md`.
2. **The include list, verbatim.** Bulleted list — copied directly from the issue body's AC1 (`pkg/tuidriver/**`, `cmd/spike-*/**`, `cmd/probe-*/**`, `cmd/e2e-runner/**`, `cmd/e2e-snapshot-check/**`, `Makefile`, `claude-version.lock`). State explicitly that this is a **prefix include** — file paths starting with these prefixes match; everything else does not. State explicitly that doc-only PRs (no path matching) skip the gate.
3. **The path-filter command, verbatim** (this is load-bearing and must not be paraphrased — see § "Path-filter command" below for the exact form).
4. **The decision table** — three rows: green / red / infra-failure / non-library. For each, name the verdict and the routing consequence. See § "Failure-mode distinguisher" below for the deterministic rule.
5. **The red comment-block template** (see § "Comment formatting on red" below).
6. **The infra-failure comment-block template** (see § "Comment formatting on infra failure" below).
7. **Cross-link to step 1 of "## Workflow"** so a reader landing on Workflow first sees the pointer back.

**Style constraints.** Use the same bold-lead-with-bullets rhythm as the existing "## Codegraph (use it before grep)" section (lines 34–63) for visual consistency. Do not introduce `###` subsections — the section is short enough that `##` + bold leads (`**Include list.**`, `**Path-filter command.**`, `**Decision table.**`, `**Red comment template.**`, `**Infra-failure comment template.**`) preserve the document's existing heading rank.

### Edit 2 — insert a new step at the top of "## Workflow"

**Position:** the existing Workflow section is at lines 109–117 with steps 1–7. The new step 1 inserts at line 111 (immediately after the `## Workflow` heading). Steps 1–7 become 2–8.

**Required content** for the new step 1:

> **Run the e2e harness gate (see § "e2e harness gate" above).** If `gh pr diff <number> --name-only` produces NO line matching the include-list regex, skip the gate and proceed to step 2 unchanged. If at least one line matches, run `make e2e` from your worktree root; classify the outcome per the decision table; capture the verdict block for prepending to your review body in step 5. The gate's verdict (red / green / infra-failure) is **independent of and composes with** the per-diff review verdict — both must be green for the overall review to PASS.

**Existing steps 1–7 shift down by one** (now 2–8). The text of each existing step is unchanged. Step 4 in the old numbering ("Write findings as PR comments with line references") becomes step 5 and gets a one-clause amendment: "Write findings as PR comments with line references — **prepend the e2e gate's verdict block from step 1 if it produced one (red or infra-failure)**". Step 6 in the old numbering ("Make the PASS/FAIL decision") becomes step 7 and gets a one-sentence amendment: **"A red e2e gate is itself a FAIL regardless of the per-diff review findings."**

### Edit 3 — extend "## Mechanical contract — labels are the truth, prose is for humans" with the red-e2e routing rule

**Position:** the existing section is at lines 143–159. Append two bullet points to the existing two-path list (PASS path / FAIL path), positioned immediately after the existing bullets but before the "If you write 'Decision: FAIL'…" paragraph at line 150. Do not reword the existing two bullets.

**Required content** (verbatim shape — the prose is yours, the load-bearing facts are these):

- **Red e2e path:** the gate produced `❌ e2e harness FAILED`. Treated identically to a FAIL: you add `needs-rework:developer` per Workflow step 7. The GitHub-level review action is `--request-changes` (per AC2 of #33), but the label is still what the dispatcher reads. The `--request-changes` action without the label still auto-advances the ticket; the label without `--request-changes` is enough for the dispatcher but loses the GitHub-side signal. **Both are required on a red e2e.**
- **Infra-failure e2e path:** the gate produced "e2e harness could not run". This is NOT a FAIL — the gate could not produce a verdict, so the per-diff review's verdict alone decides PASS/FAIL. The GitHub-level review action is `--comment` (not `--request-changes`). If your per-diff review otherwise PASSes, do nothing label-wise (the dispatcher auto-applies `done:code-review`). If your per-diff review FAILs, add `needs-rework:developer` as usual.

### What this spec deliberately does NOT touch

- The `pyrycode/tui-driver` repo's source (`pkg/tuidriver/`, `cmd/`, `Makefile`, etc.) — the change is purely in the agents repo.
- The push-to-main workflow (`.github/workflows/e2e.yml`, #40) — the gate is additive to the workflow, not a replacement. The workflow keeps its `push: branches: [main]` + `workflow_dispatch` trigger surface as-is.
- The feature doc (`docs/knowledge/features/e2e-harness.md`) — no doc edit is needed in this repo. The PO body refers to `e2e-harness.md § "CI integration"` line 52 ("PR-time coverage is a separate concern: the code-review agent runs the harness selectively elsewhere") as the existing in-repo pointer to this gate; it stays untouched.
- The architect prompt (`tui-driver-agents/architect/CLAUDE.md`) — no architect-side change. The gate is a code-review-only concern.
- The developer prompt (`tui-driver-agents/developer/CLAUDE.md`) — no developer-side change. The developer does not pre-run `make e2e` as part of every ticket; the gate fires only at code-review time on the developer's PR.
- The dispatcher (`tui-driver-agents/dispatcher/`) — no dispatcher change. The gate is purely a prompt-level rule; the dispatcher's label-routing logic already handles `needs-rework:developer`.
- Any other agent prompt (po/, documentation/) — out of scope.

## Path-filter command

Use this exact command — copy into the prompt verbatim:

```bash
gh pr diff <PR-number> --name-only \
  | grep -qE '^(pkg/tuidriver/|cmd/spike-|cmd/probe-|cmd/e2e-runner/|cmd/e2e-snapshot-check/|Makefile$|claude-version\.lock$)'
```

- `gh pr diff … --name-only` fetches the file list from the GitHub API. Deterministic on a fresh checkout — no dependence on local merge-base resolution, no need to `git fetch` first.
- `grep -qE` exits 0 (silent) if any line matches; exits 1 if none match.
- The regex is a single anchored alternation: line must start with one of the prefixes. The `$` on `Makefile$` / `claude-version\.lock$` enforces exact match (so `Makefile.in` or `claude-version.lock.bak` would not match, in the unlikely event those files appeared).
- `pkg/tuidriver/` matches every file under the package, including `pkg/tuidriver/testdata/*-snapshot.bin` (the snapshot fixtures — PO body AC1 explicitly notes "and by extension `pkg/tuidriver/testdata/**`"; the prefix `pkg/tuidriver/` covers both code and testdata in one rule rather than enumerating them separately).
- `cmd/spike-` matches any file under any `cmd/spike-<anything>/` directory (`spike-one-turn`, `spike-multi-turn`, `spike-cancel`, `spike-permission`, `spike-multiselect`, `spike-ask-user`, and any future `spike-<X>`). Same shape for `cmd/probe-`.

**Decision shape in the prompt:**

```bash
if gh pr diff "$PR_NUMBER" --name-only \
   | grep -qE '^(pkg/tuidriver/|cmd/spike-|cmd/probe-|cmd/e2e-runner/|cmd/e2e-snapshot-check/|Makefile$|claude-version\.lock$)'; then
  echo "e2e-needed=true"
  # run make e2e, classify outcome
else
  echo "e2e-needed=false"
  # skip gate, proceed to existing review
fi
```

The agent does not need to literally emit `echo "e2e-needed=…"`; the shell snippet above is the **decision shape** the prompt should describe. The agent reads the command, runs it, and branches on exit code.

## Failure-mode distinguisher

`make e2e` produces three observable signals at the end of a run:

1. **Process exit code** — 0 on full pass; 1 on any failure (per `docs/knowledge/features/e2e-harness.md:10`).
2. **`e2e-report.json` at the repo root** — present iff the runner got far enough to write it. The runner's dominant invariant: "the report always emits when feasible — even on partial failure" (`e2e-harness.md:179`).
3. **stdout + stderr** captured by the agent during the run.

Deterministic classification rule the prompt prescribes:

| Observed | Classification | Routing |
|---|---|---|
| exit 0 | **green** | proceed to existing review with no special callout |
| exit ≠ 0 AND `e2e-report.json` exists AND parses as valid JSON AND has `.checks[]` array with at least one entry where `status ∈ {"fail","timeout"}` | **red (check failure)** | post `--request-changes` review with red comment block; add `needs-rework:developer` label; existing line-by-line review still happens (prepended below the red block) |
| exit ≠ 0 AND (`e2e-report.json` is missing OR is unparseable OR has empty `.checks[]`) | **infra failure** | post `--comment` review with infra-failure block; do NOT add `needs-rework:developer` from the gate (the per-diff review decides FAIL/PASS independently); existing line-by-line review still happens |

This rule is deterministic: `jq '.checks[] | select(.status=="fail" or .status=="timeout") | .name' e2e-report.json` is the extraction primitive. If `e2e-report.json` doesn't exist or `jq` fails to parse it, the classification falls through to infra-failure.

**Edge cases the rule covers correctly:**

- `claude-version-lock` fails because claude is missing on the runner: report emits with `claude-version-lock` status=`fail` and downstream checks marked status=`timeout`/`duration_ms=0` (short-circuit). Under the rule, this is a **red (check failure)** even though the underlying cause is closer to "infra problem." The developer sees a clear `❌ e2e harness FAILED — failing check: claude-version-lock` — actionable, even if not strictly an infra problem. This intentional simplification keeps the rule deterministic; refining it (e.g. "claude binary missing implies infra failure") adds prose without changing the operator-visible outcome.
- The runner panics before emitting the report: no report on disk → falls into infra-failure → posted as `--comment`. Correct.
- `make e2e` itself fails before invoking the runner (compile error in the runner; missing binary in `./bin/`): typically no report emitted → infra-failure. The developer's PR may itself have caused this (broken `cmd/e2e-runner/`); the comment-style report nudges the human reviewer to look but doesn't auto-reject.

**Edge case the rule deliberately does NOT cover:** wall-budget exhaustion (`-wall 10m`). When the runner hits its wall budget, the report emits with the in-flight check as `status="timeout"` and un-run checks appended with `status="timeout"`/`duration_ms=0` (e2e-harness.md:172). Under the rule, this classifies as **red (check failure)** with the in-flight check named. That's correct — the timeout is a real failure signal.

## Comment formatting on red

The agent must post a single review (not multiple comments) with `--request-changes`. The comment body is a verbatim template — the developer encodes this in the agent prompt as a literal template the agent fills in:

```
❌ **e2e harness FAILED**

Failing check(s): <name1>, <name2>, …

Last 5 lines of `make e2e` stdout/stderr:

```
<tail line 1>
<tail line 2>
<tail line 3>
<tail line 4>
<tail line 5>
```

---

(then the existing line-by-line review findings, formatted per the existing § "Output" template)
```

**Token-redaction step (security-sensitive — required because `pyrycode/tui-driver` is a public repo and the captured tail is mirrored to a PR comment).** Before substituting `<tail line N>`, the agent must filter the captured output with `sed` to redact anything resembling an Anthropic API key or GitHub token, so a malicious spike that prints `ANTHROPIC_API_KEY=sk-…` to stderr cannot exfiltrate the credential via the public comment. Concrete redaction shape the prompt prescribes — the developer encodes this in the prompt as a single shell pipeline the agent runs against the captured stderr before extracting the tail:

```bash
# Redact known credential shapes from the captured combined stdout/stderr.
sed -E \
  -e 's/(sk-ant-[A-Za-z0-9_-]{10,})/[REDACTED-ANTHROPIC-KEY]/g' \
  -e 's/(ghp_[A-Za-z0-9]{36,})/[REDACTED-GITHUB-TOKEN]/g' \
  -e 's/(ghs_[A-Za-z0-9]{36,})/[REDACTED-GITHUB-TOKEN]/g' \
  -e 's/(ANTHROPIC_API_KEY=[^[:space:]]+)/ANTHROPIC_API_KEY=[REDACTED]/g' \
  -e 's/(GITHUB_TOKEN=[^[:space:]]+)/GITHUB_TOKEN=[REDACTED]/g' \
  < captured.log | tail -n 5
```

Failing-check name extraction:

```bash
jq -r '.checks[] | select(.status=="fail" or .status=="timeout") | .name' e2e-report.json | paste -sd ', ' -
```

Add `needs-rework:developer` via the existing Workflow-step pattern:

```bash
gh issue edit <ticket-number> --add-label needs-rework:developer --repo pyrycode/tui-driver
```

Submit the review with `--request-changes`:

```bash
gh pr review <PR-number> --request-changes --body-file review.md --repo pyrycode/tui-driver
```

## Comment formatting on infra failure

Single review with `--comment` (NOT `--request-changes`). Verbatim template:

```
⚠️ **e2e harness could not run — see review log**

The harness gate ran `make e2e` against this PR but could not produce a verdict. Likely causes: missing `claude` install on the runner, MCP-startup hang before any check completed, or a runner-level failure that prevented `e2e-report.json` from being written. The existing line-by-line review still applies.

(Mention specific anomaly if visible: e.g. "no `e2e-report.json` produced after 10m wall budget" or "make: command not found".)

---

(then the existing line-by-line review findings)
```

The agent should NOT add `needs-rework:developer` from the gate's verdict alone. The per-diff review decides FAIL/PASS independently.

```bash
gh pr review <PR-number> --comment --body-file review.md --repo pyrycode/tui-driver
```

## Testing strategy

This is a prompt change in a markdown file. There is no automated test surface. Verification is layered:

- **Markdown lint (manual).** The agents repo has no markdownlint config (verified). A visual GitHub-preview check of the agents-repo PR is sufficient. Look for: heading rank consistency (no orphan `###`), bullet alignment, fenced-code-block balance (each ` ``` ` opens and closes cleanly), and the embedded shell snippets render as monospace.
- **Self-read pass.** The developer reads the edited file end-to-end once, top to bottom, AFTER making the edits. The goal is to confirm: (a) the new "e2e harness gate" section flows naturally between "Severity Levels" and "Workflow"; (b) the new step 1 in "## Workflow" reads coherently with the renumbered 2–8; (c) the appended bullets in "## Mechanical contract" don't contradict the existing two-path list. Self-reading after editing catches the kind of "looks right line-by-line but reads wrong as a whole" mistake that markdown lint can't catch.
- **Dry-run simulation (mental).** Walk through three scenarios mentally before committing: (i) a docs-only PR (e.g. an architect's spec-only PR) — the path-filter skips, the existing review runs untouched; (ii) a library PR with a green e2e — the gate produces no callout, existing review runs as normal; (iii) a library PR with a red e2e — the red comment-block lands at the top of the review body, `--request-changes` is the action, `needs-rework:developer` is the label. If any of those three reads wrong in the edited prompt, fix it before committing.
- **Live verification (deferred — post-merge of the agents-repo PR).** After the agents-repo PR merges, the next library-touching PR in tui-driver exercises the gate naturally. No need to manufacture a test PR; the next real PR is the test. Note: the **next** PR after merge will run with the new prompt; this very ticket's tui-driver PR (containing only the spec) is doc-only and will NOT trigger the gate, which is the right behaviour.

No unit tests, no integration tests, no test files anywhere. This is intentional — the change surface is a stochastic agent prompt; testing it deterministically would require simulating the agent, which the pipeline doesn't do for any other prompt change (e.g. PR #25 "docs(agents): add Dispatcher Permission Denial rule" landed with the same verification shape).

## Open questions

- **Wall-budget on `make e2e` in the gate.** `make e2e` defaults to a 10-minute wall budget (`-wall 10m`). The code-review agent's own turn budget is ~50 turns; a 10-minute external command inside one turn is unusual but supported by `Bash` with `timeout` parameter up to 10 minutes. If the runner hangs and consumes the full 10 minutes, the gate burns one turn. The spec accepts this — adding a shorter cap inside the prompt risks classifying healthy-but-slow runs as infra failures. If the failure mode manifests, file a follow-up to add a `-wall 5m` override at the gate (per the "Evidence-Based Fix Selection" principle, defer until observed).
- **Where the `make e2e` invocation runs from.** The code-review agent's worktree is the PR's checkout; `make e2e` from `<worktree>` resolves Makefile + binary builds against the PR's code. The architect verified this is the correct cwd by reading `e2e-harness.md`'s "How to run" section (line 18: `make e2e` from repo root). No alternative cwd is needed.
- **Concurrency between push-to-main workflow and code-review gate.** Both invoke `make e2e`; they cannot collide because they run on different infrastructures (workflow on GitHub Actions, gate on Juhana's machine via the code-review agent). The agent's worktree is per-PR (`feature/<N>`), so two simultaneous code-review runs against different PRs would each have their own worktree and their own `e2e-report.json` — no shared-state collision. The single `claude-version.lock` cache key in the workflow is workflow-scoped; the gate doesn't touch any shared cache.
- **What happens if the agents-repo PR is reviewed (by Juhana, manually) and rejected.** Reverting requires a new PR — the dispatch pipeline doesn't touch the agents repo. Out of scope for this ticket. The code-review agent continues to run with the prior prompt until the new prompt is merged.

## Security review

**Verdict:** PASS

This ticket is `security-sensitive` because it expands the surface where PR code (potentially untrusted) is executed on the runner — `make e2e` builds and runs every `cmd/spike-*` binary in the PR's worktree, including any new spike a contributor adds. The expansion is from "push-to-main only" (workflow runs on already-merged code by Juhana) to "every library PR" (gate runs on un-merged PR code by anyone who submits a PR). The walk below treats the gate as exploitable-by-default and finds two SHOULD FIX items addressed inline in the spec, no MUST FIX items.

**Findings:**

- **[Trust boundaries] No new boundary introduced.** The runner already executes arbitrary code from `cmd/spike-*/` whenever `make e2e` runs (push-to-main, manual operator runs, this gate). The gate moves the *trigger* earlier — to PR time — but does not change the *what* of the execution surface. The pipeline's existing trust model assumes the developer agent (the sole automated source of PRs) is trusted because it runs under Claude with the dispatcher-controlled prompt; manual contributors don't exist in this project today. If manual contributors land in the future, all three `make e2e` invocation points (workflow, gate, operator) need the same allow-list discipline applied to `cmd/spike-*` additions — that's a project-wide concern, not specific to this gate. **Out of scope for this ticket; recommend filing a follow-up ticket if manual contributors become a thing.**

- **[Tokens, secrets, credentials] SHOULD FIX — addressed inline.** The 5-line stdout/stderr tail captured by the gate is posted into a public-repo PR comment (`pyrycode/tui-driver` is a public repo per the existing `https://github.com/pyrycode/tui-driver/settings/secrets/actions` reference; verified via the e2e-harness rotation docs). A malicious spike could deliberately print `ANTHROPIC_API_KEY=<value>` to stderr to exfiltrate the credential via the comment. **Mitigation in the spec:** § "Comment formatting on red" prescribes a `sed`-based redaction step over the captured combined stdout/stderr before the `tail -n 5` extraction. Redaction shapes: `sk-ant-…`, `ghp_…`, `ghs_…`, `ANTHROPIC_API_KEY=…`, `GITHUB_TOKEN=…`. This is belt — the developer agent is trusted today, so a malicious spike is unlikely — and suspenders — the redaction makes credential exfiltration through this channel structurally hard even if the trust model erodes.

  The existing push-to-main workflow has the same exposure surface (GitHub Actions logs are public for public repos and include stdout/stderr verbatim), and the workflow does NOT redact. This is **not a regression** introduced by this ticket; the gate's mitigation goes one step beyond the workflow's status quo. Filing a follow-up to add the same redaction to the workflow is a reasonable defense-in-depth move but is out of scope here.

- **[File operations] No findings — N/A.** The gate writes nothing in the agent's `Write`-controlled surface. `make e2e` itself writes `e2e-report.json` and `./bin/` — both gitignored at the repo root (`/e2e-report.json`, `/bin/` per `.gitignore` lines 17–18), so the dispatcher's auto-commit safety net cannot accidentally commit them to the PR branch.

- **[Subprocess execution] No findings — already audited.** `make e2e` is the subprocess. The harness already lives in the codebase; this ticket adds an invocation point, not a new subprocess shape. Env-var inheritance is whatever the agent's shell environment is; the architect deliberately does NOT scrub the env on invocation because `make e2e` depends on `ANTHROPIC_API_KEY` to function (per the e2e-harness rotation procedure). The redaction step under [Tokens] above is the compensating control for env-var leakage via stderr.

- **[Cryptographic primitives] N/A — no crypto added.**

- **[Network & I/O] SHOULD FIX — addressed inline.** The gate calls `gh pr review … --body-file review.md --repo pyrycode/tui-driver`, which posts to a public-repo comment. Input-size limit: `review.md` content can be arbitrary length, but GitHub's API caps comment body at 65,536 chars. The 5-line tail + the line-by-line review is unlikely to exceed this, but a pathological case (e.g. a spike that emits a 64 KiB-wide stderr line) could. **Mitigation in the spec:** § "Comment formatting on red" specifies `tail -n 5` (post-redaction) — bounded to 5 lines. The line-by-line review section is already bounded by the existing prompt's "Findings" format. No further cap needed.

- **[Error messages, logs, telemetry] No findings — composes cleanly.** The gate's verdict block names the failing check (e.g. `spike-one-turn`) and the 5-line tail — no token leakage after the redaction step under [Tokens]; no internal-state leakage (the gate doesn't have internal state). Telemetry: the dispatcher's logs at `logs/code-review-*.log` capture the agent's stderr in the operator-private space; same shape as every other agent run, no new fields.

- **[Concurrency] No findings — N/A.** The gate is invoked in a single-pass turn by a serial agent (code-review is not multi-instance). The 50-turn budget and the agent's per-PR worktree isolate concurrent runs from each other.

- **[Threat model alignment] No findings.** tui-driver has no `docs/threat-model.md`; the project's working threat model (per `CLAUDE.md` + dispatcher CLAUDE.md) is "internal pipeline, trusted agents, Juhana's machine only." The gate inherits this model; the two SHOULD FIX mitigations above (redaction + bounded comment length) extend it slightly toward "even if the model erodes, this channel doesn't leak."

**Reviewer:** architect (self-review per `tui-driver-agents/architect/security-review.md`)
**Date:** 2026-05-19

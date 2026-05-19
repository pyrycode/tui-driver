# Spec: e2e CI doc — `ANTHROPIC_API_KEY` rotation + `--strict-mcp-config` rationale

Ticket: [#41](https://github.com/pyrycode/tui-driver/issues/41) (size: xs). Documentation-only edit to one existing file (`docs/knowledge/features/e2e-harness.md`). Split from #32; depends on #40 (merged).

## Files to read first

1. `docs/knowledge/features/e2e-harness.md:42-46` — existing § "Headless / CI plumbing" (the env-var seam paragraph). The new "why + walk-back reconciliation" content extends this section.
2. `docs/knowledge/features/e2e-harness.md:59` — existing **Authentication.** bold-lead paragraph under § "CI integration". The final sentence ("the rotation procedure is tracked in #41 (out of scope for the workflow itself)") must be removed and replaced with the actual procedure.
3. `docs/knowledge/features/e2e-harness.md:61` — the **`--strict-mcp-config` is NOT set at the CI layer.** paragraph. Confirms the env-var seam; the new content must not contradict it.
4. `.github/workflows/e2e.yml` (whole file, 70 lines) — confirms `ANTHROPIC_API_KEY` is wired at the job-`env` level via `${{ secrets.ANTHROPIC_API_KEY }}` (line 20), that `workflow_dispatch` is enabled (line 6) for manual re-dispatch after a rotation, and that `concurrency.cancel-in-progress: false` (line 13) lets the operator manually cancel an in-flight run via the Actions UI without interfering with subsequent serialised runs.

Optional context (the architect read these but the developer doesn't need to unless something is unclear):
- Vault session log `📋 Projects/2026-04-10 - Pyrycode/Session Logs/2026-05-19.md` (qmd path: `second-brain/1f4cb-projects/2026-05-16-tui-driver/session-logs/2026-05-19.md`) — section "E2E tickets filed — #31, #32, #33" captures the exact May 18 walk-back reconciliation in one sentence ("Doc-noted exception to the May 18 walk-back (which was about user-facing pyry acp defaults; CI has no user MCP feature to break)").
- Vault `🔄 Areas/Assistant Docs/instruction-design.md` — captures the May 18 walk-back source material (PR #22 reverted in `f29c15d`).

## Context

`docs/knowledge/features/e2e-harness.md` already covers the **what** and **where** of CI authentication and `--strict-mcp-config`. Two narrowly-scoped operational gaps remain:

1. **Rotation procedure.** Existing § "Authentication" defers the rotation procedure to this ticket. A maintainer rotating the API key or onboarding to the project has no in-repo documentation of (a) which Anthropic account owns the credential, (b) the GitHub Actions secrets UI steps, or (c) the in-flight revocation sequence if the key leaks during a workflow run.
2. **`--strict-mcp-config` rationale.** Existing § "Headless / CI plumbing" + the redundancy-avoidance paragraph in § "CI integration" cover *where* the flag is applied but not *why* the harness uses it at all. The May 18 walk-back ([instruction-design.md] source: PR #22 reverted in `f29c15d`) recommended against making `--strict-mcp-config` the library's user-facing default sidestep. The walk-back targeted user-facing pyry acp, not CI; the e2e harness has no user-facing MCP feature, so using the flag for determinism is consistent with the walk-back. A future reader landing on the doc has to be able to see this reconciliation explicitly, or the doc reads as self-contradictory next to the library's broader posture.

## Design

Two edits to `docs/knowledge/features/e2e-harness.md`. No other files touched. No new files created.

### Edit 1 — expand § "Authentication" (one bold-lead paragraph at line 59) with the rotation procedure

Keep the existing first two sentences (the wiring statement and the secret-must-exist statement). **Replace the existing third sentence ("the rotation procedure is tracked in #41 (out of scope for the workflow itself)") entirely** — leaving it in would make the doc self-referential after this ticket lands. Then append, in order:

1. **Owning account pointer.** One sentence naming the Anthropic Console account that owns the API key. The architect does not know the specific account; the developer must write this as a placeholder (`<the maintainer's Anthropic Console account — Juhana to fill in before merge or in a follow-up doc PR>`) and surface the gap in the PR description. See § "Open questions" below.
2. **Routine rotation procedure** (no compromise suspected). Numbered list, 3-4 short steps. Required content: log in to the owning Anthropic Console account → generate a new API key (label it so the GitHub origin is obvious, e.g. `tui-driver-ci-2026-MM-DD`) → update the `ANTHROPIC_API_KEY` secret at `https://github.com/pyrycode/tui-driver/settings/secrets/actions` → revoke the old key in Anthropic Console only after the next push-to-main run succeeds (so a green run validates the new key before the old one is gone).
3. **In-flight revocation procedure** (leak / compromise suspected). Numbered list, 4-5 short steps. Required content and ordering: revoke the compromised key in Anthropic Console **first** (stops the bleeding) → cancel any in-flight workflow run via the Actions UI (the `concurrency.cancel-in-progress: false` setting means subsequent pushes serialise but does NOT prevent manual cancellation) → generate a new key → update the GitHub Actions secret → re-dispatch via `workflow_dispatch` to confirm the new key works without waiting for the next push to `main`.
4. **Workflow file reference.** One sentence pointing at `.github/workflows/e2e.yml` (the `secrets.ANTHROPIC_API_KEY` wiring at job-`env` level) so a reader can navigate from the doc to the actual consumer.

Style constraints:
- Bold-lead format (`**Authentication.**`) stays — do not promote to a `###` subsection. Other paragraphs in § "CI integration" (Cost controls, Cache shape, Artifacts, etc.) are all bold-lead-with-content; adding the first `###` mid-`##` would visually mis-rank the surrounding paragraphs.
- The two numbered lists are inline under the bold-lead paragraph (introduced by a short prose lead-in each, e.g. *"Routine rotation:"* / *"If a key is leaked while a workflow run is in flight:"*). Keep the existing bold-lead-with-bullet rhythm from § "Cost controls".
- Total addition for this edit: ~12-15 lines of markdown.

### Edit 2 — append a paragraph to § "Headless / CI plumbing" (after line 46, before the next `##` header at line 48)

One new paragraph (~6-10 lines) that does two things:

1. **Why the e2e harness uses `--strict-mcp-config`.** State the determinism rationale: without the flag, claude waits on configured MCP servers to register before processing the first prompt; on the GitHub runner that has no MCP servers configured this is mostly a no-op, but on operator machines (where `make e2e` also runs) it produces flaky first-prompt timing. Setting `TUIDRIVER_STRICT_MCP_CONFIG=1` from the runner side makes the harness behave identically regardless of host MCP config.
2. **Reconciliation against the May 18 walk-back.** Explicitly note: the May 18 walk-back recommended against `--strict-mcp-config` as the library's user-facing default sidestep — that walk-back was about pyry acp (a consumer where MCP is a user-facing feature; stripping it would harm users). The e2e/CI context has no user-facing MCP feature, so the harness using the flag is consistent with the walk-back, not a contradiction.
3. **Workflow file reference.** Final sentence pointing at `.github/workflows/e2e.yml` for the same navigation reason as Edit 1 (a reader landing on the doc can jump to the actual job).

Style constraint: this paragraph is prose, not a bullet list. It belongs as the third paragraph of § "Headless / CI plumbing", immediately after the existing *"No TTY is required on stdin."* sentence and before the next `##` heading. Do not introduce a new `###` subsection — the existing section is short and a fourth paragraph would unbalance it.

### What this spec deliberately does NOT touch

- The workflow file `.github/workflows/e2e.yml` — no behaviour change. The ticket is documentation-only.
- The runner code or `EnsureClaudeEnv` — the `TUIDRIVER_STRICT_MCP_CONFIG=1` env-var seam stays as-is; this doc just explains the rationale.
- The redundancy paragraph at line 61 (**`--strict-mcp-config` is NOT set at the CI layer.**) — Edit 2 must be *consistent* with that paragraph but does not modify it. The redundancy paragraph already explains *where* the flag is not set; Edit 2 explains *why* the flag is used at all. The two are complementary.
- Any other subsection (Cost controls, Cache shape, Artifacts, Exit code, Concurrency). The ticket's "Do not duplicate content already covered" constraint binds.

## Testing strategy

Documentation-only edit, no code tests. Verification at developer time:

- `grep -n '## CI integration\|## Headless / CI plumbing\|^\*\*Authentication' docs/knowledge/features/e2e-harness.md` — confirm the two anchor headings still exist and the **Authentication.** bold-lead is still at the start of its paragraph (so internal anchor links elsewhere in the doc don't break).
- `grep -n 'rotation procedure is tracked in #41' docs/knowledge/features/e2e-harness.md` — must return **zero matches** after Edit 1. Leaving the deferred-to-#41 sentence in the doc after this ticket lands would self-reference.
- `grep -n '\.github/workflows/e2e\.yml' docs/knowledge/features/e2e-harness.md` — must return **at least 3 matches** (the existing reference + the two new ones added by Edits 1 and 2).
- Markdown lint clean (`markdownlint docs/knowledge/features/e2e-harness.md` if the repo has one configured; otherwise a visual `glow` / GitHub preview check suffices).
- Internal anchor sanity: any link in the doc that points to `#authentication` / `#headless--ci-plumbing` / `#ci-integration` still resolves. Anchor slugs are generated from heading text — since both Edits keep the existing heading text and bold-lead text untouched, no anchor churn is expected.

## Open questions

- **Owning Anthropic account name.** The architect does not know which Anthropic Console account owns the `ANTHROPIC_API_KEY` secret in `pyrycode/tui-driver`. The developer should write the first sentence of the rotation procedure with the explicit placeholder text `<the maintainer's Anthropic Console account — Juhana to fill in before merge or in a follow-up doc PR>` and call this out in the PR description. The reviewer (Juhana, in practice) can either replace the placeholder inline at code-review time or merge with the placeholder and open a tiny follow-up doc PR. Either is acceptable; the spec does NOT want the developer to guess or omit the sentence.
- **Where exactly to place the "in-flight revocation" sub-list.** Both numbered lists (routine + in-flight) belong under the single **Authentication.** bold-lead paragraph. If the resulting paragraph feels too dense at developer time, the developer MAY promote *only* the procedures (not the entire Authentication block) to a nested format using two short bold-lead sub-paragraphs (`**Routine rotation:**` / `**In-flight revocation:**`) within the same paragraph. Do NOT split into a separate `###` subsection — see the Edit 1 style constraints for the rationale.

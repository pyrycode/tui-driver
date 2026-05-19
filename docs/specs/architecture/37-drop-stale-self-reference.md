# Spec: drop stale '#37 not yet landed' self-reference from features/e2e-harness.md

Ticket: [#37](https://github.com/pyrycode/tui-driver/issues/37). Size: XS. Doc-only, one-sentence deletion.

## Files to read first

- `docs/knowledge/features/e2e-harness.md:208-216` — § "`claude-version.lock`" → "Updating the lock file" numbered list. Step 5 (line 214) is the only line being changed. Read the surrounding list so the developer can confirm the first sentence stays intact and no list-numbering / formatting touch-ups are needed.

That is the entire reading list. No code paths, no other docs, no related specs — every operator-runbook section originally scoped for this ticket already lives elsewhere in the same file (see ticket body for the mapping), so there is nothing else to learn.

## Context

This ticket was split off from #31 to create a separate operator runbook for `make e2e`. During the documentation phases of #34, #35, #36, and #41, the runbook content accreted directly into `docs/knowledge/features/e2e-harness.md` instead — which now explicitly claims operator-runbook ownership and covers every section the original AC asked for. Re-extracting it would churn ~150 lines of prose for cosmetic gain.

The pragmatic close-out: keep the runbook where it is, delete the one sentence in step 5 of "Updating the lock file" that still promises a separate doc is coming.

## Design

In `docs/knowledge/features/e2e-harness.md`, locate the numbered list under § "`claude-version.lock`" → "Updating the lock file". Step 5 currently reads:

> 5. Commit the lock file edit in the same commit as any library changes that depend on the new claude. The full maintainer runbook lives in docs ticket #37 (not yet landed).

Delete the second sentence. The line must end after the first sentence, exactly:

> 5. Commit the lock file edit in the same commit as any library changes that depend on the new claude.

That is the entire change. One sentence removed (the trailing `" The full maintainer runbook lives in docs ticket #37 (not yet landed)."` — note the leading space that joins the two sentences inside the same Markdown list item must also go, so the line ends cleanly with the period after `claude.`).

### What NOT to touch

- The comment in the example lock file at lines 191-192 (`# Update deliberately when bumping the installed claude; the docs ticket / # (#37) covers the workflow.`) is intentionally left alone. The PO body's AC #2 says "No other doc changes"; the lock-file-comment cross-reference is not "not-yet-landed" prose and the PO has been explicit about scoping.
- No list renumbering. Steps 1–5 remain steps 1–5.
- No `INDEX.md` change. The existing pointer to `features/e2e-harness.md` already resolves to the runbook.
- No other section of the file. Every other paragraph stays byte-identical.

## Concurrency model

N/A — doc edit.

## Error handling

N/A — doc edit.

## Testing strategy

- Visual diff review: the developer's `git diff` MUST show exactly one line changed (or one line removed and one line added if the editor preserves the leading whitespace differently), and only inside step 5 of the "Updating the lock file" list.
- Render check (eyeball, not automated): step 5 still reads as a complete instruction (it does — the first sentence stands on its own).
- No new tests. No code change.

## Open questions

None.

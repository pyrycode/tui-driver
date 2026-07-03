# Spec #182 — Reconcile the `/agents` modal classifier for claude 2.1.199 (keep dormant)

**Ticket:** [#182](https://github.com/pyrycode/tui-driver/issues/182) — Reconcile the `/agents` modal classifier for claude 2.1.199 (wizard removed) — retire or keep dormant. Split from [#178](https://github.com/pyrycode/tui-driver/issues/178) (blocked-by, merged 2026-07-03). Size **XS** (doc-only, downgraded from `size:s` — see § Size).

**One line:** The `/agents` wizard was removed in claude 2.1.199, leaving `ModalClassAgents` / `ParseAgentList` dormant against the pinned claude. **Decision: keep dormant** — annotate the classifier + parser with a source comment recording the 2.1.199 removal and the pre-2.1.199-only match; change no executable code, no tests, no fixtures.

---

## Decision — keep dormant, do NOT retire

**Recorded decision (AC1):** **Keep the agents classifier + parser dormant.** Do not retire.

**One-line rationale, tied to pre-2.1.199 support:** tui-driver's `claude-version.lock` documents `version=` as an *informational reviewed reference* whose "patch drift above this version is intentionally tolerated" (`claude-version.lock:3-6`), and the library's stated scope is to "work against any TUI-style CLI in principle" (CLAUDE.md) — so a consumer on a pre-2.1.199 claude (≤2.1.158) that still renders the real `/agents` modal is a supported build, and the classifier + parser still earn their keep; retiring would delete real, passing coverage of a modal older builds still render in exchange for removing code that has caused **no observed failure** (Evidence-Based Fix Selection).

This route is what the AC calls the "keep dormant" path: *"the code is retained, but a source comment on `ModalClassAgents` (and/or `ParseAgentList`) documents that the `/agents` wizard was removed in claude 2.1.199 and that the classifier only matches pre-2.1.199 builds; unit tests continue to pass unchanged."*

### Why dormant beats retire (the four supporting facts)

1. **The lock policy makes older builds first-class.** `version=` is not enforced at runtime (#64); it means "the committed fixtures were confirmed against this claude," and drift *above* it is tolerated by design. Nothing in the repo declares 2.1.199 a hard *floor*. A ≤2.1.158 consumer is inside the supported envelope, and for that consumer `/agents` is a live modal the classifier must recognise.
2. **Dormant-not-wrong: it cannot misfire on the pinned build.** `DetectModalClass`'s agents branch (`modal.go:108-111`) requires the `Agents` header **AND** a `Running`/`Library` tab. The 2.1.199 removal notice ("The `/agents` wizard has been removed…") carries neither anchor, so the branch never matches it → `Unknown` (verified live while working #178, recorded in `docs/knowledge/codebase/178.md`). There is no false-positive, no reroute of any other class, no dead-but-dangerous code — only a branch that is *unreachable on 2.1.199 and reachable on older builds*.
3. **Retained coverage stays honest.** `agents-snapshot.bin` is a genuine pre-2.1.199 capture (host-independent — it parses a committed fixture, not a live session). `TestParseAgentListRealFixture` (`agents_test.go:29`) and the `DetectModalClass → ModalClassAgents` fixture row (`modal_test.go:113`) exercise the full parse+classify path and pass regardless of the installed claude. Keeping them costs nothing and documents the old render.
4. **Established codebase precedent is retain-not-delete.** #129 dropped the `/picker` e2e case but retained the picker parser + `.bin`; #178 dropped the `/agents` e2e case but retained `agents-snapshot.bin` + the classifier, and its Follow-ups section frames #182 as "mirrors the #129 → picker-classifier deferral shape." The #129 shape was **retain**. Retiring here would break from that precedent for a modal that is merely *dormant on one patch*, not *irreducibly host-dependent* (picker) or *harmful*.

**Simplicity-First / Demand-Elegance check:** the elegant move is the smallest one that removes the actual defect — the defect is a *documentation gap* (a future reader sees agents listed as a live mapped class and doesn't know the modal is gone from the pinned claude), not a correctness bug. A source comment closes that gap. Deleting a working, tested classifier to close a doc gap is over-engineering the fix.

---

## Files to read first

Everything the developer needs on turn 1; the Design section references only these.

- `pkg/tuidriver/modal.go:7-18` — the `ModalClass` type doc block. The `agents (loop 6 F-1) — /agents subagent list` bullet (line 16) currently presents agents as a live mapped class; **annotate for dormancy** (Design § 1a).
- `pkg/tuidriver/modal.go:21-31` — the `const (…)` block. `ModalClassAgents` is line 24 — **the anchor for the primary dormancy comment** (Design § 1b, the AC's required `ModalClassAgents` source comment).
- `pkg/tuidriver/modal.go:42-53` — the anchor-documentation block. The `agents → "Agents" header + "Running" or "Library" tab` line (line 47) describes the live anchors; **annotate for dormancy** (Design § 1c).
- `pkg/tuidriver/modal.go:63-82` + `97-131` — the `anchorAgents*` vars and the `DetectModalClass` agents branch. **Read-only, do NOT change** — this is the executable path that must stay byte-identical (the security constraint). Read it only to confirm the branch requires header AND tab.
- `pkg/tuidriver/agents.go:5-41` — `AgentList` type doc + `ParseAgentList` doc. **The second dormancy comment site** (Design § 2).
- `docs/knowledge/codebase/178.md` — the sibling that dropped the e2e case; its "Lessons learned" (the 2.1.199 removal is *feature removal*, not byte drift) and its "Follow-ups" scoping of this ticket. Read for the verified-live claim and the retain rationale.
- `docs/specs/architecture/129-drop-picker-snapshot-case.md` — the retain-parser-+-`.bin` precedent this decision mirrors, and the model for stale-doc-comment cleanup that `go vet`/`go build` don't catch.

**Do-not-touch, read only to confirm unaffected:**

- `pkg/tuidriver/permission.go:72` — the `// ParseAgentList / ParseMcpStatus.` doc-reference in `ParseModalContent`. **Leave it.** It names the shared render-prelude siblings; because we keep `ParseAgentList`, the reference stays valid (it would only need scrubbing on the *retire* path).
- `cmd/spike-multiselect/main.go:310-326` — the `case tuidriver.ModalClassAgents:` dispatch. **Leave it.** It still compiles and still parses the modal on older claude; retaining it is consistent with keeping the classifier.
- `pkg/tuidriver/modal_test.go` (lines 28-29, 94-102, 113) and `pkg/tuidriver/agents_test.go` — the agents unit tests. **Leave them.** AC3 requires they "continue to pass unchanged"; they read committed fixtures and are claude-version-independent.
- `pkg/tuidriver/testdata/agents-snapshot.bin` — **retain, untouched.**
- `claude-version.lock` — **do not touch** (the `version=` bump rode with #178).

---

## Context

Claude 2.1.199 removed the `/agents` wizard: `/agents` no longer opens the tabbed running-subagents modal — it prints a one-line removal notice that `DetectModalClass` classifies `Unknown`. #178 (merged 2026-07-03) already dropped the corresponding e2e snapshot case and bumped the lock `version=` to 2.1.199; that made `snapshot-drift` green. This ticket is the deliberate, decision-gated hygiene follow-up #178 deferred: reconcile the now-dormant *parser/classifier* code in `pkg/tuidriver/`.

Nothing here is required for any check to go green (the agents `.bin` unit tests already pass against any installed claude; `snapshot-drift` is already green). This is a documentation-fidelity change: make the source say what is true — the classifier is retained for pre-2.1.199 builds and is dormant against the pinned claude.

---

## Design

Doc-only. Three comment annotations in `modal.go` and one in `agents.go`. **No executable statement, no `var`/`const` value, no test, no fixture changes.** The developer writes the comment prose in the package's existing screen-literal / empirical-note idiom; the sketches below fix the *content*, not the wording.

### 1. `pkg/tuidriver/modal.go` — three annotations

**1a. `ModalClass` type doc bullet (line 16).** The bullet currently reads `agents (loop 6 F-1) — /agents subagent list`. Extend it to flag dormancy, e.g. append `(removed in claude 2.1.199; classifier matches pre-2.1.199 builds only)`. One clause; keep the loop-6 provenance.

**1b. Primary dormancy comment on `ModalClassAgents` (const, line 24).** Add a short comment (a `//` line group immediately above the const, or an inline trailing comment continued above — match the file's const-comment style; the block currently has none, so a 2-3 line comment above `ModalClassAgents` is cleanest). Content it must convey:
- The `/agents` wizard was **removed in claude 2.1.199**; on 2.1.199 `/agents` prints a one-line removal notice that `DetectModalClass` classifies `Unknown`.
- `ModalClassAgents` and its anchors therefore match **pre-2.1.199 builds only** (≤2.1.158 still render the real modal).
- Retained deliberately per the lock's tolerated-drift policy and the #129/#178 retain precedent — **not** dead code. Cross-reference: `#182`, and the fact that `agents-snapshot.bin` keeps it exercised.

This is the AC's required `ModalClassAgents` source comment.

**1c. Anchor-doc line (line 47).** The `agents → "Agents" header + "Running" or "Library" tab` line documents live anchors. Append a brief dormancy pointer (e.g. `(pre-2.1.199 only — see ModalClassAgents)`) so a reader scanning the anchor table isn't misled into thinking the modal renders on the pinned claude. Keep it a pointer, not a duplicate of 1b, to avoid comment drift across two sites.

**Executable code in `modal.go` is untouched.** The `anchorAgents*` vars (68-70) and the `DetectModalClass` agents branch (108-111) stay byte-identical. This is a hard constraint (see § Security review): the classifier's routing must not change.

### 2. `pkg/tuidriver/agents.go` — one annotation

Add a dormancy note to the `ParseAgentList` doc comment (line 24 block) and/or the `AgentList` type doc (line 5 block). Content: `ParseAgentList` parses the pre-2.1.199 `/agents` modal, which claude **removed in 2.1.199**; retained for pre-2.1.199-claude compatibility and exercised host-independently by `agents-snapshot.bin` (`TestParseAgentListRealFixture`). Prefer a single note on `ParseAgentList` (the exported entry point) with a one-line pointer, mirroring 1b/1c's single-source-of-truth split, rather than repeating the full rationale in both the type and function docs.

### What is deliberately NOT changed

- `permission.go:72`'s `ParseAgentList` doc-reference — valid because `ParseAgentList` is retained.
- `cmd/spike-multiselect/main.go`'s agents dispatch — retained; still parses on older claude.
- All agents unit tests + `agents-snapshot.bin` — retained, must pass unchanged (AC3).
- `claude-version.lock` — untouched (AC4).
- The e2e `agents` snapshot case — stays dropped; not re-introduced (AC4).

---

## Concurrency model

None. Comment-only change; no goroutines, locks, or shared state touched.

## Error handling

No new failure modes. `DetectModalClass` behaviour is byte-identical on every claude version: `ModalClassAgents` on a pre-2.1.199 `/agents` render, `Unknown` on the 2.1.199 removal notice (and on idle). No branch added or removed.

## Testing strategy

- **No test changes.** AC3 requires the agents unit tests "continue to pass unchanged."
- **Regression gate (must stay green, untouched):** `go test ./pkg/tuidriver/` — `TestParseAgentListRealFixture` (`agents_test.go:29`), `TestParseAgentListEmpty` (`agents_test.go:10`), `TestDetectModalClassSyntheticAnchors`'s two agents rows (`modal_test.go:28-29`), `TestDetectModalClassAgentsNeedsHeaderAndTab` (`modal_test.go:94`), and the `agents-snapshot.bin → ModalClassAgents` fixture row (`modal_test.go:113`) all pass unchanged — they read committed fixtures, not a live session.
- **Build/vet:** `go vet ./...`, `go build ./...` — confirm the comment edits don't break compilation (they can't; they're comments).
- **Grep sanity:** `grep -rn 'ModalClassAgents\|ParseAgentList\|AgentList' pkg cmd` still returns the expected retained references (this is the *keep* path — unlike the retire path, references SHOULD survive).

## Size

Downgraded **`size:s` → XS**. The dormant path is doc-only: **2 production `.go` files** (`modal.go`, `agents.go`), **0 test files**, 0 fixtures, ~4 comment annotations. Well under every split red line (files, LOC, exported types, call sites, ACs, reject branches). Scope self-check: production source files with new/modified content = 2 (< 5). Stays as one ticket; no PO split needed. (The *retire* path would have tripped the >3-file red line and required a `needs-rework:po` split per the ticket's Technical Notes — but retire was not chosen.)

## Open questions

None blocking. Two judgment calls, both resolved:

1. **Retire vs. dormant** — resolved to *dormant* above (the ticket's central decision; rationale tied to pre-2.1.199 support per AC1).
2. **How many comment sites** — resolved to one primary source of truth (1b on `ModalClassAgents`) plus two brief pointers (1a bullet, 1c anchor-doc) and one in `agents.go`, to avoid the same fact drifting across sites. A developer preferring a single comment on `ModalClassAgents` alone still satisfies AC3's letter, but the pointer-annotations prevent the type-doc and anchor-doc from silently staying stale.

---

## Security review

**Verdict:** PASS

The `security-sensitive` label flags that the *retire* path would edit `DetectModalClass` (the shared classifier routing the permission/trust modals that feed the auto-answer gate) and scrub a comment in `permission.go` (the permission/trust parser). The chosen **dormant** path makes neither edit — the concern is structurally defused rather than merely mitigated.

**Findings (walked adversarially against the chosen design):**

- **[Trust boundaries]** No finding. The security-relevant boundary is `DetectModalClass` — the untrusted `claude` PTY bytes → trusted `ModalClass` routing that `ParseModalContent` / `AnswerModal` build the auto-answer gate on. The design changes **zero bytes** of that function: the agents branch (`modal.go:108-111`), its `anchorAgents*` vars (68-70), and every other case are byte-identical to `main`. The label's specific worry — "confirm the branch removal cannot reroute a permission/trust render into a different class" — is answered by *removing no branch*: the permission (`117-119`), trust-folder (`115-116`), and permissions-config (`123-127`) branches keep their exact position and predicates, so no permission/trust render can be reclassified. The one adversarial question that remains — *could the retained agents branch ever swallow a permission/trust render?* — is answered no by construction and is unchanged from `main`: the agents branch requires `Agents` + (`Running`|`Library`), none of which appear in a permission ("Do you want to proceed") or trust ("Quick safety check") render, and it sits *after* mcp and *before* ask-user/trust/permission with disjoint anchors. This spec neither introduces nor removes that property.
- **[Subprocess / external command execution]** Not applicable. No `exec`, no keystroke injection, no new input path — comment-only. The retained `spike-multiselect` agents dispatch (which does call `ParseAgentList` on a snapshot) is unchanged and reads a captured buffer, not a live grant surface.
- **[Error messages, logs, telemetry]** No finding. No log lines, error strings, or telemetry added or changed. The retained parser's spike log lines (`agents-parsed …`) are untouched and carry only modal structure, no secrets.
- **[Concurrency]** Not applicable. No goroutine, lock, or shared-state change.
- **[Threat model alignment]** The relevant threat (a hostile prompt influencing claude's rendered modal to redirect a grant) lives on the *answer* path — `ParseModalContent` / `AnswerModal` route on `Index`, never on prompt-influenceable `Label` (system-overview.md, #146/#147). This spec touches none of that path and does not weaken it. The agents modal is read-only reporting (`spike-multiselect`), not a grant surface.

The label is, as the ticket anticipated, "harmlessly conservative" for the dormant path — but the review was run in full because the label is the gate, not the architect's size judgment.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-07-04

---

## Acceptance criteria (developer deliverables)

1. `pkg/tuidriver/modal.go`: a source comment on `ModalClassAgents` documents that the `/agents` wizard was removed in claude 2.1.199, that `DetectModalClass` returns `Unknown` for the 2.1.199 removal notice, and that the classifier matches pre-2.1.199 builds only (retained per the tolerated-drift lock policy + #129/#178 precedent). The `ModalClass` type-doc `agents` bullet (line 16) and the anchor-doc `agents` line (line 47) carry a brief dormancy pointer to it.
2. `pkg/tuidriver/agents.go`: `ParseAgentList` (and/or `AgentList`) carries a dormancy note — parses the pre-2.1.199 `/agents` modal (removed in 2.1.199), retained for pre-2.1.199 compatibility, exercised by `agents-snapshot.bin`.
3. No executable code changes: `anchorAgents*` vars, the `DetectModalClass` agents branch, `permission.go:72`, the `spike-multiselect` dispatch, all agents unit tests, and `agents-snapshot.bin` are unchanged.
4. `go vet ./...`, `go build ./...`, `go test ./...` are green; the agents unit tests pass **unchanged**.
5. `claude-version.lock` is not touched; the e2e `agents` snapshot case is not re-introduced (`snapshot-drift`, `claude-version-lock`, `spike-multiselect` continue to pass).

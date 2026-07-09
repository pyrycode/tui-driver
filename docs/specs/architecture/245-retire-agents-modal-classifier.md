# Spec #245 — Retire the `agents` modal classifier arm (dead surface on pinned claude)

**Ticket:** [#245](https://github.com/pyrycode/tui-driver/issues/245) — detect: retire or version-gate the agents modal class, dead surface on the pinned claude. Size **XS** (PO), confirmed XS here. `security-sensitive` — this spec carries the required security-review pass (verdict PASS, below). Reverses the "keep dormant" decision of [#182](https://github.com/pyrycode/tui-driver/issues/182). Blocks [#244](https://github.com/pyrycode/tui-driver/issues/244).

**One line:** Remove the `agents` arm from `detectModalClassWithGrid` so transcript content can no longer classify as `ModalClassAgents` on the pinned claude. Keep the exported constant, `ParseAgentList`/`AgentList`, and `agents-snapshot.bin` as frozen artifacts. Flip the two agents test assertions that pinned the old classify path.

---

## Decision — RETIRE, do NOT version-gate

**Recorded decision:** **Retire the classifier arm.** Delete the `case … return ModalClassAgents` arm (and its now-orphaned `anchorAgents*` vars) from `detectModalClassWithGrid`. Do not build a version gate.

### Why retire beats version-gate

Both options drive the pinned-claude false-positive surface to zero. The tie-breakers all point to retire:

1. **Simplicity-First / Demand-Elegance.** `DetectModalClass(snap []byte) ModalClass` is a pure function of the snapshot bytes — it has no channel to a "substrate version." A real version gate means either (a) threading a version/`enablePre2199Modals` value through `DetectModalClass` → `detectModalClassWithGrid` and every caller (the per-tick classifier in `events.go`'s `classify`, `HasUnknownDialog`, `isSlashPicker`, the `ParseModalContent`/`AnswerModal` path), an **exported-signature break with edit fan-out**, or (b) a build tag — clunky machinery no consumer would set. Retire deletes ~8 lines and adds zero surface. Building a gate to preserve a path with **zero current consumers** is over-engineering the fix.

2. **Evidence-Based Fix Selection — the #182 basis is now inverted.** #182 kept the class dormant on a *no-observed-cost* footing (no failure had been seen). The #227 `cmd/corpus-replay` harness now shows the healthy production recording `20260703T204559Z-…-ok.cast` classifying `agents` **six times** on plain transcript content. Every one of those is false by definition on 2.1.199. The defense (removal) now targets an *observed* failure, which is exactly when the pipeline escalates from advisory to code-level enforcement.

3. **The lock's tolerated-drift policy does not actually cover pre-2.1.199.** #182's second pillar was "a ≤2.1.158 consumer is inside the lock's tolerated-drift envelope." But `claude-version.lock` pins `version=2.1.199` and states drift is tolerated **above** that version (*"claude patch drift above this version is intentionally tolerated"*). A pre-2.1.199 build is **below** the reviewed pin — the opposite direction from what the policy protects. The only builds the agents classifier can ever true-fire on sit outside the tolerated-drift envelope. Both legs #182 stood on are gone.

4. **Retirement is reversible at the granularity that matters.** The exported `ModalClassAgents` constant, `ParseAgentList`/`AgentList`, and the frozen `agents-snapshot.bin` fixture all stay. A future consumer who genuinely needs pre-2.1.199 `/agents` classification can reconstruct the ~5-line arm from git history against the retained fixture. Nothing irrecoverable is deleted.

**Scope guard honored.** Retirement is limited to the classifier switch arm plus its dead anchor vars. `agents.go` (parser), `agents-snapshot.bin`, and `cmd/spike-multiselect/main.go`'s `case tuidriver.ModalClassAgents:` dispatch all stay — the full teardown of those spans >3 files and would trip the split red line (per `docs/knowledge/codebase/182.md`'s file map). This spec does not go there.

---

## Files to read first

Everything the developer needs on turn 1; the Design section references only these. **Self-reference warning applies** — anchors are referenced by location, not quoted; AC2 (`corpus-replay`) is hand-run outside the pipeline from a clean recording dir.

- `pkg/tuidriver/modal.go:230-263` — `detectModalClassWithGrid`, the classifier `switch`. **The agents arm is lines 234-238** (`gridContains(anchorAgentsHeader) && (Running||Library) && snapHasPickerHighlight → ModalClassAgents`). This is the retire target. Confirm the arm sits between `mcp` (232-233) and `trust` (239-240) and that deleting it leaves every other arm's predicate byte-identical.
- `pkg/tuidriver/modal.go:109-121` — the `var (…)` anchor block. Lines **111-113** are `anchorAgentsHeader` / `anchorAgentsTabRunning` / `anchorAgentsTabLibrary`. These are used **only** by the arm (grep-confirmed: no other reference in `pkg/` or `cmd/`), so they become dead on removal — delete them too.
- `pkg/tuidriver/modal.go:7-42` — `ModalClass` type-doc (agents bullet at **16-17**) and the `const (…)` block. `ModalClassAgents` (line 35) with its dormancy comment (**25-34**). **Keep the constant; rewrite the comment** from "dormant / matches pre-2.1.199 builds only" to "classifier arm retired in #245; constant + `ParseAgentList` + `agents-snapshot.bin` retained as frozen artifacts."
- `pkg/tuidriver/modal.go:44-108` — the anchor-documentation block. The agents anchor description is **70-73**. With the vars gone there is no agents anchor to document — remove that entry (the retirement rationale lives on `ModalClassAgents`).
- `pkg/tuidriver/modal_test.go:257-294` — `TestDetectModalClassRealFixtures`. **Line 269** `{"agents-snapshot.bin", ModalClassAgents}` flips to `ModalClassUnknown` (AC1).
- `pkg/tuidriver/modal_test.go:173-181` — `TestDetectModalClassAgentsNeedsHeaderAndTab`. Repurpose into the AC3 "arm retired, cannot fire" regression (Design § 3).
- `pkg/tuidriver/modal_test.go:25-59` — `TestDetectModalClassSyntheticAnchors`. Line 34 (`agents header + tab text alone is NOT agents`) already asserts `ModalClassUnknown`; it stays valid (now trivially). Refresh its inline comment only.
- `pkg/tuidriver/anchor_forgery_test.go:241-265` — `TestWholeGridAnchorsRejectContentForgery`; the agents case is **line 254**, already asserting `ModalClassUnknown`. Assertion is unchanged (still Unknown) but now holds by construction, not by co-signal — refresh its framing. Also sync the file-header enumeration (**line 30** "the agents pair", **46-48** the co-signal list) to record the retirement.
- `pkg/tuidriver/dialog_shape.go:8-36` — `selectionOptionRe` + `gridHasSelectionDialog`. Read to understand **why the fall-through is safe** (Error handling § below): the agents footer carries "Enter to select", but `selectionOptionRe = ^❯\s+(?:\d+\.|\[.\])` does not match the `❯ /agents` input row, so ask-user does not fire.
- `pkg/tuidriver/picker.go:78, 209-256` — `pickerRowStartRe`, `gridHasPickerRow`, `pickerRegionRows`, `isSlashPickerWithGrid`. Read to confirm the other fall-through candidate is also closed: `pickerRowStartRe = ^[ \t\r]*/[a-zA-Z]` does not match the `❯ /agents` row (it starts with `❯`, not `/`).
- `pkg/tuidriver/agents.go:24-45` — `ParseAgentList` doc (the "Dormant / See #182" note at 26-29). One-line pointer refresh (Design § 4); parser body untouched.
- `claude-version.lock` — the `version=2.1.199` pin and the drift-tolerated-**above** wording. **Do not touch it** (AC5); read only to ground the Decision.
- `docs/knowledge/codebase/182.md` — the dormant decision this reverses, and its retire-path file map (the split red line the scope guard respects).
- `cmd/corpus-replay/README.md` — how AC2 is hand-run (external recordings dir, deterministic byte replay, not a CI gate).

---

## Context

The `agents` class is pure false-positive surface on the pinned claude. Its anchors are the most generic in the set: the arm fires on `anchorAgentsHeader` ("Agents" — a word claude also paints in its status bar, per the old `TestDetectModalClassAgentsNeedsHeaderAndTab`), OR-ed with one of two generic tab words ("Running" matches claude's own progress line, "Library" appears inside code identifiers), AND `snapHasPickerHighlight` — a highlight color (xterm index 153 and its truecolor twin) that `picker.go`'s own docs describe as "a general light blue claude paints on paths, links and markers, so it appears in a large fraction of frames." No structural co-signal binds the class to real panel chrome, so on a healthy transcript all three coincide and forge the class. The #227 corpus harness caught it firing 6× on one production-ok recording.

The `/agents` wizard that made this class real was **removed in claude 2.1.199** (`ModalClassAgents`'s own doc records this; #178 dropped the e2e case and bumped the lock to 2.1.199). On the pinned claude the modal cannot render, so every fire is false by construction. #182 kept the class dormant-and-annotated on a no-observed-cost basis; #227 supplies the observed cost, and this ticket removes the arm.

---

## Design

One substantive production file (`modal.go`), one one-line doc pointer (`agents.go`), two test files. No new files, no new types, no signature changes.

### 1. `pkg/tuidriver/modal.go` — remove the arm and its anchors

- **Delete the agents `case` (234-238).** After removal the `switch` runs `mcp → trust → permission → model-select → permissions-config → ask-user`, then slash-picker last. Every remaining arm's predicate and order is byte-identical (verified: removing only this arm compiles and every other fixture still classifies correctly — see Testing strategy).
- **Delete the three `anchorAgents*` vars (111-113).** They have no other consumer.
- **Keep `ModalClassAgents` (35); rewrite its comment (25-34).** New content: the `/agents` wizard was removed in claude 2.1.199; the classifier arm was **retired in #245** because on the pinned claude it could only fire falsely (6× on a #227 corpus-ok recording); the constant, `ParseAgentList`, and `agents-snapshot.bin` are retained as frozen artifacts (source-compat for the external consumer + `spike-multiselect` dispatch; the parser still parses a captured pre-2.1.199 modal host-independently). This is the single source of truth for the retirement.
- **Remove the agents anchor-doc entry (70-73).** With the vars gone there is no anchor to document; the rationale lives on the const.
- **Update the `ModalClass` type-doc agents bullet (16-17)** to a one-line pointer: agents retired (see `ModalClassAgents`), not a live class.

**No other executable byte in `modal.go` changes.** `gridContains`, `gridHasSelectionDialog`, `snapHasPickerHighlight`, `gridHasPermissionDialog`, and every other arm are untouched — the hard security constraint (§ Security review).

### 2. `pkg/tuidriver/modal_test.go` — flip the classify assertion

- **`TestDetectModalClassRealFixtures` line 269:** `{"agents-snapshot.bin", ModalClassAgents}` → `{"agents-snapshot.bin", ModalClassUnknown}`. Add a one-line comment: the fixture is a real pre-2.1.199 capture; the arm is retired (#245), so it now reads Unknown — and the fall-through is Unknown, not a misclassification (see Error handling). This is AC1's fixture flip.

### 3. `pkg/tuidriver/modal_test.go` — the retired-arm negative regression (AC3)

Repurpose `TestDetectModalClassAgentsNeedsHeaderAndTab` (173-181) into the "arm cannot fire" regression. The old test proved a weak input (status-bar "Agents") didn't classify. The strong regression is: **the very fixture that used to classify agents no longer does.**

- Rename to something like `TestDetectModalClassAgentsRetiredNeverFires`.
- Load `agents-snapshot.bin` (the real render that classified `ModalClassAgents` before #245) and assert `DetectModalClass(snap) != ModalClassAgents` — inert by construction, no version gate.
- Keep (or fold in) the original weak synthetic case as a second sub-assertion if desired; the fixture case is the load-bearing one.

Bullet-point scenarios (developer writes the Go in the file's idiom):
- Input: `agents-snapshot.bin` bytes → expect `DetectModalClass` **not** `ModalClassAgents` (in fact `ModalClassUnknown`).
- Input: synthetic `"...press ← for agents..."` → expect not `ModalClassAgents` (unchanged intent).

### 4. `pkg/tuidriver/anchor_forgery_test.go` — reframe, assertion unchanged

- The agents case in `TestWholeGridAnchorsRejectContentForgery` (line 254) still asserts `ModalClassUnknown`. **Do not change the assertion** — change the surrounding framing: agents no longer fires because the arm is retired, not because a co-signal is unmet.
- Sync the file-header anchor-site enumeration (line 30 "the agents pair") and the co-signal list (46-48) to note agents is retired in #245 (drop it from the "co-signal-protected whole-grid panel classes" set).

### 5. `pkg/tuidriver/agents.go` — one-line pointer refresh

`ParseAgentList`'s doc (26-29) currently says "Dormant on the pinned claude … (see ModalClassAgents) … Retained … See #182." Update the pointer: the **classifier arm was retired in #245**; the parser is retained for pre-2.1.199 captures and stays exercised by `agents-snapshot.bin`. Parser body and every struct untouched. (This preserves the #182 single-source-of-truth split: full rationale on `ModalClassAgents`, one-line pointer here.)

### What is deliberately NOT changed

- `ParseAgentList`/`AgentList` bodies, `agents-snapshot.bin`, `TestParseAgentListRealFixture`, `TestParseAgentListEmpty` — retained, must stay green (AC4).
- `cmd/spike-multiselect/main.go:310` `case tuidriver.ModalClassAgents:` — still compiles (constant retained); becomes an unreachable-via-`DetectModalClass` branch, which is harmless and in scope to keep.
- `permission.go` — no agents reference to scrub (that was a `ParseAgentList` doc-reference; parser retained → reference stays valid).
- `claude-version.lock`, the e2e `agents` snapshot case — untouched / not re-introduced (AC5).

---

## Concurrency model

None. Synchronous classifier + comment + test edits. No goroutines, locks, or shared state touched.

---

## Error handling — the fall-through is the load-bearing analysis

Removing an arm changes what a snapshot that *used* to hit it now classifies as. The one real risk is a **new misclassification**: the agents render carries the generic "Enter to select" footer, so it could fall through to `ask-user-question`; and it has a `❯ /agents` input row, so it could fall through to `slash-picker`. Both are closed:

- **ask-user fall-through — closed.** `ask-user` requires `gridContains("Enter to select")` **AND** `gridHasSelectionDialog`. The footer satisfies the first, but `gridHasSelectionDialog` matches `selectionOptionRe = ^❯\s+(?:\d+\.|\[.\])` — a pointer glyph followed by a **numbered or checkbox** token. The only `❯` row in the agents fixture is `❯ /agents` (the user's typed command at the input line), which is neither. So `gridHasSelectionDialog` returns false → ask-user does **not** fire.
- **slash-picker fall-through — closed.** `slash-picker` requires a bottom-region row matching `pickerRowStartRe = ^[ \t\r]*/[a-zA-Z]`. The `❯ /agents` row starts with `❯`, not optional-whitespace-then-`/`, so `pickerRowStartRe` does not match it. (And the region scope excludes it regardless.) So slash-picker does **not** fire.

**Ground truth (verified during spec authoring, not asserted from reasoning):** with the arm physically removed, `DetectModalClass("agents-snapshot.bin")` returns `ModalClassUnknown`, and `HasUnknownDialog` returns **false** — so the fixture reads as a clean idle/unknown screen and does not trip the #224 novel-dialog bail either. Every other committed fixture (`mcp`, `picker`, `permission`, `trust-folder`, `ask-user-question-screen`) still classifies to its own class unchanged.

**One honest residual (out of scope, zero observed surface):** the committed fixture is the *empty* Running tab (no numbered option rows). A hypothetical *populated* pre-2.1.199 agents modal — if claude rendered its list as `❯ 1. <agent>` numbered rows — would satisfy both `gridHasSelectionDialog` and the "Enter to select" footer and could fall through to `ask-user-question` on a pre-2.1.199 build. This is not reachable on the pinned claude (the modal doesn't render), no current consumer runs pre-2.1.199, and a version gate would produce the *same* fall-through in its default-off state anyway (so it buys nothing here). Recorded, not defended — no fixture exists for a populated agents modal and the upstream feature is gone, so it cannot be captured. See Security review § Threat model.

---

## Testing strategy

- **AC1 — fixture classify flip:** `TestDetectModalClassRealFixtures` `agents-snapshot.bin` row now expects `ModalClassUnknown`; the whole table stays green (all other fixtures unaffected — verified).
- **AC3 — retired-arm regression:** `TestDetectModalClassAgentsRetiredNeverFires` (repurposed) asserts the real fixture no longer classifies agents; the `anchor_forgery_test.go` agents case still asserts Unknown.
- **AC4 — parser + source compat:** `TestParseAgentListRealFixture`, `TestParseAgentListEmpty` pass unchanged (they read the retained fixture, not the classifier). `go build ./...` confirms `ModalClassAgents` still compiles for `spike-multiselect` and the external consumer.
- **Regression gate:** `make check` (`go vet ./...` + `go test -race ./...`) green. Note `make check` runs **no** staticcheck, so the dead `anchorAgents*` vars would not fail the build if left — but the Design removes them anyway (clean retire, honest anchor-doc).
- **AC2 — corpus replay (hand-run, not `make check`):** from a clean recordings dir, `make corpus-replay CORPUS_DIR=<operator recordings>` (or the binary directly) replays `20260703T204559Z-…-ok.cast` and reports **zero** `agents` structural fires. Deterministic recorded-byte playback, no live claude. Per the org rule and the ticket's self-reference warning, run this outside the pipeline.

Test cases are described as scenarios above; the developer writes the Go in the package's existing table-driven idiom.

---

## Size

**XS (confirmed).** Production source files with new/modified content: **`modal.go` + `agents.go` = 2** (< 5 self-check gate). Test files: `modal_test.go`, `anchor_forgery_test.go` (excluded from the production count). No new files, no new exported types, no new call sites, no signature changes — the change is a net deletion (one arm, three vars, one anchor-doc entry) plus a comment rewrite and three test-assertion/framing edits. Total written work well under 600 LOC and every split red line. Both the retire and version-gate options were weighed at size-check; retire is the smaller, and version-gate was rejected on merit (Decision § 1), not merely on size. No route-back to PO.

---

## Open questions

None blocking.

1. **Retire vs version-gate** — resolved to *retire* (Decision).
2. **Remove or tombstone the anchor-doc agents entry** — resolved to *remove* (vars gone; rationale consolidated on `ModalClassAgents`). A developer who prefers a one-line "retired, see `ModalClassAgents`" tombstone in the anchor-doc still satisfies the ACs.
3. **Populated-agents-modal fall-through on pre-2.1.199** — recorded as an out-of-scope residual (Error handling / Security review); not reachable on the pinned claude, no fixture exists, no current consumer.

---

## Security review

**Verdict:** PASS

The `security-sensitive` label gates on the surface touched, not the blast radius of the chosen path (the label-is-the-gate rule, per `docs/knowledge/codebase/182.md`). The arm removed here lives in `detectModalClassWithGrid` — the shared classifier that also routes `permission`/`trust-folder` into the `ParseModalContent` → `AnswerModal` auto-answer gate. Full adversarial walk below.

**Findings:**

- **[Trust boundaries]** No finding. The security-relevant boundary is `detectModalClassWithGrid`: untrusted claude PTY bytes → trusted `ModalClass` routing. This change **removes one arm and its three anchor vars and touches no other arm's bytes.** The `permission` (241-242), `trust-folder` (239-240), `model-select` (243-244), `permissions-config` (245-250), and `ask-user` (251-252) arms keep their exact predicates and switch order, so no permission/trust render can be reclassified by a reorder. The one adversarial question the label demands — *can removing the agents arm reroute a permission/trust render into a wrong class?* — is answered **no** by the fall-through analysis: a snapshot reaching the deleted arm falls through to the arms below it, all of which require their own disjoint anchors (proceed-prompt, trust header, Allow/Ask/Deny, etc.) that an agents render does not carry. Ground-truth verified: every non-agents committed fixture still classifies to its own class after removal.
- **[Trust boundaries — new-misclassification risk]** No MUST-FIX. The specific worry that a de-classified agents render acquires a *grant-bearing* class (`permission`/`trust-folder`) is refuted: neither the "Do you want to proceed" prompt nor the "Quick safety check" header appears in an agents render, and both grant classes additionally require the `modalOptionRe` `❯ <n>.` dialog shape the agents empty-state lacks. The only theoretically reachable non-Unknown fall-through is `ask-user-question` (generic "Enter to select" footer) — and it is closed for the committed fixture because `selectionOptionRe` does not match `❯ /agents` (ground-truth: fixture → `Unknown`, `HasUnknownDialog=false`). The residual populated-modal case (Error handling §) is **OUT OF SCOPE**: unreachable on the pinned claude, no current pre-2.1.199 consumer, `ask-user-question` is itself an *Index*-routed selection surface (grant redirection there is already mitigated structurally — system-overview.md, #146/#147), and a version gate would not close it in its default state. Named, not deferred to a specific ticket, because there is no capturable artifact for it.
- **[Subprocess / external command execution]** Not applicable. No `exec`, no keystroke injection, no new input path. The retained `spike-multiselect` agents dispatch reads a captured buffer via `ParseAgentList`, not a live grant surface, and is unchanged.
- **[Error messages, logs, telemetry]** No finding. No log line, error string, or telemetry added or changed. `AnswerModal`'s error path (enum + integer `Choice`, no screen literal — system-overview.md) is not touched.
- **[Concurrency]** Not applicable. No goroutine, lock, or shared-state change; the classifier is a pure function.
- **[File operations] / [Tokens] / [Crypto] / [Network & I/O]** Not applicable — none present in a classifier-arm deletion.
- **[Threat model alignment]** The relevant threat (a hostile prompt shaping claude's rendered screen to redirect a grant) lives on the *answer* path — `ParseModalContent`/`AnswerModal` route on `Index`, never on prompt-influenceable `Label`. This spec touches none of that path. The agents modal was read-only reporting (`spike-multiselect`), never a grant surface, so **removing** its classification cannot cause a wrong grant — it strictly *reduces* the false-classification surface (6 false fires → 0 on the pinned claude), which is a net threat-model improvement.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-07-09

---

## Acceptance criteria (developer deliverables)

1. `pkg/tuidriver/modal.go`: the `agents` arm (`… → ModalClassAgents`) is removed from `detectModalClassWithGrid`, along with the now-orphaned `anchorAgentsHeader`/`anchorAgentsTabRunning`/`anchorAgentsTabLibrary` vars and the agents anchor-doc entry. `ModalClassAgents` is **retained**; its comment (and the `ModalClass` type-doc bullet) records the #245 retirement and the retained-artifact rationale. `TestDetectModalClassRealFixtures`' `agents-snapshot.bin` case now asserts `ModalClassUnknown`.
2. `pkg/tuidriver/agents.go`: `ParseAgentList`'s doc pointer is updated (classifier arm retired in #245; parser retained, exercised by `agents-snapshot.bin`). Parser body and structs unchanged.
3. The negative-suite agents cases assert the arm cannot fire, inert by construction: `TestDetectModalClassAgentsNeedsHeaderAndTab` is repurposed (e.g. `…AgentsRetiredNeverFires`) to load `agents-snapshot.bin` and assert `!= ModalClassAgents`; the `anchor_forgery_test.go` agents case (`TestWholeGridAnchorsRejectContentForgery`) still asserts `ModalClassUnknown` with framing/header-enumeration updated to note the retirement.
4. Source compatibility preserved: `ModalClassAgents` still compiles for the external consumer and `cmd/spike-multiselect`'s dispatch; `agents-snapshot.bin`, `TestParseAgentListRealFixture`, and `TestParseAgentListEmpty` stay green unchanged.
5. `make check` (`go vet ./...` + `go test -race ./...`) is green; `claude-version.lock` is not touched and the e2e `agents` snapshot case is not re-introduced. (Hand-run, not a CI gate: `make corpus-replay` over `20260703T204559Z-…-ok.cast` reports zero `agents` structural fires.)

# Spec #173 — Invert the permissive default for the pre-first-prompt window

**Ticket:** [#173](https://github.com/pyrycode/tui-driver/issues/173) · size:s · security-sensitive
**One line:** `WaitReady` stops presenting a recognized-but-unexpected startup modal as the clean-ready happy path — it returns a loud, typed error instead, so a consumer that reads only the top-line signal (in Go, the `error`) halts rather than typing its first prompt into a dialog.

---

## Files to read first

- `pkg/tuidriver/ready.go` (whole file, ~96 lines) — `Readiness` struct, `WaitReady:69`, `isUnknownModal:88`, and the **sibling** `ProcessExitedError:46` typed error. `WaitReady` already returns one typed error for an abnormal pre-idle condition; this ticket adds a second for an abnormal *at-idle* condition. Mirror `ProcessExitedError`'s shape.
- `pkg/tuidriver/modal.go:22-42` — the `ModalClass` const set (`ModalClassUnknown`, `…Permission`, `…MCP`, `…Agents`, `…AskUserQuestion`, `…ModelSelect`, `…PermissionsConfig`, `…SlashPicker`, `…TrustFolder`). This is what the new predicate switches on.
- `pkg/tuidriver/modal.go:121-170` — `DetectModalClass`. Note the #158 overload: it returns `ModalClassUnknown` for **both** the clean no-modal idle screen **and** a genuinely-unrecognized modal. That overload is why the novel-chrome case is out of scope (see § Open questions).
- `pkg/tuidriver/state.go:104-122` — `IsIdle`, the precondition `WaitReady` waits on. **Do not touch it** (AC4). It stays the sole idle/busy predicate for mid-session flows.
- `pkg/tuidriver/ready_test.go:10-64` — `TestWaitReady`'s table. The `"unrecognized modal at idle"` case (currently asserts `Readiness{Idle:true, UnknownModal:true}`) flips to assert the new error; the clean-idle / trust / mcp / network cases stay unchanged.
- `pkg/tuidriver/modal_test.go:235-259` — `TestDetectModalClassRealFixtures`: the fixture→class map and the `os.ReadFile(filepath.Join("testdata", …))` idiom to reuse for the fixture-based `WaitReady` test.
- Consumer context (cross-repo, do **not** edit from here): pyrycode `internal/agentrun/ptyrunner` — its `Run` calls `WaitReady`, converts each `Readiness` flag into a typed `Err*` (`TestRun_TrustModalDetected` / `…McpFailureDetected` / `…NetworkFailureDetected`), and returns `nil` on the clean-idle happy path. An unexpected modal today falls through those known-flag branches into the happy path → the consumer types into it. See pyrycode `docs/knowledge/features/ptyrunner-package.md`.

**Fixture IsIdle grounding** (measured 2026-07-07 — `WaitReady` blocks until the fixture is `IsIdle`, so a test must only feed it fixtures that render idle):

| fixture | `IsIdle` | `DetectModalClass` | usable in a `WaitReady` test? |
|---|---|---|---|
| `permission-snapshot.bin` | **true** | `permission` | ✅ unexpected modal → error |
| `mcp-empty-snapshot.bin` | **true** | `mcp` | ✅ unexpected modal → error |
| `trust-folder-snapshot.bin` | **true** | `trust-folder` | ✅ benign → `TrustModal` flag, nil error |
| `mcp-snapshot.bin` | false | `mcp` | ❌ never idle — `WaitReady` would hang |
| `agents-snapshot.bin` | false | `agents` | ❌ never idle |
| `picker-snapshot.bin` | false | `slash-picker` | ❌ never idle |
| `ask-user-question-snapshot.bin` | false | `""` (JSONL dump) | ❌ never idle, classifies Unknown |

---

## Context

**The problem.** `WaitReady` is the pre-first-prompt readiness call. It waits for `IsIdle`, then builds a `Readiness` that is *always* `Idle: true` on a nil-error return, decorating it with report-only advisory flags (`TrustModal`, `McpFailure`, `NetworkFailure`, and #158's `UnknownModal`). A consumer that reaches idle and doesn't recognize any flag it cares about proceeds to type its first prompt. #158 added `UnknownModal` as an *advisory* flag the consumer **may** check — a permissive, opt-in default. That is exactly what let the Claude 2.1.199 change break silently: unrecognized/unexpected startup chrome defaulted to "ready."

**The inversion.** For the narrow pre-first-prompt window (which maps cleanly onto `WaitReady` alone — no new latch needed), invert the default: a recognized modal class that is not part of a clean startup must **fail loudly**, not ride as an ignorable flag. This turns #158's advisory precursor into a fail-safe.

**Why "loud error" is the mechanism** (the ticket delegates the mechanism to the architect). The binding requirement is: *"a consumer that reads only the top-line ready signal must not silently proceed."* Three mechanisms were considered:

1. **New positive `Ready bool` field / flip `Idle`.** Rejected: the consumer's *existing* code (`ptyrunner.Run`) branches on the known flags and falls through to a `nil`-error happy path. A new/flipped field the existing code doesn't read leaves the hole open until the consumer opts in — the same opt-in weakness #158 already had. Not fail-safe.
2. **Keep-waiting-until-ctx-timeout.** Rejected: slow, diagnostic-free ("context deadline exceeded"), and it denies the driver the chance to inspect and handle the modal.
3. **Loud typed error carrying the class** — chosen. In idiomatic Go the top-line signal of a `(T, error)` call *is* the `error`; the consumer's existing `if err != nil { return err }` catches it with **zero consumer change**, closing the hole structurally. The driver still *decides*: `errors.As` recovers the `ModalClass`, so a driver that wants to dismiss e.g. a model-select modal can. This is the ticket's literal title — *invert the permissive default* from "silently proceed" to "halt unless you explicitly handle" — and it is a direct sibling of the existing `ProcessExitedError` return.

This does not violate "the library detects; the driver decides": for the **known** conditions (trust / mcp / network) a nuanced policy exists → keep them as flags (AC3). For the **unexpected** condition the driver by definition has no policy → the safe default is halt, with recovery available.

---

## Design

All changes are in `pkg/tuidriver/ready.go` (+ its test). No new files, no other package touched.

### 1. New typed error `UnexpectedModalError`

A sibling of `ProcessExitedError`. Contract:

- Struct with one exported field `Class ModalClass` (the `DetectModalClass` result that tripped the gate).
- `Error() string` → an operator-readable message embedding the class, e.g. `"tuidriver: unexpected modal at startup: permission"`. `Class` is a value from the fixed `ModalClass` const set, never attacker free-text (see § Security review).
- **No `Unwrap`** — there is no wrapped cause (unlike `ProcessExitedError`, which wraps the process exit status).
- Matched by consumers with `errors.As(err, &ume)`.

### 2. `WaitReady` gains one classify-and-gate step

Behaviour contract (signature unchanged: `(ctx context.Context) (Readiness, error)`):

1. Wait for idle exactly as today (`waitUntilOrExit` on `IsIdle`); on its error, return `Readiness{}, err` unchanged.
2. Snapshot once, compute `DetectModalClass(snap)` **once**.
3. If the class is an *unexpected startup modal* (predicate below), return `Readiness{}, &UnexpectedModalError{Class: class}`.
4. Otherwise build and return the `Readiness` (nil error) with the report-only flags, exactly as today **minus** the removed `UnknownModal` field.

The **zero-Readiness-on-error invariant is preserved** — the new error path returns `Readiness{}`, same as the ctx and process-exited paths. `TestWaitReadyContextCancelled`'s `got == Readiness{}` assertion continues to hold for the new error too.

### 3. Predicate: `isUnexpectedStartupModal(class ModalClass) bool`

Repurpose the existing `isUnknownModal` (rename + change to take the already-computed `ModalClass`, so `DetectModalClass` renders the grid only once instead of twice). Same exclusion set as today:

```
func isUnexpectedStartupModal(class ModalClass) bool
// false for: ModalClassUnknown  (the common clean no-modal idle screen)
//            ModalClassTrustFolder (benign startup; surfaced by Readiness.TrustModal per AC3)
// true  for: every other recognized class (permission, mcp, agents, ask-user-question,
//            model-select, permissions-config, slash-picker) — unexpected at startup
```

Why each excluded class is safe to exclude:
- `ModalClassUnknown` → the clean idle path; excluding it is what keeps the clean-ready case nil-error.
- `ModalClassTrustFolder` → AC3: trust stays "driver decides," returned as `Readiness{Idle:true, TrustModal:true}`, nil error.

Why `ModalClassMCP` is correctly treated as unexpected without regressing AC3: the benign **MCP-failure banner** is `HasMcpFailureBanner` (regex `"N MCP servers failed"`, a status line) — a *different* detection from `ModalClassMCP` (the `/mcp` "Manage MCP servers" / "No MCP servers configured" panel, which only appears if `/mcp` was typed). A startup MCP-failure banner leaves `DetectModalClass == ModalClassUnknown` → nil error → `Readiness{McpFailure:true}` as today.

### 4. Remove the `Readiness.UnknownModal` field

The field was #158's advisory precursor; #173 supersedes it with the error. Keeping it would make it **permanently false** on the nil-error path (any class that would have set it now errors before the `Readiness` is built) — a dead, contradictory field. Removal is the completion of the inversion, not scope creep. In-repo it is referenced only in `ready.go` and `ready_test.go` (grep-verified); `WaitReady` has no in-repo callers (codegraph-verified). This is a **breaking API change** flagged for the consumer in § Open questions — it surfaces as a compile error (loud, coordinated), never a silent behaviour change.

### Data flow (unchanged except the new gate)

```
WaitReady(ctx)
  └─ waitUntilOrExit(IsIdle)  ──err──▶ return Readiness{}, err        (ctx / ProcessExitedError — unchanged)
       │ idle
       ▼
     class := DetectModalClass(snap)          (rendered once)
       │
       ├─ isUnexpectedStartupModal(class) ──▶ return Readiness{}, &UnexpectedModalError{class}   ← NEW
       │
       └─ else ──▶ return Readiness{Idle:true, TrustModal, McpFailure, FailedMcpCount, NetworkFailure}, nil
                   (report-only flags — AC3 preserved; UnknownModal field gone)
```

---

## Concurrency model

None new. `WaitReady` is a synchronous call; `waitUntilOrExit` is the existing wait primitive. `DetectModalClass`/`IsIdle` are pure functions over a snapshot. No goroutines spawned, no locks introduced, no shutdown sequence changed.

---

## Error handling

- **Ctx cancellation / process exit before idle** — unchanged: `waitUntilOrExit` returns the ctx cause or `*ProcessExitedError`; `WaitReady` returns `Readiness{}, err`.
- **Unexpected startup modal at idle** — new: `Readiness{}, &UnexpectedModalError{Class: class}`. Recoverable by the driver via `errors.As`.
- **Zero-Readiness invariant** — all error paths return `Readiness{}`. Documented on the `Readiness` type; do not weaken it.
- The three benign conditions never produce an error — they remain report-only flags on a nil-error `Readiness`.

---

## Testing strategy

Update `TestWaitReady` and add fixture-based coverage. All in claude-free `make check`. Scenarios (developer writes them in the package's testing idiom — table-driven, `os.ReadFile` for fixtures):

- **Clean idle still ready** (AC1, AC5 second half): synthetic idle snapshot (`idleGlyphTest + " "`, the existing idiom — there is no clean-idle `.bin`) → nil error, `Readiness{Idle: true}`.
- **Unexpected recognized modal → loud error, real fixture** (AC2, AC5): feed `permission-snapshot.bin` (real, `IsIdle`=true, classifies `permission`) → assert `errors.As(err, &UnexpectedModalError{})`, the recovered `Class == ModalClassPermission`, and `got == Readiness{}`. Add `mcp-empty-snapshot.bin` (→ `Class == ModalClassMCP`) for a second real fixture.
- **Do NOT feed** `mcp-snapshot.bin` / `agents-snapshot.bin` / `picker-snapshot.bin` / `ask-user-question-snapshot.bin` to `WaitReady` — they are not `IsIdle`, so `WaitReady` would block until the ctx deadline (use a bounded `context.WithTimeout` in tests regardless, as a backstop). For synthetic coverage of a non-`IsIdle`-fixture class (e.g. ask-user-question, model-select), append the class anchor to an idle glyph the way the existing `"unrecognized modal at idle"` case does (`append([]byte("Enter to select"), idle...)`), if broader class coverage is wanted.
- **Trust modal stays report-only** (AC3): `trust-folder-snapshot.bin` (real, `IsIdle`=true) **and** the synthetic `append([]byte("Quick safety check"), idle...)` case → nil error, `Readiness{Idle:true, TrustModal:true}` (no error).
- **MCP-failure banner stays report-only** (AC3): synthetic `append([]byte("2 MCP servers failed "), idle...)` → nil error, `Readiness{Idle:true, McpFailure:true, FailedMcpCount:2}`. Confirms the banner (not a modal class) does not trip the gate.
- **Network-failure banner stays report-only** (AC3): synthetic `append([]byte("FailedToOpenSocket "), idle...)` → nil error, `Readiness{Idle:true, NetworkFailure:true}`.
- **Existing error-path tests unchanged** (AC1 invariant): `TestWaitReadyContextCancelled`, `TestWaitReadyProcessExited`, `TestWaitReadyProcessExitedCleanExit` keep passing — the zero-Readiness invariant is preserved.
- **Mid-session untouched** (AC4): no new test needed; assert-by-omission that `IsIdle`/`state.go` are not modified. State-machine callers keep using `IsIdle` directly.

---

## Scope — explicitly NOT touched (AC4)

- `IsIdle`, `IsThinking`, `busyInRegion`, `statusRegionRows` (`state.go`) — untouched; mid-session readiness predicates unchanged, no new false stalls after the first turn.
- `DetectModalClass` and its anchors (`modal.go`) — read-only; the inversion consumes its result, does not change classification.
- Trust / MCP-banner / network detectors (`trust.go`, `mcp_banner.go`, `network.go`) — untouched; their `Readiness` flags keep report-only semantics.

---

## Open questions

1. **Genuinely-novel / unclassifiable chrome — OUT OF SCOPE, deferred to a spike-gated follow-up.** The 2.1.199 motivation includes chrome that renders *no recognized anchor* → `DetectModalClass` returns `ModalClassUnknown`, which is **indistinguishable from clean idle** (the #158 overload). This spine catches recognized-but-unexpected modals (fail-safe); it does **not** catch truly novel chrome — that case still reads as clean idle, unchanged from today. Closing it needs a *positive* signature of the clean idle prompt that novel chrome fails, and the rendered grid has no frame/border primitive (the input box itself has a border, so a naïve "separator present" heuristic is ambiguous). **Design assessment: infeasible in claude-free `make check` without a live capture of the real novel chrome** (a fixture we do not have; closest live symptom is `probe-first-prompt-hang` / #181). Per the ticket's own instruction, this must NOT be faked with a self-descriptive fixture. It should become a separate spike-from-implement pair (child A: capture the real chrome, ship the fixture green; child B, blocked-by-A: apply the novel-chrome discriminator) when warranted. This ticket ships the AC-complete spine; it does not route back, because all five AC are satisfiable by the spine with existing fixtures today.
2. **Consumer migration (cross-repo, coordinated).** Removing `Readiness.UnknownModal` breaks any consumer that reads it. pyrycode `internal/agentrun/ptyrunner` should: (a) drop any `r.UnknownModal` reference; (b) optionally add an `errors.As(err, &tuidriver.UnexpectedModalError{})` branch in `Run` to produce an operator-readable message and a typed `Err*`, mirroring the trust/mcp/network cases. Even absent (b), the fail-safe holds — the run halts on the propagated error rather than typing. Track under the tui-driver→pyrycode consumer-sync path.

---

## Security review

**Verdict:** PASS

This ticket is `security-sensitive`: `WaitReady` is the startup gate that decides whether the consumer types its first prompt into the session, and its input (`DetectModalClass` over the rendered grid) is attacker-influenceable claude output — the same threat surface as the #150–155 rendered-grid classification chain that feeds the permission auto-answer gate.

**Findings:**

- **[Trust boundaries]** The boundary is `WaitReady`'s single classify step: untrusted subprocess PTY output → `DetectModalClass(snap)` → a decision. It is explicit and single-site (one function, `ready.go`). The change *narrows* the trusted-ready set (positive confirmation) — it strictly reduces what crosses as "ready," so it moves the boundary in the safe direction. No new boundary introduced.
- **[Threat model alignment — false-CLEAN (the dangerous direction)]** Worst case: forged content makes `WaitReady` report clean-ready while a real modal is up, so the consumer types into a dialog. For a *recognized* modal this now errors (the whole point). To bypass, an attacker needs a real blocking modal that `DetectModalClass` does **not** recognize (novel chrome). That is a **pre-existing** gap (today it returns `Idle:true` too), explicitly named OUT OF SCOPE in § Open questions #1 — this change does not widen it. No regression; strict improvement for every recognized class.
- **[Threat model alignment — false-ERROR (the safe direction) / DoS]** Worst case: forged transcript content makes `WaitReady` error on an actually-clean screen (a liveness/availability dent). Mitigated by the existing region-scoping the inversion inherits unchanged: the permission anchor is bounded to `permissionRegionRows` (rejects mid-transcript "Do you want to proceed?" forgeries, the #153 work), and full-panel classes match only the rendered grid, which excludes scrolled-off history. Any content that could forge a class must occupy the visible status region — at which point it is indistinguishable from a real modal, and **erroring is the fail-safe direction** (the consumer halts; it never types into anything). No availability primitive (retry/backoff) is weakened.
- **[Error messages, logs, telemetry]** `UnexpectedModalError.Error()` embeds only `e.Class`, a value from the fixed `ModalClass` const set (`"permission"`, `"mcp"`, …) — never attacker free-text, no snapshot bytes, no PTY content. No log-injection / format surprise, no leakage of session content into the error string. `Readiness{}` (zero) is returned on the error path, so no partial trusted state leaks alongside the error.
- **[Concurrency]** No goroutine, lock, or shared-state change; `WaitReady` remains synchronous over an immutable snapshot. Not applicable beyond stating it.
- **[Tokens/secrets, file ops, subprocess exec, crypto, network I/O]** Not applicable — this change adds no file/network/subprocess/crypto/token handling. It is a pure in-memory classification-and-return over an existing snapshot.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-07-07

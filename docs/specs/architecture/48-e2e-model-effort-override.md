# 48 — e2e harness: model/effort override env-var seam

## Files to read first

- `pkg/tuidriver/pty.go:1-90` — full `EnsureClaudeEnv` + the `StrictMcpConfigEnv` precedent (lines 24-35 are the design template you mirror).
- `pkg/tuidriver/pty_test.go:66-117` — the four `TestEnsureClaudeEnvStrictMcp*` cases. Your new tests mirror these one-for-one per env var.
- `cmd/e2e-runner/main.go:495-498` — the one runner line that forces `TUIDRIVER_STRICT_MCP_CONFIG=1` onto each child. Read it to confirm what you must NOT change (see § Design).
- `cmd/probe-first-prompt-hang/main.go:144-148` + any one spike (`cmd/spike-one-turn/main.go:103-106` is the simplest) — confirm the `exec.Command("claude", ...) → EnsureClaudeEnv(cmd)` shape every spike+probe shares. This is the seam your change rides; no spike-side code changes.
- `Makefile` (whole file — 34 lines) — the surface where `MODEL=` / `EFFORT=` get introduced.
- `claude-version.lock` (whole file — 10 lines) — format reminder for the two `flag=` additions.
- `docs/knowledge/features/e2e-harness.md` § "Headless / CI plumbing" + § "CI integration" — the existing analog you extend for model/effort docs.
- `README.md:13-20` (Status section) — the only README touchpoint; add the cost-differential paragraph immediately after it.
- Ticket issue body — the cost reasoning (~18-90× cheaper) lives there verbatim; the README doc must echo the same numbers.

## Context

The 7 spike binaries and `probe-first-prompt-hang` all `exec.Command("claude", ...)` and then call `tuidriver.EnsureClaudeEnv(cmd)` before PTY-starting. Today they inherit the operator's interactive Claude config — currently Opus 4.7 + high effort. CI (`.github/workflows/e2e.yml`, push-to-main) hits the metered API key for the same defaults, putting the harness on a $3-$15-per-run cost path. Pinning `--model haiku --effort low` for CI cuts that 18-90×; preserving the unset/Max default for local dev keeps subscription billing intact (the strategic reason this library exists — see `README.md:141-145`).

The repo already ships the exact seam this ticket extends: `TUIDRIVER_STRICT_MCP_CONFIG=1` in the child's env causes `EnsureClaudeEnv` to append `--strict-mcp-config` to `cmd.Args` without touching any spike code. Adding two more env vars on the same seam — `TUIDRIVER_CLAUDE_MODEL` and `TUIDRIVER_CLAUDE_EFFORT` — is purely additive and gives the harness Makefile a single place to inject CI overrides.

## Design

### Seam choice — env vars through `EnsureClaudeEnv`

Extend `EnsureClaudeEnv` with two new env-var checks that mirror the existing strict-mcp block. When `TUIDRIVER_CLAUDE_MODEL` is set to a non-empty value, append `--model <value>` to `cmd.Args` (skip if `--model` is already present). When `TUIDRIVER_CLAUDE_EFFORT` is set to a non-empty value, append `--effort <value>` (skip if `--effort` is already present). Empty / unset for either env var is a no-op for that env var's argv pair (preserves today's "inherit operator's config" behaviour — required for Max-subscription local dev).

Why env-var seam, not per-spike flags:
- The existing strict-mcp precedent set this contract; consistency matters more than design novelty for a passthrough mechanism.
- Per-spike flag plumbing would touch 8 binaries' `main.go` files + the runner's `commonArgs` + each binary's `flag.Parse` — that's the ticket's stated upper-bound rationale for splitting (~80-100 LOC across ~10 files). The env-var seam is ~30 LOC across 1 production .go file.
- Per-spike flags also defeat the "library's `EnsureClaudeEnv` is the one place claude-side argv mutations happen" invariant. Future overrides ride the same seam.

### Constants and contract (`pkg/tuidriver/pty.go`)

Two new exported `const string` values added next to `StrictMcpConfigEnv`:

```go
// ClaudeModelEnv is the env var the e2e harness sets to ask
// EnsureClaudeEnv to additionally append --model <value> to cmd.Args.
// Non-empty: appended verbatim. Empty / unset: no-op (preserves the
// operator's interactive Claude default — required for Max-subscription
// local development). Idempotent: if --model is already in cmd.Args,
// no second occurrence is added.
const ClaudeModelEnv = "TUIDRIVER_CLAUDE_MODEL"

// ClaudeEffortEnv — same contract for --effort <level>. Accepts whatever
// string claude accepts (today: low/medium/high/xhigh/max — see
// claude --help). Validation is delegated to claude itself.
const ClaudeEffortEnv = "TUIDRIVER_CLAUDE_EFFORT"
```

Two new unexported flag-name constants alongside `strictMcpConfigFlag`:

```go
const claudeModelFlag  = "--model"
const claudeEffortFlag = "--effort"
```

### `EnsureClaudeEnv` extension

Append two new blocks AFTER the existing strict-mcp block (so argv ordering becomes: existing-args, then `--strict-mcp-config`, then `--model X`, then `--effort Y`; only the conditional ones are present). Each block:

1. Read the env var via `os.Getenv`.
2. If empty, no-op.
3. If non-empty, scan `cmd.Args` for the flag name (`--model` / `--effort`); skip if already present.
4. If not present, append two args in a single `cmd.Args = append(cmd.Args, flagName, value)` call.

The shape mirrors the strict-mcp block 1:1 except (a) the value is a non-empty string check rather than `== "1"`, and (b) the append is two args (flag + value) rather than one. No new helper; copy-and-adapt is clearer than abstracting at N=3.

Behaviour examples:

- `TUIDRIVER_CLAUDE_MODEL` unset, `TUIDRIVER_CLAUDE_EFFORT` unset, `TUIDRIVER_STRICT_MCP_CONFIG=1` → existing behaviour, `--strict-mcp-config` appended only.
- `TUIDRIVER_CLAUDE_MODEL=haiku` → `--model haiku` appended.
- `TUIDRIVER_CLAUDE_MODEL=haiku TUIDRIVER_CLAUDE_EFFORT=low` → both `--model haiku` and `--effort low` appended.
- `TUIDRIVER_CLAUDE_MODEL=""` (set but empty) → no-op for model (same as unset).
- Caller already passed `--model X` in args → respected verbatim; env var no-op (idempotence; caller intent wins).

No validation of the value beyond non-empty. Claude itself rejects unknown models / effort levels; the spike's existing failure handling surfaces that.

### Runner (`cmd/e2e-runner/main.go`) — NO changes

The runner currently sets `cmd.Env = append(os.Environ(), "TUIDRIVER_STRICT_MCP_CONFIG=1")` on each child. Because `os.Environ()` already carries whatever the parent (Make) exported, any `TUIDRIVER_CLAUDE_MODEL`/`TUIDRIVER_CLAUDE_EFFORT` set on the `make e2e` invocation flows transparently: shell → Make (via inline env-on-recipe) → runner → spike child. Verify this by reading the line — it's an `os.Environ()`-based seed, so additional env vars hitch a ride for free.

This is deliberate. Adding explicit pass-through to the runner would:
- Require a new flag or env-read on the runner — surface area for no behavioural gain.
- Risk drift between "what Make sets" and "what the runner injects."

Keep the runner unchanged. Document the chain in the harness doc instead.

### `Makefile`

Two new variables and two recipe edits:

```make
MODEL  ?=
EFFORT ?=

e2e: build-bin
	$(if $(MODEL),TUIDRIVER_CLAUDE_MODEL=$(MODEL)) $(if $(EFFORT),TUIDRIVER_CLAUDE_EFFORT=$(EFFORT)) $(BIN_DIR)/$(RUNNER) -bin-dir $(BIN_DIR) -report $(REPORT)
```

Why `$(if …)` inline rather than `export`:
- `export` in a Makefile leaks the var to ALL recipes (including `build-bin`'s `go build`). Inline scoping is per-recipe and audit-friendly.
- Empty values must be a true no-op. `TUIDRIVER_CLAUDE_MODEL= ./bin/...` would set an empty env var (which `EnsureClaudeEnv` treats as no-op, so functionally fine, but cleaner to omit entirely). The `$(if $(MODEL),…)` form omits the assignment when `MODEL` is empty.
- Single-line recipe keeps the existing two-line file layout (just adds the two `?=` declarations + edits the `e2e:` recipe).

Invocations:
- `make e2e` — no overrides (today's behaviour preserved).
- `make e2e MODEL=haiku` — `--model haiku` only.
- `make e2e MODEL=haiku EFFORT=low` — both flags (the CI default).
- `make e2e EFFORT=max` — `--effort max` only.

### `claude-version.lock` — pin the two new flags

Add two `flag=` lines so the `claude-version-lock` check (already running first in the harness) verifies these flags remain available whenever the harness boots:

```
flag=--model
flag=--effort
```

This matches the existing pattern (`flag=--session-id`, `flag=--permission-mode`). It does NOT pin specific values (e.g. `value=haiku`) — `--effort`'s `low/medium/high/xhigh/max` set might be worth pinning, but defer that to a follow-up if drift bites. For this ticket, pin only the flag names. (The lock check runs unconditionally, regardless of whether MODEL/EFFORT are actually set on this run — the assertion is "we depend on these flags existing," not "we used them this run.")

### Docs

`docs/knowledge/features/e2e-harness.md`:

- Extend the existing "Headless / CI plumbing" section: after the strict-mcp paragraph, add a parallel paragraph documenting the `TUIDRIVER_CLAUDE_MODEL` / `TUIDRIVER_CLAUDE_EFFORT` env vars, the Makefile `MODEL=` / `EFFORT=` invocations, and the "unset = inherit operator's config (preserves Max-subscription path)" semantics.
- Extend the "CI integration" section's cost-controls bullet list with a third bullet pointing at the recommended `MODEL=haiku EFFORT=low` defaults and citing the 18-90× cost differential.
- Update the "How to run" code block to include `make e2e MODEL=haiku EFFORT=low` as a third invocation example.
- Add the two new env vars + Makefile variables to the "Files" section's `pkg/tuidriver/pty.go` and `Makefile` entries (one-line each).

`README.md`:

- Add a short ~10-line section ("## Running the e2e harness" or absorb into Status) listing the two invocations (`make e2e` for local Max-subscription dev; `make e2e MODEL=haiku EFFORT=low` for CI / metered API usage) and the cost differential (echo the issue body's numbers: Opus high ≈ $0.50-$2 per spike × 6 spikes + probe ≈ $3-$15 per CI run; Haiku low ≈ $0.20-$0.70 per run → 18-90× cheaper). Point at `docs/knowledge/features/e2e-harness.md` for the full doc.

## Testing strategy

Mirror the four existing `TestEnsureClaudeEnvStrictMcp*` cases per new env var. The strict-mcp tests are at `pkg/tuidriver/pty_test.go:66-117` — read them first; your tests are structural copies.

Scenarios (use `t.Setenv` exactly as the existing tests do):

**`TUIDRIVER_CLAUDE_MODEL` cases:**

- *Unset / empty leaves args.* `t.Setenv(ClaudeModelEnv, "")`. Expect `cmd.Args` unchanged — no `--model` token.
- *Set appends.* `t.Setenv(ClaudeModelEnv, "haiku")`. Expect exactly one `--model` followed by exactly one `haiku` in `cmd.Args`.
- *Already present, no double-append.* Pre-seed `cmd.Args` with `--model sonnet`, `t.Setenv(ClaudeModelEnv, "haiku")`. Expect exactly one `--model` token; expect the value to be `sonnet` (caller wins).
- *Strict-mcp + model both fire.* `t.Setenv(StrictMcpConfigEnv, "1")` AND `t.Setenv(ClaudeModelEnv, "haiku")`. Expect both `--strict-mcp-config` and `--model haiku` appended (no interaction; independent env-var blocks).

**`TUIDRIVER_CLAUDE_EFFORT` cases:**

- Same four scenarios as model, swapping the env var and flag.

**Cross-pair case:**

- *All three env vars together.* Strict-mcp + model + effort all set. Expect all three appendages present, none duplicated. (Belt-and-suspenders for the "blocks are independent" invariant.)

Total: 9 new test functions (4 + 4 + 1). Each is ~10-12 lines following the existing pattern. No new test helpers needed; reuse `contains(haystack, needle)` already defined at `pkg/tuidriver/pty_test.go:118`.

Existing tests stay unchanged — the strict-mcp tests already use `t.Setenv` which is scoped per-test, so adding new env-var reads in the function body doesn't affect them.

**No spike-side tests.** The override is a mechanical passthrough via the existing seam; the e2e harness is the integration test for the whole stack. The ticket explicitly acknowledges this (AC bullet 3). The first CI run on `MODEL=haiku EFFORT=low` will exercise the full chain end-to-end.

## Error handling

Three failure modes worth naming:

- **Invalid model name** (e.g. `MODEL=notamodel`). Claude itself emits an error on stderr and exits; the spike's existing watchdog or PTY-quiet timeout surfaces as a failed check. The harness reports `status="fail"` with the spike's stderr visible in mirrored output. No new error handling.
- **Invalid effort level** (e.g. `EFFORT=ludicrous`). Same as model — claude rejects, spike fails, harness reports.
- **`claude --help` no longer documents `--model` or `--effort`** (future drift). The `claude-version-lock` check (already first in the harness) will emit `claude --help no longer mentions flag --model; review claude-version.lock` on stderr, fail, and short-circuit the rest of the run. This is the lock file's job; nothing to add in this ticket beyond the two `flag=` lines.

No new error paths in library code. The seam is "append two args if env var set"; failure is the same surface as today's strict-mcp path.

## Open questions

None blocking. Two minor judgement calls noted, resolved here:

- **Env var naming** — `TUIDRIVER_CLAUDE_MODEL` vs `TUIDRIVER_MODEL`. Picked the longer form because the library's domain extends past claude (in principle — the project's CLAUDE.md says "tui-driver should work against any TUI-style CLI"). `CLAUDE_MODEL` namespaces the var to "claude-CLI passthrough" cleanly. Strict-mcp doesn't need the prefix because `STRICT_MCP_CONFIG` is itself an unambiguous claude flag name.
- **`value=low`, `value=high` etc. in `claude-version.lock`** — declined for this ticket. The value set is plausibly stable but not load-bearing; adding values would commit us to lock-file edits whenever Anthropic adds a new effort level. Pin flag names only; revisit if drift bites.

## Non-goals

- Per-spike model override (e.g. `spike-cancel` always Haiku regardless of harness default) — out of scope per ticket body. Speculative; not needed yet.
- Validating Haiku-low parity with Opus-high across all spike code paths — out of scope per ticket body. Defer to first CI run.
- CI workflow changes (setting `MODEL=haiku EFFORT=low` in `.github/workflows/e2e.yml`) — out of scope; that's #32 territory or a follow-up ticket. This ticket lands the mechanism; the workflow consumes it separately.

## Files touched

| File | Change | Approx LOC |
|---|---|---|
| `pkg/tuidriver/pty.go` | +2 exported const, +2 unexported const, +2 conditional append blocks in `EnsureClaudeEnv`, +doc comment on each | ~35 |
| `pkg/tuidriver/pty_test.go` | +9 test functions (4 model, 4 effort, 1 combined) | ~110 |
| `Makefile` | +2 `?=` declarations, edit `e2e:` recipe to inline-prefix two env vars conditionally | ~3 |
| `claude-version.lock` | +2 `flag=` lines (`--model`, `--effort`) | +2 |
| `docs/knowledge/features/e2e-harness.md` | Extend "Headless / CI plumbing", "CI integration", "How to run", "Files" sections | ~30 |
| `README.md` | Add ~10-line cost-control / harness-invocation section | ~10 |

Total: 1 production .go file modified, 1 test .go file modified, 4 non-.go files modified, 0 new files. Production-source count well under the 5-file split threshold.

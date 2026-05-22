# Spec: e2e check `snapshot-drift` — normalise the volatile `Try` line, then re-record

Ticket: [#72](https://github.com/pyrycode/tui-driver/issues/72). Branch: `feature/72`.

**This spec supersedes the prior re-record-only version of `72-snapshot-drift-rerecord.md`.** That version's premise (re-record + bump lock = green) was disproved on HEAD `c265a2f` by the developer's multi-capture experiment ([routing-back comment](https://github.com/pyrycode/tui-driver/issues/72#issuecomment-4521397209)): claude `2.1.148`'s picker emits a per-invocation random `❯ Try "<starter prompt>"` line, so back-to-back captures under identical env diff on every fixture. Re-record alone cannot satisfy AC #2 (back-to-back determinism). The fix is a surgical normaliser applied symmetrically to both sides of the byte-compare, **plus** the re-record + lockfile bump.

Read the issue body first; this spec is the *how* and the safety nets, not the *what*.

## Files to read first

The work is concentrated in three Go files plus a lockfile and three binary fixtures. Read targets are small.

- **Issue body of #72** — authoritative call-chain reconstruction, root-cause assignment, and the four-option redesign list with rationale for choosing Option 1.
- **[Developer routing-back comment](https://github.com/pyrycode/tui-driver/issues/72#issuecomment-4521397209)** — table of observed `Try` line rotations across all three fixtures, plus the buffer-eviction explanation for why this was hidden pre-#34. The evidence the normaliser is targeted at; do not re-derive.
- `cmd/e2e-snapshot-check/main.go:38-115` — full file is 129 lines; `fixture` table at 50-54, capture path 71-97, the `bytes.Equal` at 110. Lines 99-113 are where the normaliser is inserted.
- `cmd/spike-multiselect/main.go:90-100, 226-235` — `exec.Command("claude")` + `EnsureClaudeEnv`, and the `/tmp/spike-multiselect-bytes-<ns>.bin` dump emission with the `picker-snapshot path=… raw_len=…` log line that `e2e-snapshot-check` scrapes via `dumpPathRe`. This is the capture path; the developer doesn't modify it, but reads it to understand what's in the raw bytes the normaliser will see.
- `pkg/tuidriver/pty.go:62-117` — `EnsureClaudeEnv`. The env-var → flag mapping (`TUIDRIVER_STRICT_MCP_CONFIG=1` → `--strict-mcp-config`). What the re-record env must reproduce.
- `cmd/e2e-runner/main.go:281-288, 518` — confirms the runner sets `TUIDRIVER_STRICT_MCP_CONFIG=1` on every child env. The re-record must match.
- `docs/specs/architecture/35-snapshot-drift.md:258-273` — Open Question 1's resolution block. **Re-record commands live here verbatim**; do not duplicate inline. This spec cites them.
- `pkg/tuidriver/testdata/{picker,mcp,agents}-snapshot.bin` — the committed fixtures. Sizes today: picker 4096, mcp 4096, agents 2236. The picker / mcp fill the rolling buffer exactly (`DefaultBufferCap=4096`), which is why pre-#34 captures evicted past the `Try` line as a side-effect of larger emission volume.
- `claude-version.lock` — 6 effective lines. Only `version=` changes here.
- `Makefile` (top ~40 lines) — for the optional `rerecord-snapshots` recipe.

You do **not** need to read `pkg/tuidriver/{parsers,modal,strip}.go`, `pkg/tuidriver/buffer.go` beyond the `DefaultBufferCap` constant, or any spike binary other than `spike-multiselect`. The check is renderer-driven; nothing in this ticket touches the parsers.

## Context

The previous round's spec assumed three intentional upstream changes since `9fae70d` (#21, 2026-04-25) — `--strict-mcp-config` plumbing (#34), `MODEL`/`EFFORT` env seam (#48), claude `2.1.144 → 2.1.148` — explained the drift fully, and that re-recording under the runner's env would re-baseline the fixtures. The developer's multi-capture experiment showed a fourth, more important factor:

- Claude `2.1.148`'s picker emits a `❯ Try "<starter prompt>"` line whose text rotates **per invocation** (not from `~/.claude/history.jsonl` — baked into the binary).
- Pre-#34, claude emitted enough bytes that the 4096-byte rolling ring buffer (`pkg/tuidriver/buffer.go:13`) evicted older bytes including the `Try` line before `rb.Snapshot()` captured the visible window. Determinism was an accidental side-effect of emission volume.
- Post-#34 (`--strict-mcp-config` set), claude emits fewer bytes, the buffer no longer fills, and `rb.Snapshot()` retains the volatile `Try` line. Same parser, same rendering library, but the bytes that survive eviction now contain run-to-run randomness.

The renderer is the producer of the randomness; the snapshot-check is the byte-level consumer. Spec 35's design choice — compare raw bytes from `rb.Snapshot()` to a committed fixture via `bytes.Equal` — is sound, but it requires the producer to be deterministic. It no longer is. The cheapest fix that preserves spec 35's byte-compare model is to mask the one known volatile region on both sides of the compare. That's this ticket.

Three other changes in `git log 9fae70d..HEAD` still need re-record absorption (the strict-mcp emission shape, possibly the model/effort header text — though the host runs bare `make e2e`, see § Step 1 — and any other 2.1.144→2.1.148 renderer differences). Those would have been resolved by re-record alone; the `Try` line is what blocked the prior round.

## Design

Two pieces:

1. **A pure normaliser function** in shared code, applied symmetrically to the live capture AND the committed fixture before `bytes.Equal`.
2. **Re-record the three fixtures** under the runner's env, after the normaliser is in place and after the post-normalisation byte-diff inspection confirms the residual diff looks like renderer drift (not a parser regression, and not additional volatility).

The normaliser is the load-bearing piece. Re-record + lockfile bump are mechanical follow-ups.

### Normaliser — placement, signature, contract

**Placement.** New file `cmd/e2e-snapshot-check/normalize.go`, alongside `main.go`. Keeping it local to the check binary is correct: the check is the only consumer today, and a `pkg/tuidriver/internal/...` location would imply the library itself does normalisation (it does not — the library exposes `rb.Snapshot()` as raw bytes, and that contract stays unchanged). If a second consumer ever needs the same normaliser, promote it then.

**Signature.**

```go
// normalize masks volatile content out of a raw PTY-byte capture so that
// back-to-back captures of the same TUI state under identical env compare
// equal. The function is pure; the same input always produces the same
// output. Apply to both the live capture and the committed fixture before
// bytes.Equal — asymmetric normalisation defeats the byte-compare model.
//
// Currently masks: the `Try "<starter prompt>"` line that claude 2.1.148+
// emits in the picker with a per-invocation random prompt text.
func normalize(b []byte) []byte
```

**Contract.**

- Pure function. No I/O. No env reads. No mutation of the input (return a new slice, or only mutate a copy).
- Deterministic. `normalize(x)` always returns the same bytes for the same `x`.
- Idempotent. `normalize(normalize(x)) == normalize(x)`.
- Targeted. Masks **only** the `Try "<prompt>"` line. Do NOT extend it to a general "strip everything that looks volatile" function. If the developer observes additional volatile content during the post-normalisation inspection (AC #4), the rule is § Step 3's STOP-and-route-back — not silently broaden the normaliser.
- Replacement is a constant marker, e.g. `Try "<MASKED>"` (literal ASCII bytes). The exact marker is the developer's call; it just needs to be a constant string and not contain `"` itself.

**How to derive the regex.** The committed fixtures and a fresh `spike-multiselect` capture both contain the Try line in raw, ANSI-encoded form. Claude renders text positionally — words are separated by `\x1b[1C` (cursor-right-1) escape sequences rather than spaces, color codes (`\x1b[38;5;<n>m`) wrap individual segments. The Try line's structure in raw bytes is approximately:

```
<optional color codes> Try <space or \x1b[1C> " <prompt text, may contain \x1b[1C> "
```

The closing `"` is the safe terminator because the prompt text itself contains no `"` character (claude wraps the prompt in literal quotes; quotes inside the prompt would be escaped or absent). A working starting regex:

```go
// Match the literal substring `Try` followed by any bytes up through the
// closing quote of the rotating prompt. The first non-quote run absorbs
// any color codes / cursor-rights / spaces between `Try` and `"`; the
// second absorbs the prompt body. Both runs are bounded to avoid runaway
// matching across unrelated content.
var tryLineRe = regexp.MustCompile(`Try[^"]{0,32}"[^"]{0,256}"`)
```

The developer **must verify** this regex against actual captured bytes before committing: capture one fixture, dump the regex's matched substring with a small test harness or `go run` snippet, and confirm it covers exactly the Try line and nothing else. If the regex matches anything besides the Try line in any of the three fixtures' raw bytes, tighten it (anchor to `❯` if present, narrow the upper bound, etc.). If it cannot be made specific without false-positive risk, document the trade-off in the PR description.

**Why a regex on raw bytes, not strip-ANSI-then-compare.** Spec 35's byte-compare model is on raw bytes (this is what `rb.Snapshot()` produces; this is what the fixture files are). Switching the compare to "strip ANSI then `bytes.Equal`" would broaden the change far beyond what AC #1 allows (it would also mask color-code drift, layout drift, etc.) and is a different design choice — Option 3 territory. The surgical regex preserves spec 35's model: bytes in, bytes out, only one named volatile region masked.

### Apply at the comparison site

Modify `cmd/e2e-snapshot-check/main.go` lines 99-113 to apply `normalize` to both `captured` and `committed` before the `bytes.Equal`. The change is two function calls, no restructuring of `runFixture`. Diff sketch (not implementation):

```
captured  := os.ReadFile(dumpPath)
committed := os.ReadFile(fixturePath)
if !bytes.Equal(normalize(captured), normalize(committed)) {
    ...
}
```

The fixtures stay as raw bytes on disk. The normaliser runs in-process, on both sides, at compare time. That's what AC #1 calls "symmetric normalisation".

### Re-record after the normaliser is in place

The fixtures committed on `9fae70d` were captured pre-#34 with a Try-line-evicted buffer. New captures will start at the screen-refresh preamble and include the Try line. Re-record under the runner's env so the rest of the bytes (header, model line, `/mcp` server lines absent under strict-mcp, etc.) match what the runner will produce on subsequent runs. The normaliser handles the Try line; everything else must already match.

### Lockfile bump

`claude-version.lock:11` — change `version=2.1.144` to whatever `claude --version` reports on the dispatcher host (today: `2.1.148`). Do **not** touch `flag=` or `value=` lines — those are independent assertions against `claude --help`, enforced by `claude-version-lock`. They only change if `claude --help` actually changed shape, which is a separate ticket.

## Workflow (sequenced steps)

The order matters. The normaliser must be in place before re-record, because re-record without the normaliser reproduces the developer's observed non-determinism (and you'd be unable to tell whether the re-recorded bytes actually correspond to the picker state or are just one random draw).

### Step 1 — Establish the re-record env

`tui-driver-agents/qa/CLAUDE.md` is the authoritative answer to the prior round's Open Question 1: the dispatcher host runs bare `make e2e` (no `MODEL=…`/`EFFORT=…`). The required re-record env is therefore:

| Runner-side                     | Required in re-record env? |
|---------------------------------|----------------------------|
| `TUIDRIVER_STRICT_MCP_CONFIG=1` | Always.                    |
| `TUIDRIVER_CLAUDE_MODEL=<v>`    | No (host does not set).    |
| `TUIDRIVER_CLAUDE_EFFORT=<v>`   | No (host does not set).    |

If you discover the dispatcher's invocation has changed since this spec was written and now sets `MODEL=…`/`EFFORT=…`, STOP and re-scope via PO — that's a load-bearing premise shift and the fixtures must be captured under whatever env the host actually uses.

`EnsureClaudeEnv` reads these via `os.Getenv`, so `export TUIDRIVER_STRICT_MCP_CONFIG=1` before invoking `spike-multiselect` is sufficient. No flag-passing needed.

### Step 2 — Implement the normaliser

1. Create `cmd/e2e-snapshot-check/normalize.go` with the `normalize(b []byte) []byte` function per § Design.
2. Wire it into `runFixture` in `main.go` at the comparison site (two call additions, no other restructuring).
3. Write a unit test `cmd/e2e-snapshot-check/normalize_test.go` covering at minimum:
   - **Different `Try` prompts normalise to equal bytes.** Two synthetic byte strings with different prompt texts inside `Try "..."` produce `bytes.Equal == true` after normalisation. This is the load-bearing assertion that satisfies AC #2.
   - **Identical bytes stay identical.** `normalize(x) == normalize(x)` for an input with no Try line (purity check).
   - **Idempotence.** `normalize(normalize(x)) == normalize(x)`.
   - **Untouched content outside the Try line.** A synthetic input containing both a Try line and other bytes (e.g. a fake box-drawing prefix) — normalisation changes the Try region and leaves the rest byte-identical. This guards against the regex accidentally over-matching.
   The test runs without any live `claude` invocation — pure unit test, fast, deterministic.

Build and run the unit test before moving to step 3. If it fails, fix the regex; do not advance with a broken normaliser.

### Step 3 — Capture ONE fixture, byte-diff against the (still-committed) old one

Do **not** re-record all three at once. Capture exactly one (`picker` is the simplest — short settle, simple trigger) using the procedure in spec 35 OQ1 (`docs/specs/architecture/35-snapshot-drift.md:262-267`). Then:

- Run `xxd <new-dump-path> > /tmp/new.xxd` and `xxd pkg/tuidriver/testdata/picker-snapshot.bin > /tmp/old.xxd`.
- `diff /tmp/old.xxd /tmp/new.xxd | head -100` (the full diff is large; first ~100 lines tell the story).
- **Also** run a normalised diff: a tiny `go run` snippet that reads both files, calls `normalize` on each, and prints the byte-by-byte diff of the normalised outputs (or just `bytes.Equal` + a sample-region xxd dump). This is the AC #4 inspection — what's the **residual** drift after the normaliser has done its job?

Classify the residual diff:

- **Renderer drift (good — proceed):**
  - ANSI escape-sequence shifts (`\x1b[38;5;…m` color codes, cursor moves).
  - String literal swaps matching known causes: model-name strings (`opus` ↔ `sonnet` ↔ `haiku`), absence of MCP server names (strict-mcp suppression), version strings in the header.
  - Box-drawing reshuffles (`╭╮╰╯│─`) within an otherwise-identical structure.
- **Parser regression (red flag — STOP and route back to PO):**
  - The committed bytes appear *inside* the new dump, but extra junk is prepended/appended that shouldn't be there (suggests `rb.Snapshot()` is returning more than the picker frame).
  - The new dump is structurally different — missing the picker frame entirely, or showing an error message in claude's output, or showing the trust-folder modal.
  - Length wildly off (committed ~1–10 KB; new dump < 500 B or > 100 KB).
- **Additional volatile content (red flag — STOP and route back to PO per AC #5):**
  - The normalised diff still differs between two back-to-back captures of the **same** fixture under the **same** env. That means something besides the Try line is rotating per-invocation: timestamps, session IDs, random tips, etc.
  - Concretely: do **two** captures of `picker` 5 seconds apart, normalise each, and run `bytes.Equal` between the two normalised live captures. If false, this branch fires.

**If parser regression OR additional volatility:** STOP. Add a comment to #72 with the xxd diff excerpt (or the failed equality demonstration) and route via `needs-rework:po` with framing:

- Parser regression: *"Drift on `picker` is not consistent with renderer drift after normalisation — see diff excerpt below. A parser fix becomes its own ticket; this ticket becomes 're-record under unchanged parser' once that lands."*
- Additional volatility: *"Multi-capture verification surfaces volatile content beyond the `Try` line — see evidence below. Per AC #5, not broadening the normaliser unilaterally; PO needs to decide whether to broaden normalisation or escalate to Option 3 (parsed-shape comparison)."*

Do not commit anything in this branch. Your worktree should be untouched at the end of the route-back.

**If renderer drift only:** capture the diff signature in 1–2 lines for the PR description (e.g. *"Header line shifted from `Model: claude-3-7-sonnet-20250219` to `Model: claude-opus-4-7`; MCP server lines absent (strict-mcp suppression); colour codes unchanged."*), then proceed to step 4.

### Step 4 — Re-record all three fixtures

Run the three captures from spec 35 OQ1's recipe (`docs/specs/architecture/35-snapshot-drift.md:262-267`) with the re-record env exported. Copy each dump into place:

- `pkg/tuidriver/testdata/picker-snapshot.bin`
- `pkg/tuidriver/testdata/mcp-snapshot.bin`
- `pkg/tuidriver/testdata/agents-snapshot.bin`

Do not edit the bytes. Do not strip or pre-normalise on disk. The fixtures are raw `rb.Snapshot()` output; the normaliser runs in-process at compare time.

### Step 5 — Bump `claude-version.lock`

Read `claude --version` on the host (actual command, not memory). Strip the leading version token (e.g. `2.1.148 (Claude Code)` → `2.1.148`) and replace `claude-version.lock:11`'s `version=2.1.144` with `version=<observed-version>`.

Do not touch `flag=` / `value=` lines.

### Step 6 — Back-to-back determinism check (AC #2)

Run `./bin/e2e-snapshot-check` twice in succession (no other env changes between runs). Both runs must report `SNAPSHOT picker match`, `SNAPSHOT mcp match`, `SNAPSHOT agents match`. **A single passing run is not sufficient evidence** — the prior round's failure mode was precisely "one capture matches, the next one doesn't". If either run fails, the normaliser is incomplete; do **not** proceed to step 7. Either tighten the regex (if the failure looks like Try-line variants the regex missed) or STOP per AC #5 (if the failure looks like additional volatile content).

### Step 7 — Full `make e2e`

Run `make e2e` end-to-end. Required outputs:

- `e2e-runner: snapshot-drift -> pass`.
- In `e2e-report.json`, the `snapshot-drift` entry has `snapshots: [...]` with all three `result: "match"`.
- `e2e-runner: claude-version-lock -> pass` (regression check for AC #6).
- No other check status flips from pass to fail (regression check for AC #7).

If any other check fails, that's out-of-scope — file a follow-up; do **not** absorb.

### Step 8 (optional) — `make rerecord-snapshots` Makefile recipe

Architect's call: **add it**. The cost is ~10–20 lines of Makefile; the value is that the next renderer-drift cycle (claude `2.1.149+`) reuses the recipe verbatim instead of re-deriving the env wiring from spec 35 plus this spec. Constraint: do **not** generalise. No `FIXTURE=picker rerecord-one` parametrisation, no `-record` mode on `e2e-snapshot-check`, no shell script extracted to `scripts/`. One recipe in the Makefile, three captures, copy three files, done. If the recipe ends up >30 lines or grows conditional branches, cut it — leave spec 35 OQ1 as the source of truth and ship without the recipe.

The recipe must NOT bypass step 3's byte-diff sanity check. The recipe is for the *steady-state* re-record after a maintainer has already inspected one fixture; do not promote it into the developer's first invocation on this ticket.

If skipped, no further justification needed; the AC list does not require the recipe.

## Concurrency model

N/A. This is a pure function call inside an existing single-goroutine compare loop, plus a sequential maintainer workflow. No goroutines added; no new channels; no shared state introduced.

## Error handling

The check binary's existing error handling (capture-failure → `diff` verdict + stderr "drift in <path>: <err>") is untouched. The normaliser itself has no failure modes — it's a pure regex substitution. If the regex doesn't match the input, the input passes through unchanged (which is the correct behaviour: bytes without a `Try` line are already deterministic).

External error modes the developer must handle:

- **`claude --version` fails on the host:** cannot proceed. Surface as hard error in the PR; do not guess a version.
- **`spike-multiselect` capture times out or returns no `picker-snapshot path=…` line:** upstream spike failure, not a snapshot-drift problem. Out of scope; file follow-up.
- **Byte-diff in step 3 is ambiguous** (neither clearly renderer drift nor clearly a parser regression / additional volatility): default to safety — route back to PO. The cost of one rework cycle is far less than silently re-recording over a parser bug or shipping a normaliser that hides real volatility.

## Testing strategy

- **Unit test (Go).** `cmd/e2e-snapshot-check/normalize_test.go` — purity, idempotence, masks-Try-prompt-differences, leaves-non-Try-bytes-alone. Pure unit test, fast, no `claude` invocation.
- **Integration test (e2e).** The `make e2e` run itself, run **twice back-to-back** per AC #2 / step 6. Both runs must pass.
- **No new test files outside `cmd/e2e-snapshot-check/`.** Spec 35's existing unit tests cover wire format; they don't cover fixture content and shouldn't.
- **No `t.Skip`.** The ticket's first constraint is explicit. If a capture is flaky in a way the normaliser can't address, that's evidence of additional volatility → STOP per AC #5.

## PR description requirements (AC #7)

The PR description must include:

1. **Contributors absorbed.** Output of `git log 9fae70d..HEAD -- cmd/e2e-runner/main.go pkg/tuidriver/pty.go cmd/spike-multiselect/main.go cmd/e2e-snapshot-check/main.go` (run this; do not rely on a memorised enumeration). Expected as of 2026-05-22: `e9d338a` (#34, strict-mcp), `c91259b` (#48, MODEL/EFFORT seam), plus the implicit `claude 2.1.144 → 2.1.148` host upgrade. Anything else `git log` surfaces — include it.
2. **Re-record env.** Verbatim: which `TUIDRIVER_*` vars were exported during capture, and on which `claude --version`. So the next maintainer can reproduce bit-for-bit.
3. **Normaliser pattern (AC #7c).** The exact regex (or substring rule) the normaliser uses to mask the Try line. So the next maintainer reading a future drift can tell what's normalised away vs what's still byte-significant. Include the matched-substring shape from the verification snippet in step 2.
4. **Byte-diff signature characterisation (AC #4).** The 1–2-line description from step 3 of what the residual diff looked like.

## Open questions

1. **Exact form of the `Try` line in raw bytes.** The starting regex `Try[^"]{0,32}"[^"]{0,256}"` is the architect's best guess from the developer's stripped-form evidence. The developer must verify against actual captured bytes (claude `2.1.148` on the dispatcher host) before committing — false-positives or missed matches mean the regex needs tuning. If the developer finds the Try line begins with `❯` consistently in raw bytes, anchoring to `❯` (`\xe2\x9d\xaf` in UTF-8) tightens the regex; do that.

2. **Is `\x1b[1C` interleaved within the literal characters of `Try`?** Claude's positional rendering sometimes interleaves cursor-rights even within single words. If so, the regex needs adjustment to allow non-`Try` bytes between `T`, `r`, `y`. Verification snippet in step 2 will surface this immediately.

3. **`make rerecord-snapshots` recipe — add or skip?** Architect recommends add (see § Step 8). Developer's call if the recipe ends up awkward against existing Makefile idioms.

4. **Should the lockfile assert `--strict-mcp-config`?** Currently `claude-version.lock` does not, even though the runner depends on it (`pty.go:78-89`). Out of scope for this ticket — flag the gap in the PR description if surfaced, do **not** bundle the lockfile expansion. Separate ticket.

## Out of scope

- Bumping `flag=` / `value=` lines in `claude-version.lock` absent observed change in `claude --help` shape.
- Adding `-record` mode to `e2e-snapshot-check` (spec 35 § 283 rules this out explicitly).
- Enlarging `pkg/tuidriver/buffer.go DefaultBufferCap` (Option 2 from the developer's redesign list — dead end per issue body's Out of Scope).
- Removing `--strict-mcp-config` from the snapshot capture path (Option 4 — wrong direction).
- Switching from byte-compare to parsed-shape comparison (Option 3 — long-term direction, not this ticket; file a separate ticket if normalisation maintenance grows).
- General claude-version pinning policy beyond bumping `version=`.
- Touching the parsers in `pkg/tuidriver/{picker,mcp,agents}.go`.
- The other live-claude spike checks #69 and #70.
- Fixing the architectural debt around the library JSONL API (#58–#62).
- Broadening the normaliser beyond the Try line. If multi-capture verification surfaces more volatility, STOP and route back to PO per AC #5 — do not silently broaden.

If any of these become *necessary* to make the e2e green (e.g. claude `2.1.148` changed `--help` shape and broke `flag=`/`value=` matching), STOP and re-scope via PO. Do not bundle.

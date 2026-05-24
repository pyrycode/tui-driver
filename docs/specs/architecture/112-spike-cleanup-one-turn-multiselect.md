# spec: spike-cleanup — migrate spike-one-turn + spike-multiselect off duplicated helpers (#112)

## Files to read first

- `cmd/spike-long-prompt/main.go` (entire file, ~300 lines) — canonical post-migration shape. Pay particular attention to:
  - L91–100 — `os.UserHomeDir` + `os.Getwd` + `tuidriver.SessionJSONLPath` composition.
  - L131–139 — the `RunWatchdog` goroutine (`go func() { if err := tuidriver.RunWatchdog(...); err != nil { logger.Printf("%v", err); cancelCause(err) } }`). This is the exact shape both target spikes adopt.
  - L187–193 — `WaitForSessionJSONL` with a scoped `context.WithTimeout(rootCtx, sessionFileWait)` and a separate `jsonlCancel()` call. **Do not** drop the scoped cancel — leaking the inner ctx would defer the budget enforcement until rootCtx itself dies.
  - L283–296 — slimmed `resolveSession(flagValue) → (sessionID, error)` (no `dir` argument; no jsonlPath return). This is the shape spike-one-turn's `resolveSession` is reshaped into.

- `cmd/spike-one-turn/main.go:43–60` — constant block. The migration prunes `jsonlTailInterval`, `sessionFilePoll`, `watchdogTick`; keeps `statePollInterval`, `sessionFileWait`, `ptyQuietLimit`, `spinnerFreezeLimit`, `shutdownGrace`, `ptyQuietWindow`, `promptText`.

- `cmd/spike-one-turn/main.go:218–286` — **the PTY-quiescence termination predicate** (`check()` closure + outer `for !check()` ticker loop, with the three-clause conjunction `gotEndTurn && idle-glyph-present && rb.QuietFor() >= ptyQuietWindow`). The 60+ lines of inline rationale (referencing #97) are load-bearing — the predicate must NOT be collapsed into spike-long-prompt's `gotEndTurn && (!thinkingObserved || spinnerGone)` event-driven shape. This spike intentionally diverges from that template for the reason finding #97 documents. Preserve the closure, the comments, and the constants verbatim.

- `cmd/spike-one-turn/main.go:288–460` — helpers being deleted: `matchSpinner` + `spinnerRe`, `projectsDir`, `openSessionJSONL`, `tailJSONL`, `isEndTurn`, `extractAssistantText`. The `resolveSession` helper is kept but reshaped (see Design § B).

- `cmd/spike-multiselect/main.go:45–58` — constant block + `spinnerRe`. Removes `watchdogTick`, `spinnerRe`; keeps the rest.

- `cmd/spike-multiselect/main.go:117–141` — inline watchdog goroutine to be replaced with `RunWatchdog`.

- `cmd/spike-multiselect/main.go:389–404` — `matchSpinner` to be deleted.

- `pkg/tuidriver/jsonl.go:35–37, 58–78, 108–113, 171–183, 297–305, 321–356` — library signatures + `JSONLEntry` shape. Note `IsEndTurn` and `AssistantText` are null-safe on the zero `JSONLEntry`; the spike does not need to pre-filter the channel.

- `pkg/tuidriver/watchdog.go:16–22, 66–85` — `WatchdogOpts{}` + `RunWatchdog` contract. Confirms `RunWatchdog` already runs `ParseSpinner` + `Tracker.ObserveSpinner` + `Tracker.CheckWatchdog` per tick — the spike-side `matchSpinner` + manual `tr.ObserveSpinner(ok, total)` + `tr.CheckWatchdog(rb)` shape collapses to a single library call.

- `pkg/tuidriver/state.go:115, 138–149` — `spinnerForRe` regex is byte-identical to the spike-local `spinnerRe` (verified in the size check). `RunWatchdog`/`ParseSpinner` is a true behavior-preserving drop-in.

- `pkg/tuidriver/cwd.go:20` — `EncodeCwd(cwd string) string` signature. Already imported transitively via `SessionJSONLPath`; the spike does not need to call `EncodeCwd` directly.

## Context

PRs #87/#89/#60/#98/#99 (merged 2026-05-23) promoted the load-bearing helpers from spike binaries into `pkg/tuidriver/`, and #98 reshaped `cmd/spike-long-prompt/main.go` to the canonical post-migration form (zero local helper copies). Two more spikes still hand-roll the same primitives:

- **spike-one-turn** keeps full local copies of `projectsDir`, `openSessionJSONL`, `tailJSONL`, `isEndTurn`, `extractAssistantText`, plus a hand-rolled watchdog goroutine.
- **spike-multiselect** keeps a hand-rolled watchdog goroutine (no JSONL tail in this spike — it is purely observational).

This ticket finishes the cleanup so the two spikes become library-API exemplars. Behavior, log-line shape, and assertions must remain identical — this is mechanical replacement, not redesign.

## Design

### A. spike-multiselect (smaller, do this first as the warm-up)

Three changes, all deletions or one-line replacements:

1. **Watchdog goroutine** (L117–141) → replace the entire `go func() { ... }` block with the spike-long-prompt-shaped one-liner: spawn a goroutine that calls `tuidriver.RunWatchdog(rootCtx, rb, tr, tuidriver.WatchdogOpts{})`, logs the non-nil return, and `cancelCause(err)`. Drop the `ticker` + manual `ObserveSpinner` + manual `CheckWatchdog` plumbing — `RunWatchdog` already does all three per tick at the same 1 Hz cadence (`DefaultWatchdogTick == 1 * time.Second`).
2. **Constants** — delete `watchdogTick` (L47) — now unused.
3. **Dead code** — delete `spinnerRe` (L58) and `matchSpinner` (L389–404) — both become unused after step 1.
4. **Imports** — remove `"regexp"` (only `spinnerRe` referenced it).

Everything else (idle-detect, trust-modal handling, trigger keystroke, settle window, post-trigger keys, snapshot + parse + dismiss) is untouched. No log lines change.

### B. spike-one-turn (larger, more moving parts)

Map each local helper to the library replacement:

| Local helper | Library replacement | Notes |
|---|---|---|
| `projectsDir()` (L312–322) | `tuidriver.EncodeCwd` (transitively, via `SessionJSONLPath`) | No call site needs `EncodeCwd` directly — `SessionJSONLPath` wraps it. Delete `projectsDir`. |
| `openSessionJSONL(path)` (L355–370) | `tuidriver.WaitForSessionJSONL(ctx, path)` inside a scoped `context.WithTimeout(rootCtx, sessionFileWait)` | Match spike-long-prompt L187–193. The library uses `DefaultPollInterval` internally — drop the local `sessionFilePoll` constant. |
| `tailJSONL(ctx, logger, path, off, out)` (L372–436) | `tuidriver.TailJSONL(rootCtx, jsonlPath, 0)` | Returns `<-chan JSONLEntry, error`. **Spawns its own goroutine** — no `wg.Add(1) + go func()` wrapper at the call site. The previous wrapper's `cancelCause(terr)` branch goes away: open/seek errors come back synchronously, and post-start read errors close the channel silently. The outer ticker loop already handles channel close (see § C). |
| `isEndTurn(ev)` over `map[string]any` (L440–444) | `tuidriver.IsEndTurn(ev JSONLEntry)` | Library version is null-safe. The previous tailer's pre-filter to `type == "assistant"` is shifted client-side: `IsEndTurn` returns false for non-assistant entries (no `Message`), so the conjunction behavior is preserved. |
| `extractAssistantText(ev)` (L446–460) | `tuidriver.AssistantText(ev JSONLEntry)` | Forced replacement: the channel type changes from `map[string]any` to `JSONLEntry`, so the local helper would need re-typing anyway. Use the library equivalent — matches spike-long-prompt L250. |
| inline watchdog goroutine (L131–153) | `tuidriver.RunWatchdog(rootCtx, rb, tr, tuidriver.WatchdogOpts{})` in a goroutine | Identical to § A.1. Drop `matchSpinner` + `spinnerRe` once this is in place. |

Reshape `resolveSession` (L331–347) to mirror spike-long-prompt's L283–296: take only `flagValue string`, return `(sessionID string, err error)` — no `dir` arg, no `jsonlPath` return. The call site then computes:

```
home, err := os.UserHomeDir()      // existing-style error handling
cwd, err := os.Getwd()             // existing-style error handling
jsonlPath := tuidriver.SessionJSONLPath(home, cwd, sessionID)
```

— same three-line composition spike-long-prompt uses at L91–100. The startup log lines that report `projects-dir path=...` and `session-id-resolved id=... jsonl=...` reduce to the single `session-id-resolved id=... jsonl=...` line (spike-long-prompt L100). Acceptable behavior change: the `projects-dir` line is redundant with `jsonl=` (the path contains the projects dir), and spike-long-prompt has already shed it. (If preserving the exact log surface matters, an extra `logger.Printf` is trivial — see § Open questions.)

### C. The PTY-quiescence termination loop — preserve verbatim

The outer poll-driven loop at L246–277 (the `check()` closure + `for !check()` over a `statePollInterval` ticker, with the three-clause conjunction `gotEndTurn && bytes.Contains(stripped, tuidriver.IdleGlyph) && rb.QuietFor() >= ptyQuietWindow`) **is the #97 design.** Do not replace it with spike-long-prompt's event-driven `for ev := range events` shape — that shape uses `(!thinkingObserved || spinnerGone)` instead of PTY-quiescence, and finding #97 documents the 1/5 failure rate that shape produces on this spike.

Two small adjustments are needed to keep the loop correct when `eventCh` is now closed by the library on ctx-cancel:

1. The receive becomes `case ev, ok := <-eventCh:` — when `ok == false`, set `eventCh = nil` and `continue` so the (now-nil) case never fires again. (A closed channel ready-receives the zero value at every tick; without this guard the loop would spin on stale zero-value entries.)
2. The receive body becomes `if !gotEndTurn && tuidriver.IsEndTurn(ev) { assistantText = tuidriver.AssistantText(ev); ... }` — null-safe on the zero `JSONLEntry`, but the `gotEndTurn` guard short-circuits anyway.

Everything else in the loop — the inline `check()`, the 60+ lines of rationale comments, `statePollInterval`, `ptyQuietWindow`, the `tr.RecordTransition("end-turn-detected")` + `logger.Printf("end-turn-detected")` calls — is preserved byte-for-byte.

### D. Constants + imports — what survives

**spike-one-turn constants** (L43–59):
- Keep: `statePollInterval`, `sessionFileWait`, `ptyQuietLimit`, `spinnerFreezeLimit`, `shutdownGrace`, `ptyQuietWindow`, `promptText`.
- Delete: `jsonlTailInterval` (was only inside `tailJSONL`), `sessionFilePoll` (was only inside `openSessionJSONL`), `watchdogTick` (was only inside the inline watchdog).

**spike-one-turn imports** (L20–41):
- Remove: `"bufio"`, `"encoding/json"`, `"io"`, `"path/filepath"`, `"regexp"`, `"strconv"`, `"strings"`. All trace to deleted helpers; `"strings"` was only used by `extractAssistantText.strings.Builder`.
- Keep: `"bytes"` (still used by the `check()` closure's `bytes.Contains(stripped, tuidriver.IdleGlyph)`), `"context"`, `"errors"`, `"flag"`, `"fmt"`, `"log"`, `"os"`, `"os/exec"`, `"sync"`, `"time"`, `"github.com/google/uuid"`, `"github.com/pyrycode/tui-driver/pkg/tuidriver"`.

**spike-multiselect constants** (L45–56): delete `watchdogTick`. Keep the rest.

**spike-multiselect imports**: delete `"regexp"`. Keep the rest.

## Concurrency model

Unchanged in shape — only the goroutine bodies shift to library calls:

- **rootCtx** with `context.WithCancelCause`. `cancelCause` is called by `defer` on `run` return, on `shutdown`, by the watchdog goroutine on wedge, and (in spike-one-turn pre-migration) by the tail goroutine on read error.
  - Post-migration in spike-one-turn, the tail goroutine is owned by `TailJSONL` internally and exits silently on ctx-done / read error. The `cancelCause` propagation from that path goes away by design (read errors on the append-only JSONL are effectively impossible; the watchdog catches any resulting wedge).
- **Watchdog goroutine** — `tuidriver.RunWatchdog(rootCtx, rb, tr, tuidriver.WatchdogOpts{})` in a goroutine. `cancelCause(err)` on non-nil return. Same 1 Hz cadence as before.
- **Tail goroutine** (spike-one-turn only) — internal to `TailJSONL`, not visible at the call site. Channel closes when rootCtx is done.
- **`sync.WaitGroup`** — pre-migration spike-one-turn used `wg.Add(1)` for the watchdog AND the tailer. Post-migration:
  - Spike-long-prompt drops the wg entirely (the watchdog goroutine's lifecycle is governed by rootCtx; on rootCtx-cancel it returns within one tick, and the `defer session.Close()` + the library-owned tail goroutine's own teardown are enough). Mirror that — drop `var wg sync.WaitGroup` and the `wg.Add(1)` calls. This matches spike-long-prompt verbatim and removes the only remaining sync.WaitGroup user in either spike.
  - Note: `wg.Wait()` was never actually called in pre-migration spike-one-turn (search the file — there is no `wg.Wait()` line; the WaitGroup was declared but never waited on). Removing it is a no-op for shutdown behavior.

## Error handling

Three failure modes mapped:

1. **`SessionJSONLPath` does not error** — pure path computation. Replaces `projectsDir` (which returned errors from `UserHomeDir` / `Getwd`); those errors now surface at the new call site `home, _ := os.UserHomeDir(); cwd, _ := os.Getwd()`, with the same `fmt.Errorf("resolve home dir: %w", err)` / `fmt.Errorf("resolve cwd: %w", err)` shape spike-long-prompt uses at L91–98. Acceptable: the `projects-dir` log line goes away but the `home`/`cwd` errors still abort with the same wrapping.

2. **`WaitForSessionJSONL` deadline** — returns a wrapped error including `context.Cause(ctx)`. The previous `openSessionJSONL` returned `"session JSONL did not appear at %s within %s"`. The new error string differs ("session jsonl %s did not appear: %w" vs old). Log surface change is acceptable per the ticket framing ("snapshot-equivalent" — the README + e2e harness assert on `session-jsonl-opened`, not on the failure-path string). If the e2e harness asserts on the exact failure string anywhere, the developer should grep `internal/e2e` and `pkg/tuidriver/testdata` for `did not appear` and flag in the PR.

3. **`TailJSONL` open/seek error** — returned synchronously from `TailJSONL(rootCtx, jsonlPath, 0)`. The call site wraps as `fmt.Errorf("open events stream: %w", err)` (matching spike-long-prompt L196–198) or `fmt.Errorf("tail session jsonl: %w", err)` — caller's choice; the developer should pick the one that produces minimal log-snapshot churn (spike-long-prompt's wording is `"open events stream"`).

4. **Channel close mid-loop** (the new case) — handled by the `ev, ok := <-eventCh` guard described in § C. If `ok == false` before `gotEndTurn` flips, the loop continues ticking; the watchdog or `ptyQuietLimit` will eventually wedge it and return an error. This is strictly safer than the pre-migration `cancelCause(terr)` behavior (which exited immediately on any tail error, masking the underlying wedge cause in log post-mortems).

## Testing strategy

- **`go build ./...`** in the worktree — both binaries must compile cleanly.
- **`go vet ./...`** — catches unused imports + unused constants that the migration leaves behind. If vet complains, the developer missed a cleanup item from § D.
- **`grep -n 'func projectsDir\|func openSessionJSONL\|func tailJSONL\|func isEndTurn\|func extractAssistantText\|func matchSpinner\|var spinnerRe' cmd/spike-one-turn/main.go cmd/spike-multiselect/main.go`** — must return empty. (AC #5 lists the four ticket-named functions; add the two side-effect deletions to keep the migration honest.)
- **`make e2e`** — runs the cross-spike harness against the pinned `claude` version. Pre-migration baseline: spike-one-turn's existing assertion is the `SUCCESS: <text>` line; spike-multiselect's existing assertions are the snapshot fixtures under `pkg/tuidriver/testdata/{picker,mcp,agents}-snapshot.json`. None of these should change.
- **Log-line diffs** — if the developer wants a fast sanity check before `make e2e`, run each binary directly (`go run ./cmd/spike-one-turn`, `go run ./cmd/spike-multiselect`) and compare the state-log lines against the most recent merge to main. Expected diffs: the `projects-dir` line in spike-one-turn disappears (see § B); no other line changes. Any unexpected diff is a regression — investigate before committing.

## Open questions

- **Preserve `projects-dir` log line?** Spike-long-prompt does not emit it. Spike-one-turn currently does, at L91. Recommendation: drop it (mirror the template). Cost of preservation if needed: one `logger.Printf("projects-dir path=%s", filepath.Dir(jsonlPath))` line after the `SessionJSONLPath` call — but that brings back the `"path/filepath"` import. Pick "drop it" unless a downstream log-tail consumer (grep against vault state logs?) is asserting on it, in which case keep the printf and the import. The developer should confirm by checking `git log --diff-filter=D` for any prior removal of similar telemetry, then make a one-line decision.

- **`tailJSONL` parse-warning log line** — pre-migration `tailJSONL` emitted `jsonl-parse-warning err=%v` for malformed JSON lines (L407). The library's `TailJSONL` silently drops malformed lines (parseEntry returns `(zero, false)` and the loop `continue`s). Behavior change is acceptable in practice (claude's JSONL writer is deterministic; malformed lines have never been observed in the wild), and the prior log line was diagnostic-only. No mitigation needed.

## Notes for the developer

- The migration is deletion-heavy. Touch lines you need to touch; do not refactor surrounding code "while you're there" (CLAUDE.md scope discipline). The two `main` files should look like spike-long-prompt's, not like spike-long-prompt-but-improved.
- If `go vet` flags an unused symbol you did not anticipate, that is the signal to extend the cleanup, not to add a `_ = symbol` line.
- Commit message convention: `cleanup(spike-one-turn,spike-multiselect): migrate off duplicated helpers (#112)` or similar — short, deletes-first framing.

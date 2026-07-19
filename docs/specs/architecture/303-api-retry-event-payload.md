# Spec: live API-error retry detector → `EventKindPtyApiRetry*` carrying attempt N/M

**Ticket:** [#303](https://github.com/pyrycode/tui-driver/issues/303)
**Size:** S
**Status:** ready for development
**Security-sensitive:** no (advisory status-banner detector; a forged retry line routes nothing — mirrors #220's non-sensitive classification, unlike the permission auto-answer path #219/#242).

## Files to read first

Load these in order; each entry says what to extract.

- `pkg/tuidriver/network.go:27-59` — `HasNetworkFailure` / `hasNetworkFailureGrid` and the `networkFailureAnchors` list. This is the **exact pattern** to mirror: a region-scoped detector iterating `g.LastRows(bannerRegionRows)`, an exported thin `NewGrid` wrapper over a grid-variant, and the "ADVISORY, not fatal" doc framing. Note the scope comment (lines 21-24): network.go deliberately owns **only** `Unable to connect to API`; the auth line `API Error: 401` is out of its scope. #303 owns the *generic transient retry* line and must stay disjoint from both.
- `pkg/tuidriver/mcp_banner.go:17,35-87` — `bannerRegionRows = 8` (the shared status-region window; reuse it, do not introduce a new constant), `mcpFailureBannerRe`, `mcpBannerMatchInRegion` (per-row region scan returning submatches), and `FailedMcpCount` / `mcpCountFromGrid` (the capture-group → `strconv.Atoi` → `(int, false-on-error)` idiom). This is the parse-with-capture-group template for the attempt counter.
- `pkg/tuidriver/state.go:187-271` — `ParseSpinnerTokens` and `ParseSpinner`: the `(value…, ok bool)` parse-result idiom returning `(zero, false)` on no-match / malformed, never panicking. `ParseApiRetry`'s contract follows this shape exactly.
- `pkg/tuidriver/events.go:21-100` — the `EventKind` iota block. New constants slot into the **PTY-side group**, immediately after `EventKindPtyNetworkFailureHidden` (line 70) and before `EventKindJsonlEntry`, keeping PTY kinds contiguous. (The iota values of the JSONL/stall/error kinds shift by 2; nothing serialises `EventKind` by integer, so this is cosmetic — same call the #100 spec made.)
- `pkg/tuidriver/events.go:120-150` — the `Event` struct + its "Field population by Kind" doc block. A new field is added here (see § Design / Event payload). This is **the first PTY event carrying a payload** — the field is populated only on the two new kinds.
- `pkg/tuidriver/events.go:245-497` — `mergeEvents` (the ticker arm's emission blocks, esp. the network/mcp banner blocks at 385-418), `ptyState` (452-463), and `classify` (488-497). The new axis adds two `ptyState` fields, two `classify` assignments, and one emission block. Read the banner blocks (385-418) as the template, then read § Design / Emission for the one divergence (re-emit on count-change).
- `pkg/tuidriver/network_test.go:10-88` — `TestHasNetworkFailureEmpty` (nil/empty), and especially `TestHasNetworkFailureRegion` (lines 51-76): the **template** for the detector's positive/negative region test — a forged-above-region phrase that must NOT fire, and an in-region status line that must. The in-region `live` fixture there (`"✻ " + anchor + " · Retrying in 1s · attempt 1/10"`) is nearly the shape of this ticket's line; mirror it with the `API error` token. `TestHasNetworkFailureIgnoresAuthError` (83-88) is the scope-boundary template.
- `pkg/tuidriver/anchor_forgery_test.go:72-89,126-154,476-511` — `forgedBodyForms` (renders an anchor as transcript-body content pushed above the region), `TestBannerAnchorsRejectBodyForgery` (add one case here — AC5), and `TestNegativeSuitePositiveControls` (the committed-`.bin` positive-control list; **not** touched by this ticket — see § Testing / fixture).
- `pkg/tuidriver/events_test.go:14-83` — `testSnap`, `mustReceiveEvent`, `assertEventChClosed`, `neverQuiet`, `zeroDims`. The new merge-loop test reuses all of these; do not introduce parallel scaffolding. The `TestMergeEvents_NetworkFailureTransitions` test in this file (added by #100) is the closest precedent for the new test's shape — find it and mirror its phase structure, then add the count-change phase this ticket needs.
- `pkg/tuidriver/state_test.go:10-15` — `gridRows(rows...)` (joins with `\r\n` so vt10x renders flat rows). Use it to synthesize the retry-line fixtures in Go, avoiding the `.bin` raw-ESC landmine (see § Testing / fixture).
- `pkg/tuidriver/grid.go:101-113` — `LastRows(n)` contract (bottom-n rows, single-row matches only). The region scope that defeats transcript-body forgery.
- `docs/specs/architecture/100-mcp-network-failure-events.md` — the parent spec that added the network/mcp banner axes to `mergeEvents`. Read § "Why banner events fire independently of modal state" and § "Emission block for the new axes"; this ticket adds a third banner-style axis with **one new wrinkle** (payload + re-emit-on-change) and otherwise follows #100 exactly.

## Context

Reviewing the #257 corpus-labeling exercise, API-error content appears on ~1,730 of the 64k sampled `agent-run` screens — errors and retries are common — yet today they render as ordinary "thinking" to a remote head. The high-value signal is the attempt count: `attempt 3/10` means the session is degrading, not merely slow. A remote head (pyrycode-desktop#488) wants to render "retrying, 3 of 10"; pyrycode#1074 relays the new status events over the v2 stream. Both already depend on this — the detector will not be stranded.

This is the region-scoped status-banner pattern already shipped for network-failure (#220) and mcp-failure, but with a **first-of-its-kind payload**: the parsed attempt count travels on the event. Split from #297 (its sibling, #304, adds a payload-free mid-response partial-output flag and is blocked-by this ticket).

### The #220 double-fire landmine (AC2 — the crux)

The real 2.1.199 network-failure fixture already renders the retry suffix:

```
✻ Unable to connect to API (ConnectionRefused) · Retrying in 1s · attempt 1/10
```

So `· Retrying in Ns · attempt N/M` is **shared sub-structure** between #220's network-unreachable line and the generic transient-API-error line this ticket targets. The two detectors must never classify the same rendered row. They stay disjoint because each requires a token the other's line lacks:

| Rendered status row | contains `Unable to connect to API` | contains `API error` | #220 fires | #303 fires |
|---|---|---|---|---|
| `✻ Unable to connect to API (…) · Retrying in 1s · attempt 1/10` | yes | no | **yes** | no |
| `✻ API error · Retrying in 1s · attempt 3/10` | no | yes | no | **yes** |
| `API Error: 401 Invalid authentication credentials` (auth) | no | no¹ | no | no |

¹ The auth line reads `API Error:` (capital E, colon) and has no `Retrying in` retry structure; #303's anchor requires the lowercase `API error` token **and** the `Retrying in` co-token, so the auth line is excluded on both counts — matching network.go's deliberate "network-only, auth is the dispatcher's 401 retry" scope boundary.

**Invariant to lock in a test:** `HasApiRetry(network-failure line) == false` and `HasNetworkFailure(api-error retry line) == false`. See § Testing.

## Design

### Files touched

```
pkg/tuidriver/apiretry.go        (NEW — ApiRetryAttempt type, HasApiRetry, ParseApiRetry, region-scoped grid-variants, anchors)
pkg/tuidriver/events.go          (MODIFIED — 2 EventKind constants, Event payload field, 2 ptyState fields, classify, emission block, doc updates)
pkg/tuidriver/apiretry_test.go   (NEW — detector + parser unit tests)
pkg/tuidriver/events_test.go     (MODIFIED — TestMergeEvents_ApiRetryTransitions, additive)
pkg/tuidriver/anchor_forgery_test.go (MODIFIED — one new banner-forgery case, additive)
```

**Production source files: 2** (`apiretry.go`, `events.go`) — under the ≥5 red line. No cross-package fan-out: `classify` is unexported with a single call site; `Event` gains a field (backward-compatible — every construction is field-named, external consumers only read events off the channel).

### Public API (`apiretry.go`)

Small surface, mirroring `network.go` + the `FailedMcpCount` parse idiom:

- **`type ApiRetryAttempt struct { Current int; Total int }`** — the parsed `attempt N/M`. `Current` is N, `Total` is M. The zero value `{0, 0}` is the "count unavailable" sentinel (a real counter is always `≥ 1 / M≥1`).

- **`func HasApiRetry(snap []byte) bool`** — reports whether the bottom status region (`LastRows(bannerRegionRows)`) contains claude's live API-error retry status row. Returns false on nil/empty snap. Thin `NewGrid(snap, 0, 0)` wrapper over `hasApiRetryGrid(g)`, exactly like `HasNetworkFailure`. Doc it as **ADVISORY** (claude retries itself; the PTY-quiet watchdog is the net for a genuinely hung session), matching `HasNetworkFailure`'s framing.

- **`func ParseApiRetry(snap []byte) (ApiRetryAttempt, bool)`** — returns `(attempt, true)` when the retry row is present **and** its `attempt N/M` counter parses; returns `({0,0}, false)` when no retry row is present, or the row is present but the counter is malformed/absent. Never panics. Follows the `ParseSpinner`/`ParseSpinnerTokens` `(value, ok)` idiom.

### Anchoring rule (the contract that keeps #220 disjoint)

The detector matches **per row within `bannerRegionRows`** (not the whole snapshot — region scoping is what defeats a transcript-body forgery, #220). A row counts as the API-error retry row iff it contains the stable token **`API error`** *and* the retry co-token **`Retrying in`** (structure + token, not the full literal — the seconds value, the `·` separators, and spacing all drift across claude versions and must not be anchored). Suggested internal regex, matching `mcp_banner.go`'s style (a package-level `regexp.MustCompile`, documented with the observed live form and a ⚠️ pointer to the forgery suite):

- present-ness / structure: a row matching `API error` … `Retrying in` (ordered `.*` between them; order is not load-bearing but is marginally stronger and matches the real render).
- counter: `attempt\s+(\d+)\s*/\s*(\d+)` applied **to the same matched row** (not the region) — binding the counter to the API-error row so it can never scrape the network line's `attempt 1/10` when both happen to be on screen.

Do **not** anchor `Unable to connect to API` (owned by #220) or the full literal. The `API error` token is the sole discriminator from the network line; `Retrying in` is the sole discriminator from the auth line — keep both required.

> ⚠️ The `API error` casing and the `Retrying in` phrasing are taken from the ticket's description of the live 2.1.199 render; no live `.bin` capture exists yet. If a real capture surfaces later and the token drifts, this is the one place to adjust. The two-token structural anchor (rather than the full literal) is chosen precisely so minor drift in the seconds/separators does not break it. See Open questions.

Internal decomposition is the developer's call, but mirror the render-once ethos (#225) the codebase uses: `classify` renders one `Grid` per tick and threads it into grid-variants (`hasApiRetryGrid(g)`, and a helper that returns the parsed attempt), while the public `HasApiRetry` / `ParseApiRetry` stay thin `NewGrid` wrappers — exactly as `hasNetworkFailureGrid` / `mcpBannerMatchInRegion` relate to their exported wrappers. A single region scan yielding `(present bool, attempt ApiRetryAttempt, parsed bool)` for `classify` to consume is the natural shape.

### Event payload (`events.go`)

Two new constants on the `EventKind` iota, in the PTY group after `EventKindPtyNetworkFailureHidden`:

- **`EventKindPtyApiRetryShown`** — fires on the rising edge of the retry state (retry row appears) **and re-fires whenever the attempt count changes while the state persists** (3/10 → 4/10). Carries the parsed attempt in the payload. Independent of the modal / idle / thinking axes (a banner axis, like network/mcp — fires even with a modal up or the spinner running).
- **`EventKindPtyApiRetryHidden`** — the paired falling edge: the retry row clears. Carries the last-known attempt (the value from the tick before it cleared) so a consumer's final render is coherent; a consumer that only cares about clearing ignores the payload.

One new field on `Event`:

```go
Retry ApiRetryAttempt // populated only on EventKindPtyApiRetry{Shown,Hidden}
```

Add the corresponding bullet to the "Field population by Kind" doc block. This is the first payload-carrying PTY event; note that explicitly in the field doc (every prior PTY event is payload-free; the kind was the whole signal).

### Emission (`mergeEvents`)

Two new `ptyState` fields:

```go
apiRetry        bool            // retry row present this tick (hasApiRetryGrid)
apiRetryAttempt ApiRetryAttempt // parsed counter; zero-value when the row is absent or the counter didn't parse
```

`classify` sets both from the one shared grid. New emission block, slotted alongside the existing network/mcp banner blocks (after them, before the stall block and `prev = cur`), following the banner-axis independence rule (no modal suppression — see #100 § "Why banner events fire independently of modal state"). Behaviour contract (developer writes the code):

1. **Rising edge** — `cur.apiRetry && !prev.apiRetry`: send `EventKindPtyApiRetryShown` with `Retry: cur.apiRetryAttempt`.
2. **Count change while present** — `cur.apiRetry && prev.apiRetry && cur.apiRetryAttempt != prev.apiRetryAttempt`: send `EventKindPtyApiRetryShown` with `Retry: cur.apiRetryAttempt`. (This is the **one divergence** from the network/mcp boolean-flip pattern: the payload can change while the state persists, and the consumer needs to watch the count climb — Technical Notes confirm re-emit is the intent.)
3. **Falling edge** — `!cur.apiRetry && prev.apiRetry`: send `EventKindPtyApiRetryHidden` with `Retry: prev.apiRetryAttempt` (last-known).

The three branches are mutually exclusive per tick. Use the existing backpressure-aware `send(ev)` helper; `Source: EventSourcePty`, `Time: now`, no `Modal`/`Entry`. `prev = cur` at tick end already captures both new fields (struct assignment), so the count-change comparison works across ticks with no extra bookkeeping.

**Malformed-counter interaction with re-emit:** when the row is present but the counter never parses, `apiRetryAttempt` stays `{0,0}` across ticks, so branch 2 never spuriously fires; the Shown event from branch 1 carries `{0,0}` ("retrying, count unknown"). A transient parse glitch (parsed → `{0,0}` → parsed) produces an extra Shown pair, which is acceptable and self-corrects.

### Doc-comment updates in `events.go`

Mirror the #100 edits: extend the "(idle / thinking / modal / mcp-failure / network-failure / stall)" enumerations in the `Session.Events`, `ScreenEvents`, and `mergeEvents` doc comments to include `api-retry`; add the new "Field population by Kind" bullet; note in the `mergeEvents` initial-tick comment that the api-retry axis follows the same rising-edge-from-zero rule (a snapshot already showing a retry row fires `Shown` on tick one). `ScreenEvents` emits the api-retry axis too (it is screen-derived, not JSONL-derived) — include it there.

### Concurrency / error handling

Unchanged. No new goroutines, no new shared state, no new error paths — the detector and parser are pure functions over a snapshot. Backpressure shape unchanged (the max events-per-tick rises by at most one; the channel is buffered at 32 and the existing `send` handles a lagging consumer).

## Testing strategy

### `apiretry_test.go` (new) — detector + parser

Reuse `gridRows` (state_test.go) and plain `[]byte` literals; the `✻` glyph and the retry phrase are ordinary UTF-8 in Go source. Scenarios (bullet-pointed inputs → expected; developer writes them in the project idiom):

- **Empty/nil:** `HasApiRetry(nil)` and `HasApiRetry([]byte("idle TUI bytes"))` → false; `ParseApiRetry(nil)` → `({0,0}, false)`.
- **In-region positive (mirror `TestHasNetworkFailureRegion`'s `live` half):** a snapshot ending with `"✻ API error · Retrying in 1s · attempt 3/10\r\n────\r\n❯ \r\n"` preceded by transcript-body filler → `HasApiRetry` true; `ParseApiRetry` → `({3, 10}, true)`.
- **Forged-above-region (mirror `TestHasNetworkFailureRegion`'s `forged` half):** the full retry phrase on the top row, pushed above the region by ~25 body rows below it → `HasApiRetry` false. Include the `strings.Contains` fixture-integrity guard so the forgery contrast is not silently voided.
- **#220 disjointness (both directions — AC2 crux):**
  - the real network line `"✻ Unable to connect to API (ConnectionRefused) · Retrying in 1s · attempt 1/10"` in-region → `HasApiRetry` false (lacks `API error`); and
  - the api-error line in-region → `HasNetworkFailure` false (lacks `Unable to connect to API`). Assert both so a future anchor loosening that reintroduces the overlap fails here. (Loading the committed `network-failure-snapshot.bin` for the first half is the strongest form; a synthesized `gridRows` line is acceptable per the ticket's fixture note.)
- **Auth-line exclusion (mirror `TestHasNetworkFailureIgnoresAuthError`):** `"API Error: 401 Invalid authentication credentials"` in-region → `HasApiRetry` false (capital-E `Error`, no `Retrying in`).
- **Parse valid / malformed / absent (AC3):** `attempt 3/10` → `({3,10}, true)`; a retry row with a malformed counter (`attempt 3/`, `attempt /10`, `attempt x/y`) → `({0,0}, false)` and no panic; a retry row with no `attempt` token at all → `({0,0}, false)`.

### `events_test.go` (modified) — `TestMergeEvents_ApiRetryTransitions`

Mirror `TestMergeEvents_NetworkFailureTransitions` (added by #100) with a count-change phase, using `testSnap`, `mustReceiveEvent`, `assertEventChClosed`, `neverQuiet`, `zeroDims`:

- Phase 1 — empty snap → assert quiescence (no event within ~2 poll intervals).
- Phase 2 — set an in-region `API error … attempt 3/10` snap → receive `EventKindPtyApiRetryShown`, `Source == EventSourcePty`, `Retry == {3,10}`, `Time` non-zero, `Modal == ModalClassUnknown`.
- Phase 3 — same row, counter now `attempt 4/10` (state persists, count climbs) → receive **another** `EventKindPtyApiRetryShown` with `Retry == {4,10}` (the re-emit-on-change contract).
- Phase 4 — clear the retry row (plain idle bytes) → receive `EventKindPtyApiRetryHidden` with `Retry == {4,10}` (last-known).
- Phase 5 — (optional, strengthens the independence claim) drive a modal + retry row simultaneously and assert the api-retry event still fires with the modal up.
- Phase 6 — cancel ctx; `assertEventChClosed`.

All existing `TestMergeEvents_*` must stay green unchanged (the `ptyState`/`classify`/`Event` changes are additive). `go test ./pkg/tuidriver/...` green; `make check` green.

### `anchor_forgery_test.go` (modified) — AC5

Add one case to `TestBannerAnchorsRejectBodyForgery`'s table: `{"api-error retry (#303)", "API error · Retrying in 1s · attempt 3/10", HasApiRetry}`. `forgedBodyForms` renders that full phrase as transcript-echo and source-read content pushed above the region; `HasApiRetry` must fire nothing (region scoping is the guard). The phrase is a full two-token structure so the forgery is non-vacuous (a bare `API error` with no `Retrying in` would trivially not fire for the wrong reason). Per the file's standing rule, add the `apiretry.go` anchor to the "Anchor sites enumerated here" comment block (lines 29-37).

### Fixture

**No new `.bin` fixture.** All positives are synthesized in Go via `gridRows` / `[]byte` literals — the retry line is plain UTF-8, so the `.cast`/`.bin` raw-ESC landmine (Go `json` rejects `0x1b`; the Write tool mangles `\uXXXX`) is sidestepped entirely, exactly as `TestHasNetworkFailureRegion` does. Consequently `TestNegativeSuitePositiveControls` (which pins committed `.bin` fixtures) is **not** touched — the api-retry positive control lives in `apiretry_test.go`. If a real 2.1.199 capture is later committed, adding it to `TestNegativeSuitePositiveControls` is a trivial follow-up, out of scope here.

## Open questions

- **Exact live token / casing.** The `API error` token and `Retrying in` co-token come from the ticket's description of the 2.1.199 render, not a committed capture. The two-token structural anchor is drift-tolerant by design; if a live capture shows a different token, adjust the single regex in `apiretry.go`. Not a blocker — the ticket explicitly permits a synthesized fixture.
- **Should `Hidden` carry the last-known attempt or the zero value?** Spec chooses last-known (`prev.apiRetryAttempt`) so a consumer that renders on every event ends on a coherent "was 4/10, now cleared" frame. A consumer keying purely on the kind ignores it. Resolved as last-known; revisit only if a consumer reports it confusing.
- **Cross-axis ordering within one tick** (api-retry vs network vs mcp Shown when several flip together). Internal emission order only, not part of the public contract — same stance as #100. Consumers must not depend on it.

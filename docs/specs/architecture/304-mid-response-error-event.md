# Spec: mid-response API-error (partial-output) detector → payload-free `EventKindPtyMidResponseError*`

**Ticket:** [#304](https://github.com/pyrycode/tui-driver/issues/304)
**Size:** S
**Status:** ready for development
**Security-sensitive:** no (advisory status-banner detector; a forged partial-output line routes nothing — mirrors #220's network-failure and #303's api-retry non-sensitive classification. Only the permission auto-answer path #219/#242 carries the label.)

This is the **payload-free sibling of #303** (the live-retry detector, shipped in PR #305 with zero deviation). #303 was the payload-carrying variant (a parsed `attempt N/M` counter travelling on the event). This ticket is the *simpler* case: the event kind itself is the whole signal, exactly like the network-failure and mcp-failure banners. **The clean mental model: this is network-failure (#220), not api-retry (#303).** Where the two specs diverge, follow network-failure and delete #303's payload machinery.

## Files to read first

Load these in order; each entry says what to extract.

- `pkg/tuidriver/network.go:27-59` — **the exact pattern to mirror**: `networkFailureAnchors` (a `[]string` of literal phrases, lines 27-29), the thin `HasNetworkFailure(snap)` `NewGrid` wrapper (43-45), and `hasNetworkFailureGrid(g)` iterating `g.LastRows(bannerRegionRows)` with `strings.Contains` (50-59). Note the scope comment (21-24): network.go deliberately owns **only** `Unable to connect to API`; the auth line `API Error: 401` is out of its scope. This ticket owns the *mid-response partial-output* line and must stay disjoint from both the network line and the auth line. Your detector is this file with a different anchor phrase.
- `pkg/tuidriver/mcp_banner.go:8-17` — `bannerRegionRows = 8`, the shared status-region window and its rationale (a status banner sits ≤8 rows from the bottom; scoping to it is the #173/#220 forgery fix). **Reuse this constant; do not introduce a new one.**
- `pkg/tuidriver/apiretry.go:54-71` — `HasApiRetry`'s doc framing: **ADVISORY, not fatal** (claude/the consumer surface it to a host UI; the PTY-quiet watchdog is the real net for a genuinely hung session). Copy that framing tone. **Do NOT copy anything else from this file** — `ApiRetryAttempt`, `ParseApiRetry`, the counter regex `apiRetryAttemptRe`, the `(present, attempt, parsed)` triple. This ticket is payload-free: there is no counter, no parse function, no struct. The file exists here only as the "what NOT to clone" reference.
- `pkg/tuidriver/events.go:63-70` — `EventKindPtyNetworkFailureShown`/`Hidden`: **the exact two-constant shape to clone** (payload-free banner pair). Lines 72-92 (`EventKindPtyApiRetryShown`/`Hidden`) are the payload-carrying pair — read them only to confirm you are *not* replicating their `Retry`-payload doc language.
- `pkg/tuidriver/events.go:142-178` — the `Event` struct and its "Field population by Kind" doc block. You add a **payload-free bullet** for the two new kinds (like the network-failure bullet at 149-152). **No new struct field** — this is not #303; nothing is added to `Event`.
- `pkg/tuidriver/events.go:412-449` — the mcp-failure (416-432) and network-failure (433-449) emission blocks in `mergeEvents`: **the exact boolean-flip template** (`cur.X && !prev.X` → Shown; `!cur.X && prev.X` → Hidden). Lines 450-485 (the api-retry block, with its count-change re-emit `else if` middle branch) are what you do **not** replicate — this axis has no middle branch.
- `pkg/tuidriver/events.go:519-536` — `ptyState`: you add **one** `bool` field alongside `networkFailure` (not the two fields #303 added).
- `pkg/tuidriver/events.go:561-573` — `classify`: you add **one** assignment (`midResponseError: hasMidResponseErrorGrid(g)`), mirroring the `networkFailure:` line at 569.
- `pkg/tuidriver/network_test.go:53-95` — `TestHasNetworkFailureRegion` (the region positive/negative template: a phrase forged above the region must NOT fire, the same phrase in the bottom region must; note the `strings.Contains` fixture-integrity guard) and `TestHasNetworkFailureIgnoresAuthError` (**the 401 scope-boundary template — directly reusable, AC1's teeth**). These two tests are your detector-test blueprint.
- `pkg/tuidriver/events_test.go:14-90` — `testSnap` (mutable snapshot source), `mustReceiveEvent`, `assertEventChClosed`, `neverQuiet`, `zeroDims`. Reuse all; introduce no parallel scaffolding.
- `pkg/tuidriver/events_test.go:578-627` — `TestMergeEvents_NetworkFailureTransitions`: **the exact merge-loop test to mirror** — a payload-free Shown → Hidden → (ANSI-wrapped) Shown-again → cancel/closed sequence. This is a closer precedent for #304 than it was for #303 (both payload-free). Mirror its phases verbatim with the new anchor; drop nothing, add no count-change phase.
- `pkg/tuidriver/events_test.go:629+` — `TestMergeEvents_BannerCoexistsWithIdleAndModal`: the precedent showing a banner Shown event co-firing with an idle/modal event in one tick (independence from the modal axis). Read for the "collect both into a set, order is not contract" idiom if you add the optional coexistence phase.
- `pkg/tuidriver/anchor_forgery_test.go:29-37,73-90,127-161` — the "Anchor sites enumerated here" comment block (add your new anchor to it), `forgedBodyForms` (renders an anchor as transcript-echo + source-read body content pushed above the region), and `TestBannerAnchorsRejectBodyForgery` (**add one case here — AC4**). Lines 484-518 (`TestNegativeSuitePositiveControls`) pin committed `.bin` fixtures and are **not** touched by this ticket (see § Testing / Fixture).
- `pkg/tuidriver/state_test.go:13` — `gridRows(rows...)` (joins rows with `\r\n` so vt10x renders flat rows). Use it to synthesize fixtures in Go, avoiding the `.bin` raw-ESC landmine.
- `pkg/tuidriver/grid.go:107` — `LastRows(n)` contract (bottom-n rendered rows, single-row matches only). The region scope that defeats transcript-body forgery.
- `docs/specs/architecture/303-api-retry-event-payload.md` — the sibling spec. Read § Design / Emission and § Testing, then **subtract** every payload element (the `Retry` field, the count-change re-emit branch, the `ParseApiRetry` contract, the `ApiRetryAttempt` type). What remains is this ticket.
- `docs/specs/architecture/100-mcp-network-failure-events.md` — the parent spec that first added the network/mcp banner axes to `mergeEvents`. Read § "Why banner events fire independently of modal state" and the emission-block section; this ticket adds a **fourth** banner-style axis with the *same* boolean-flip shape as the first two (no wrinkle at all).

## Context

From the same #257 corpus-labeling exercise that surfaced the live-retry line (#303), a distinct failure renders when claude's API connection drops **mid-turn**:

```
API Error: Connection closed mid-response. The response above may be incomplete.
```

The visible output above is truncated. Today a remote head cannot tell this partial turn from a normally finished one — the run reads as ordinary "thinking" then "idle." This detector emits a payload-free event so a remote head (pyrycode-desktop#488 / pyrycode#1074, both already depending on the #297 family) can mark the response **incomplete** instead of complete.

This is the region-scoped status-banner pattern already shipped for network-failure (#220), mcp-failure, and api-retry (#303). Unlike #303 it carries **no payload**: the event kind is the whole signal (the turn's output is partial). Split from #297; the blocking child (#303) merged in PR #305, so the shared `events.go` axis-and-emission plumbing is settled and this ticket rides on it with no new plumbing.

### The `API Error:` shared-prefix landmine (AC1 — the crux)

Three lines share the leading `API Error:` / `API error` text; each detector must classify only its own row:

| Rendered status row | distinctive token | owner | this detector fires |
|---|---|---|---|
| `API Error: Connection closed mid-response. The response above may be incomplete.` | `Connection closed mid-response` | **#304 (this)** | **yes** |
| `API Error: 401 Invalid authentication credentials` | `401` (dispatcher's own 401 retry) | out of scope (network.go/apiretry.go both exclude it) | **no** |
| `✻ Unable to connect to API (…) · Retrying in 1s · attempt 1/10` | `Unable to connect to API` | #220 | no |
| `✻ API error · Retrying in 1s · attempt 3/10` | `API error` + `Retrying in` | #303 | no |

The bare `API Error:` prefix is shared with the out-of-scope 401 auth line, so anchoring on it would false-fire on 401 — exactly the disjointness discipline #303 used to stay clear of #220's shared `· Retrying in Ns · attempt N/M` suffix. **Anchor on the partial-output token `Connection closed mid-response`** (and/or `The response above may be incomplete`), which appears in none of the three neighbouring lines. `Connection closed mid-response` is present in neither the 401 auth line, the network line, nor the retry line, so it is disjoint from all of them on its own.

**Invariant to lock in a test (AC1's teeth):** `HasMidResponseError("API Error: 401 …") == false`, and the neighbour detectors stay quiet on the mid-response line: `HasNetworkFailure(mid-response line) == false`, `HasApiRetry(mid-response line) == false`.

## Design

### Files touched

```
pkg/tuidriver/midresponse.go        (NEW — anchors + HasMidResponseError + hasMidResponseErrorGrid)
pkg/tuidriver/events.go             (MODIFIED — 2 EventKind constants, 1 ptyState field, 1 classify line, 1 emission block, doc updates)
pkg/tuidriver/midresponse_test.go   (NEW — detector unit tests)
pkg/tuidriver/events_test.go        (MODIFIED — TestMergeEvents_MidResponseErrorTransitions, additive)
pkg/tuidriver/anchor_forgery_test.go (MODIFIED — one new banner-forgery case + anchor-sites comment, additive)
```

**Production source files: 2** (`midresponse.go`, `events.go`) — under the ≥5 red line. **No cross-package fan-out:** `classify` is unexported with a single call site (`events.go:355`); `Event` gains **no** field (payload-free), so external consumers are entirely unaffected — they only read events off the channel.

### Naming (recommendation — the ticket says "describe behaviour, not names")

Concrete names so the developer has an unambiguous contract; adjust only if the codebase idiom clearly wants otherwise:

- Detector: **`HasMidResponseError(snap []byte) bool`**, file **`midresponse.go`**.
- Events: **`EventKindPtyMidResponseErrorShown`** / **`EventKindPtyMidResponseErrorHidden`**.
- `ptyState` field: **`midResponseError bool`**.

Named after the observable error condition (like `NetworkFailure`, `McpFailure`, `ApiRetry`), consistent with AC3's own phrase "the mid-response-error state." The *meaning* — the turn's output is partial/incomplete — is documented in the event's doc comment (that is where the consumer reads the interpretation), not baked into a `Partial…` name that could be confused with normal streaming partials.

### Public API (`midresponse.go`)

A single exported function, mirroring `network.go` (no `Parse*`, no type — payload-free):

- **`func HasMidResponseError(snap []byte) bool`** — reports whether the bottom status region (`LastRows(bannerRegionRows)`) contains claude's mid-response partial-output error line. Returns false on nil/empty snap. Thin `NewGrid(snap, 0, 0)` wrapper over the internal `hasMidResponseErrorGrid(g)` grid-variant, exactly like `HasNetworkFailure` → `hasNetworkFailureGrid`. Doc it as **ADVISORY** (a consumer surfaces "response incomplete" to a host UI; it is not a fatal condition — matching `HasNetworkFailure`/`HasApiRetry` framing).

Internal decomposition mirrors the render-once ethos (#225): `classify` renders one `Grid` per tick and threads it into `hasMidResponseErrorGrid(g)`; the public `HasMidResponseError` stays the thin `NewGrid` wrapper — exactly as `hasNetworkFailureGrid` relates to `HasNetworkFailure`.

### Anchoring rule (the contract that keeps 401 / #220 / #303 disjoint)

Match **per row within `bannerRegionRows`** (not the whole snapshot — region scoping is what defeats a transcript-body forgery, #173/#220), with `strings.Contains`, against a package-level `[]string` of anchor phrases — the **exact `networkFailureAnchors` shape**. Recommended anchor list:

```go
var midResponseErrorAnchors = []string{
	"Connection closed mid-response",
}
```

- **`Connection closed mid-response`** is the sole required phrase. It is the distinctive partial-output token, drawn from the partial-output part of the line per AC2 — disjoint from `API Error: 401 …`, `Unable to connect to API`, and `API error … Retrying in` on its own. Do **not** anchor the bare `API Error:` prefix (shared with 401) or the full literal (which drifts across claude versions — the sentence punctuation, casing, and trailing clause are all version-variable).
- The second phrase **`The response above may be incomplete`** is an equally-distinctive alternative from the same line; the `[]string` OR-match means adding it later (or now) is safe and widens drift tolerance without weakening disjointness. Developer's call whether to include it now — one phrase satisfies every AC; a second only adds robustness. Document the choice with the same ⚠️-pointer-to-the-forgery-suite comment `networkFailureAnchors` carries.

> ⚠️ These tokens come from the ticket's description of the live render; no committed `.bin` capture exists yet. If a real capture surfaces and a token drifts, this `[]string` is the single place to adjust. Add every anchor here to the #221 negative regression suite (`anchor_forgery_test.go`) so a transcript quotation stays non-firing — see § Testing.

### Event kinds (`events.go`)

Two new payload-free constants on the `EventKind` iota, in the PTY group. Slot them immediately after `EventKindPtyApiRetryHidden` (line 92) and before `EventKindJsonlEntry`, keeping PTY kinds contiguous (the iota values of the JSONL/stall/error kinds shift by 2; nothing serialises `EventKind` by integer, so this is cosmetic — the same call #303 and #100 made):

- **`EventKindPtyMidResponseErrorShown`** — fires on the **rising edge** of the mid-response-error state (the error line appears in the status region). Independent of the modal / idle / thinking axes (a banner axis, like network/mcp/api-retry — fires even with a modal up or the spinner running). **No payload fields** — the kind itself is the signal; the consumer marks the turn's output partial. Doc it with the "the turn's output is partial/incomplete" meaning here.
- **`EventKindPtyMidResponseErrorHidden`** — the paired **falling edge**: the line clears. Same payload-free shape.

Add a bullet to the "Field population by Kind" doc block (events.go:142-178) grouping these two with the other payload-free banner kinds (`EventKindPtyMcpFailure*`, `EventKindPtyNetworkFailure*`): "no payload fields. The kind itself is the signal." **No field is added to the `Event` struct** — contrast #303, which added `Retry`; this axis adds nothing.

### Emission (`mergeEvents`)

One new `ptyState` field:

```go
midResponseError bool // mid-response partial-output error line present this tick (hasMidResponseErrorGrid)
```

`classify` sets it from the one shared grid (one new assignment mirroring `networkFailure: hasNetworkFailureGrid(g)`). New emission block, slotted alongside the existing network/mcp banner blocks (after the network block at 449, before the api-retry block — grouping the payload-free banners together, keeping the payload-carrying api-retry block last), following the banner-axis independence rule (no modal suppression — see #100 § "Why banner events fire independently of modal state"). Behaviour contract (developer writes the code — **the exact boolean-flip shape of the network-failure block at 433-449**):

1. **Rising edge** — `cur.midResponseError && !prev.midResponseError`: send `EventKindPtyMidResponseErrorShown`.
2. **Falling edge** — `!cur.midResponseError && prev.midResponseError`: send `EventKindPtyMidResponseErrorHidden`.

Two branches, mutually exclusive per tick — **no third (count-change) branch**; that was #303's payload wrinkle and does not exist here. Use the existing backpressure-aware `send(ev)` helper; `Source: EventSourcePty`, `Time: now`, no `Modal`/`Entry`/`Retry`. `prev = cur` at tick end already captures the new field (struct assignment).

### Doc-comment updates in `events.go`

Mirror the #100/#303 edits: extend the "(idle / thinking / modal / mcp-failure / network-failure / api-retry / stall)" axis enumerations in the `Session.Events`, `ScreenEvents`, and `mergeEvents` doc comments to include the new axis (e.g. "… / api-retry / mid-response-error / stall"); add the new "Field population by Kind" bullet; note in the `mergeEvents` initial-tick comment that the new axis follows the same rising-edge-from-zero rule (a snapshot already showing the error line fires `Shown` on tick one). **`ScreenEvents` emits this axis too** (it is screen-derived, not JSONL-derived) — include it in that doc's enumeration.

### Concurrency / error handling

Unchanged. No new goroutines, no new shared state, no new error paths — the detector is a pure function over a snapshot. Backpressure shape unchanged (max events-per-tick rises by at most one; the channel is buffered at 32 and the existing `send` handles a lagging consumer).

## Testing strategy

### `midresponse_test.go` (new) — detector

Reuse `gridRows` (state_test.go) and plain `[]byte` literals; the error line is ordinary UTF-8 (no glyph, no control byte), so no `.bin` is needed. Scenarios (bullet-pointed inputs → expected; developer writes them in the project idiom, mirroring `network_test.go`):

- **Empty/nil:** `HasMidResponseError(nil)` and `HasMidResponseError([]byte("idle TUI bytes"))` → false.
- **Normal non-error screen:** an ordinary idle snapshot (`❯` prompt + transcript, no error line) → false. (Covers AC1's "false on a normal screen.")
- **In-region positive** (mirror `TestHasNetworkFailureRegion`'s `live` half): a snapshot ending with the error line in the bottom region, e.g. `body + "API Error: Connection closed mid-response. The response above may be incomplete.\r\n────\r\n❯ \r\n"` → `HasMidResponseError` true.
- **Forged-above-region** (mirror `TestHasNetworkFailureRegion`'s `forged` half): the full error line on the top row, pushed above the region by ~25 body rows below it → false. Include the `strings.Contains` fixture-integrity guard so the forgery contrast is not silently voided.
- **401 auth-line exclusion (AC1 teeth — mirror `TestHasNetworkFailureIgnoresAuthError`):** `"API Error: 401 Invalid authentication credentials"` in-region → `HasMidResponseError` false (shares only the `API Error:` prefix, lacks the partial-output token).
- **Disjointness from neighbours (both directions, AC1/AC2 crux):**
  - the mid-response line in-region → `HasNetworkFailure` false **and** `HasApiRetry` false (it lacks `Unable to connect to API` and lacks the `API error … Retrying in` two-token structure); and
  - the real network line `"✻ Unable to connect to API (ConnectionRefused) · Retrying in 1s · attempt 1/10"` in-region → `HasMidResponseError` false (lacks `Connection closed mid-response`). Assert both directions so a future anchor loosening that reintroduces overlap fails here.

### `events_test.go` (modified) — `TestMergeEvents_MidResponseErrorTransitions`

**Mirror `TestMergeEvents_NetworkFailureTransitions` (events_test.go:578-627) verbatim** with the new anchor, using `testSnap`, `mustReceiveEvent`, `assertEventChClosed`, `neverQuiet`, `zeroDims`:

- Phase 1 — empty snap → assert quiescence (no event within ~2 poll intervals).
- Phase 2 — set an in-region mid-response error snap → receive `EventKindPtyMidResponseErrorShown`, `Source == EventSourcePty`, `Time` non-zero.
- Phase 3 — clear the line (plain "recovered" / idle bytes) → receive `EventKindPtyMidResponseErrorHidden`.
- Phase 4 — ANSI-wrapped reappearance (`"\x1b[31m" + line + "\x1b[0m"`) → `EventKindPtyMidResponseErrorShown` again (the grid render consumes the CSI, preserves the phrase — the network test's Phase 4).
- Phase 5 — (optional, strengthens the independence claim; mirror `TestMergeEvents_BannerCoexistsWithIdleAndModal`) drive an idle/modal snapshot that also carries the error line and assert the mid-response event still fires alongside the idle/modal event (order not contract — collect into a set).
- Final — cancel ctx; `assertEventChClosed`.

All existing `TestMergeEvents_*` must stay green unchanged (the `ptyState`/`classify` changes are additive; `Event` is untouched). `go test ./pkg/tuidriver/...` green; `make check` green.

### `anchor_forgery_test.go` (modified) — AC4

Add one case to `TestBannerAnchorsRejectBodyForgery`'s table (alongside the network, mcp-failure, and #303 api-retry cases at lines 136-147):

```go
{"mid-response error (#304)", "API Error: Connection closed mid-response. The response above may be incomplete.", HasMidResponseError},
```

`forgedBodyForms` renders that full line as transcript-echo and source-read content pushed above the region; `HasMidResponseError` must fire nothing (region scoping is the guard). Use the **full line** (not just the anchor phrase) so the forgery is non-vacuous — a bare `API Error:` with no partial-output token would trivially not fire for the wrong reason. Per the file's standing rule, add the `midresponse.go` anchor to the "Anchor sites enumerated here" comment block (lines 29-37).

### Fixture

**No new `.bin` fixture.** The mid-response line is plain UTF-8, so all positives are synthesized in Go via `gridRows` / `[]byte` literals, sidestepping the `.cast`/`.bin` raw-ESC landmine (Go `json` rejects `0x1b`; the Write tool mangles `\uXXXX`) — exactly as `TestHasNetworkFailureRegion` and #303 do. The Technical-Notes preference for a real `.bin` is deferred: none exists in `testdata/`, and synthesizing one buys nothing over `gridRows` for a glyph-free ASCII line while incurring the escaping hazard. Consequently `TestNegativeSuitePositiveControls` (which pins committed `.bin` fixtures) is **not** touched — the mid-response positive control lives in `midresponse_test.go`. If a real 2.1.199 capture is later committed, adding it to `TestNegativeSuitePositiveControls` is a trivial follow-up, out of scope here.

## Open questions

- **Exact live token / casing.** `Connection closed mid-response` and `The response above may be incomplete` come from the ticket's description of the render, not a committed capture. The `[]string` anchor is the single adjustment point if a live capture shows drift. Not a blocker — the ticket explicitly permits a synthesized fixture.
- **Region placement vs. transcript flow.** This spec follows the ticket's explicit model: the line sits in the bottom status region and clears (Shown/Hidden edges), scoped to `bannerRegionRows` like network-failure. If a real capture shows claude renders it inside the *scrolling transcript* instead (so it scrolls away rather than clears), the region scope and the Hidden semantics would need revisiting. The ticket's region-scoped + Shown/Hidden framing is unambiguous, so this spec commits to it; flag any capture that contradicts it.
- **One anchor phrase or two.** One (`Connection closed mid-response`) satisfies every AC and stays disjoint. Including `The response above may be incomplete` in the same OR-list adds drift tolerance at zero disjointness cost. Left to the developer; both are safe.

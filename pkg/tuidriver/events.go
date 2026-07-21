package tuidriver

import (
	"context"
	"errors"
	"time"
)

// defaultEventBuffer is the buffered-channel capacity of the unified
// stream returned by Session.Events. Matches defaultJSONLTailBuffer
// (the upstream half of the merge) — same precedent, same blocking-
// on-full backpressure shape. Not exposed as a knob until a real
// consumer surfaces a real backpressure problem.
const defaultEventBuffer = 32

// EventKind classifies an Event on the unified Session.Events stream.
// Payload-field population is kind-dependent — see Event for the
// per-kind rules.
type EventKind int

const (
	// EventKindUnknown is the zero value. Exposed so `switch ev.Kind`
	// gets a compile-time-detectable default arm; the merge loop never
	// emits this kind.
	EventKindUnknown EventKind = iota

	// EventKindPtyIdle is the rising edge into the idle state — claude
	// returned to the input prompt with no spinner and no modal
	// active. No payload fields.
	EventKindPtyIdle

	// EventKindPtyThinking is the rising edge into the thinking state
	// — claude's thinking spinner appeared and no modal is active. No
	// payload fields.
	EventKindPtyThinking

	// EventKindPtyModalShown fires when a modal class becomes active.
	// Modal carries the appearing class. On a class→class change, the
	// merge loop emits Hidden(old) immediately before Shown(new).
	EventKindPtyModalShown

	// EventKindPtyModalHidden fires when a modal class becomes
	// inactive (cleared, or replaced by a different class). Modal
	// carries the just-hidden class.
	EventKindPtyModalHidden

	// EventKindPtyMcpFailureShown fires when the "N MCP server(s)
	// failed" status banner appears in the snapshot (rising edge:
	// HasMcpFailureBanner false → true on a poll tick). No payload
	// fields — the kind itself is the signal; consumers wanting the
	// failure count call FailedMcpCount(snap) directly.
	//
	// Independent of modal/idle/thinking state: this event can fire
	// while a modal is up or while the spinner is running. The banner
	// persists in the status area regardless of the dominant UI axis.
	EventKindPtyMcpFailureShown

	// EventKindPtyMcpFailureHidden is the paired falling edge —
	// HasMcpFailureBanner true → false on a poll tick. Same
	// payload-free shape.
	EventKindPtyMcpFailureHidden

	// EventKindPtyNetworkFailureShown fires when a network-failure
	// anchor (e.g. "Unable to connect to API") appears in the status region
	// of the snapshot. Same rising-edge semantics, same payload-free shape,
	// same independence from the modal/idle/thinking axes.
	EventKindPtyNetworkFailureShown

	// EventKindPtyNetworkFailureHidden is the paired falling edge.
	EventKindPtyNetworkFailureHidden

	// EventKindPtyApiRetryShown fires on the rising edge of claude's live
	// API-error retry state (the "API error … Retrying in … attempt N/M"
	// status row appears in the status region) AND re-fires whenever the
	// parsed attempt count changes while the state persists (3/10 → 4/10
	// climbs). Retry carries the parsed ApiRetryAttempt.
	//
	// This is the FIRST payload-carrying PTY event: every prior PTY banner
	// event is payload-free (the kind is the whole signal); this one adds
	// the attempt count so a remote head can render "retrying, 3 of 10".
	//
	// Independent of the modal/idle/thinking axes, like the mcp-failure and
	// network-failure banners: it fires while a modal is up or the spinner
	// runs. When the row is present but the counter did not parse, Retry is
	// the zero value {0, 0} ("retrying, count unknown").
	EventKindPtyApiRetryShown

	// EventKindPtyApiRetryHidden is the paired falling edge — the retry row
	// clears. Retry carries the last-known attempt (the value from the tick
	// before it cleared) so a consumer's final render stays coherent; a
	// consumer keying only on the kind ignores it.
	EventKindPtyApiRetryHidden

	// EventKindPtyMidResponseErrorShown fires on the rising edge of claude's
	// mid-response partial-output error state — the "API Error: Connection closed
	// mid-response …" line appears in the status region. It means the turn's
	// output is PARTIAL: the API connection dropped mid-turn and the visible
	// output above the line is truncated, so a remote head marks the response
	// incomplete instead of treating it as a normally finished turn.
	//
	// Payload-free, like the mcp-failure and network-failure banners (contrast
	// EventKindPtyApiRetry*, which carries a count): the kind itself is the whole
	// signal. Independent of the modal/idle/thinking axes — it fires while a modal
	// is up or the spinner runs, as the banner sits in the lower status area.
	EventKindPtyMidResponseErrorShown

	// EventKindPtyMidResponseErrorHidden is the paired falling edge — the
	// mid-response error line clears. Same payload-free shape.
	EventKindPtyMidResponseErrorHidden

	// EventKindPtyCompactingShown fires on the rising edge of claude's
	// auto-compaction state — the "Compacting conversation" banner with its
	// progress bar appears in the status region. Claude is summarizing its
	// context to reclaim room and is silent on the content channel meanwhile, so
	// a remote head renders a distinct "Compacting conversation" status instead
	// of looking frozen, and the runner does not mis-read the quiet as a wedge.
	//
	// Payload-free, like the mcp-failure, network-failure and mid-response
	// banners (contrast EventKindPtyApiRetry*, which carries a count): the kind
	// itself is the whole signal. The animating progress percentage is available
	// via ParseCompacting for a consumer that polls; it is deliberately NOT
	// re-emitted per tick, which would flood the stream as the bar climbs 0→100.
	// Independent of the modal/idle/thinking axes — the banner sits in the lower
	// status area and coexists with the dominant axis.
	EventKindPtyCompactingShown

	// EventKindPtyCompactingHidden is the paired falling edge — the compaction
	// banner clears (the summary completes and claude resumes). Same payload-free
	// shape.
	EventKindPtyCompactingHidden

	// EventKindJsonlEntry carries one parsed JSONL line forwarded from
	// the internal tail. Entry carries the JSONLEntry.
	EventKindJsonlEntry

	// EventKindJsonlEndOfTurn is the synthetic per-entry signal that
	// the immediately-preceding JsonlEntry satisfied IsEndTurn. Entry
	// carries the same JSONLEntry as the preceding event. Per-entry
	// semantics (no msg_id grouping) — see IsEndTurn.
	EventKindJsonlEndOfTurn

	// EventKindStallDetected is the safe-degrade marker: on a poll tick
	// the session is mid-turn (not idle), the PTY has been quiet beyond
	// the watchdog's PTYQuietLimit, AND no JSONL entry has arrived
	// within that same window. Rising-edge: fires once on entry into the
	// stalled condition, not every tick (consistent with the idle /
	// thinking / banner edges). No payload fields — the kind is the
	// signal; consumers wanting the quiet duration call Session.QuietFor.
	EventKindStallDetected

	// EventKindError is the terminal error signal: the internal JSONL
	// tail goroutine hit a non-EOF read error and closed its stream
	// while ctx was still live. Emitted at most once, immediately before
	// Events()' channel closes; Err carries the reason (ErrJSONLTailRead)
	// and Source is EventSourceJsonl. A clean shutdown (ctx cancel, or
	// EOF-then-cancel) closes the stream without an EventKindError, so a
	// consumer that only watches the stream can tell a broken tail apart
	// from Claude finishing cleanly.
	EventKindError
)

// ErrJSONLTailRead is the error carried by a terminal EventKindError:
// the internal JSONL tail's underlying read failed with a non-EOF error
// while ctx was still live. Match it with
// errors.Is(ev.Err, ErrJSONLTailRead). It marks that the tail broke, not
// why — it does not wrap the os-level read cause (see spec #167
// § Error handling).
var ErrJSONLTailRead = errors.New("tuidriver: jsonl tail read failed")

// EventSource tags an Event with its origin. Redundant with Kind for
// the cases consumers care about; convenient for switches that
// dispatch on source first.
type EventSource int

const (
	EventSourcePty EventSource = iota
	EventSourceJsonl
)

// Event is one element of the unified Session.Events stream.
//
// Field population by Kind:
//   - EventKindPtyIdle, EventKindPtyThinking: no payload fields.
//   - EventKindPtyModalShown, EventKindPtyModalHidden: Modal carries
//     the class (the appearing class on Shown; the just-hidden class
//     on Hidden).
//   - EventKindPtyMcpFailureShown, EventKindPtyMcpFailureHidden,
//     EventKindPtyNetworkFailureShown, EventKindPtyNetworkFailureHidden,
//     EventKindPtyMidResponseErrorShown, EventKindPtyMidResponseErrorHidden:
//     no payload fields. The kind itself is the signal; the predicate
//     identity is implicit.
//   - EventKindPtyApiRetryShown, EventKindPtyApiRetryHidden: Retry carries
//     the parsed attempt N/M (the current count on Shown; the last-known
//     count on Hidden; the zero value {0,0} when the counter did not parse).
//     This is the first PTY event that carries a payload — every other PTY
//     event is payload-free.
//   - EventKindJsonlEntry, EventKindJsonlEndOfTurn: Entry carries the
//     parsed entry. Both events for one end-of-turn carry the same
//     entry.
//   - EventKindStallDetected: no payload fields. The kind is the
//     signal; consumers wanting the quiet duration call Session.QuietFor.
//   - EventKindError: Err carries the terminal reason (ErrJSONLTailRead);
//     Source is EventSourceJsonl. No other payload field is populated.
//
// Time is the wall-clock instant the merge loop detected the event —
// the poll tick for PTY events, the channel-receive instant for JSONL
// events. Consumers wanting the JSONL envelope's own timestamp read
// it from Entry.Raw["timestamp"].
type Event struct {
	Kind   EventKind
	Source EventSource
	Time   time.Time
	Modal  ModalClass      // populated only on EventKindPtyModal*
	Entry  JSONLEntry      // populated only on EventKindJsonl*
	Err    error           // populated only on EventKindError
	Retry  ApiRetryAttempt // populated only on EventKindPtyApiRetry{Shown,Hidden}
}

// Events spawns a merge goroutine that emits PTY-state transitions
// (idle / thinking / modal / mcp-failure / network-failure / api-retry /
// mid-response-error / compacting / stall)
// and per-entry JSONL events on a single unified channel in arrival
// order. Internally tails the JSONL file at jsonlPath from
// startOffset (composes with WaitForSessionJSONL +
// SessionJSONLPath); errors from the synchronous open/seek phase are
// returned directly with the wrap shape TailJSONL provides. The
// returned channel is buffered (capacity 32 — matches TailJSONL); it
// is closed when ctx is cancelled, when the internal JSONL tail
// closes, or when the session terminates. If the internal JSONL tail
// hits a runtime (non-EOF) read error, a single terminal
// EventKindError carrying ErrJSONLTailRead is emitted immediately
// before the channel closes; a clean shutdown (ctx cancel or
// EOF-then-cancel) emits no such event.
//
// The merge loop is single-threaded with respect to the output
// channel — at most one Event is emitted per select iteration; the
// consumer sees a fully ordered stream. PTY-state polling cadence is
// DefaultPollInterval (50 ms); idle/thinking events are suppressed
// while a modal is active (the modal axis dominates). The mcp-failure,
// network-failure, api-retry, and mid-response-error banner axes are
// independent: their Shown/Hidden events fire regardless of modal state because
// the banners coexist with modal/idle/thinking UI in the lower status area. The
// api-retry Shown also re-fires on an attempt-count change while the
// retry state persists, carrying the new count in Event.Retry.
//
// tr is required (non-nil). The stall arm reuses tr's configured
// PTYQuietLimit — the same value Tracker.CheckWatchdog reads — so a
// single PTY-quiet timeout governs both the fatal watchdog and this
// non-fatal degrade marker (no second cadence, no duplicate timeout).
// EventKindStallDetected fires on the rising edge of the three-condition
// predicate (not idle AND PTY quiet beyond PTYQuietLimit AND no JSONL
// within that same window); it does not repeat while the stall persists.
// A nil tr panics on first dereference, consistent with RunWatchdog.
//
// Calling Events twice on one *Session spawns two independent merge
// loops; both work but each polls the same buffer at the same
// cadence — typical consumers call it once per session.
func (s *Session) Events(ctx context.Context, jsonlPath string, startOffset int64, tr *Tracker) (<-chan Event, error) {
	jsonlCh, err := TailJSONL(ctx, jsonlPath, startOffset)
	if err != nil {
		return nil, err
	}
	out := make(chan Event, defaultEventBuffer)
	go mergeEvents(ctx, s.buffer.Snapshot, s.gridDims, s.buffer.QuietFor, tr.ptyQuietLimit, jsonlCh, out, DefaultPollInterval)
	return out, nil
}

// ScreenEvents is Events without a JSONL transcript: it spawns the same merge
// loop but tails no file, so it emits ONLY the screen-derived axes
// (idle / thinking / modal / mcp-failure / network-failure / api-retry /
// mid-response-error) and
// never a JSONL entry, an end-of-turn, or a stall event. Because it opens no file it cannot
// fail, so it returns the channel directly with no error.
//
// Use it for consumers that need screen/modal events BEFORE (or without) a
// session transcript. Motivating case: surfacing a permission modal on a
// per-conversation session that has not written its JSONL yet. Interactive claude
// under --session-id defers JSONL creation until it produces output, and a claude
// blocked on a permission prompt produces none — so Events would block in
// WaitForSessionJSONL forever while the very modal that would unblock it goes
// unseen. ScreenEvents reads the modal straight off the PTY grid instead.
//
// The stall axis (EventKindStallDetected) is a turn-progress signal defined
// against JSONL arrival, so it is suppressed here (no transcript ⇒ no stall).
// The returned channel is buffered (capacity defaultEventBuffer) and closes when
// ctx is cancelled or the session terminates, same as Events.
func (s *Session) ScreenEvents(ctx context.Context) <-chan Event {
	out := make(chan Event, defaultEventBuffer)
	// nil jsonlCh ⇒ the merge loop tails no file and never fires the stall arm;
	// ptyQuietLimit is unused in that mode, so 0 is passed.
	go mergeEvents(ctx, s.buffer.Snapshot, s.gridDims, s.buffer.QuietFor, 0, nil, out, DefaultPollInterval)
	return out
}

// mergeEvents owns the unified merge loop. Polls snapshot at
// pollInterval for PTY-state transitions (idle / thinking / modal /
// mcp-failure / network-failure / api-retry / mid-response-error / stall),
// drains entries from jsonlCh,
// and writes typed Event values to out in arrival order. Closes out on
// every return path.
//
// quietFor + ptyQuietLimit drive the stall arm: a tick fires
// EventKindStallDetected when the session is not idle, quietFor()
// exceeds ptyQuietLimit, AND no JSONL entry has arrived within that
// same window (now − lastJsonlAt > ptyQuietLimit). lastJsonlAt is
// goroutine-local, refreshed by every JSONL arrival; its zero value
// (no JSONL yet) makes the no-progress condition hold, but the arm
// still cannot fire until the PTY itself goes quiet for the full limit.
//
// The loop starts transition-blind (prev state all-zero); the first
// tick emits whichever rising edges hold relative to that — a buffer
// already idle at subscription fires EventKindPtyIdle on tick one. A
// buffer already showing a modal fires EventKindPtyModalShown. The
// mcp-failure, network-failure, api-retry, mid-response-error, and stall
// axes follow the same rising-edge rule; mcp-failure, network-failure,
// api-retry, and mid-response-error are not suppressed by modal state. (A
// snapshot already showing a retry row fires EventKindPtyApiRetryShown on
// tick one, carrying its attempt count; one already showing the mid-response
// error line fires EventKindPtyMidResponseErrorShown.)
func mergeEvents(
	ctx context.Context,
	snapshot func() []byte,
	dims func() (cols, rows int),
	quietFor func() time.Duration,
	ptyQuietLimit time.Duration,
	jsonlCh <-chan JSONLEntry,
	out chan<- Event,
	pollInterval time.Duration,
) {
	defer close(out)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var prev ptyState
	// lastJsonlAt is the receive instant of the most recent JSONL entry
	// — the reference point for the stall arm's "no JSONL progress"
	// condition. Zero until the first arrival (treated as "no progress").
	var lastJsonlAt time.Time

	// send delivers ev with backpressure-aware ctx-abort. Returns
	// false on ctx cancellation; the caller returns from the loop.
	send := func(ev Event) bool {
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-jsonlCh:
			if !ok {
				// The tail closes jsonlCh with ctx still live ONLY on its
				// default: read-error arm (jsonl.go) — every other tail
				// exit is ctx-driven. So ctx.Err()==nil here means the tail
				// broke on a read; emit one terminal EventKindError before
				// the deferred close(out) so a stream-only consumer can
				// tell failure from a clean end. ctx.Err()!=nil is a clean
				// shutdown: emit nothing. (Reverses spec #61's out-of-band
				// discriminator; see spec #167.)
				if ctx.Err() == nil {
					_ = send(Event{
						Kind:   EventKindError,
						Source: EventSourceJsonl,
						Time:   time.Now(),
						Err:    ErrJSONLTailRead,
					})
				}
				return
			}
			now := time.Now()
			// Any JSONL entry is "progress" — structured output means
			// claude is responding, per the safe-degrade ladder.
			lastJsonlAt = now
			if !send(Event{
				Kind:   EventKindJsonlEntry,
				Source: EventSourceJsonl,
				Time:   now,
				Entry:  e,
			}) {
				return
			}
			if IsEndTurn(e) {
				if !send(Event{
					Kind:   EventKindJsonlEndOfTurn,
					Source: EventSourceJsonl,
					Time:   now,
					Entry:  e,
				}) {
					return
				}
			}
		case <-ticker.C:
			cols, rows := dims()
			cur := classify(snapshot(), cols, rows)
			now := time.Now()
			// Stall predicate (ADR 025 safe-degrade): mid-turn AND PTY
			// quiet beyond the limit AND no JSONL progress within that
			// same window. Computed here (not in classify, which sees
			// only the snapshot, never the quiet timings).
			// Stall is a turn-progress signal defined against JSONL arrival, so a
			// screen-only subscription (nil jsonlCh, e.g. ScreenEvents) never stalls.
			cur.stalled = jsonlCh != nil &&
				!cur.idle &&
				quietFor() > ptyQuietLimit &&
				now.Sub(lastJsonlAt) > ptyQuietLimit
			// Modal axis dominates: emit modal transitions first
			// (Hidden before Shown on a class→class change), then
			// idle/thinking only when no modal is active.
			if cur.modal != prev.modal {
				if prev.modal != ModalClassUnknown {
					if !send(Event{
						Kind:   EventKindPtyModalHidden,
						Source: EventSourcePty,
						Time:   now,
						Modal:  prev.modal,
					}) {
						return
					}
				}
				if cur.modal != ModalClassUnknown {
					if !send(Event{
						Kind:   EventKindPtyModalShown,
						Source: EventSourcePty,
						Time:   now,
						Modal:  cur.modal,
					}) {
						return
					}
				}
			}
			if cur.modal == ModalClassUnknown {
				if cur.idle && !prev.idle {
					if !send(Event{
						Kind:   EventKindPtyIdle,
						Source: EventSourcePty,
						Time:   now,
					}) {
						return
					}
				}
				if cur.thinking && !prev.thinking {
					if !send(Event{
						Kind:   EventKindPtyThinking,
						Source: EventSourcePty,
						Time:   now,
					}) {
						return
					}
				}
			}
			// Banner axes are independent of modal state — they fire
			// whenever the boolean flips, even with a modal up or the
			// spinner running. Internal emission order (MCP then
			// network) is not part of the public contract.
			if cur.mcpFailure && !prev.mcpFailure {
				if !send(Event{
					Kind:   EventKindPtyMcpFailureShown,
					Source: EventSourcePty,
					Time:   now,
				}) {
					return
				}
			} else if !cur.mcpFailure && prev.mcpFailure {
				if !send(Event{
					Kind:   EventKindPtyMcpFailureHidden,
					Source: EventSourcePty,
					Time:   now,
				}) {
					return
				}
			}
			if cur.networkFailure && !prev.networkFailure {
				if !send(Event{
					Kind:   EventKindPtyNetworkFailureShown,
					Source: EventSourcePty,
					Time:   now,
				}) {
					return
				}
			} else if !cur.networkFailure && prev.networkFailure {
				if !send(Event{
					Kind:   EventKindPtyNetworkFailureHidden,
					Source: EventSourcePty,
					Time:   now,
				}) {
					return
				}
			}
			// Mid-response partial-output error is a payload-free banner axis too
			// (independent of modal state), with the same appear/clear boolean-flip
			// shape as the network banner — no count and no re-emit-on-change middle
			// branch (that is #303's api-retry wrinkle). The kind is the whole
			// signal: the turn's output is partial.
			if cur.midResponseError && !prev.midResponseError {
				if !send(Event{
					Kind:   EventKindPtyMidResponseErrorShown,
					Source: EventSourcePty,
					Time:   now,
				}) {
					return
				}
			} else if !cur.midResponseError && prev.midResponseError {
				if !send(Event{
					Kind:   EventKindPtyMidResponseErrorHidden,
					Source: EventSourcePty,
					Time:   now,
				}) {
					return
				}
			}
			// Auto-compaction is a payload-free banner axis too, with the same
			// appear/clear boolean-flip shape as the network and mid-response
			// banners — no count and no re-emit-on-change middle branch. The kind
			// is the whole signal (claude is summarizing its context and is quiet
			// on the content channel); the animating progress percentage is polled
			// via ParseCompacting, not streamed per tick.
			if cur.compacting && !prev.compacting {
				if !send(Event{
					Kind:   EventKindPtyCompactingShown,
					Source: EventSourcePty,
					Time:   now,
				}) {
					return
				}
			} else if !cur.compacting && prev.compacting {
				if !send(Event{
					Kind:   EventKindPtyCompactingHidden,
					Source: EventSourcePty,
					Time:   now,
				}) {
					return
				}
			}
			// API-error retry is a banner axis too (independent of modal
			// state), but with a payload that can change while the state
			// persists: unlike the boolean banners above it re-emits Shown
			// when the parsed attempt count climbs (3/10 → 4/10) — the
			// consumer needs to watch the count. The three branches are
			// mutually exclusive per tick. Hidden carries prev's last-known
			// count; the count-change branch compares the whole struct, so a
			// malformed counter (stuck at {0,0} across ticks) never re-emits.
			if cur.apiRetry && !prev.apiRetry {
				if !send(Event{
					Kind:   EventKindPtyApiRetryShown,
					Source: EventSourcePty,
					Time:   now,
					Retry:  cur.apiRetryAttempt,
				}) {
					return
				}
			} else if cur.apiRetry && prev.apiRetry && cur.apiRetryAttempt != prev.apiRetryAttempt {
				if !send(Event{
					Kind:   EventKindPtyApiRetryShown,
					Source: EventSourcePty,
					Time:   now,
					Retry:  cur.apiRetryAttempt,
				}) {
					return
				}
			} else if !cur.apiRetry && prev.apiRetry {
				if !send(Event{
					Kind:   EventKindPtyApiRetryHidden,
					Source: EventSourcePty,
					Time:   now,
					Retry:  prev.apiRetryAttempt,
				}) {
					return
				}
			}
			// Stall is a rising-edge degrade marker: fires once on entry
			// into the stalled condition and re-arms only after it clears
			// (a PTY byte, a JSONL entry, or a return to idle).
			if cur.stalled && !prev.stalled {
				if !send(Event{
					Kind:   EventKindStallDetected,
					Source: EventSourcePty,
					Time:   now,
				}) {
					return
				}
			}
			// Symmetric modal suppression (fixes #61-deferred → #157): while a
			// modal is active the idle/thinking emissions above are suppressed
			// (modal axis dominates), so their prev trackers must be frozen too.
			// Otherwise prev.idle silently tracks the ❯-under-modal
			// classification; when the modal clears with claude still idle,
			// cur.idle && !prev.idle evaluates false and the idle rising edge is
			// lost. Freezing preserves the pre-modal edge across the modal
			// window so it fires on the clear tick — after EventKindPtyModalHidden,
			// which the modal block above already emitted first. stalled is
			// untouched: it was computed above (before this line) and reads
			// prev.stalled, not prev.idle.
			if cur.modal != ModalClassUnknown {
				cur.idle, cur.thinking = prev.idle, prev.thinking
			}
			prev = cur
		}
	}
}

// ptyState is one tick's PTY-derived classification across the axes
// the merge loop tracks. Internal to events.go — never exported.
type ptyState struct {
	idle           bool
	thinking       bool
	modal          ModalClass
	mcpFailure     bool
	networkFailure bool
	// midResponseError is whether claude's mid-response partial-output error
	// line is present in the status region this tick (hasMidResponseErrorGrid).
	// Payload-free — no counterpart field, unlike apiRetry.
	midResponseError bool
	// apiRetry is whether claude's API-error retry status row is present
	// this tick; apiRetryAttempt is its parsed attempt N/M, zero-value when
	// the row is absent or its counter did not parse. Both are set by
	// classify from the one shared grid.
	apiRetry        bool
	apiRetryAttempt ApiRetryAttempt
	// compacting is whether claude's auto-compaction banner ("Compacting
	// conversation" + a progress-bar run) is present in the status region this
	// tick (compactingInRegion). Payload-free — the animating percentage is not
	// tracked as event payload, unlike apiRetry; see EventKindPtyCompactingShown.
	compacting bool
	// stalled is the ADR 025 safe-degrade marker. Unlike the other
	// axes it is NOT set by classify (which sees only the snapshot) —
	// the merge loop computes it from the quiet-timing inputs after
	// classify returns. See mergeEvents' ticker arm.
	stalled bool
}

// gridForClassify builds the single Grid one classify tick renders from. A
// package var bound to NewGrid so a test can wrap it to assert classify renders
// the snapshot exactly once per tick (#225 TestClassifyRendersGridOncePerTick);
// production never rebinds it. Before #225 classify called five exported
// predicates that each rebuilt their own Grid, rendering the same 4 KB snapshot
// through the terminal emulator five times per tick, 20 ticks per second, for
// the whole run.
var gridForClassify = NewGrid

// classify reduces snap to the independent PTY-state axes the merge loop
// tracks. It renders the snapshot once (gridForClassify) at cols x rows and
// threads that one Grid into the grid-accepting variant of each predicate —
// isIdleGrid, busyInRegion, detectModalClassWithGrid, mcpBannerMatchInRegion,
// and hasNetworkFailureGrid — so a tick renders once, not once per axis. The
// exported single-snapshot predicates (IsIdle, DetectModalClass, …) stay as thin
// wrappers over these same variants, so non-tick callers and the public API are
// unchanged.
//
// cols/rows are the render dimensions (#226); the merge loop passes the
// session's current terminal size so a resized interactive session is rendered
// at the size claude drew it for. 0 for either falls through to the package
// default (NewGrid's convention), which is what non-session callers and the
// default-size case get.
func classify(snap []byte, cols, rows int) ptyState {
	g := gridForClassify(snap, cols, rows)
	apiRetry, apiRetryAttempt, _ := apiRetryInRegion(g)
	compacting, _, _ := compactingInRegion(g)
	return ptyState{
		idle:             isIdleGrid(g),
		thinking:         busyInRegion(g),
		modal:            detectModalClassWithGrid(g, snap),
		mcpFailure:       mcpBannerMatchInRegion(g) != nil,
		networkFailure:   hasNetworkFailureGrid(g),
		midResponseError: hasMidResponseErrorGrid(g),
		apiRetry:         apiRetry,
		apiRetryAttempt:  apiRetryAttempt,
		compacting:       compacting,
	}
}

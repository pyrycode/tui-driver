package tuidriver

import (
	"context"
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
	// anchor (e.g. FailedToOpenSocket) appears in the snapshot. Same
	// rising-edge semantics, same payload-free shape, same
	// independence from the modal/idle/thinking axes.
	EventKindPtyNetworkFailureShown

	// EventKindPtyNetworkFailureHidden is the paired falling edge.
	EventKindPtyNetworkFailureHidden

	// EventKindJsonlEntry carries one parsed JSONL line forwarded from
	// the internal tail. Entry carries the JSONLEntry.
	EventKindJsonlEntry

	// EventKindJsonlEndOfTurn is the synthetic per-entry signal that
	// the immediately-preceding JsonlEntry satisfied IsEndTurn. Entry
	// carries the same JSONLEntry as the preceding event. Per-entry
	// semantics (no msg_id grouping) — see IsEndTurn.
	EventKindJsonlEndOfTurn
)

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
//     EventKindPtyNetworkFailureShown, EventKindPtyNetworkFailureHidden:
//     no payload fields. The kind itself is the signal; the predicate
//     identity is implicit.
//   - EventKindJsonlEntry, EventKindJsonlEndOfTurn: Entry carries the
//     parsed entry. Both events for one end-of-turn carry the same
//     entry.
//
// Time is the wall-clock instant the merge loop detected the event —
// the poll tick for PTY events, the channel-receive instant for JSONL
// events. Consumers wanting the JSONL envelope's own timestamp read
// it from Entry.Raw["timestamp"].
type Event struct {
	Kind   EventKind
	Source EventSource
	Time   time.Time
	Modal  ModalClass // populated only on EventKindPtyModal*
	Entry  JSONLEntry // populated only on EventKindJsonl*
}

// Events spawns a merge goroutine that emits PTY-state transitions
// (idle / thinking / modal / mcp-failure / network-failure) and
// per-entry JSONL events on a single unified channel in arrival
// order. Internally tails the JSONL file at jsonlPath from
// startOffset (composes with WaitForSessionJSONL +
// SessionJSONLPath); errors from the synchronous open/seek phase are
// returned directly with the wrap shape TailJSONL provides. The
// returned channel is buffered (capacity 32 — matches TailJSONL); it
// is closed when ctx is cancelled, when the internal JSONL tail
// closes, or when the session terminates.
//
// The merge loop is single-threaded with respect to the output
// channel — at most one Event is emitted per select iteration; the
// consumer sees a fully ordered stream. PTY-state polling cadence is
// DefaultPollInterval (50 ms); idle/thinking events are suppressed
// while a modal is active (the modal axis dominates). The mcp-failure
// and network-failure banner axes are independent: their Shown/Hidden
// events fire regardless of modal state because the banners coexist
// with modal/idle/thinking UI in the lower status area.
//
// Calling Events twice on one *Session spawns two independent merge
// loops; both work but each polls the same buffer at the same
// cadence — typical consumers call it once per session.
func (s *Session) Events(ctx context.Context, jsonlPath string, startOffset int64) (<-chan Event, error) {
	jsonlCh, err := TailJSONL(ctx, jsonlPath, startOffset)
	if err != nil {
		return nil, err
	}
	out := make(chan Event, defaultEventBuffer)
	go mergeEvents(ctx, s.Buffer.Snapshot, jsonlCh, out, DefaultPollInterval)
	return out, nil
}

// mergeEvents owns the unified merge loop. Polls snapshot at
// pollInterval for PTY-state transitions (idle / thinking / modal /
// mcp-failure / network-failure), drains entries from jsonlCh, and
// writes typed Event values to out in arrival order. Closes out on
// every return path.
//
// The loop starts transition-blind (prev state all-zero); the first
// tick emits whichever rising edges hold relative to that — a buffer
// already idle at subscription fires EventKindPtyIdle on tick one. A
// buffer already showing a modal fires EventKindPtyModalShown. The
// mcp-failure and network-failure axes follow the same rising-edge
// rule and are not suppressed by modal state.
func mergeEvents(
	ctx context.Context,
	snapshot func() []byte,
	jsonlCh <-chan JSONLEntry,
	out chan<- Event,
	pollInterval time.Duration,
) {
	defer close(out)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var prev ptyState

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
				return
			}
			now := time.Now()
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
			cur := classify(snapshot())
			now := time.Now()
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
}

// classify reduces snap to the independent PTY-state axes the merge
// loop tracks. Each predicate strips ANSI internally; the snap is
// bounded by DefaultBufferCap (4 KB) so the cost of stripping per
// tick is negligible.
func classify(snap []byte) ptyState {
	return ptyState{
		idle:           IsIdle(snap),
		thinking:       IsThinking(snap),
		modal:          DetectModalClass(snap),
		mcpFailure:     HasMcpFailureBanner(snap),
		networkFailure: HasNetworkFailure(snap),
	}
}

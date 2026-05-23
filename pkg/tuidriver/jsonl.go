package tuidriver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// defaultJSONLTailBuffer is the buffered-channel capacity returned by
// TailJSONL. Matches the spike-side observed-stable size (every
// spike-* binary uses chan map[string]any with cap 32). Not exposed
// as a knob until a real consumer surfaces a real backpressure
// problem.
const defaultJSONLTailBuffer = 32

// SessionJSONLPath returns the path that claude writes its per-session
// JSONL log to for a session pinned via `claude --session-id <sessionID>`
// running with working directory cwd. The home argument is typically
// os.UserHomeDir(); pass it explicitly so the function stays pure and
// testable.
//
// The path is computed as filepath.Join(home, ".claude", "projects",
// EncodeCwd(cwd), sessionID+".jsonl"). EncodeCwd is used internally —
// no separate encoder can drift at the call site.
//
// The function does not validate sessionID (consumers do, e.g. via
// uuid.Parse), does not stat the file, and does not error.
func SessionJSONLPath(home, cwd, sessionID string) string {
	return filepath.Join(home, ".claude", "projects", EncodeCwd(cwd), sessionID+".jsonl")
}

// WaitForSessionJSONL polls path with os.Stat at DefaultPollInterval until
// the file exists or ctx is cancelled. Returns nil when the file appears;
// returns a wrapped context.Cause(ctx) on cancellation / deadline (so
// errors.Is(err, context.DeadlineExceeded) and errors.Is(err,
// context.Canceled) both work). Stat errors that are NOT os.IsNotExist
// are returned wrapped without further polling — permission errors and
// ENOTDIR do not resolve by retry.
//
// Distinct from WaitUntil because stat errors short-circuit the loop;
// WaitUntil's bool-only predicate cannot signal "stop polling".
//
// Use after WritePrompt — interactive claude under --session-id defers
// JSONL creation until first input lands (see
// docs/knowledge/architecture/jsonl-layout.md § "Empirical surprise").
// The caller owns the timeout via ctx — typical usage:
//
//	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
//	defer cancel()
//	err := tuidriver.WaitForSessionJSONL(ctx, path)
func WaitForSessionJSONL(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat session jsonl %s: %w", path, err)
	}
	ticker := time.NewTicker(DefaultPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("session jsonl %s did not appear: %w", path, context.Cause(ctx))
		case <-ticker.C:
			if _, err := os.Stat(path); err == nil {
				return nil
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("stat session jsonl %s: %w", path, err)
			}
		}
	}
}

// JSONLEntry is one parsed line from a claude session JSONL log.
//
// Type is the envelope kind (assistant, user, attachment,
// permission-mode, file-history-snapshot, ai-title, system,
// last-prompt — see docs/knowledge/architecture/jsonl-layout.md §
// "Observed top-level type values"). Message is non-nil iff the
// envelope carried a "message" object (i.e. only on assistant and
// user). Raw holds the full parsed JSON for fields outside the typed
// shape — envelope-specific fields like "attachment", "sessionId",
// future additions, and the message object itself.
//
// Fields are populated best-effort. Missing or type-mismatched JSON
// yields zero values (Type == "", Message == nil, Content == nil);
// the entry is still emitted. Consumers requiring presence-vs-absence
// semantics check `_, ok := e.Raw["message"]` directly.
//
// RawLine is the verbatim source-line bytes the tail goroutine read,
// with the trailing "\r\n" or "\n" stripped — byte-identical to what
// parseEntry consumed. It is NOT a round-trip of Raw: re-marshalling
// Raw via encoding/json normalises key order and whitespace, which
// RawLine preserves. Consumers that re-emit JSONL byte-for-byte (e.g.
// stream-json bridges) should read from RawLine, not from Raw.
//
// RawLine is populated for every entry delivered on the TailJSONL
// channel and owned by the entry — the library will not mutate it
// after emission. For entries constructed directly by callers (test
// fixtures, synthetic events), RawLine may be nil: its presence is
// the TailJSONL contract, not a struct invariant.
type JSONLEntry struct {
	Type    string
	Message *EntryMessage
	Raw     map[string]any
	RawLine []byte
}

// EntryMessage is the nested `message` object on assistant and user
// envelopes. The library populates ID, StopReason, and Content
// best-effort from the JSON; consumers reach through Raw (the full
// message map) for fields outside this typed view (model, role, usage,
// stop_sequence, …).
type EntryMessage struct {
	ID         string
	StopReason string
	Content    []ContentBlock
	Raw        map[string]any
}

// ContentBlock is one element of `message.content[]`. Type is the
// block's kind (text, thinking, tool_use, tool_result, …); Raw holds
// the full block JSON so consumers can extract type-specific fields
// (text, input, name, tool_use_id, …) without the library committing
// to a type-tagged union over claude's content-block schema.
type ContentBlock struct {
	Type string
	Raw  map[string]any
}

// TailJSONL opens path, seeks to startOffset, and spawns a goroutine
// that reads lines, parses each as JSON into a JSONLEntry, and emits
// them on the returned channel. Use 0 for startOffset to start at the
// beginning; use a previously recorded offset (e.g. f.Tell()-ish) to
// resume mid-file.
//
// Returns an error synchronously if the file cannot be opened or the
// seek fails — both indicate a programmer error or permission issue
// that polling will not resolve. Use WaitForSessionJSONL first to
// guarantee the file exists.
//
// The channel is closed when ctx is cancelled or when an
// unrecoverable read error occurs (rare on an append-only file). The
// goroutine returns within one poll tick (DefaultPollInterval, 50 ms)
// after ctx is cancelled.
//
// Malformed JSON lines are silently dropped — the goroutine continues
// reading. Partial lines split across reads are reassembled before
// parsing, so each completed JSON line is delivered as exactly one
// channel value, never two. EOF on a still-active log is treated as
// "wait for more bytes" (sleep one poll tick, retry).
//
// The channel is buffered (capacity 32 — matches the spike-side
// observed-stable size). Consumers should drain promptly; if the
// buffer fills, the tail goroutine blocks on the next send until
// either a receiver makes space or ctx is cancelled.
//
// Compose with SessionJSONLPath and WaitForSessionJSONL:
//
//	path := tuidriver.SessionJSONLPath(home, cwd, sessionID)
//	if err := tuidriver.WaitForSessionJSONL(ctx, path); err != nil { /* … */ }
//	entries, err := tuidriver.TailJSONL(ctx, path, 0)
//	if err != nil { /* … */ }
//	for ev := range entries { /* … */ }
func TailJSONL(ctx context.Context, path string, startOffset int64) (<-chan JSONLEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open session jsonl %s: %w", path, err)
	}
	if _, serr := f.Seek(startOffset, io.SeekStart); serr != nil {
		_ = f.Close()
		return nil, fmt.Errorf("seek session jsonl %s to %d: %w", path, startOffset, serr)
	}
	ch := make(chan JSONLEntry, defaultJSONLTailBuffer)
	go tailJSONLLoop(ctx, f, ch)
	return ch, nil
}

// tailJSONLLoop owns f and ch from entry. It closes both on every
// exit path. Runs on its own goroutine spawned by TailJSONL.
func tailJSONLLoop(ctx context.Context, f *os.File, ch chan<- JSONLEntry) {
	defer close(ch)
	defer f.Close()
	reader := bufio.NewReader(f)
	var partial []byte
	for {
		if ctx.Err() != nil {
			return
		}
		chunk, rerr := reader.ReadBytes('\n')
		if len(chunk) > 0 {
			partial = append(partial, chunk...)
		}
		switch {
		case rerr == nil:
			line := bytes.TrimRight(partial, "\r\n")
			partial = partial[:0]
			if len(line) == 0 {
				continue
			}
			entry, ok := parseEntry(line)
			if !ok {
				continue
			}
			select {
			case ch <- entry:
			case <-ctx.Done():
				return
			}
		case rerr == io.EOF:
			select {
			case <-ctx.Done():
				return
			case <-time.After(DefaultPollInterval):
			}
		default:
			return
		}
	}
}

// parseEntry parses one already-trimmed JSONL line into a JSONLEntry.
// Returns (zero-value, false) if the bytes are not valid JSON or do
// not decode into a top-level object. Caller is responsible for
// filtering out empty/whitespace-only lines.
func parseEntry(line []byte) (JSONLEntry, bool) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return JSONLEntry{}, false
	}
	if raw == nil {
		return JSONLEntry{}, false
	}
	// bytes.Clone is mandatory — line aliases tailJSONLLoop's rolling
	// `partial` buffer, which the next iteration's append may clobber.
	entry := JSONLEntry{Raw: raw, RawLine: bytes.Clone(line)}
	entry.Type, _ = raw["type"].(string)
	if m, ok := raw["message"].(map[string]any); ok {
		msg := parseMessage(m)
		entry.Message = &msg
	}
	return entry, true
}

func parseMessage(m map[string]any) EntryMessage {
	msg := EntryMessage{Raw: m}
	msg.ID, _ = m["id"].(string)
	msg.StopReason, _ = m["stop_reason"].(string)
	if blocks, ok := m["content"].([]any); ok {
		msg.Content = make([]ContentBlock, 0, len(blocks))
		for _, b := range blocks {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			msg.Content = append(msg.Content, parseContentBlock(bm))
		}
	}
	return msg
}

func parseContentBlock(b map[string]any) ContentBlock {
	cb := ContentBlock{Raw: b}
	cb.Type, _ = b["type"].(string)
	return cb
}

// IsEndTurn reports whether e marks turn-end under the Phase-A
// discriminator: the entry is an assistant envelope, its
// message.stop_reason == "end_turn", AND its combined "text" content
// (concatenation of content[] blocks whose type == "text") has
// non-zero length.
//
// The text-non-empty half disambiguates a per-block delta line that
// carries only thinking or tool_use blocks from the line that carries
// the user-visible reply. Both can land with stop_reason=end_turn (a
// single assistant message is serialised as one JSONL line per
// content block, and every delta line carries stop_reason); the text
// content is what distinguishes them. See
// docs/knowledge/architecture/jsonl-layout.md § "Turn lifecycle".
//
// Returns false for non-assistant entries, entries with a nil
// Message, entries with stop_reason != "end_turn", and zero-value
// entries. Safe to call on a JSONLEntry{} — never panics.
//
// Per-entry semantics: this function returns true for the single
// JSONL line that carries the (non-empty) text block of a turn. A
// turn whose text is split across multiple lines (one block per line,
// all sharing message.id) needs msg_id grouping on top — out of scope
// for the library; consumers compose if needed.
func IsEndTurn(e JSONLEntry) bool {
	if e.Type != "assistant" || e.Message == nil {
		return false
	}
	if e.Message.StopReason != "end_turn" {
		return false
	}
	return AssistantText(e) != ""
}

// AssistantText returns the concatenation of e.Message.Content[]
// blocks whose Type == "text", reading each block's "text" field from
// its Raw map. Returns "" if e is not an assistant entry, has a nil
// Message, or carries no non-empty text blocks. Safe to call on a
// JSONLEntry{} — never panics.
//
// Blocks are joined in JSONL arrival order (the order content[] was
// parsed in). Non-string or missing "text" fields contribute nothing
// (zero-value-on-mismatch type assertion); type="thinking",
// "tool_use", "tool_result", etc. are skipped.
//
// This is the per-entry primitive. Consumers needing the full turn's
// text (split across lines under one message.id) build msg_id
// grouping on top — out of scope for the library.
func AssistantText(e JSONLEntry) string {
	if e.Type != "assistant" || e.Message == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range e.Message.Content {
		if c.Type != "text" {
			continue
		}
		text, _ := c.Raw["text"].(string)
		if text == "" {
			continue
		}
		b.WriteString(text)
	}
	return b.String()
}

// Usage is the four token counters carried by an assistant entry's
// message.usage block (Anthropic API shape: input_tokens, output_tokens,
// cache_creation_input_tokens, cache_read_input_tokens). A counter
// value of zero is the wire value; absence of the usage block itself
// is signalled by AssistantUsage returning nil — see that function.
//
// Additional usage fields claude may emit (service_tier, cache TTL
// breakdowns, …) are intentionally not surfaced here; consumers needing
// them reach through e.Message.Raw["usage"] directly.
type Usage struct {
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

// AssistantUsage returns the four token counters carried by e's
// message.usage block as a *Usage, or nil if e is not an assistant
// entry, has a nil Message, or carries no usage map. Safe to call on a
// JSONLEntry{} — never panics.
//
// A nil return distinguishes "usage block absent" from "usage block
// present, all counters zero" (the latter returns a non-nil &Usage{}).
// Each counter is read from Message.Raw["usage"] as a JSON number
// (float64 after encoding/json) and converted to int; missing or
// non-numeric keys contribute 0, matching AssistantText's
// zero-value-on-mismatch posture.
//
// This is the per-entry primitive. Consumers needing per-turn totals
// aggregate across entries (typically grouped by message.id) — out of
// scope for the library.
func AssistantUsage(e JSONLEntry) *Usage {
	if e.Type != "assistant" || e.Message == nil {
		return nil
	}
	usage, ok := e.Message.Raw["usage"].(map[string]any)
	if !ok {
		return nil
	}
	intField := func(key string) int {
		n, _ := usage[key].(float64)
		return int(n)
	}
	return &Usage{
		InputTokens:              intField("input_tokens"),
		OutputTokens:             intField("output_tokens"),
		CacheCreationInputTokens: intField("cache_creation_input_tokens"),
		CacheReadInputTokens:     intField("cache_read_input_tokens"),
	}
}

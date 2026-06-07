package tuidriver

// ToolResult is the structural shape of a `tool_result` content block as
// carried in a single JSONL user envelope. claude emits one such block to
// report the outcome of a tool it invoked earlier (the reply to the
// matching `tool_use` block — see ParseToolUse); the same shape arrives on
// JSONL for headless consumers and is the robust source for a ToolUpdate
// signal — unlike the screen-scraped PTY spinner, which is dead at claude
// 2.1.158 (see CLAUDE.md).
//
// Unlike `tool_use`, `tool_result` blocks ride **user**-role envelopes,
// not assistant — the observed sequence is assistant(tool_use) →
// user(tool_result). The assistant-only tailer filter drops them.
//
// Content is intentionally typed `any`: the wire shape is a union — a
// string for the common Bash/Read result, or an array of content blocks
// (`[{"type":"text","text":"…"}]`, the Anthropic structured shape). It is
// preserved exactly as found on the wire (a string projects to string, an
// array to []any, an absent key to nil) rather than normalised, the same
// generic-preservation choice ToolUse.Input makes for its per-tool shape.
//
// Safe to read on a zero value — never panics.
type ToolResult struct {
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
	Content   any    `json:"content"`
}

// ParseToolResult extracts the first `tool_result` content block from snap,
// the bytes of a single JSONL user-envelope line, and projects its
// tool_use_id, is_error, and content into a *ToolResult.
//
// Behaviour:
//   - snap must be one JSON object. Envelope/message/content decoding is
//     delegated to parseEntry so the wire shape stays single-sourced.
//   - The envelope gate is "user", NOT "assistant" — the one structural
//     divergence from ParseToolUse / ParseAskUserQuestion. tool_result
//     blocks ride user envelopes.
//   - The first content[] block whose type is "tool_result" wins; later
//     blocks are ignored.
//   - Fields read from the block's Raw map. tool_use_id and is_error use
//     zero-value-on-mismatch type assertions; content is read with a plain
//     map index (no assertion) because its wire shape is a string|array
//     union — there is nothing to assert against, and an absent key already
//     yields nil. Missing or type-mismatched fields become zero values
//     ("", false, nil), matching the library's AssistantText /
//     AssistantUsage posture. A `tool_result` block with no fields still
//     returns a non-nil *ToolResult — the presence of type:"tool_result"
//     alone is the match signal, NOT any field's presence. (This mirrors
//     ParseToolUse's deliberate non-gating; do not "fix" it into a gate.)
//
// Returns nil on any of: not valid JSON, not a JSON object, not a user
// envelope, a nil message, or no `tool_result` block present (e.g. a
// user(text) cancel marker, which carries only a text block).
// Safe to call on a nil/zero-value snap — never panics.
func ParseToolResult(snap []byte) *ToolResult {
	entry, ok := parseEntry(snap)
	if !ok || entry.Type != "user" || entry.Message == nil {
		return nil
	}
	for _, c := range entry.Message.Content {
		if c.Type != "tool_result" {
			continue
		}
		tr := &ToolResult{}
		tr.ToolUseID, _ = c.Raw["tool_use_id"].(string)
		tr.IsError, _ = c.Raw["is_error"].(bool)
		tr.Content = c.Raw["content"]
		return tr
	}
	return nil
}

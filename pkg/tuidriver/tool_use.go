package tuidriver

// ToolUse is the structural shape of a `tool_use` content block as
// carried in a single JSONL assistant envelope. claude emits one such
// block when it invokes a tool mid-turn (Bash, Read, AskUserQuestion,
// …); the same shape arrives on JSONL for headless consumers and is
// the robust source for a ToolStart signal — unlike the screen-scraped
// PTY spinner, which is dead at claude 2.1.158 (see CLAUDE.md).
//
// Input preserves the tool's arbitrary input object exactly as found on
// the wire (the shape varies per tool: Bash→command, Read→file_path, …),
// so it is a generic map rather than a typed-per-tool union.
//
// Safe to read on a zero value — never panics.
type ToolUse struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// ParseToolUse extracts the first `tool_use` content block from snap,
// the bytes of a single JSONL assistant-envelope line, and projects its
// id, name, and input into a *ToolUse.
//
// Behaviour:
//   - snap must be one JSON object. Envelope/message/content decoding
//     is delegated to parseEntry so the wire shape stays single-sourced.
//   - The first content[] block whose type is "tool_use" wins; later
//     blocks are ignored. Unlike ParseAskUserQuestion, the match is NOT
//     name-gated: any tool projects, whatever its name.
//   - Fields read from the block's Raw map (id, name, input). Missing or
//     type-mismatched fields become zero values ("", "", nil), matching
//     the library's AssistantText / AssistantUsage posture. A `tool_use`
//     block with no name/id/input still returns a non-nil *ToolUse — the
//     presence of type:"tool_use" alone is the match signal, NOT any
//     field's presence. (This is the one deliberate divergence from
//     ParseAskUserQuestion, which gates on input.questions; do not
//     "fix" it into a gate.)
//
// Returns nil on any of: not valid JSON, not a JSON object, not an
// assistant envelope, a nil message, or no `tool_use` block present.
// Safe to call on a nil/zero-value snap — never panics.
func ParseToolUse(snap []byte) *ToolUse {
	entry, ok := parseEntry(snap)
	if !ok || entry.Type != "assistant" || entry.Message == nil {
		return nil
	}
	for _, c := range entry.Message.Content {
		if c.Type != "tool_use" {
			continue
		}
		tu := &ToolUse{}
		tu.ID, _ = c.Raw["id"].(string)
		tu.Name, _ = c.Raw["name"].(string)
		tu.Input, _ = c.Raw["input"].(map[string]any)
		return tu
	}
	return nil
}

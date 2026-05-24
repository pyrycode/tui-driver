package tuidriver

// AskUserQuestion is the structural shape of an `AskUserQuestion`
// tool_use event as carried in a single JSONL assistant envelope.
// claude raises this event mid-turn when the AskUserQuestion tool is
// invoked: the turn pauses pending user input, the TUI renders a modal,
// and the same shape arrives on JSONL for headless consumers.
//
// One assistant envelope carries one such event. The Questions slice
// is always non-empty on a successfully parsed value (ParseAskUserQuestion
// returns nil if it would be empty).
type AskUserQuestion struct {
	Questions []AskUserQuestionItem `json:"questions"`
}

// AskUserQuestionItem is one question within an AskUserQuestion event.
// Question is the prompt text shown to the user. Header is a short
// label shown above the option list (may be empty). MultiSelect mirrors
// the wire boolean: true means the user may pick more than one option.
// Options is the list of choices and is non-empty in the captured
// shapes seen so far, but the parser does not gate on its length.
type AskUserQuestionItem struct {
	Question    string                  `json:"question"`
	Header      string                  `json:"header,omitempty"`
	MultiSelect bool                    `json:"multiSelect"`
	Options     []AskUserQuestionOption `json:"options"`
}

// AskUserQuestionOption is one choice presented for an
// AskUserQuestionItem. Label is the short user-facing string the
// option is selected by. Description is the longer explanation
// rendered beside the label (may be empty).
type AskUserQuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// ParseAskUserQuestion extracts the AskUserQuestion tool_use shape
// from snap, the bytes of a single JSONL assistant-envelope line.
// Returns items in JSONL-arrival order (the order the wire put them in).
//
// Behaviour:
//   - snap must be one JSON object. Envelope/message/content decoding
//     is delegated to parseEntry so the wire shape stays single-sourced.
//   - The first content[] block whose type is "tool_use" and whose
//     name is "AskUserQuestion" wins; later blocks are ignored.
//   - Per-question fields read from the wire's camelCase names
//     (question, header, multiSelect, options). Missing or
//     type-mismatched fields become zero values, matching the
//     library's AssistantUsage / parseContentBlock posture.
//
// Returns nil on any of: not valid JSON, not an assistant envelope,
// no matching tool_use block, input.questions absent or empty, every
// questions element malformed. On success, len(Questions) >= 1.
func ParseAskUserQuestion(snap []byte) *AskUserQuestion {
	entry, ok := parseEntry(snap)
	if !ok || entry.Type != "assistant" || entry.Message == nil {
		return nil
	}
	for _, c := range entry.Message.Content {
		if c.Type != "tool_use" {
			continue
		}
		name, _ := c.Raw["name"].(string)
		if name != "AskUserQuestion" {
			continue
		}
		input, _ := c.Raw["input"].(map[string]any)
		if input == nil {
			return nil
		}
		rawQuestions, _ := input["questions"].([]any)
		if len(rawQuestions) == 0 {
			return nil
		}
		items := make([]AskUserQuestionItem, 0, len(rawQuestions))
		for _, q := range rawQuestions {
			qmap, ok := q.(map[string]any)
			if !ok {
				continue
			}
			item := AskUserQuestionItem{}
			item.Question, _ = qmap["question"].(string)
			item.Header, _ = qmap["header"].(string)
			item.MultiSelect, _ = qmap["multiSelect"].(bool)
			rawOptions, _ := qmap["options"].([]any)
			for _, o := range rawOptions {
				omap, ok := o.(map[string]any)
				if !ok {
					continue
				}
				opt := AskUserQuestionOption{}
				opt.Label, _ = omap["label"].(string)
				opt.Description, _ = omap["description"].(string)
				item.Options = append(item.Options, opt)
			}
			items = append(items, item)
		}
		if len(items) == 0 {
			return nil
		}
		return &AskUserQuestion{Questions: items}
	}
	return nil
}

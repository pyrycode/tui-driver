package tuidriver

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ModalContent is the neutral, screen-sourced value for a claude
// permission / trust-folder modal. The JSON tags ARE the push shape — no
// separate marshaling framework, same convention as AskUserQuestion / Agent.
// A consumer (the mobile-remote-head daemon) receives it as a typed value and
// never reads claude's screen text.
type ModalContent struct {
	Class   ModalClass    `json:"class"`   // the shipped DetectModalClass enum value
	Title   string        `json:"title"`   // modal header (per-class extraction)
	Prompt  string        `json:"prompt"`  // the question line
	Options []ModalOption `json:"options"` // render order, top-to-bottom
	Default int           `json:"default"` // 1-based index of the ❯-marked option; 0 = none
}

// ModalOption is one numbered choice in a ModalContent.
//
// Index is claude's rendered 1-based number — it is the keystroke the
// downstream answer child sends (Answer(strconv.Itoa(opt.Index))) and the
// security-relevant selector: the grant is chosen by the number claude
// renders. Route the answer on Index.
//
// Label is advisory display text (the "N." prefix and ❯ marker stripped). It
// is prompt-/tool-influenceable — a hostile prompt can make claude render
// arbitrary label text — so the consumer MUST NOT parse or route a decision on
// it; a spoofed label can mislead a human but cannot change which grant a
// given keystroke applies.
type ModalOption struct {
	Index int    `json:"index"`
	Label string `json:"label"`
}

// modalOptionRe matches one numbered option row in a Render-normalized modal
// line: an optional ❯ marker, the 1-based number, a dot, then the label. The
// "N." prefix and the ❯ marker fall outside the label capture so the label is
// already clean. Group 1 (the marker) marks the pre-selected default.
var modalOptionRe = regexp.MustCompile(`^(❯\s*)?(\d+)\.\s*(.+)$`)

// anchorTrustHeaderSpaced locates the trust-folder header line in Render
// output (cursor-forwards expanded back to spaces, so the space-stripped
// anchorTrustFolder from modal.go does not match the rendered line). Kept
// unexported per the package's screen-literal discipline.
var anchorTrustHeaderSpaced = []byte("Quick safety check")

// ParseModalContent extracts claude's permission / trust-folder modal from a
// PTY snapshot into a neutral typed value. Returns nil when the snapshot is
// not one of those two modals (idle screen, a different modal, or garbage),
// or when a detected modal yields no parseable option rows — no false
// positive.
//
// Screen-sourced sibling of ParsePicker, NOT of the JSONL extractors
// (ParseToolUse / ParseToolResult): permission and trust modals have zero
// JSONL footprint and never reach Events(). Classification reuses the shipped
// DetectModalClass; this adds the structured extraction on top.
func ParseModalContent(snap []byte) *ModalContent {
	class := DetectModalClass(snap)
	if class != ModalClassPermission && class != ModalClassTrustFolder {
		return nil
	}

	// Render (not StripANSI) so claude's `\x1b[1C` cursor-forwards expand
	// back to spaces ("2. Yes, allow…" not "2.Yes,allow…"). Normalize each
	// line by collapsing space runs and trimming — same prelude as
	// ParseAgentList / ParseMcpStatus.
	lines := modalLines(Render(snap, 0, 0))

	var mc *ModalContent
	switch class {
	case ModalClassPermission:
		mc = parsePermissionModal(lines)
	case ModalClassTrustFolder:
		mc = parseTrustModal(lines)
	}
	// A detected-but-unparseable modal yields nil, not a half-empty struct
	// (mirrors ParseAskUserQuestion's "nil if it would be empty" posture).
	if mc == nil || len(mc.Options) == 0 {
		return nil
	}
	return mc
}

// modalLines splits Render output into lines, collapsing internal space runs
// to one and trimming each — the shared normalization prelude.
func modalLines(text string) []string {
	raw := strings.Split(text, "\n")
	lines := make([]string, len(raw))
	for i, l := range raw {
		for strings.Contains(l, "  ") {
			l = strings.ReplaceAll(l, "  ", " ")
		}
		lines[i] = strings.TrimSpace(l)
	}
	return lines
}

// parsePermissionModal extracts a permission modal. Layout (post-Render):
//
//	────────────…────────────        ← separator (modal top anchor)
//	Bash command                     ← Title
//	ls /tmp                          ← action detail (not captured)
//	List files in /tmp               ← action detail (not captured)
//	Do you want to proceed?          ← Prompt
//	❯ 1. Yes                         ← option, ❯ → Default
//	  2. Yes, allow reading from tmp/ from this project
//	  3. No
//	(Esc to cancel · Tab to amend)   ← hint bar (skipped)
func parsePermissionModal(lines []string) *ModalContent {
	promptIdx := -1
	for i, line := range lines {
		if lineHasPermissionPrompt(line) {
			promptIdx = i
			break
		}
	}
	if promptIdx < 0 {
		return nil
	}

	// Title = first non-empty, non-option line after the last separator that
	// precedes the prompt. Requiring the separator means a layout without one
	// (the inline-packed Read variant) degrades to an empty title rather than
	// a wrong one.
	sepIdx := -1
	for i := 0; i < promptIdx; i++ {
		if isSeparatorLine(lines[i]) {
			sepIdx = i
		}
	}
	title := ""
	if sepIdx >= 0 {
		for i := sepIdx + 1; i < promptIdx; i++ {
			if lines[i] == "" || modalOptionRe.MatchString(lines[i]) {
				continue
			}
			title = lines[i]
			break
		}
	}

	opts, def := extractOptions(lines)
	return &ModalContent{
		Class:   ModalClassPermission,
		Title:   title,
		Prompt:  lines[promptIdx],
		Options: opts,
		Default: def,
	}
}

// parseTrustModal extracts a trust-folder modal. Layout (post-Render):
//
//	Quick safety check: Is this a project you created or one you trust?
//	❯ 1. Yes, I trust this folder
//	  2. No, …
//
// Title is the header up to the first ":"; Prompt is the remainder.
func parseTrustModal(lines []string) *ModalContent {
	headerIdx := -1
	for i, line := range lines {
		if bytes.Contains([]byte(line), anchorTrustHeaderSpaced) {
			headerIdx = i
			break
		}
	}
	if headerIdx < 0 {
		return nil
	}
	title, prompt := splitTrustHeader(lines[headerIdx])

	opts, def := extractOptions(lines)
	return &ModalContent{
		Class:   ModalClassTrustFolder,
		Title:   title,
		Prompt:  prompt,
		Options: opts,
		Default: def,
	}
}

// splitTrustHeader splits the trust header on the first ":" into title and
// prompt. With no ":", the whole line is the title and the prompt is empty.
func splitTrustHeader(line string) (title, prompt string) {
	if i := strings.IndexByte(line, ':'); i >= 0 {
		return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
	}
	return strings.TrimSpace(line), ""
}

// extractOptions scans lines for numbered option rows, returning them in
// render order plus the 1-based index of the ❯-marked default (0 = none).
func extractOptions(lines []string) (opts []ModalOption, def int) {
	for _, line := range lines {
		m := modalOptionRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		idx, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		opts = append(opts, ModalOption{Index: idx, Label: strings.TrimSpace(m[3])})
		if m[1] != "" {
			def = idx
		}
	}
	return opts, def
}

// lineHasPermissionPrompt reports whether line is the permission modal's
// proceed question, reusing the anchors from modal.go. The spaced anchor
// matches Render output; the stripped anchor is checked too for robustness.
func lineHasPermissionPrompt(line string) bool {
	b := []byte(line)
	return bytes.Contains(b, anchorPermissionSpaced) || bytes.Contains(b, anchorPermissionStripped)
}

// isSeparatorLine reports whether line is the modal's box-drawing separator: a
// run of ─ (U+2500) of at least 20 runes (claude renders ~80), matching the
// spike's `─{20,}` anchor.
func isSeparatorLine(line string) bool {
	if strings.Trim(line, "─") != "" {
		return false
	}
	return utf8.RuneCountInString(line) >= 20
}

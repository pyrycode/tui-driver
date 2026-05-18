// Spike: observational probe of claude's slash-command picker — a
// multiselect-style modal with a different PTY shape from the permission
// modal documented in spike #13. Loop 3 exp C-2 (2026-05-18).
//
// Sequence:
//
//	allocate PTY → spawn `claude` → wait for ❯ (idle)
//	→ handle trust-folder modal per policy (same shape as spike-one-turn)
//	→ write the trigger keystroke (default '/') without an Enter
//	→ sleep settleWindow for the picker to render fully
//	→ snapshot rolling buffer, write a binary dump to /tmp
//	→ log structural observations (line count, longest option, etc.)
//	→ ESC to dismiss the picker (no commitment to running anything)
//	→ shutdown
//
// The probe is intentionally minimal — purely observational. The output is
// the binary snapshot file + structural log lines. Library extraction will
// use these to design a multiselect-modal predicate + extractor (distinct
// from the permission-modal primitives, which assume 3 options + literal-
// text predicate).
//
// Spike-quality: single binary, no public API. Helpers copy-pasted from
// cmd/spike-one-turn (attributed inline) — kept here until library
// extraction lands.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pyrycode/tui-driver/pkg/tuidriver"
)

const (
	statePollInterval  = 50 * time.Millisecond
	watchdogTick       = 1 * time.Second
	ptyQuietLimit      = 30 * time.Second
	spinnerFreezeLimit = 30 * time.Second
	shutdownGrace      = 3 * time.Second

	// settleWindow: time to wait after sending the trigger keystroke before
	// snapshotting the picker. Claude's picker animates in (fade + slide),
	// so a too-short window misses the fully-rendered state. 1.5 s is plenty.
	settleWindow = 1500 * time.Millisecond
)

var oscRe = regexp.MustCompile(`\x1b\][^\x07]*\x07`)
var idleGlyph = []byte("\xe2\x9d\xaf")
var spinnerGlyph = []byte("\xe2\x9c\xbb")
var spinnerRe = regexp.MustCompile(`✻\s+(\S+(?:\s+\S+)?)\s+for\s+(?:(\d+)m\s+)?(\d+)s`)

// Picker-parser regexes (loop 4 D-1 unfiltered, D-2 filtered, 2026-05-18).
//
// Claude renders the `/` picker in two structurally different modes:
//
// UNFILTERED (user typed `/` only): each row is a single color block.
//   \x1b[38;5;<color>m/<cmd><CSI-fwd>[(<cat>) ]<desc>\x1b[39m
//   - color=153 (light blue) marks the HIGHLIGHTED row (default
//     selection — the one Enter would commit). color=246 (gray) marks
//     every other row.
//   - CSI cursor-forward (\x1b[<N>C) substitutes for spaces between
//     words (same root cause as spike #1 finding #8).
//
// FILTERED (user typed e.g. `/fi`): each row has MULTIPLE color
//   switches WITHIN it. Claude wraps each occurrence of the typed
//   filter substring in color 153 to highlight matching characters,
//   while the rest of the row stays in 246. So `/figma-use` with
//   filter `fi` renders as:
//   \x1b[38;5;246m/\x1b[38;5;153mfi\x1b[38;5;246mgma-use\x1b[39m
//   The whole-row highlight semantic is lost in filtered mode; claude
//   instead surfaces the SELECTED row's full description in a separate
//   right-column color-153 block. Default selection in filtered mode
//   is always the first match (claude convention).
//
// Parser strategy: walk the raw snapshot top-to-bottom, find each
// "item start" — `\x1b[38;5;{246|153}m/<letter>` — capture content
// until next item start OR end of buffer. Strip color codes (preserve
// adjacency), convert cursor-forward to single spaces. Highlight bit:
// unfiltered mode = items with 153 opening; filtered mode = first
// item (heuristic).
var pickerItemStartRe = regexp.MustCompile(
	// `\x1b[38;5;{246|153}m/` followed by either a letter (unfiltered or
	// no leading substring match) OR another color code (filtered mode
	// where the matched substring starts at position 1, e.g. `/figma-*`
	// filtered by `fi` renders as `\x1b[38;5;246m/\x1b[38;5;153mfi…`).
	// Requiring `/` + (color-code? + letter) covers both shapes without
	// false-positiving on description-embedded slashes (which never have
	// a `\x1b[38;5;{246|153}m` prefix).
	`\x1b\[38;5;(246|153)m/(?:\x1b\[38;5;\d+m)?[a-zA-Z]`,
)

var csiCursorFwdRe = regexp.MustCompile(`\x1b\[(\d+)C`)

// colorCodeRe matches the foreground-color SGR codes (open + close) we
// strip during command-name + description reconstruction. Cursor-forward
// (\x1b[NC) is handled separately because it's positional, not stylistic.
var colorCodeRe = regexp.MustCompile(`\x1b\[(?:38;5;\d+|39)m`)

// pickerCategoryRe matches the optional "(category)" prefix at the start
// of a description (e.g. "(figma) **MANDATORY...").
var pickerCategoryRe = regexp.MustCompile(`^\(([^)]+)\)\s*(.*)`)

func main() {
	triggerFlag := flag.String("trigger", "/",
		"keystroke(s) to trigger the picker/modal. '/' = slash-command picker (default), '@' = file/agent picker, '/mcp\\r' = MCP status modal, '/agents\\r' = agents modal, etc. Use printf '\\r' for CR.")
	trustFolderFlag := flag.String("trust-folder", "fail",
		"policy when claude's trust-folder dialog appears at idle: 'fail' (default — return clear error) or 'accept' (send `1\\r` to auto-trust this cwd, then proceed)")
	postKeysFlag := flag.String("post-trigger-keys", "",
		"comma-separated keys to send AFTER trigger and BEFORE snapshot: 'down' (\\x1b[B), 'up' (\\x1b[A), 'left' (\\x1b[D), 'right' (\\x1b[C). Each key gets a brief settle delay. Use for D-3 highlight-via-navigation probes.")
	settleFlag := flag.Duration("settle", 1500*time.Millisecond,
		"how long to wait after trigger (and any post-trigger keys) before snapshotting. Bump for modals whose state resolves over time (e.g. /mcp's MCP-server-connecting state).")
	flag.Parse()

	if *trustFolderFlag != "fail" && *trustFolderFlag != "accept" {
		fmt.Fprintf(os.Stderr, "invalid -trust-folder value %q (want 'fail' or 'accept')\n", *trustFolderFlag)
		os.Exit(2)
	}

	if err := run(*triggerFlag, *trustFolderFlag, *postKeysFlag, *settleFlag); err != nil {
		fmt.Fprintf(os.Stderr, "spike failed: %v\n", err)
		os.Exit(1)
	}
}

func run(trigger string, trustFolderPolicy string, postTriggerKeys string, settle time.Duration) error {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	startedAt := time.Now()
	logger.Printf("trigger=%q", trigger)

	rootCtx, cancelCause := context.WithCancelCause(context.Background())
	defer cancelCause(errors.New("run: returning"))

	rb := tuidriver.NewBuffer(0)
	tr := newTracker()
	tr.recordTransition("start")

	// No --session-id flag — we're not driving a turn, just observing UI.
	// No --permission-mode either; the picker is a UI affordance, doesn't
	// invoke tools.
	cmd := exec.Command("claude")
	tuidriver.EnsureClaudeEnv(cmd)

	ptmx, err := tuidriver.StartPTY(cmd)
	if err != nil {
		return fmt.Errorf("pty.Start: %w", err)
	}

	cmdExited := make(chan error, 1)
	go func() { cmdExited <- cmd.Wait() }()

	var wg sync.WaitGroup
	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			logger.Printf("shutdown-signalled")
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-cmdExited:
			case <-time.After(shutdownGrace):
				_ = cmd.Process.Signal(syscall.SIGKILL)
				<-cmdExited
			}
			_ = ptmx.Close()
			cancelCause(errors.New("shutdown"))
			wg.Wait()
		})
	}
	defer shutdown()

	// PTY reader.
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				rb.Append(chunk)
				_, _ = os.Stderr.Write(chunk)
			}
			if rerr != nil {
				return
			}
		}
	}()

	// Watchdog: PTY-quiet + spinner-freeze. No wall-cap (per loop 2 wrap-up).
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(watchdogTick)
		defer ticker.Stop()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-ticker.C:
				snap := rb.Snapshot()
				stripped := tuidriver.StripANSI(snap)
				_, total, ok := matchSpinner(stripped)
				tr.observeSpinner(ok, total)
				if werr := tr.checkWatchdog(rb); werr != nil {
					logger.Printf("%v", werr)
					cancelCause(werr)
					return
				}
			}
		}
	}()

	// Wait for idle.
	if err := waitUntil(rootCtx, func() bool { return isIdle(rb.Snapshot()) }); err != nil {
		return fmt.Errorf("wait idle: %w", err)
	}
	tr.recordTransition("idle-detected")
	logger.Printf("idle-detected")

	// Trust-folder dialog handling. Same shape as spike-one-turn.
	if tuidriver.HasTrustModal(rb.Snapshot()) {
		switch trustFolderPolicy {
		case "fail":
			return fmt.Errorf("claude shows the trust-folder dialog — this cwd hasn't been trusted yet. Run `claude` interactively in this directory once, accept trust, exit, then re-run the spike. Or pass `-trust-folder accept` to auto-trust")
		case "accept":
			if _, err := ptmx.Write([]byte("1\r")); err != nil {
				return fmt.Errorf("write trust-accept keystroke: %w", err)
			}
			tr.recordTransition("trust-folder-accepted")
			logger.Printf("trust-folder-accepted bytes=31 0d")
			if err := waitUntil(rootCtx, func() bool {
				snap := rb.Snapshot()
				return !tuidriver.HasTrustModal(snap) && isIdle(snap)
			}); err != nil {
				return fmt.Errorf("wait for idle post-trust-accept: %w", err)
			}
			tr.recordTransition("idle-detected-post-trust")
			logger.Printf("idle-detected-post-trust")
		}
	}

	// Send the trigger keystroke (no Enter — we want the picker to OPEN,
	// not commit a command). Claude's TUI uses the keystroke to begin
	// pre-fix matching against available commands and renders the picker.
	if _, err := ptmx.Write([]byte(trigger)); err != nil {
		return fmt.Errorf("write trigger: %w", err)
	}
	tr.recordTransition("trigger-sent")
	logger.Printf("trigger-sent bytes=%x", []byte(trigger))

	// Settle window: let the picker/modal fully render. Configurable via
	// -settle flag (default 1.5s) — bump for modals whose state resolves
	// over time (e.g. /mcp's MCP-server-connecting state).
	select {
	case <-rootCtx.Done():
		return context.Cause(rootCtx)
	case <-time.After(settle):
	}

	// Loop 4 D-3: post-trigger navigation keys. Send each, settle briefly,
	// then proceed to snapshot. Snapshot captures the state AFTER all keys
	// land — sufficient for "did the highlight move?" verification. To
	// capture intermediate states, run the spike multiple times with
	// progressively-longer key sequences.
	if postTriggerKeys != "" {
		keys := strings.Split(postTriggerKeys, ",")
		for _, k := range keys {
			k = strings.TrimSpace(k)
			var keyBytes []byte
			switch k {
			case "down":
				keyBytes = []byte{0x1b, 0x5b, 0x42} // \x1b[B
			case "up":
				keyBytes = []byte{0x1b, 0x5b, 0x41} // \x1b[A
			case "left":
				keyBytes = []byte{0x1b, 0x5b, 0x44} // \x1b[D
			case "right":
				keyBytes = []byte{0x1b, 0x5b, 0x43} // \x1b[C
			default:
				return fmt.Errorf("unknown post-trigger key %q (want down/up/left/right)", k)
			}
			if _, err := ptmx.Write(keyBytes); err != nil {
				return fmt.Errorf("write %q: %w", k, err)
			}
			tr.recordTransition("post-trigger-key-" + k)
			logger.Printf("post-trigger-key-sent name=%s bytes=%x", k, keyBytes)
			// Brief settle between keys — claude redraws the highlighted row.
			select {
			case <-rootCtx.Done():
				return context.Cause(rootCtx)
			case <-time.After(300 * time.Millisecond):
			}
		}
	}

	// Snapshot. Dump raw bytes to /tmp for byte-level inspection;
	// emit structural observations to the state log.
	snap := rb.Snapshot()
	stripped := oscRe.ReplaceAll(tuidriver.StripANSI(snap), nil)
	dumpPath := fmt.Sprintf("/tmp/spike-multiselect-bytes-%d.bin", time.Now().UnixNano())
	if err := os.WriteFile(dumpPath, snap, 0o644); err != nil {
		logger.Printf("warning: write snapshot: %v", err)
	}
	tr.recordTransition("picker-snapshot")
	logger.Printf("picker-snapshot path=%s raw_len=%d stripped_len=%d", dumpPath, len(snap), len(stripped))

	// Structural metrics — what's in the snapshot?
	// 1. Box-drawing chars present? (Picker is likely rendered as a box.)
	hasBox := bytes.ContainsAny(stripped, "╭╮╰╯│─┌┐└┘├┤")
	// 2. Line count after stripping.
	lines := bytes.Split(stripped, []byte("\n"))
	// 3. Longest non-empty line (probably picker width).
	var longest int
	for _, l := range lines {
		if len(l) > longest {
			longest = len(l)
		}
	}
	// 4. Numbered options? Look for "1." through "9." patterns.
	var numberedOptions []int
	for digit := 1; digit <= 9; digit++ {
		if bytes.Contains(stripped, []byte(fmt.Sprintf("%d.", digit))) {
			numberedOptions = append(numberedOptions, digit)
		}
	}
	// 5. Slash-prefix-command lines? Count lines starting with `/`.
	var slashLines int
	for _, l := range lines {
		trimmed := bytes.TrimLeft(l, " \t")
		if len(trimmed) > 0 && trimmed[0] == '/' {
			slashLines++
		}
	}

	logger.Printf("picker-shape has_box_drawing=%v line_count=%d longest_line=%d numbered_options=%v slash_command_lines=%d",
		hasBox, len(lines), longest, numberedOptions, slashLines)

	// Loop 6 F-1: dispatch to the right parser based on detected modal class.
	// All three classes (slash-picker, /mcp, /agents) parse into structured
	// JSON that pyry acp can forward to a mobile host UI.
	modalClass := detectModalClass(snap)
	logger.Printf("modal-class detected=%s", modalClass)

	jsonPath := strings.TrimSuffix(dumpPath, ".bin") + ".parsed.json"
	switch modalClass {
	case "slash-picker":
		items := parsePickerItems(snap)
		logger.Printf("picker-parsed item_count=%d", len(items))
		for i, it := range items {
			marker := " "
			if it.Highlighted {
				marker = "*"
			}
			cat := ""
			if it.Category != "" {
				cat = "[" + it.Category + "] "
			}
			desc := it.Description
			if len(desc) > 80 {
				desc = desc[:80] + "…"
			}
			logger.Printf("picker-item[%02d]%s %s %s%s", i, marker, it.Command, cat, desc)
		}
		if len(items) > 0 {
			jsonBytes, _ := json.MarshalIndent(items, "", "  ")
			if err := os.WriteFile(jsonPath, jsonBytes, 0o644); err != nil {
				logger.Printf("warning: write json: %v", err)
			} else {
				logger.Printf("parsed-json path=%s", jsonPath)
			}
		}
	case "mcp":
		mcp := parseMcpStatus(snap)
		logger.Printf("mcp-parsed total_servers=%d category_count=%d", mcp.TotalServers, len(mcp.Categories))
		for _, g := range mcp.Categories {
			logger.Printf("mcp-category name=%q path=%q server_count=%d", g.Name, g.Path, len(g.Servers))
			for _, s := range g.Servers {
				marker := " "
				if s.Highlighted {
					marker = "*"
				}
				toolStr := ""
				if s.ToolCount > 0 {
					toolStr = fmt.Sprintf(" (%d tools)", s.ToolCount)
				}
				logger.Printf("mcp-server%s %s — %s%s", marker, s.Name, s.Status, toolStr)
			}
		}
		jsonBytes, _ := json.MarshalIndent(mcp, "", "  ")
		if err := os.WriteFile(jsonPath, jsonBytes, 0o644); err != nil {
			logger.Printf("warning: write json: %v", err)
		} else {
			logger.Printf("parsed-json path=%s", jsonPath)
		}
	case "agents":
		al := parseAgentList(snap)
		logger.Printf("agents-parsed current_tab=%q item_count=%d empty=%q",
			al.CurrentTab, len(al.Items), al.EmptyText)
		for i, a := range al.Items {
			marker := " "
			if a.Highlighted {
				marker = "*"
			}
			logger.Printf("agents-item[%02d]%s %s", i, marker, a.Name)
		}
		jsonBytes, _ := json.MarshalIndent(al, "", "  ")
		if err := os.WriteFile(jsonPath, jsonBytes, 0o644); err != nil {
			logger.Printf("warning: write json: %v", err)
		} else {
			logger.Printf("parsed-json path=%s", jsonPath)
		}
	default:
		logger.Printf("no-parser-for-modal-class class=%s", modalClass)
	}

	// Look for some specific picker hints we might recognize.
	hints := []string{
		"to apply", "to select", "to navigate", "Esc to", "esc to",
		"Tab to", "tab to", "Enter to", "enter to",
	}
	var foundHints []string
	for _, h := range hints {
		if bytes.Contains(stripped, []byte(h)) {
			foundHints = append(foundHints, h)
		}
	}
	if len(foundHints) > 0 {
		logger.Printf("picker-hints found=%v", foundHints)
	}

	// Send ESC to dismiss the picker — no commitment to running anything.
	if _, err := ptmx.Write([]byte{0x1b}); err != nil {
		return fmt.Errorf("write esc: %w", err)
	}
	tr.recordTransition("picker-dismissed")
	logger.Printf("picker-dismissed bytes=1b")

	// Brief verification window: confirm picker is gone (we want to know
	// the dismiss keystroke works and how long it takes).
	dismissDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(dismissDeadline) {
		if isIdle(rb.Snapshot()) {
			logger.Printf("idle-detected-post-dismiss elapsed=%s", time.Since(dismissDeadline.Add(-3*time.Second)).Round(time.Millisecond))
			break
		}
		select {
		case <-rootCtx.Done():
			return context.Cause(rootCtx)
		case <-time.After(100 * time.Millisecond):
		}
	}

	logger.Printf("complete elapsed=%s", time.Since(startedAt).Round(time.Millisecond))
	fmt.Printf("OBSERVED: picker snapshot at %s\n", dumpPath)
	return nil
}

// --- shared primitives copied from cmd/spike-one-turn / cmd/spike-permission ---

type tracker struct {
	mu                    sync.Mutex
	currentState          string
	lastTransitionAt      time.Time
	lastSpinnerProgressAt time.Time
	lastSpinnerTotal      int
	spinnerActive         bool
}

func newTracker() *tracker { return &tracker{} }

func (t *tracker) recordTransition(state string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.currentState = state
	t.lastTransitionAt = time.Now()
}

func (t *tracker) observeSpinner(visible bool, totalSeconds int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if !visible {
		t.spinnerActive = false
		t.lastSpinnerTotal = 0
		return
	}
	if !t.spinnerActive {
		t.spinnerActive = true
		t.lastSpinnerTotal = totalSeconds
		t.lastSpinnerProgressAt = now
		return
	}
	if totalSeconds > t.lastSpinnerTotal {
		t.lastSpinnerTotal = totalSeconds
		t.lastSpinnerProgressAt = now
	}
}

func (t *tracker) checkWatchdog(rb *tuidriver.Buffer) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if quiet := rb.QuietFor(); quiet > ptyQuietLimit {
		return fmt.Errorf("watchdog: PTY quiet for %s (last state: %s)",
			quiet.Round(time.Second), t.currentState)
	}
	if t.spinnerActive && now.Sub(t.lastSpinnerProgressAt) > spinnerFreezeLimit {
		return fmt.Errorf("watchdog: spinner counter frozen at %ds for %s",
			t.lastSpinnerTotal, now.Sub(t.lastSpinnerProgressAt).Round(time.Second))
	}
	return nil
}

func waitUntil(ctx context.Context, predicate func() bool) error {
	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()
	for {
		if predicate() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func isIdle(snap []byte) bool {
	stripped := tuidriver.StripANSI(snap)
	if !bytes.Contains(stripped, idleGlyph) {
		return false
	}
	if bytes.Contains(stripped, spinnerGlyph) {
		return false
	}
	return true
}

func matchSpinner(stripped []byte) (verb string, totalSeconds int, ok bool) {
	m := spinnerRe.FindSubmatch(stripped)
	if m == nil {
		return "", 0, false
	}
	verb = string(m[1])
	if len(m[3]) > 0 {
		fmt.Sscanf(string(m[3]), "%d", &totalSeconds)
		if len(m[2]) > 0 {
			var minutes int
			fmt.Sscanf(string(m[2]), "%d", &minutes)
			totalSeconds += minutes * 60
		}
	}
	return verb, totalSeconds, true
}

// PickerItem represents one row of claude's `/` slash-command picker as
// extracted from PTY bytes. Loop 4 D-1 deliverable shape for `pyry acp`
// to forward to a mobile host UI.
type PickerItem struct {
	Command     string `json:"command"`
	Category    string `json:"category,omitempty"`
	Description string `json:"description"`
	Highlighted bool   `json:"highlighted"`
}

// McpStatus represents the parsed /mcp modal contents. Loop 6 F-1.
type McpStatus struct {
	TotalServers int        `json:"total_servers"`
	Categories   []McpGroup `json:"categories"`
}

type McpGroup struct {
	Name    string      `json:"name"`           // "Project MCPs", "User MCPs", "Built-in MCPs"
	Path    string      `json:"path,omitempty"` // path/parenthetical context
	Servers []McpServer `json:"servers"`
}

type McpServer struct {
	Name        string `json:"name"`
	Status      string `json:"status"`               // "connecting", "connected", "disabled", "auth_required", "failed"
	ToolCount   int    `json:"tool_count,omitempty"` // when status=connected
	Highlighted bool   `json:"highlighted"`          // ❯ marker
}

// AgentList represents the parsed /agents modal contents. Loop 6 F-1.
type AgentList struct {
	Tabs       []string `json:"tabs"`        // ["Running", "Library"]
	CurrentTab string   `json:"current_tab"` // active tab name
	Items      []Agent  `json:"items"`       // items in the current tab
	EmptyText  string   `json:"empty_text,omitempty"`
}

type Agent struct {
	Name        string `json:"name"`
	Highlighted bool   `json:"highlighted"`
}

// parsePickerItems extracts structured items from a raw PTY snapshot of
// claude's `/` slash-command picker. Returns items in render order
// (top-to-bottom). Handles both UNFILTERED and FILTERED modes (see
// pickerItemStartRe comment for the structural difference).
//
// Items past the visible-window cutoff in the 4096-byte rolling buffer
// won't appear; picker scrolling requires a follow-up snapshot after
// arrow-down keys (D-3 territory).
func parsePickerItems(snap []byte) []PickerItem {
	// Strip OSC sequences first (window title, etc. — irrelevant noise).
	cleaned := oscRe.ReplaceAll(snap, nil)

	// Find all item starts. Each start is the byte offset of \x1b[38;5;<color>m/
	starts := pickerItemStartRe.FindAllSubmatchIndex(cleaned, -1)
	if len(starts) == 0 {
		return nil
	}

	// Detect mode: filtered mode has lots of mid-row 153 substring highlights;
	// unfiltered has only one 153 — the highlighted row. Heuristic: count how
	// many of the item-start matches have color=153. In unfiltered, 0 or 1.
	// In filtered, every item-start tends to be 246 (substring highlights are
	// MID-name, not at item-start). The simpler signal: if the FIRST item's
	// start is color 246 AND it contains a 153 segment within (filtered mode
	// indicator), treat as filtered.
	highlightedCount153 := 0
	for _, m := range starts {
		// The color is captured group 1 — its bytes are at m[2]:m[3]
		color := string(cleaned[m[2]:m[3]])
		if color == "153" {
			highlightedCount153++
		}
	}

	var items []PickerItem
	for i, m := range starts {
		// Determine the byte range for this item: from m[0] to EITHER the
		// next item's start OR the first newline (whichever is sooner).
		// Capping at the first newline prevents the right-pane (selected-
		// item full description, color 153) from being absorbed into the
		// LAST left-column item's range — left-column items are single-line.
		itemStart := m[0]
		var itemEnd int
		if i+1 < len(starts) {
			itemEnd = starts[i+1][0]
		} else {
			itemEnd = len(cleaned)
		}
		// Trim to first \r or \n (single-line per item in the left column).
		if nl := bytes.IndexAny(cleaned[itemStart:itemEnd], "\r\n"); nl >= 0 {
			itemEnd = itemStart + nl
		}
		raw := cleaned[itemStart:itemEnd]

		// Opening color of this item.
		openColor := string(cleaned[m[2]:m[3]])

		// Reconstruct command + description text:
		// 1. Strip ALL color codes (preserve adjacency).
		// 2. Convert cursor-forward to single space.
		// 3. Normalize whitespace.
		text := colorCodeRe.ReplaceAllString(string(raw), "")
		text = csiCursorFwdRe.ReplaceAllString(text, " ")
		text = tuidriver.StripANSIString(text)
		for strings.Contains(text, "  ") {
			text = strings.ReplaceAll(text, "  ", " ")
		}
		text = strings.TrimSpace(text)

		// Parse "/<cmd> <rest>" — split on first whitespace after the slash-word.
		if !strings.HasPrefix(text, "/") {
			continue
		}
		spaceIdx := strings.IndexAny(text, " \t")
		var cmd, rest string
		if spaceIdx < 0 {
			cmd, rest = text, ""
		} else {
			cmd, rest = text[:spaceIdx], strings.TrimSpace(text[spaceIdx+1:])
		}

		// Peel off "(category) " prefix.
		var category, description string
		if catMatch := pickerCategoryRe.FindStringSubmatch(rest); catMatch != nil {
			category = catMatch[1]
			description = catMatch[2]
		} else {
			description = rest
		}

		// Highlight detection:
		//   - UNFILTERED mode: exactly one row OPENS with 153 (the
		//     highlighted/default-selected row). highlightedCount153 == 1.
		//   - FILTERED mode: every row opens with 246 in the left column;
		//     the right pane shows the full description of the selected
		//     item in its own 153 block (not at any item-start). So
		//     highlightedCount153 == 0 among item-starts. Default-selected
		//     in filtered mode is the FIRST item (claude convention).
		var highlighted bool
		if highlightedCount153 >= 1 {
			highlighted = openColor == "153"
		} else {
			highlighted = i == 0
		}

		items = append(items, PickerItem{
			Command:     cmd,
			Category:    category,
			Description: description,
			Highlighted: highlighted,
		})
	}
	return items
}

// detectModalClass returns the modal currently rendered in the snapshot.
// Anchors per class (loop 6 F-1):
//   - "ManageMCPservers"  → /mcp status modal
//   - "Agents" header + (Running OR Library tabs) → /agents modal
//   - "?forshortcuts"  → / slash-command picker (unfiltered or filtered)
//   - "Entertoselect"  → AskUserQuestion modal (loop 5 E-1)
//   - "Quicksafetycheck" → trust-folder modal (loop 2 B-5)
//   - "Doyouwanttoproceed" → permission modal (spike #13)
//
// Each anchor is unique to its class (verified empirically). Returns
// "unknown" if no anchor matches.
func detectModalClass(snap []byte) string {
	stripped := oscRe.ReplaceAll(tuidriver.StripANSI(snap), nil)
	switch {
	case bytes.Contains(stripped, []byte("ManageMCPservers")):
		return "mcp"
	case bytes.Contains(stripped, []byte("Agents")) &&
		(bytes.Contains(stripped, []byte("Running")) || bytes.Contains(stripped, []byte("Library"))):
		return "agents"
	case bytes.Contains(stripped, []byte("?forshortcuts")) ||
		bytes.Contains(stripped, []byte("? for shortcuts")):
		return "slash-picker"
	case bytes.Contains(stripped, []byte("Entertoselect")) ||
		bytes.Contains(stripped, []byte("Enter to select")):
		return "ask-user-question"
	case bytes.Contains(stripped, []byte("Quicksafetycheck")):
		return "trust-folder"
	case bytes.Contains(stripped, []byte("Doyouwanttoproceed")) ||
		bytes.Contains(stripped, []byte("Do you want to proceed")):
		return "permission"
	default:
		return "unknown"
	}
}

// parseMcpStatus extracts /mcp modal contents into structured McpStatus.
//
// Layout observed (loop 6 F-1):
//
//	Manage MCP servers
//	N servers                                ← total count
//	Project MCPs (/path/to/.mcp.json)        ← category header
//	❯ <name> · <status indicator>            ← ❯ marker = highlighted
//	  <name> · <status indicator>
//	User MCPs (/path/to/.claude.json)        ← next category
//	  <name> · <status indicator>
//	Built-in MCPs (always available)
//	  <name> · <status indicator>
//	https://...                              ← footer URL
//	↑/↓ to navigate · Enter to confirm · Esc to cancel
//
// Status indicator vocabulary:
//
//	◯ connecting…    → status="connecting"
//	◯ disabled       → status="disabled"
//	✔ed · N tools    → status="connected", tool_count=N
//	△ needs auth...  → status="auth_required"
//	✘ failed         → status="failed"
//
// Uses tuidriver.Render (vt10x-backed) to interpret cursor-positioning
// escapes faithfully. The previous regex-strip path produced truncated
// names like "claude.ai Gmail" → "laude.ai Gmail" because cursor-forward
// and cursor-up sequences were collapsed to single spaces, losing
// adjacency information across rows.
func parseMcpStatus(snap []byte) *McpStatus {
	text := tuidriver.Render(snap, 120, 40)
	lines := strings.Split(text, "\n")
	for i := range lines {
		l := lines[i]
		for strings.Contains(l, "  ") {
			l = strings.ReplaceAll(l, "  ", " ")
		}
		lines[i] = strings.TrimSpace(l)
	}

	status := &McpStatus{}
	var currentGroup *McpGroup

	// Track which categories we've already added so re-renders (same /mcp
	// modal redrawn after MCP servers finish connecting) don't duplicate.
	seenCategory := map[string]bool{}

	for _, line := range lines {
		if line == "" {
			continue
		}
		// Skip hint-bar lines (contain navigation hints) and URL footers.
		if strings.Contains(line, "↑/↓") || strings.Contains(line, "Esc to cancel") ||
			strings.HasPrefix(line, "https://") {
			continue
		}
		// Total-server count line: "N servers" or "N server". Allow no-space
		// variants from cursor-forward stripping (e.g. "10servers"). Match
		// last-wins so the final count after re-renders is captured.
		if m := regexp.MustCompile(`^(\d+)\s*servers?$`).FindStringSubmatch(line); m != nil {
			fmt.Sscanf(m[1], "%d", &status.TotalServers)
			continue
		}
		// Category header: "Project MCPs ..." / "User MCPs ..." / "Built-in MCPs ..."
		// Tolerate missing space between word and "MCPs" (CSI cursor-forward
		// rendering may produce "ProjectMCPs" with no separator). Dedup by
		// canonical name across re-renders.
		if m := regexp.MustCompile(`^(Project|User|Built-in)\s*MCPs\s*(.*)$`).FindStringSubmatch(line); m != nil {
			canonical := m[1] + " MCPs"
			if !seenCategory[canonical] {
				seenCategory[canonical] = true
				status.Categories = append(status.Categories, McpGroup{Name: canonical, Path: strings.TrimSpace(m[2])})
			}
			// Either way, point currentGroup at this canonical category so
			// subsequent items get attributed correctly.
			for i := range status.Categories {
				if status.Categories[i].Name == canonical {
					currentGroup = &status.Categories[i]
					break
				}
			}
			continue
		}
		// Item line: optional ❯ marker, name, ·, status. Detect ·.
		if strings.Contains(line, "·") && currentGroup != nil {
			highlighted := strings.HasPrefix(line, "❯")
			body := strings.TrimPrefix(line, "❯")
			body = strings.TrimSpace(body)
			// Split on first ·
			idx := strings.Index(body, "·")
			if idx < 0 {
				continue
			}
			name := strings.TrimSpace(body[:idx])
			statusPart := strings.TrimSpace(body[idx+len("·"):])

			srv := McpServer{Name: name, Highlighted: highlighted}
			switch {
			case strings.Contains(statusPart, "connecting"):
				srv.Status = "connecting"
			case strings.Contains(statusPart, "disabled"):
				srv.Status = "disabled"
			case strings.Contains(statusPart, "✔"):
				srv.Status = "connected"
				if tm := regexp.MustCompile(`(\d+)\s*tools?`).FindStringSubmatch(statusPart); tm != nil {
					fmt.Sscanf(tm[1], "%d", &srv.ToolCount)
				}
			case strings.Contains(statusPart, "△"):
				srv.Status = "auth_required"
			case strings.Contains(statusPart, "✘"):
				srv.Status = "failed"
			default:
				srv.Status = "unknown"
			}
			// Skip placeholder/garbage rows: empty name, or name that's
			// actually a status indicator (e.g. "✔ed" standalone — claude
			// sometimes re-renders just the status string for a server
			// whose name has already been written further up).
			if srv.Name == "" {
				continue
			}
			if strings.HasPrefix(srv.Name, "✔") || strings.HasPrefix(srv.Name, "✘") ||
				strings.HasPrefix(srv.Name, "△") || strings.HasPrefix(srv.Name, "◯") {
				continue
			}
			// Dedupe by name within the current group: re-renders may emit
			// the same server multiple times with progressively-resolved
			// status. Keep the last observation (most-resolved state).
			replaced := false
			for i := range currentGroup.Servers {
				if currentGroup.Servers[i].Name == srv.Name {
					currentGroup.Servers[i] = srv
					replaced = true
					break
				}
			}
			if !replaced {
				currentGroup.Servers = append(currentGroup.Servers, srv)
			}
		}
	}
	return status
}

// parseAgentList extracts /agents modal contents into structured AgentList.
// The modal has a tabbed shape with Running / Library tabs; this parser
// captures the currently-visible tab's items + tab structure.
//
// Layout observed (loop 6 F-1):
//
//	Agents  Running   Library                  ← tab bar
//	No subagents are currently running.        ← empty-state for Running tab
//	←/→ to switch · ↑/↓ to navigate · Enter to select · Esc to close
func parseAgentList(snap []byte) *AgentList {
	text := tuidriver.Render(snap, 120, 40)
	lines := strings.Split(text, "\n")
	for i := range lines {
		l := lines[i]
		for strings.Contains(l, "  ") {
			l = strings.ReplaceAll(l, "  ", " ")
		}
		lines[i] = strings.TrimSpace(l)
	}

	agents := &AgentList{Tabs: []string{"Running", "Library"}}

	for _, line := range lines {
		if line == "" {
			continue
		}
		// Tab bar: contains "Agents" + Running/Library (selected one
		// rendered with different ANSI in raw but normalized here).
		if strings.HasPrefix(line, "Agents") && (strings.Contains(line, "Running") || strings.Contains(line, "Library")) {
			// Default heuristic: the tab listed first after "Agents " is
			// the current one. Empirically the rendered ordering puts the
			// current tab adjacent to the header.
			if strings.Index(line, "Running") < strings.Index(line, "Library") {
				agents.CurrentTab = "Running"
			} else {
				agents.CurrentTab = "Library"
			}
			continue
		}
		// Empty-state text for the Running tab.
		if strings.Contains(line, "subagents") && strings.Contains(line, "running") {
			agents.EmptyText = line
			continue
		}
	}
	return agents
}

// Compile-time references so we keep parity with the rest of the spike suite.
var (
	_ = filepath.Join
	_ = strings.Repeat
)

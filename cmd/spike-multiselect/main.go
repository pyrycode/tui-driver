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
	"regexp"
	"strings"
	"sync"
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

var spinnerRe = regexp.MustCompile(`✻\s+(\S+(?:\s+\S+)?)\s+for\s+(?:(\d+)m\s+)?(\d+)s`)

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

	tr := tuidriver.NewTracker(tuidriver.TrackerOpts{
		PTYQuietLimit:      ptyQuietLimit,
		SpinnerFreezeLimit: spinnerFreezeLimit,
	})
	tr.RecordTransition("start")

	// No --session-id flag — we're not driving a turn, just observing UI.
	// No --permission-mode either; the picker is a UI affordance, doesn't
	// invoke tools.
	cmd := exec.Command("claude")
	tuidriver.EnsureClaudeEnv(cmd)

	session, err := tuidriver.Spawn(cmd, tuidriver.SpawnOpts{
		Mirror:        os.Stderr,
		ShutdownGrace: shutdownGrace,
	})
	if err != nil {
		return fmt.Errorf("pty.Start: %w", err)
	}
	defer func() {
		logger.Printf("shutdown-signalled")
		_ = session.Close()
		cancelCause(errors.New("shutdown"))
	}()
	rb := session.Buffer
	ptmx := session.PTY

	var wg sync.WaitGroup

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
				tr.ObserveSpinner(ok, total)
				if werr := tr.CheckWatchdog(rb); werr != nil {
					logger.Printf("%v", werr)
					cancelCause(werr)
					return
				}
			}
		}
	}()

	// Wait for idle.
	if err := tuidriver.WaitUntil(rootCtx, func() bool { return tuidriver.IsIdle(rb.Snapshot()) }); err != nil {
		return fmt.Errorf("wait idle: %w", err)
	}
	tr.RecordTransition("idle-detected")
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
			tr.RecordTransition("trust-folder-accepted")
			logger.Printf("trust-folder-accepted bytes=31 0d")
			if err := tuidriver.WaitUntil(rootCtx, func() bool {
				snap := rb.Snapshot()
				return !tuidriver.HasTrustModal(snap) && tuidriver.IsIdle(snap)
			}); err != nil {
				return fmt.Errorf("wait for idle post-trust-accept: %w", err)
			}
			tr.RecordTransition("idle-detected-post-trust")
			logger.Printf("idle-detected-post-trust")
		}
	}

	// Send the trigger keystroke (no Enter — we want the picker to OPEN,
	// not commit a command). Claude's TUI uses the keystroke to begin
	// pre-fix matching against available commands and renders the picker.
	if _, err := ptmx.Write([]byte(trigger)); err != nil {
		return fmt.Errorf("write trigger: %w", err)
	}
	tr.RecordTransition("trigger-sent")
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
			tr.RecordTransition("post-trigger-key-" + k)
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
	stripped := tuidriver.StripOSC(tuidriver.StripANSI(snap))
	dumpPath := fmt.Sprintf("/tmp/spike-multiselect-bytes-%d.bin", time.Now().UnixNano())
	if err := os.WriteFile(dumpPath, snap, 0o644); err != nil {
		logger.Printf("warning: write snapshot: %v", err)
	}
	tr.RecordTransition("picker-snapshot")
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
	modalClass := tuidriver.DetectModalClass(snap)
	logger.Printf("modal-class detected=%s", modalClass)

	jsonPath := strings.TrimSuffix(dumpPath, ".bin") + ".parsed.json"
	switch modalClass {
	case tuidriver.ModalClassSlashPicker:
		items := tuidriver.ParsePicker(snap)
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
	case tuidriver.ModalClassMCP:
		mcp := tuidriver.ParseMcpStatus(snap)
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
	case tuidriver.ModalClassAgents:
		al := tuidriver.ParseAgentList(snap)
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
	tr.RecordTransition("picker-dismissed")
	logger.Printf("picker-dismissed bytes=1b")

	// Brief verification window: confirm picker is gone (we want to know
	// the dismiss keystroke works and how long it takes).
	dismissDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(dismissDeadline) {
		if tuidriver.IsIdle(rb.Snapshot()) {
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


// Spike: observe what happens when claude invokes the `AskUserQuestion`
// tool mid-turn — a claude-initiated multiselect that pauses the turn
// pending user input. Loop 5 E-1 (2026-05-18).
//
// JSONL gives us structured data directly:
//
//   {"name": "AskUserQuestion", "input": {"questions": [{
//     "question": "...", "header": "...", "multiSelect": bool,
//     "options": [{"label": "...", "description": "..."}, ...]
//   }]}}
//
// For mobile UX, JSONL is the authoritative source — no PTY parsing
// needed for the data. This probe captures BOTH the JSONL shape AND
// the PTY rendering so we know:
//  1. How to detect AskUserQuestion via JSONL (look for tool_use with
//     name == "AskUserQuestion")
//  2. How the TUI renders the modal (so we know how to dismiss / answer
//     it via the PTY when the mobile user picks an option)
//
// Probe flow:
//   spawn claude → wait for idle → handle trust modal
//   → type prompt that triggers AskUserQuestion + CR
//   → tail JSONL until we see assistant(tool_use name=AskUserQuestion)
//   → wait briefly for the PTY rendering to settle
//   → snapshot PTY + dump structured JSONL question
//   → send `1\r` to answer "first option" (same keystroke as permission
//     modal — claude likely shares this convention)
//   → wait for end_turn to confirm the answer landed
//
// Spike-quality: single binary, no public API. Copied scaffolding
// inline.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pyrycode/tui-driver/pkg/tuidriver"
)

const (
	sessionFileWait = 10 * time.Second
	// AskUserQuestion modals render then claude goes QUIET waiting for
	// user input. The PTY-heartbeat semantic ("no bytes = wedge") wrongly
	// fires in this state. Bumped to 120 s so the probe can capture the
	// modal AND send the answer keystroke before quiet trips. Library
	// extraction needs to detect AskUserQuestion modal-active state and
	// suspend the heartbeat watchdog — see new finding #25.
	ptyQuietLimit      = 120 * time.Second
	spinnerFreezeLimit = 30 * time.Second
	shutdownGrace      = 3 * time.Second

	defaultPrompt = "Use the AskUserQuestion tool to ask me which of three programming languages I should learn next: Rust, Zig, or Go. Then wait for my answer before doing anything else."

	askUserQuestionLimit = 60 * time.Second
	postAnswerLimit      = 60 * time.Second
	settleWindow         = 1500 * time.Millisecond
)

var oscRe = regexp.MustCompile(`\x1b\][^\x07]*\x07`)

func main() {
	promptFlag := flag.String("prompt", defaultPrompt, "prompt to send that should trigger AskUserQuestion")
	trustFolderFlag := flag.String("trust-folder", "fail",
		"policy when claude's trust-folder dialog appears at idle: 'fail' (default) or 'accept'")
	answerFlag := flag.String("answer", "1",
		"which option to pick when the modal appears: digit (1, 2, 3) sent with CR. Use 'dismiss' to send ESC instead.")
	flag.Parse()

	if *trustFolderFlag != "fail" && *trustFolderFlag != "accept" {
		fmt.Fprintf(os.Stderr, "invalid -trust-folder value %q\n", *trustFolderFlag)
		os.Exit(2)
	}

	if err := run(*promptFlag, *trustFolderFlag, *answerFlag); err != nil {
		fmt.Fprintf(os.Stderr, "spike failed: %v\n", err)
		os.Exit(1)
	}
}

func run(prompt, trustFolderPolicy, answer string) error {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	startedAt := time.Now()

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve cwd: %w", err)
	}
	sessionID := uuid.NewString()
	jsonlPath := tuidriver.SessionJSONLPath(home, cwd, sessionID)
	logger.Printf("session-id-resolved id=%s jsonl=%s", sessionID, jsonlPath)

	rootCtx, cancelCause := context.WithCancelCause(context.Background())
	defer cancelCause(errors.New("run: returning"))

	tr := tuidriver.NewTracker(tuidriver.TrackerOpts{
		PTYQuietLimit:      ptyQuietLimit,
		SpinnerFreezeLimit: spinnerFreezeLimit,
	})
	tr.RecordTransition("start")

	cmd := exec.Command("claude", "--session-id", sessionID)
	tuidriver.EnsureClaudeEnv(cmd)

	session, err := tuidriver.Spawn(cmd, tuidriver.SpawnOpts{
		MirrorStderr:  true,
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
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := session.RunWatchdog(rootCtx, tr, tuidriver.WatchdogOpts{}); err != nil {
			logger.Printf("%v", err)
			cancelCause(err)
		}
	}()

	if err := tuidriver.WaitUntil(rootCtx, func() bool { return tuidriver.IsIdle(session.Snapshot()) }); err != nil {
		return fmt.Errorf("wait idle: %w", err)
	}
	tr.RecordTransition("idle-detected")
	logger.Printf("idle-detected")

	if tuidriver.HasTrustModal(session.Snapshot()) {
		switch trustFolderPolicy {
		case "fail":
			return fmt.Errorf("claude shows the trust-folder dialog — pass `-trust-folder accept`")
		case "accept":
			if err := session.AcceptTrust(); err != nil {
				return fmt.Errorf("write trust-accept: %w", err)
			}
			tr.RecordTransition("trust-folder-accepted")
			logger.Printf("trust-folder-accepted")
			if err := tuidriver.WaitUntil(rootCtx, func() bool {
				snap := session.Snapshot()
				return !tuidriver.HasTrustModal(snap) && tuidriver.IsIdle(snap)
			}); err != nil {
				return fmt.Errorf("wait idle post-trust: %w", err)
			}
		}
	}

	// Send prompt char-by-char with a trailing `\r`. Bulk-writing
	// `prompt+"\r"` (the original shape) was observed to drop the submit
	// keystroke against claude 2.1.148 — the input handler swallows `\r`
	// when it arrives in the same buffered write as a long prompt body,
	// per the empirical finding documented in docs/knowledge/codebase/9.md.
	// Matches the typePrompt convention used by every other spike.
	if err := session.ClearInputLine(); err != nil {
		return fmt.Errorf("clear input line: %w", err)
	}
	if err := session.TypePrompt(prompt); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	tr.RecordTransition("prompt-written")
	logger.Printf("prompt-written prompt=%q", prompt)

	// Tail JSONL from start; the deterministic --session-id path means we
	// know exactly where to look.
	jsonlCtx, jsonlCancel := context.WithTimeout(rootCtx, sessionFileWait)
	jsonlErr := tuidriver.WaitForSessionJSONL(jsonlCtx, jsonlPath)
	jsonlCancel()
	if jsonlErr != nil {
		return fmt.Errorf("open session jsonl: %w", jsonlErr)
	}
	logger.Printf("session-jsonl-opened path=%s", jsonlPath)

	eventCh, terr := tuidriver.TailJSONL(rootCtx, jsonlPath, 0)
	if terr != nil {
		return fmt.Errorf("open events stream: %w", terr)
	}

	// Detect AskUserQuestion modal via PTY (zero JSONL footprint pre-answer,
	// same as permission modal per finding #15). The modal's unique marker
	// is the hint bar: "Enter to select · ↑/↓ to navigate · Esc to cancel".
	// We anchor on a non-space-stripped fragment that's robust across both
	// CSI-cursor-forward and literal-space rendering paths.
	deadline := time.Now().Add(askUserQuestionLimit)
	for !hasAskUserModal(session.Snapshot()) {
		select {
		case <-rootCtx.Done():
			return context.Cause(rootCtx)
		case <-time.After(500 * time.Millisecond):
			if time.Now().After(deadline) {
				return fmt.Errorf("watchdog: AskUserQuestion modal not seen within %s", askUserQuestionLimit)
			}
		}
	}
	tr.RecordTransition("ask-user-modal-detected")
	logger.Printf("ask-user-modal-detected")

	// Let the PTY rendering settle so we snapshot a complete modal.
	select {
	case <-rootCtx.Done():
		return context.Cause(rootCtx)
	case <-time.After(settleWindow):
	}

	// Snapshot PTY for byte-level inspection.
	snap := session.Snapshot()
	dumpPath := fmt.Sprintf("/tmp/spike-ask-user-bytes-%d.bin", time.Now().UnixNano())
	if err := os.WriteFile(dumpPath, snap, 0o644); err != nil {
		logger.Printf("warning: write snapshot: %v", err)
	}
	stripped := oscRe.ReplaceAll(tuidriver.StripANSI(snap), nil)
	tr.RecordTransition("modal-snapshot")
	logger.Printf("modal-snapshot path=%s raw_len=%d stripped_len=%d", dumpPath, len(snap), len(stripped))

	// Look for shape hints in the rendered modal.
	hasBox := bytes.ContainsAny(stripped, "╭╮╰╯│─┌┐└┘├┤")
	hasQuestionText := bytes.Contains(stripped, []byte("language")) || bytes.Contains(stripped, []byte("Rust")) || bytes.Contains(stripped, []byte("Zig"))
	var numberedOptions []int
	for digit := 1; digit <= 9; digit++ {
		if bytes.Contains(stripped, []byte(fmt.Sprintf("%d.", digit))) {
			numberedOptions = append(numberedOptions, digit)
		}
	}
	logger.Printf("modal-shape has_box_drawing=%v has_question_text=%v numbered_options=%v",
		hasBox, hasQuestionText, numberedOptions)

	// Send the answer keystroke.
	var answerBytes []byte
	switch answer {
	case "dismiss":
		answerBytes = []byte{0x1b}
	default:
		// Assume digit + CR — same shape as permission modal answers.
		answerBytes = []byte(answer + "\r")
	}
	if err := session.SendKeys(string(answerBytes)); err != nil {
		return fmt.Errorf("write answer: %w", err)
	}
	tr.RecordTransition("answer-sent")
	logger.Printf("answer-sent bytes=%x kind=%q", answerBytes, answer)

	// Wait for end_turn — confirms the answer landed and claude finished.
	endDeadline := time.Now().Add(postAnswerLimit)
	var gotEnd bool
	for !gotEnd {
		select {
		case <-rootCtx.Done():
			return context.Cause(rootCtx)
		case ev, ok := <-eventCh:
			if !ok {
				eventCh = nil
				continue
			}
			if ev.Type != "assistant" {
				continue
			}
			if tuidriver.IsEndTurn(ev) {
				gotEnd = true
				tr.RecordTransition("end-turn-detected")
				logger.Printf("end-turn-detected")
			}
		case <-time.After(500 * time.Millisecond):
			if time.Now().After(endDeadline) {
				return fmt.Errorf("watchdog: end_turn not seen within %s after answer", postAnswerLimit)
			}
		}
	}

	logger.Printf("complete elapsed=%s", time.Since(startedAt).Round(time.Millisecond))
	fmt.Printf("OBSERVED: AskUserQuestion question + options structure logged; PTY snapshot at %s\n", dumpPath)
	return nil
}

// extractAskUserQuestion returns the first AskUserQuestion tool_use input
// from a JSONL assistant event, or nil if not present.
func extractAskUserQuestion(ev map[string]any) map[string]any {
	msg, _ := ev["message"].(map[string]any)
	content, _ := msg["content"].([]any)
	for _, c := range content {
		block, _ := c.(map[string]any)
		if block["type"] != "tool_use" {
			continue
		}
		if block["name"] != "AskUserQuestion" {
			continue
		}
		input, _ := block["input"].(map[string]any)
		return input
	}
	return nil
}

// hasAskUserModal: claude's AskUserQuestion tool renders an interactive
// modal in the TUI with a unique hint-bar at the bottom:
// "Enter to select · ↑/↓ to navigate · Esc to cancel". This phrase
// doesn't appear in any other observed UI state, so it's a clean anchor.
// Stripping ANSI handles both space-stripped and space-preserved render
// variants (the hint-bar literals don't contain CSI cursor-forward in
// the renderings observed so far, but matching with-spaces is safer).
func hasAskUserModal(snap []byte) bool {
	stripped := tuidriver.StripANSI(snap)
	return bytes.Contains(stripped, []byte("Enter to select")) ||
		bytes.Contains(stripped, []byte("Entertoselect"))
}


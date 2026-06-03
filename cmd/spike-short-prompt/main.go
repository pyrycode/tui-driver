// Spike: drive one interactive `claude` turn end-to-end through DeliverPrompt
// using a SHORT, single-line prompt — the human-typed-message shape.
//
// Regression rig for the short-prompt stall (tui-driver short-prompt fix).
// A short single-line prompt renders inline with no "[Pasted text]" chip, so
// DeliverPrompt's chip-based recovery cannot tell a genuinely-uncommitted
// paste from a committed-but-slow one. Before the fix, delivering such a
// prompt via a bracketed paste intermittently fails to commit and the run
// goes idle until the watchdog kills it. After the fix, DeliverPrompt routes
// short single-line prompts through TypePrompt (byte-spaced body + isolated
// \r), which commits reliably.
//
// Sequence mirrors cmd/spike-one-turn/main.go. The two intentional deviations:
//   - The prompt is delivered via session.DeliverPrompt (the deliver-confirm-
//     recover loop under test), NOT raw SendKeys — SendKeys bypasses the bug.
//   - A bounded turn-deadline classifies the outcome: if no assistant end_turn
//     lands within turnDeadline, the prompt wedged and the spike fails with a
//     STALL message instead of waiting for the global watchdog.
//
// See cmd/spike-short-prompt/README.md for the empirical log.
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
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pyrycode/tui-driver/pkg/tuidriver"
)

const (
	statePollInterval  = 50 * time.Millisecond
	ptyQuietLimit      = 60 * time.Second
	spinnerFreezeLimit = 30 * time.Second
	shutdownGrace      = 3 * time.Second

	// ptyQuietWindow: see cmd/spike-cancel/main.go:81-90 for the empirical
	// derivation; reused unchanged by cmd/spike-one-turn.
	ptyQuietWindow = 1500 * time.Millisecond

	// turnDeadline bounds how long the spike waits for the delivered prompt to
	// produce an assistant end_turn. A healthy short turn on haiku/low commits
	// and answers in well under this; exceeding it means the prompt never
	// committed — the stall this spike exists to catch. Kept under the 60s
	// PTY-quiet watchdog so the spike reports STALL itself, with a clear
	// message, rather than surfacing as an opaque watchdog kill.
	turnDeadline = 25 * time.Second

	// promptText is short and single-line on purpose: this is exactly the
	// shape that renders inline with no paste chip and triggers the stall.
	promptText = "What is 2+2?"
)

func main() {
	sessionIDFlag := flag.String("session-id", "", "UUID to pin claude's session ID and JSONL filename (default: generate one)")
	trustFolderFlag := flag.String("trust-folder", "fail",
		"policy when claude's trust-folder dialog appears at idle: 'fail' (default — return clear error) or 'accept' (send `1\\r` to auto-trust this cwd, then proceed)")
	flag.Parse()

	if *trustFolderFlag != "fail" && *trustFolderFlag != "accept" {
		fmt.Fprintf(os.Stderr, "invalid -trust-folder value %q (want 'fail' or 'accept')\n", *trustFolderFlag)
		os.Exit(2)
	}

	if err := run(*sessionIDFlag, *trustFolderFlag); err != nil {
		fmt.Fprintf(os.Stderr, "spike failed: %v\n", err)
		os.Exit(1)
	}
}

func run(sessionIDFlag string, trustFolderPolicy string) error {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	startedAt := time.Now()

	sessionID, err := resolveSession(sessionIDFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		flag.Usage()
		os.Exit(2)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve cwd: %w", err)
	}
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

	// Watchdog: 1 Hz inactivity + spinner-freeze enforcement.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := session.RunWatchdog(rootCtx, tr, tuidriver.WatchdogOpts{}); err != nil {
			logger.Printf("%v", err)
			cancelCause(err)
		}
	}()

	// --- linear state machine ---

	if err := tuidriver.WaitUntil(rootCtx, func() bool {
		return tuidriver.IsIdle(session.Snapshot())
	}); err != nil {
		return fmt.Errorf("wait idle: %w", err)
	}
	tr.RecordTransition("idle-detected")
	logger.Printf("idle-detected")

	if tuidriver.HasTrustModal(session.Snapshot()) {
		switch trustFolderPolicy {
		case "fail":
			return fmt.Errorf("claude shows the trust-folder dialog — this cwd hasn't been trusted yet. Run `claude` interactively in this directory once, accept trust, exit, then re-run the spike. Or pass `-trust-folder accept` to auto-trust")
		case "accept":
			if err := session.AcceptTrust(); err != nil {
				return fmt.Errorf("write trust-accept keystroke: %w", err)
			}
			tr.RecordTransition("trust-folder-accepted")
			logger.Printf("trust-folder-accepted")
			if err := tuidriver.WaitUntil(rootCtx, func() bool {
				snap := session.Snapshot()
				return !tuidriver.HasTrustModal(snap) && tuidriver.IsIdle(snap)
			}); err != nil {
				return fmt.Errorf("wait for idle post-trust-accept: %w", err)
			}
			tr.RecordTransition("idle-detected-post-trust")
			logger.Printf("idle-detected-post-trust")
		}
	}

	// Deliver the short prompt through the function under test. DeliverPrompt
	// chooses the wire method (TypePrompt for short single-line prompts after
	// the fix; bracketed paste before it), polls for a commit signal, and
	// recovers a corrupted paste. JSONLPath lets it use the per-session JSONL
	// appearing as a commit signal alongside the thinking spinner.
	res, err := session.DeliverPrompt(rootCtx, tuidriver.DeliverOpts{
		Prompt:    promptText,
		JSONLPath: jsonlPath,
	})
	if err != nil {
		return fmt.Errorf("deliver prompt: %w", err)
	}
	tr.RecordTransition("prompt-delivered")
	logger.Printf("prompt-delivered committed=%v attempts=%d", res.Committed, res.Attempts)

	// Bound the wait for a real turn so a wedge surfaces as a clear STALL well
	// before the 60s PTY-quiet watchdog would fire.
	turnCtx, turnCancel := context.WithTimeout(rootCtx, turnDeadline)
	defer turnCancel()

	// The session JSONL is created only once claude actually receives input. A
	// prompt that never commits never produces it — so its non-appearance
	// within turnDeadline is itself a stall signal.
	if err := tuidriver.WaitForSessionJSONL(turnCtx, jsonlPath); err != nil {
		return fmt.Errorf("STALL: session jsonl never appeared within %s — prompt did not commit (committed=%v attempts=%d): %w",
			turnDeadline, res.Committed, res.Attempts, err)
	}
	logger.Printf("session-jsonl-opened path=%s offset=0", jsonlPath)

	eventCh, err := tuidriver.TailJSONL(rootCtx, jsonlPath, 0)
	if err != nil {
		return fmt.Errorf("open events stream: %w", err)
	}

	// Turn-complete is the same three-clause conjunction spike-one-turn uses:
	// (a) JSONL assistant end_turn, (b) ❯ idle glyph present, (c) PTY quiet for
	// ptyQuietWindow. See cmd/spike-one-turn/main.go:185-227 for why PTY-
	// quiescence replaces a spinner-absence clause.
	var (
		gotEndTurn    bool
		assistantText string
	)

	check := func() bool {
		if !gotEndTurn {
			return false
		}
		stripped := tuidriver.StripANSI(session.Snapshot())
		if !bytes.Contains(stripped, tuidriver.IdleGlyph) {
			return false
		}
		return session.QuietFor() >= ptyQuietWindow
	}

	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()
	for !check() {
		select {
		case <-turnCtx.Done():
			return fmt.Errorf("STALL: no assistant end_turn within %s — short prompt wedged (committed=%v attempts=%d)",
				turnDeadline, res.Committed, res.Attempts)
		case <-rootCtx.Done():
			return fmt.Errorf("wait termination: %w", context.Cause(rootCtx))
		case ev, ok := <-eventCh:
			if !ok {
				eventCh = nil
				continue
			}
			if !gotEndTurn && tuidriver.IsEndTurn(ev) {
				assistantText = tuidriver.AssistantText(ev)
				gotEndTurn = true
				tr.RecordTransition("end-turn-detected")
				logger.Printf("end-turn-detected")
			}
		case <-ticker.C:
		}
	}

	tr.RecordTransition("assistant-text-extracted")
	logger.Printf("assistant-text-extracted len=%d", len(assistantText))

	fmt.Printf("SUCCESS: %s\n", assistantText)

	logger.Printf("complete elapsed=%s", time.Since(startedAt).Round(time.Millisecond))
	return nil
}

// resolveSession turns the operator-supplied flag into a normalised session ID.
// Empty flag → generate a fresh v4 UUID; non-empty → must parse as a valid
// UUID (uuid.Parse accepts hyphenated and non-hyphenated forms; .String()
// re-emits the canonical lowercase-hyphenated shape that matches claude's
// filename convention).
func resolveSession(flagValue string) (string, error) {
	if flagValue == "" {
		u, err := uuid.NewRandom()
		if err != nil {
			return "", fmt.Errorf("generate session id: %w", err)
		}
		return u.String(), nil
	}
	u, err := uuid.Parse(flagValue)
	if err != nil {
		return "", fmt.Errorf("invalid --session-id: %w", err)
	}
	return u.String(), nil
}

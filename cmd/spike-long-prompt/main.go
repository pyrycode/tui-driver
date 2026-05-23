// Spike: drive one interactive `claude` turn end-to-end through a PTY using a
// multi-KB multi-line prompt sent via Session.WritePrompt.
//
// Regression rig for the bracketed-paste path shipped in PR #43. Three drift
// vectors this binary catches:
//
//  1. tui-driver's bracketed-paste wire shape diverges from what claude expects.
//  2. claude's auto-paste-detection heuristic changes.
//  3. claude truncates / mis-receives a multi-KB multi-line prompt in a way
//     the existing single-line spikes do not surface.
//
// Sequence mirrors cmd/spike-one-turn/main.go. The two intentional deviations:
//   - The prompt body is loaded from the embedded fixture testdata/long-prompt.txt.
//   - The prompt is sent via session.WritePrompt, NOT raw ptmx.Write — that
//     is the whole point of the regression rig.
//
// Plus a substring assertion: the assistant's end-of-turn text (trimmed) must
// contain "ALPHA_42-GAMMA_88-OMEGA_13", confirming that all three token
// markers embedded in the fixture survived the bracketed-paste round-trip.
//
// See cmd/spike-long-prompt/README.md for the empirical log.
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pyrycode/tui-driver/pkg/tuidriver"
)

const (
	sessionFileWait    = 10 * time.Second
	ptyQuietLimit      = 60 * time.Second
	spinnerFreezeLimit = 30 * time.Second
	shutdownGrace      = 3 * time.Second

	// expectedTokens is the substring the assistant response (after
	// TrimSpace) must contain. The three fragments map 1:1 onto the
	// [TOKEN_START / TOKEN_MIDDLE / TOKEN_END] markers in the fixture; if
	// the bracketed-paste round-trip drops any of them, claude has nothing
	// to join and the substring search fails.
	expectedTokens = "ALPHA_42-GAMMA_88-OMEGA_13"
)

//go:embed testdata/long-prompt.txt
var promptFixture string

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

	// Strip a single optional trailing newline from the fixture. UNIX text
	// files conventionally end with \n; we do not want that newline inside
	// the bracketed-paste body. Interior newlines, leading whitespace, and
	// indentation are intentional test surface — they stay.
	promptBody := strings.TrimRight(promptFixture, "\n")

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

	// Watchdog: 1 Hz inactivity + spinner-freeze enforcement.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := tuidriver.RunWatchdog(rootCtx, rb, tr, tuidriver.WatchdogOpts{}); err != nil {
			logger.Printf("%v", err)
			cancelCause(err)
		}
	}()

	// --- linear state machine ---

	if err := tuidriver.WaitUntil(rootCtx, func() bool {
		return tuidriver.IsIdle(rb.Snapshot())
	}); err != nil {
		return fmt.Errorf("wait idle: %w", err)
	}
	tr.RecordTransition("idle-detected")
	logger.Printf("idle-detected")

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
			// Wait for claude to dismiss the modal and return to true idle.
			// HasTrustModal must be false (modal text is gone) AND isIdle
			// must be true (❯ visible + no spinner).
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

	if err := session.WritePrompt(promptBody); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	tr.RecordTransition("prompt-written")
	logger.Printf("prompt-written")

	// Wait for the deterministic session JSONL to exist. Interactive claude
	// under --session-id defers JSONL creation until first input is received
	// (verified empirically; documented in spike-one-turn README finding #9),
	// so we poll AFTER prompt-written rather than after idle. The file is
	// brand new in this flow — tail from offset 0 and let the library's
	// per-entry discriminator filter out non-end_turn lines.
	jsonlCtx, jsonlCancel := context.WithTimeout(rootCtx, sessionFileWait)
	jsonlErr := tuidriver.WaitForSessionJSONL(jsonlCtx, jsonlPath)
	jsonlCancel()
	if jsonlErr != nil {
		return fmt.Errorf("open session jsonl: %w", jsonlErr)
	}
	logger.Printf("session-jsonl-opened path=%s offset=0", jsonlPath)

	events, err := session.Events(rootCtx, jsonlPath, 0)
	if err != nil {
		return fmt.Errorf("open events stream: %w", err)
	}

	// Terminate on JSONL end_turn. Spinner observation is opportunistic: the
	// slow path emits thinking-detected → spinner-gone; the fast path skips
	// both (trivial prompts can resolve before the spinner renders, and even
	// when it does render, the spike-one-turn finding #8 currently blocks
	// regex match). The thinkingObserved && !spinnerGone guard preserves
	// slow-path log order.
	var (
		thinkingObserved bool
		thinkingVerb     string
		spinnerGone      bool
		gotEndTurn       bool
		assistantText    string
	)
	for ev := range events {
		switch ev.Kind {
		case tuidriver.EventKindPtyThinking:
			if thinkingObserved {
				continue
			}
			// The library's PtyThinking event fires on ✻ glyph presence;
			// the spike gates `thinkingObserved` on ParseSpinner matching
			// the verb. Reason: the rolling buffer (4 KB) retains the ✻
			// bytes past the visual end-of-spinner, so the library's
			// IsIdle predicate (✻-absent) often never flips back to true
			// for short turns. Gating on ParseSpinner match preserves the
			// pre-refactor fast-path skip-through (gotEndTurn &&
			// !thinkingObserved) when ParseSpinner misses (the documented
			// class-C case; spike-one-turn finding #8).
			v, _, ok := tuidriver.ParseSpinner(rb.Snapshot())
			if !ok {
				continue
			}
			thinkingVerb = v
			thinkingObserved = true
			tr.RecordTransition("thinking-detected")
			logger.Printf("thinking-detected verb=%q", thinkingVerb)
		case tuidriver.EventKindPtyIdle:
			// The merge loop is start-blind, so an already-idle buffer at
			// subscription fires this on tick one. Gate on thinkingObserved
			// to ignore that pre-thinking idle event.
			if !thinkingObserved || spinnerGone {
				continue
			}
			spinnerGone = true
			tr.RecordTransition("spinner-gone")
			logger.Printf("spinner-gone")
		case tuidriver.EventKindJsonlEndOfTurn:
			if gotEndTurn {
				continue
			}
			assistantText = tuidriver.AssistantText(ev.Entry)
			gotEndTurn = true
			tr.RecordTransition("end-turn-detected")
			logger.Printf("end-turn-detected")
		}
		if gotEndTurn && (!thinkingObserved || spinnerGone) {
			break
		}
	}

	if rootCtx.Err() != nil {
		return fmt.Errorf("wait termination: %w", context.Cause(rootCtx))
	}

	tr.RecordTransition("assistant-text-extracted")
	logger.Printf("assistant-text-extracted len=%d", len(assistantText))

	trimmed := strings.TrimSpace(assistantText)
	if !strings.Contains(trimmed, expectedTokens) {
		return fmt.Errorf("FAIL: substring %q not found in %q", expectedTokens, trimmed)
	}

	fmt.Printf("SUCCESS: %s\n", trimmed)

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

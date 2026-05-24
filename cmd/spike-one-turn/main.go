// Spike: drive one interactive `claude` turn end-to-end through a PTY.
//
// Sequence:
//
//	allocate PTY → spawn `claude` → wait for ❯ (idle)
//	→ open the session JSONL claude already created at startup and record its size
//	→ write "What is 2+2?\r"
//	→ start JSONL tailer (seek to recorded offset, then tail appended lines)
//	→ wait for assistant event with stop_reason=="end_turn"
//	  AND ❯ idle glyph visible in the rolling buffer
//	  AND PTY quiet for ptyQuietWindow (1.5 s) — see #97 rationale
//	→ concatenate every content[].text on that record → SUCCESS: <text>
//	→ SIGTERM (3 s grace) → SIGKILL → close PTY → wg.Wait
//
// This file is spike-quality: single binary, no public API. The reusable
// primitives are deliberately not extracted yet — they settle after the spike
// surfaces friction. See cmd/spike-one-turn/README.md for the empirical log.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pyrycode/tui-driver/pkg/tuidriver"
)

const (
	statePollInterval  = 50 * time.Millisecond
	jsonlTailInterval  = 50 * time.Millisecond
	sessionFileWait    = 10 * time.Second
	sessionFilePoll    = 100 * time.Millisecond
	watchdogTick       = 1 * time.Second
	ptyQuietLimit      = 60 * time.Second
	spinnerFreezeLimit = 30 * time.Second
	shutdownGrace      = 3 * time.Second

	// ptyQuietWindow: see cmd/spike-cancel/main.go:81-90 for the empirical
	// derivation. Reused unchanged across cmd/spike-multi-turn (#73),
	// cmd/spike-permission (#70), and cmd/spike-cancel itself (#11/#69).
	ptyQuietWindow = 1500 * time.Millisecond

	promptText = "What is 2+2?\r"
)

// spinnerRe matches the thinking indicator. The verb (group 1) is 1–2 words and
// varies per prompt — capture it for the empirical log; do NOT depend on any
// specific value. Time-tail is `Ns` or `Nm Ns` (groups 2,3).
var spinnerRe = regexp.MustCompile(`✻\s+(\S+(?:\s+\S+)?)\s+for\s+(?:(\d+)m\s+)?(\d+)s`)

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

	projDir, err := projectsDir()
	if err != nil {
		return fmt.Errorf("resolve projects dir: %w", err)
	}
	logger.Printf("projects-dir path=%s", projDir)

	sessionID, jsonlPath, err := resolveSession(sessionIDFlag, projDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		flag.Usage()
		os.Exit(2)
	}
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
		ticker := time.NewTicker(watchdogTick)
		defer ticker.Stop()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-ticker.C:
				snap := rb.Snapshot()
				stripped := tuidriver.StripANSI(snap)
				verb, total, ok := matchSpinner(stripped)
				_ = verb
				tr.ObserveSpinner(ok, total)
				if werr := tr.CheckWatchdog(rb); werr != nil {
					logger.Printf("%v", werr)
					cancelCause(werr)
					return
				}
			}
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

	if _, err := ptmx.Write([]byte(promptText)); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	tr.RecordTransition("prompt-written")
	logger.Printf("prompt-written")

	// Wait for the deterministic session JSONL to exist. Interactive claude
	// under --session-id defers JSONL creation until first input is received
	// (verified empirically; documented in README finding #9), so we poll
	// AFTER prompt-written rather than after idle. The file is brand new in
	// this flow — tail from offset 0 and let the parser filter skip startup
	// envelopes the same way it does for non-assistant events.
	if err := openSessionJSONL(jsonlPath); err != nil {
		return fmt.Errorf("open session jsonl: %w", err)
	}
	logger.Printf("session-jsonl-opened path=%s offset=0", jsonlPath)

	eventCh := make(chan map[string]any, 32)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if terr := tailJSONL(rootCtx, logger, jsonlPath, 0, eventCh); terr != nil {
			if !errors.Is(terr, context.Canceled) {
				logger.Printf("jsonl-tailer-error err=%v", terr)
				cancelCause(terr)
			}
		}
	}()

	// Wait for turn complete via a three-clause conjunction: (a) JSONL has
	// surfaced an assistant message with stop_reason=end_turn, (b) the ❯
	// idle glyph is present in the stripped rolling buffer, AND (c) the PTY
	// has been quiet for at least ptyQuietWindow. All three are load-
	// bearing — JSONL end_turn says "model done speaking"; ❯-present says
	// "input prompt is on screen"; PTY-quiescence says "claude has stopped
	// redrawing."
	//
	// We deliberately do NOT use tuidriver.IsIdle here. IsIdle's spinner-
	// absent half is exactly what wedges this spike on claude 2.1.150:
	// a stuck `✻ <verb> for Ns` glyph stays painted in the 4 KB rolling
	// buffer (pkg/tuidriver/buffer.go:8-13) because post-end_turn claude
	// doesn't emit enough bytes to roll it past the cap. Any predicate
	// composed against "spinner absent" wedges the same way — the previous
	// `gotEndTurn && (!thinkingObserved || spinnerGone)` shape that lived
	// here was exactly that, and #97 documents the 1/5 failure rate it
	// produced before this swap. PTY-quiescence subsumes the safety
	// property the spinner-absent clause was meant to provide: when claude
	// has emitted zero bytes for 1.5 s it has by definition stopped
	// redrawing the spinner (or anything else), so it is safe to act on
	// the JSONL-side end_turn. PTY-quiescence is strictly stronger than a
	// glyph-absence-on-buffer debounce on the rendering-activity axis —
	// 1500 ms of zero bytes cannot be faked by a transient buffer state.
	//
	// Same predicate shape as cmd/spike-multi-turn/main.go:391-400, which
	// adopted PTY-quiescence in #73 / PR #74 against the identical stuck-
	// glyph-in-rolling-buffer failure mode (see cmd/spike-cancel/main.go:
	// 81-90 for the empirical derivation of the 1500 ms window).
	var (
		gotEndTurn    bool
		assistantText string
	)

	check := func() bool {
		if !gotEndTurn {
			return false
		}
		stripped := tuidriver.StripANSI(rb.Snapshot())
		if !bytes.Contains(stripped, tuidriver.IdleGlyph) {
			return false
		}
		return rb.QuietFor() >= ptyQuietWindow
	}

	ticker := time.NewTicker(statePollInterval)
	defer ticker.Stop()
	for !check() {
		select {
		case <-rootCtx.Done():
			return fmt.Errorf("wait termination: %w", context.Cause(rootCtx))
		case ev := <-eventCh:
			if !gotEndTurn && isEndTurn(ev) {
				assistantText = extractAssistantText(ev)
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

// --- pattern matching ---

func matchSpinner(stripped []byte) (verb string, totalSeconds int, ok bool) {
	m := spinnerRe.FindSubmatch(stripped)
	if m == nil {
		return "", 0, false
	}
	var minutes int
	if len(m[2]) > 0 {
		minutes, _ = strconv.Atoi(string(m[2]))
	}
	seconds, _ := strconv.Atoi(string(m[3]))
	return string(m[1]), minutes*60 + seconds, true
}



// --- JSONL discovery + tailing ---

// projectsDir resolves $HOME/.claude/projects/<encoded-cwd>/ at runtime.
// The encoding is byte-by-byte: '/', '.', and ' ' become '-', everything else
// passes through. Adjacent chars therefore produce '--' (non-reversible).
// Space mapping confirmed empirically against vault cwd `Second Brain`
// landing at `...-Second-Brain` in claude's projects dir (2026-05-17).
func projectsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects", tuidriver.EncodeCwd(cwd)), nil
}

// resolveSession turns the operator-supplied flag into a normalised session ID
// and the deterministic JSONL path. Empty flag → generate a fresh v4 UUID;
// non-empty → must parse as a valid UUID (uuid.Parse accepts hyphenated and
// non-hyphenated forms; .String() re-emits the canonical lowercase-hyphenated
// shape that matches claude's filename convention).
//
// The function does not stat jsonlPath — the discovery loop owns that.
func resolveSession(flagValue string, dir string) (sessionID string, jsonlPath string, err error) {
	if flagValue == "" {
		u, gerr := uuid.NewRandom()
		if gerr != nil {
			return "", "", fmt.Errorf("generate session id: %w", gerr)
		}
		sessionID = u.String()
	} else {
		u, perr := uuid.Parse(flagValue)
		if perr != nil {
			return "", "", fmt.Errorf("invalid --session-id: %w", perr)
		}
		sessionID = u.String()
	}
	jsonlPath = filepath.Join(dir, sessionID+".jsonl")
	return sessionID, jsonlPath, nil
}

// openSessionJSONL polls the deterministic JSONL path until it appears. The
// path was pinned up-front via --session-id, so there is no directory scan
// and no mtime heuristic — pre-existing stale .jsonl files in the same
// directory are structurally invisible. Interactive claude defers JSONL
// creation until first input (see README finding #9), so callers should wait
// until after prompt-written before invoking this.
func openSessionJSONL(jsonlPath string) error {
	deadline := time.Now().Add(sessionFileWait)
	for {
		_, err := os.Stat(jsonlPath)
		if err == nil {
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat session jsonl: %w", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("session JSONL did not appear at %s within %s", jsonlPath, sessionFileWait)
		}
		time.Sleep(sessionFilePoll)
	}
}

func tailJSONL(
	ctx context.Context,
	logger *log.Logger,
	path string,
	startOffset int64,
	out chan<- map[string]any,
) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open session jsonl: %w", err)
	}
	defer f.Close()

	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return fmt.Errorf("seek session jsonl: %w", err)
	}

	reader := bufio.NewReader(f)
	var partial []byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, rerr := reader.ReadBytes('\n')
		if len(chunk) > 0 {
			partial = append(partial, chunk...)
		}
		if rerr == nil {
			line := bytes.TrimRight(partial, "\r\n")
			partial = partial[:0]
			if len(line) == 0 {
				continue
			}
			var obj map[string]any
			if jerr := json.Unmarshal(line, &obj); jerr != nil {
				logger.Printf("jsonl-parse-warning err=%v", jerr)
				continue
			}
			// Filter: only assistant events with a message object can carry
			// stop_reason. Everything else (permission-mode, file-history-snapshot,
			// attachment, ai-title, system, last-prompt, user, and future
			// envelopes) is silently ignored.
			if _, ok := obj["message"].(map[string]any); !ok {
				continue
			}
			if t, _ := obj["type"].(string); t != "assistant" {
				continue
			}
			select {
			case out <- obj:
			case <-ctx.Done():
				return ctx.Err()
			}
		} else if rerr == io.EOF {
			// EOF with no newline → keep the partial bytes and wait for more.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(jsonlTailInterval):
			}
		} else {
			return fmt.Errorf("read session jsonl: %w", rerr)
		}
	}
}

// --- assistant-event helpers ---

func isEndTurn(ev map[string]any) bool {
	msg, _ := ev["message"].(map[string]any)
	stop, _ := msg["stop_reason"].(string)
	return stop == "end_turn"
}

func extractAssistantText(ev map[string]any) string {
	msg, _ := ev["message"].(map[string]any)
	content, _ := msg["content"].([]any)
	var b strings.Builder
	for _, c := range content {
		cm, _ := c.(map[string]any)
		if t, _ := cm["type"].(string); t != "text" {
			continue
		}
		if txt, _ := cm["text"].(string); txt != "" {
			b.WriteString(txt)
		}
	}
	return b.String()
}

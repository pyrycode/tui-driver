// Probe: capture a fresh claude session's first-prompt PTY + JSONL recording
// start-to-finish, including hangs. Throwaway binary — not part of the library
// regression set. Reproduces / characterises the "first-prompt blocks on MCP
// tool registration" hang documented in vault Open Questions (2026-05-18).
//
// What it does:
//
//	spawn claude with a fresh --session-id
//	→ record PTY bytes to /tmp/probe-<ts>/pty.cast (asciinema v2; per-event
//	  elapsed timestamps, `asciinema play`-able)
//	→ wait for idle, send "ready?\r"
//	→ open JSONL, tail ALL event types (no assistant-only filter)
//	→ log every JSONL line to /tmp/probe-<ts>/jsonl.log (timestamp-prefixed)
//	→ extract+highlight deferred_tools_delta.pendingMcpServers snapshots
//	→ wait for end_turn OR 3-min wall timeout
//	→ print summary: time-to-first-pty-byte, time-to-idle, time-to-first-jsonl,
//	  time-to-first-assistant, time-to-end-turn, hang-detected (>10s for first-asst)
//
// Hand-edit constants if the hang is longer than 3 min and never resolves.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pyrycode/tui-driver/pkg/tuidriver"
)

const (
	statePollInterval = 50 * time.Millisecond
	sessionFileWait   = 30 * time.Second // longer than spike-one-turn — hangs can delay JSONL creation
	wallTimeout       = 3 * time.Minute
	hangThreshold     = 10 * time.Second // first-assistant > this → flag as hang in summary

	promptText = "ready?\r"
)

func main() {
	trustFolderFlag := flag.String("trust-folder", "accept",
		"trust-folder policy: 'fail' or 'accept' (default 'accept' so the probe doesn't trip on a fresh cwd)")
	flag.Parse()

	if *trustFolderFlag != "fail" && *trustFolderFlag != "accept" {
		fmt.Fprintf(os.Stderr, "invalid -trust-folder value %q\n", *trustFolderFlag)
		os.Exit(2)
	}

	if err := run(*trustFolderFlag); err != nil {
		fmt.Fprintf(os.Stderr, "probe failed: %v\n", err)
		os.Exit(1)
	}
}

type marker struct {
	name string
	at   time.Time
}

func run(trustFolderPolicy string) error {
	startedAt := time.Now()
	tsTag := startedAt.Format("20060102-150405")
	outDir := filepath.Join(os.TempDir(), "probe-first-prompt-hang-"+tsTag)
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return fmt.Errorf("mkdir outDir: %w", err)
	}

	ptyCastPath := filepath.Join(outDir, "pty.cast")
	jsonlLogPath := filepath.Join(outDir, "jsonl.log")
	markersPath := filepath.Join(outDir, "markers.log")

	jsonlLog, err := os.Create(jsonlLogPath)
	if err != nil {
		return fmt.Errorf("create jsonl.log: %w", err)
	}
	defer jsonlLog.Close()

	markersLog, err := os.Create(markersPath)
	if err != nil {
		return fmt.Errorf("create markers.log: %w", err)
	}
	defer markersLog.Close()

	// Single source of timing truth — startedAt. recordMarker logs "[+t.tttts]
	// name" to both stderr and markers.log.
	var (
		markersMu sync.Mutex
		markers   []marker
	)
	recordMarker := func(name string) {
		now := time.Now()
		markersMu.Lock()
		markers = append(markers, marker{name: name, at: now})
		markersMu.Unlock()
		line := fmt.Sprintf("[+%9.3fs] %s\n", now.Sub(startedAt).Seconds(), name)
		fmt.Fprint(os.Stderr, line)
		fmt.Fprint(markersLog, line)
	}

	logger := log.New(os.Stderr, "", 0)
	logger.Printf("probe outDir=%s", outDir)
	recordMarker("start")

	// Fresh session ID — never reused.
	u, err := uuid.NewRandom()
	if err != nil {
		return fmt.Errorf("generate session id: %w", err)
	}
	sessionID := u.String()
	logger.Printf("session-id=%s", sessionID)

	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	jsonlPath := tuidriver.SessionJSONLPath(home, cwd, sessionID)
	logger.Printf("expected-jsonl=%s", jsonlPath)

	rootCtx, cancelCause := context.WithCancelCause(context.Background())
	defer cancelCause(errors.New("run: returning"))

	// Wall timeout — graceful cancel, NOT process kill. Probe summarises what
	// happened up to the timeout.
	wallTimer := time.AfterFunc(wallTimeout, func() {
		cancelCause(fmt.Errorf("wall timeout (%s) reached", wallTimeout))
	})
	defer wallTimer.Stop()

	cmd := exec.Command("claude", "--session-id", sessionID)
	tuidriver.EnsureClaudeEnv(cmd)

	session, err := tuidriver.Spawn(cmd, tuidriver.SpawnOpts{
		RecordTo: ptyCastPath,
	})
	if err != nil {
		return fmt.Errorf("Spawn: %w", err)
	}
	defer func() {
		recordMarker("shutdown-signalled")
		_ = session.Close()
	}()

	// First-PTY-byte marker via a tiny goroutine that polls the buffer.
	var firstByteOnce sync.Once
	go func() {
		t := time.NewTicker(statePollInterval)
		defer t.Stop()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-t.C:
				if session.LastAppendAt().IsZero() {
					continue
				}
				firstByteOnce.Do(func() { recordMarker("first-pty-byte") })
				return
			}
		}
	}()

	// Wait idle.
	if err := tuidriver.WaitUntil(rootCtx, func() bool {
		return tuidriver.IsIdle(session.Snapshot())
	}); err != nil {
		return fmt.Errorf("wait idle: %w", err)
	}
	recordMarker("idle-detected")

	// Trust modal handling.
	if tuidriver.HasTrustModal(session.Snapshot()) {
		if trustFolderPolicy == "fail" {
			return fmt.Errorf("trust-folder modal — pass -trust-folder=accept")
		}
		if err := session.AcceptTrust(); err != nil {
			return fmt.Errorf("write trust-accept: %w", err)
		}
		recordMarker("trust-folder-accepted")
		if err := tuidriver.WaitUntil(rootCtx, func() bool {
			snap := session.Snapshot()
			return !tuidriver.HasTrustModal(snap) && tuidriver.IsIdle(snap)
		}); err != nil {
			return fmt.Errorf("wait idle post-trust: %w", err)
		}
		recordMarker("idle-detected-post-trust")
	}

	// Send the prompt.
	if err := session.SendKeys(promptText); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	recordMarker("prompt-written")

	// Wait for JSONL.
	jsonlCtx, jsonlCancel := context.WithTimeout(rootCtx, sessionFileWait)
	jsonlErr := tuidriver.WaitForSessionJSONL(jsonlCtx, jsonlPath)
	jsonlCancel()
	if jsonlErr != nil {
		return fmt.Errorf("open session jsonl: %w", jsonlErr)
	}
	recordMarker("session-jsonl-appeared")

	// Tail JSONL — ALL events, no filter. Log every line with timestamp,
	// extract deferred_tools_delta specifics.
	eventCh, terr := tuidriver.TailJSONL(rootCtx, jsonlPath, 0)
	if terr != nil {
		return fmt.Errorf("open events stream: %w", terr)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	gotFirstAssistant := false
	gotEndTurn := false
	go func() {
		defer wg.Done()
		for ev := range eventCh {
			fmt.Fprintf(jsonlLog, "[+%9.3fs] %s\n", time.Since(startedAt).Seconds(), ev.RawLine)
			if ev.Type == "attachment" {
				if att, _ := ev.Raw["attachment"].(map[string]any); att != nil {
					if atype, _ := att["type"].(string); atype == "deferred_tools_delta" {
						if pending, ok := att["pendingMcpServers"].([]any); ok {
							names := make([]string, 0, len(pending))
							for _, p := range pending {
								if s, ok := p.(string); ok {
									names = append(names, s)
								}
							}
							recordMarker(fmt.Sprintf("deferred_tools_delta pending=%d [%s]",
								len(names), strings.Join(names, ",")))
						}
					}
				}
			}
			if ev.Type == "assistant" && !gotFirstAssistant {
				gotFirstAssistant = true
				recordMarker("first-assistant-event")
			}
			if ev.Type == "assistant" && tuidriver.IsEndTurn(ev) && !gotEndTurn {
				gotEndTurn = true
				recordMarker("end-turn-detected")
				cancelCause(errors.New("end-turn reached"))
			}
		}
	}()

	// Block until either end-turn fires (cancelCause above) or wall timeout.
	<-rootCtx.Done()
	cause := context.Cause(rootCtx)
	recordMarker(fmt.Sprintf("wait-done cause=%q", cause))

	// Give the tailer 1s to finish flushing.
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer closeCancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-closeCtx.Done():
	}

	// Print summary.
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "=== probe summary ===")
	fmt.Fprintf(os.Stderr, "recordings: %s\n", outDir)
	fmt.Fprintln(os.Stderr, "markers:")
	for _, m := range markers {
		fmt.Fprintf(os.Stderr, "  [+%9.3fs] %s\n", m.at.Sub(startedAt).Seconds(), m.name)
	}
	// Hang verdict.
	var idleAt, promptAt, firstAsstAt time.Time
	for _, m := range markers {
		switch m.name {
		case "idle-detected":
			idleAt = m.at
		case "prompt-written":
			promptAt = m.at
		case "first-assistant-event":
			firstAsstAt = m.at
		}
	}
	if !promptAt.IsZero() && !firstAsstAt.IsZero() {
		latency := firstAsstAt.Sub(promptAt)
		verdict := "fast"
		if latency > hangThreshold {
			verdict = "HANG"
		}
		fmt.Fprintf(os.Stderr, "first-prompt latency (prompt-written → first-assistant): %s [%s]\n",
			latency.Round(time.Millisecond), verdict)
	} else if !promptAt.IsZero() {
		fmt.Fprintln(os.Stderr, "first-prompt latency: NEVER RESOLVED — hung past wall timeout")
	}
	if !idleAt.IsZero() {
		fmt.Fprintf(os.Stderr, "startup latency (start → idle-detected): %s\n",
			idleAt.Sub(startedAt).Round(time.Millisecond))
	}

	return nil
}


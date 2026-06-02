package tuidriver

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// Prompt-commit retry bounds. A corrupted or uncommitted bracketed paste under
// MCP-init churn (the "Mode B" wedge, root-caused 2026-05-29 against live
// claude 2.1.156) is recovered by clearing the input line and re-delivering. A
// healthy run commits within ~1s, far inside the timeout, so it never retries.
const (
	DefaultMaxPromptAttempts   = 3
	DefaultPromptCommitTimeout = 3 * time.Second
	promptCommitPoll           = 150 * time.Millisecond
)

// pastedTextChip is the literal claude renders in the input box for a held,
// uncommitted bracketed paste: "[Pasted text +N lines]". Its presence is
// positive evidence the paste did not commit. This is claude-TUI substrate
// knowledge — owned here in the library, never in a consumer.
const pastedTextChip = "Pasted text"

// DeliverOpts configures DeliverPrompt.
type DeliverOpts struct {
	// Prompt is the user-turn text to deliver. Required.
	Prompt string

	// JSONLPath is the per-session JSONL file claude writes once a turn
	// commits. DeliverPrompt treats the file's appearance as a commit signal
	// alongside the thinking spinner. Optional: pass "" to rely on the spinner
	// signal alone. The session stays ignorant of home / cwd / sessionID — the
	// consumer resolves the path (via SessionJSONLPath) and passes it in.
	JSONLPath string

	// CommitTimeout bounds how long each delivery attempt waits for a commit
	// signal before deciding the paste was corrupted. 0 picks
	// DefaultPromptCommitTimeout.
	CommitTimeout time.Duration

	// MaxAttempts caps the deliver-and-confirm retries. 0 picks
	// DefaultMaxPromptAttempts.
	MaxAttempts int

	// Logger receives the decision markers: re-deliver, committed-but-slow,
	// gave-up. nil falls back to slog.Default().
	Logger *slog.Logger
}

// DeliverResult reports the outcome of DeliverPrompt.
type DeliverResult struct {
	// Committed is true when a commit signal was observed, OR when the input
	// box showed no pasted-text chip after a delivery — the committed-but-slow
	// case. False means the prompt may still be wedged and the caller should
	// fall through to the JSONL wait plus watchdog.
	Committed bool

	// Attempts is how many deliveries were made; always >= 1.
	Attempts int
}

// DeliverPrompt delivers opts.Prompt to claude and confirms the turn committed,
// recovering from the corrupted-paste wedge.
//
// It absorbs three concerns that previously lived in the consumer:
//
//   - Method selection. Today it always delivers via bracketed paste
//     (WritePrompt). The deferred short-prompt fix — choosing TypePrompt for
//     short single-line prompts — slots into the one marked line below, behind
//     a length/shape check, with zero change to any consumer.
//   - Commit confirmation. After each delivery it polls for a commit signal
//     (the thinking spinner is visible, or the per-session JSONL has appeared)
//     up to CommitTimeout.
//   - Recovery. If no commit signal lands AND the input box still shows the
//     "[Pasted text]" chip, the paste was corrupted: it clears the input line
//     and re-delivers, up to MaxAttempts. If no chip is present, the paste
//     committed and the signals are merely lagging a slow MCP cold-start —
//     re-delivering would re-paste an in-flight turn (the destructive #227
//     path), so it stops and reports Committed.
//
// It only observes and re-delivers; it never interprets JSONL. The caller
// proceeds to the JSONL tail / event loop regardless of Committed — a false
// negative costs one fall-through to the watchdog, never a corrupted turn.
//
// Returns an error only on a PTY write failure (write or clear). Context
// cancellation during the commit poll ends the current attempt early and is
// not itself an error here; the caller observes ctx via the subsequent wait.
func (s *Session) DeliverPrompt(ctx context.Context, opts DeliverOpts) (DeliverResult, error) {
	return deliverPrompt(opts, deliverDeps{
		write: s.WritePrompt,
		clear: s.ClearInputLine,
		didCommit: func(timeout time.Duration) bool {
			return s.promptDidCommit(ctx, opts.JSONLPath, timeout)
		},
		hasChip: func() bool { return hasPastedChip(s.Snapshot()) },
	})
}

// deliverDeps are the seams deliverPrompt drives. DeliverPrompt wires the real
// session methods; tests inject fakes to script commit/chip outcomes per
// attempt without a live PTY. Mirrors the runWatchdogLoop / mergeEvents seam
// pattern used elsewhere in the package.
type deliverDeps struct {
	write     func(text string) error
	clear     func() error
	didCommit func(timeout time.Duration) bool
	hasChip   func() bool
}

func deliverPrompt(opts DeliverOpts, deps deliverDeps) (DeliverResult, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	commitTimeout := opts.CommitTimeout
	if commitTimeout <= 0 {
		commitTimeout = DefaultPromptCommitTimeout
	}
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxPromptAttempts
	}

	var res DeliverResult
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		res.Attempts = attempt
		if attempt > 1 {
			if err := deps.clear(); err != nil {
				return res, fmt.Errorf("tuidriver: clear input line: %w", err)
			}
		}
		// Method selection. The deferred TypePrompt-for-short-prompts fix slots
		// in HERE — pure tui-driver change, no consumer edit.
		if err := deps.write(opts.Prompt); err != nil {
			return res, fmt.Errorf("tuidriver: write prompt: %w", err)
		}
		if deps.didCommit(commitTimeout) {
			res.Committed = true
			return res, nil
		}
		if !deps.hasChip() {
			// No "Pasted text" chip means the paste committed; the commit
			// signals are just lagging a slow MCP cold-start. Re-delivering
			// here would re-paste an already-in-flight turn — the destructive
			// #227 path. Treat as committed-but-slow and stop; the caller's
			// JSONL wait still picks up the lagging turn.
			logger.Warn("tuidriver: commit signals slow but input box empty (no pasted-text chip) — assuming committed-but-slow, not re-delivering")
			res.Committed = true
			return res, nil
		}
		logger.Warn("tuidriver: prompt uncommitted (pasted-text chip present); re-delivering")
	}
	// Backstop: the retries recover the common corrupted-paste case; a residual
	// wedge surfaces downstream as num_turns=0 and the consumer retries.
	logger.Warn("tuidriver: prompt uncommitted after retries; proceeding (may wedge)")
	return res, nil
}

// promptDidCommit reports whether claude started a turn after a prompt write:
// the thinking spinner is visible OR the per-session JSONL has appeared (claude
// writes it only once input lands). Polls at promptCommitPoll until a signal is
// seen, the timeout elapses, or ctx is cancelled. It only observes — never
// writes — so a false negative costs one extra re-delivery, never a corrupted
// live turn.
func (s *Session) promptDidCommit(ctx context.Context, jsonlPath string, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tk := time.NewTicker(promptCommitPoll)
	defer tk.Stop()
	for {
		if IsThinking(s.Snapshot()) {
			return true
		}
		if jsonlPath != "" {
			if _, err := os.Stat(jsonlPath); err == nil {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-tk.C:
		}
	}
}

// hasPastedChip reports whether snap still shows the "[Pasted text]" chip —
// positive evidence a bracketed paste is uncommitted. Strips ANSI first so an
// ANSI-escaped chip still hits. Total over a possibly-empty snapshot.
func hasPastedChip(snap []byte) bool {
	return bytes.Contains(StripANSI(snap), []byte(pastedTextChip))
}

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
	// commits and appends to thereafter. DeliverPrompt treats the file growing
	// past its pre-delivery size as a commit signal alongside the thinking
	// spinner — mere existence is not enough, since the file persists across
	// turns. Optional: pass "" to rely on the spinner signal alone. The session
	// stays ignorant of home / cwd / sessionID — the consumer resolves the path
	// (via SessionJSONLPath) and passes it in.
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
//   - Method selection. Short single-line prompts are delivered with TypePrompt
//     (byte-spaced body + isolated \r), which stays under claude's paste-
//     detection heuristic; long or multi-line prompts use WritePrompt (bracketed
//     paste), whose "[Pasted text]" chip drives the corrupted-paste recovery
//     below. See shouldTypePrompt.
//   - Commit confirmation. After each delivery it polls for a commit signal
//     (the thinking spinner is visible, or the per-session JSONL has grown past
//     its pre-delivery size) up to CommitTimeout.
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
// cancellation during the commit poll ends the current attempt early and stops
// before any further re-delivery; it is not itself an error here, and the
// caller observes ctx via the subsequent wait.
func (s *Session) DeliverPrompt(ctx context.Context, opts DeliverOpts) (DeliverResult, error) {
	// Method selection: short single-line prompts trip claude's paste-detection
	// heuristic when bracketed-pasted, so type them byte-by-byte instead. The
	// chosen write seam is reused for re-deliveries within the loop.
	write := s.WritePrompt
	if shouldTypePrompt(opts.Prompt) {
		write = s.TypePrompt
	}
	// Capture the JSONL size once, before delivery: the commit signal is the
	// file growing past this baseline, not merely existing. A "" path or a
	// missing file reads as size 0, so turn one — where the file first appears
	// with content — still commits, while a stale file from a prior turn that
	// does not grow does not. Captured here (outside the deliverPrompt retry
	// loop) so every re-delivery attempt measures against the file state
	// before this turn began, never folding an earlier attempt's growth into
	// its own baseline.
	var baseline int64
	if opts.JSONLPath != "" {
		if info, err := os.Stat(opts.JSONLPath); err == nil {
			baseline = info.Size()
		}
	}
	return deliverPrompt(ctx, opts, deliverDeps{
		write: write,
		clear: s.ClearInputLine,
		didCommit: func(timeout time.Duration) bool {
			return s.promptDidCommit(ctx, opts.JSONLPath, baseline, timeout)
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

func deliverPrompt(ctx context.Context, opts DeliverOpts, deps deliverDeps) (DeliverResult, error) {
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
		// deps.write is the method DeliverPrompt selected for this prompt's
		// shape (TypePrompt for short single-line prompts, WritePrompt
		// otherwise); re-deliveries reuse the same method.
		if err := deps.write(opts.Prompt); err != nil {
			return res, fmt.Errorf("tuidriver: write prompt: %w", err)
		}
		if deps.didCommit(commitTimeout) {
			res.Committed = true
			return res, nil
		}
		if ctx.Err() != nil {
			// The commit poll ended because ctx was cancelled, not because of a
			// genuine timeout. Stop here: do not consult hasChip and do not begin
			// a fresh re-delivery. Committed stays false; the caller re-observes
			// ctx via the downstream JSONL wait.
			logger.Debug("tuidriver: context cancelled between delivery attempts; not re-delivering")
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
// the thinking spinner is visible OR the per-session JSONL has grown past
// baseline (the size captured immediately before this delivery). Because the
// JSONL is append-only, a fresh append for the current turn always pushes the
// size up, while a stale file from a prior turn that does not grow no longer
// counts — so paste-recovery keeps protecting turn two onward, not just the
// first. Polls at promptCommitPoll until a signal is seen, the timeout elapses,
// or ctx is cancelled. It only observes — never writes — so a false negative
// costs one extra re-delivery, never a corrupted live turn.
func (s *Session) promptDidCommit(ctx context.Context, jsonlPath string, baseline int64, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tk := time.NewTicker(promptCommitPoll)
	defer tk.Stop()
	for {
		if IsThinking(s.Snapshot()) {
			return true
		}
		if jsonlPath != "" {
			if info, err := os.Stat(jsonlPath); err == nil && info.Size() > baseline {
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

// typePromptMaxLen is the byte length at or below which a single-line prompt is
// delivered with TypePrompt rather than a bracketed paste. At
// PromptInterByteDelay (10ms/byte) a 256-byte prompt types in ~2.6s; longer
// prompts fall back to the paste path. The single-line shape (no '\n') is the
// load-bearing condition — multi-line prompts always paste.
const typePromptMaxLen = 256

// hasControlByte reports whether text contains any C0 control byte (< 0x20).
// A plain byte scan is correct for UTF-8: printable ASCII is >= 0x20 (space),
// and no byte of a multi-byte rune is < 0x80 (lead >= 0xC0, continuation
// >= 0x80), so this never trips on Unicode content. Total over the empty
// string (returns false). DEL (0x7F) is intentionally not screened — it is
// neither C0 nor a demonstrated steering byte.
func hasControlByte(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] < 0x20 {
			return true
		}
	}
	return false
}

// shouldTypePrompt reports whether text is a short, control-byte-free prompt
// that should be delivered with TypePrompt (byte-spaced body + isolated \r)
// instead of a bracketed paste. claude's terminal paste-detection heuristic
// mis-classifies a fast bulk write of such a prompt, absorbing the trailing \r
// so the turn never commits — and because a short prompt renders inline with no
// "[Pasted text]" chip, the chip-based recovery in deliverPrompt cannot catch
// it. Typing the bytes keeps the stream under the paste threshold and commits
// reliably (the #71 / PR #77 fix).
//
// A prompt carrying any C0 control byte (< 0x20) is disqualified from the typed
// path and falls through to WritePrompt (bracketed paste) instead — no separate
// routing code, the else branch in deliverPrompt already handles it. TypePrompt
// writes every byte to the PTY verbatim, so a stray \r there commits the turn
// early and a stray ESC can steer claude's TUI; the paste path embeds the body
// verbatim inside \x1b[200~ … \x1b[201~, so claude treats those same bytes as
// literal pasted text (backed by TestBracketedPasteWrapping's "embedded CR
// survives" case). Route-to-paste — not reject or sanitise — is the chosen
// strategy because it never drops a legitimate turn, never mutates user
// content, and keeps short control-byte prompts on the identical path already
// used by every long/multi-line prompt (the newline check this subsumes was
// only ever a special case of "a control byte disqualifies the typed path").
// See #172.
func shouldTypePrompt(text string) bool {
	return len(text) <= typePromptMaxLen && !hasControlByte(text)
}

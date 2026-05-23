package tuidriver

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// DefaultShutdownGrace is how long Close waits for the process to exit
// gracefully after SIGTERM before escalating to SIGKILL. Same value the
// spike binaries used.
const DefaultShutdownGrace = 3 * time.Second

// PromptInterByteDelay is the pause between bytes when TypePrompt writes a
// prompt body. Empirical value from #71 / PR #77.
const PromptInterByteDelay = 10 * time.Millisecond

// PromptCommitSettle is the pause between the last prompt byte and the
// trailing \r commit byte that TypePrompt writes. Empirical value from
// #71 / PR #77.
const PromptCommitSettle = 50 * time.Millisecond

// ClearLineSettle is the pause after the Ctrl-U byte ClearInputLine writes,
// allowing the input handler to process the line-kill before the next byte
// arrives.
const ClearLineSettle = 50 * time.Millisecond

// sleepFn is the clock seam TypePrompt and ClearInputLine call instead of
// time.Sleep directly. Tests swap this for a fake that records durations
// so the inter-byte / settle timing can be asserted without wall-clock
// dependence. Package-private — not a public knob.
var sleepFn = time.Sleep

// SpawnOpts configures Spawn.
type SpawnOpts struct {
	// Mirror, if non-nil, receives a copy of every PTY byte. Typical
	// uses: os.Stderr for spike binaries (lets the operator watch
	// claude's TUI live). Production drivers pass nil.
	Mirror io.Writer

	// BufferCap is the rolling-buffer capacity in bytes. 0 → DefaultBufferCap.
	BufferCap int

	// ShutdownGrace overrides DefaultShutdownGrace. 0 → default.
	ShutdownGrace time.Duration
}

// Session is a running PTY-wrapped subprocess. Owns the PTY file, the
// rolling buffer, the reader goroutine that feeds the buffer, and the
// shutdown lifecycle.
//
// Out of scope (consumer's responsibility): state machine, JSONL tailing,
// watchdog timers, modal handling. The Session exposes the buffer + a
// Write method; consumers compose state-detection on top.
//
// Construct with Spawn. The zero value is not usable.
type Session struct {
	// Buffer is the rolling buffer the reader goroutine appends PTY
	// bytes into. Consumers Snapshot() it to inspect TUI state.
	Buffer *Buffer

	// PTY is the PTY master side. Consumers can call pty.Setsize on it
	// directly for custom dimensions, or read it for raw streaming.
	// Prefer Buffer.Snapshot for state detection.
	PTY *os.File

	cmd           *exec.Cmd
	exited        chan struct{} // closed by the cmd.Wait goroutine
	exitErr       error         // populated before exited is closed
	readerDone    chan struct{}
	shutdownGrace time.Duration
	shutdownOnce  sync.Once
}

// Spawn launches cmd inside a PTY and starts a reader goroutine that
// streams output into Buffer. Callers should set cmd.Args + cmd.Env
// (via EnsureClaudeEnv if driving claude) before calling Spawn.
//
// Returns a *Session on success. Caller MUST defer Close to release the
// PTY and reap the subprocess.
func Spawn(cmd *exec.Cmd, opts SpawnOpts) (*Session, error) {
	ptmx, err := StartPTY(cmd)
	if err != nil {
		return nil, err
	}

	grace := opts.ShutdownGrace
	if grace <= 0 {
		grace = DefaultShutdownGrace
	}

	s := &Session{
		Buffer:        NewBuffer(opts.BufferCap),
		PTY:           ptmx,
		cmd:           cmd,
		exited:        make(chan struct{}),
		readerDone:    make(chan struct{}),
		shutdownGrace: grace,
	}

	// cmd.Wait observer — populates exitErr then closes exited. The
	// close-broadcast pattern lets Wait() and Close() (and any other
	// caller) all observe the exit without racing each other.
	go func() {
		s.exitErr = cmd.Wait()
		close(s.exited)
	}()

	// PTY reader goroutine — drains the master FD into Buffer, optionally
	// mirroring to opts.Mirror.
	go func() {
		defer close(s.readerDone)
		buf := make([]byte, 4096)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				s.Buffer.Append(chunk)
				if opts.Mirror != nil {
					_, _ = opts.Mirror.Write(chunk)
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	return s, nil
}

// Write sends bytes to the PTY. Returns the underlying ptmx.Write result.
// Safe to call concurrently with the reader goroutine — *os.File's Write
// is goroutine-safe on POSIX systems.
func (s *Session) Write(p []byte) (int, error) {
	return s.PTY.Write(p)
}

// WritePrompt sends text to the PTY wrapped in bracketed-paste escape
// sequences (\x1b[200~ ... \x1b[201~) followed by \r OUTSIDE the markers.
// Use this for any prompt longer than ~1 KB or containing newlines.
//
// Why: claude's TUI auto-detects pastes (input bytes arriving faster than
// human typing trigger paste-detection). When it fires, the input is held
// in the input area as "[Pasted text +N lines]" chips and claude waits for
// an explicit Enter to commit. A naive Session.Write(prompt + "\r") for a
// long or multi-line prompt gets the \r swallowed into the paste body, so
// the turn never commits and claude stays idle indefinitely.
//
// WritePrompt makes the boundaries explicit: \x1b[200~ opens the paste,
// the text follows verbatim (newlines OK), \x1b[201~ closes it, and the
// trailing \r is the commit signal.
//
// Empirically validated 2026-05-19 against claude 2.1.144 with a 3.5 KB
// multi-line prompt containing markers at start/middle/end — claude
// received all three. The naive path (Write of the same bytes + \r) left
// the input pending with three "Pasted text" chips and never committed.
//
// Returns the first non-nil error from the underlying PTY write. Short
// prompts (e.g. single-token responses to modals like "1\r" for permission
// approval) should still use Write, not WritePrompt — paste-wrapping a
// single token has no upside.
func (s *Session) WritePrompt(text string) error {
	_, err := s.PTY.Write(bracketedPaste(text))
	return err
}

// bracketedPaste builds the byte payload WritePrompt writes: open marker,
// text verbatim, close marker, trailing \r outside the markers. Factored
// out so the wire shape can be unit-tested without spawning a PTY.
func bracketedPaste(text string) []byte {
	out := make([]byte, 0, len(text)+13)
	out = append(out, "\x1b[200~"...)
	out = append(out, text...)
	out = append(out, "\x1b[201~\r"...)
	return out
}

// TypePrompt sends text to the PTY one byte at a time, waiting
// PromptInterByteDelay between bytes, pausing PromptCommitSettle after the
// body, then writing a single \r commit byte as a separate write. Use this
// for short prompts (no newlines, well under ~1 KB); use WritePrompt for
// long or multi-line prompts.
//
// Why: claude 2.1.148's TUI auto-paste-detection heuristic mis-classifies
// fast bulk writes of short prompts. When tripped, the trailing \r is
// absorbed into the paste body and the turn never commits, leaving claude
// idle indefinitely. Spacing the bytes (and isolating the \r) keeps the
// stream below the paste-detection threshold. Empirical fix from #71 /
// PR #77, commit e7c3dd2.
//
// Single-byte or single-bulk-write keystrokes (e.g. ESC, "1\r" for a
// permission modal) should still use Write — they are below the paste
// heuristic by construction.
//
// Returns the first non-nil error from the underlying PTY write.
func (s *Session) TypePrompt(text string) error {
	for i := 0; i < len(text); i++ {
		if _, err := s.PTY.Write([]byte{text[i]}); err != nil {
			return err
		}
		sleepFn(PromptInterByteDelay)
	}
	sleepFn(PromptCommitSettle)
	if _, err := s.PTY.Write([]byte("\r")); err != nil {
		return err
	}
	return nil
}

// ClearInputLine sends Ctrl-U (0x15, kill-to-beginning-of-line) to the PTY
// and waits ClearLineSettle so the input handler can process the line-kill
// before the next byte arrives. Idempotent on an empty input box.
//
// Why: after a cancel (or any wind-down that leaves drafted input in
// claude's input area), the next TypePrompt would concatenate onto that
// residue rather than starting fresh. Recommended before every TypePrompt
// that follows a prior turn. See cmd/spike-cancel/README.md surprise 2.
//
// Returns the first non-nil error from the underlying PTY write.
func (s *Session) ClearInputLine() error {
	if _, err := s.PTY.Write([]byte{0x15}); err != nil {
		return err
	}
	sleepFn(ClearLineSettle)
	return nil
}

// Wait blocks until the underlying process exits. Returns the exit error
// (or nil for clean exit). Safe to call from multiple goroutines and
// before/after Close.
func (s *Session) Wait() error {
	<-s.exited
	return s.exitErr
}

// Close shuts the session down cleanly:
//
//  1. SIGTERM the process.
//  2. Wait up to ShutdownGrace for the process to exit.
//  3. If still alive, SIGKILL and wait for the exit.
//  4. Close the PTY file.
//  5. Wait for the reader goroutine to finish (PTY close unblocks it).
//
// Idempotent — subsequent calls return the same error without re-killing.
// Returns the process's exit status (the same value Wait would return).
func (s *Session) Close() error {
	s.shutdownOnce.Do(func() {
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-s.exited:
			case <-time.After(s.shutdownGrace):
				_ = s.cmd.Process.Signal(syscall.SIGKILL)
				<-s.exited
			}
		}
		_ = s.PTY.Close()
		<-s.readerDone
	})
	return s.exitErr
}

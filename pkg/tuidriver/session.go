package tuidriver

import (
	"fmt"
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

// defaultMirrorOutputBuffer is the capacity of the raw-output mirror stream
// (SpawnOpts.MirrorOutput / Session.MirrorOutput). Larger than Events'
// defaultEventBuffer (32) on purpose: the slow-consumer policy here is
// non-blocking drop-newest, so buffer depth maps to dropped-frame frequency,
// not to producer-stall duration — a different tradeoff from the lossless
// Events channel. 256 chunks (≤ ~1 MB at the 4 KB read size) absorb
// multi-frame repaint bursts before any chunk is dropped. Internal, not a
// public knob (same posture as defaultEventBuffer); retune on evidence from
// the downstream attach spike.
const defaultMirrorOutputBuffer = 256

// SpawnOpts configures Spawn.
type SpawnOpts struct {
	// RecordTo, if non-empty, is a filesystem path the session opens (0600,
	// O_EXCL) and writes an asciinema v2 .cast recording of every PTY byte to.
	// tui-driver owns the file end to end: it opens it, writes the header,
	// frames each PTY read as one cast event, and closes it on Close. The
	// consumer owns only the path lifecycle — directory creation, pruning, and
	// any post-Close rename. No io.Writer ever crosses the package boundary, so
	// the byte tap cannot be reused as a substrate seam.
	//
	// SECURITY: a .cast holds every byte the terminal showed — the prompt,
	// claude's output, and all tool output, which can include file contents and
	// secrets. The file is owner-only, but unencrypted by design so it stays
	// `asciinema play`-able. Point RecordTo at a non-synced, non-backed-up
	// location.
	RecordTo string

	// MirrorStderr, when true, tees every PTY byte to os.Stderr — the spike
	// "live view" so an operator can watch claude's TUI. Production drivers
	// leave it false. Composes with RecordTo: both sinks receive the bytes.
	MirrorStderr bool

	// MirrorOutput, when true, makes Spawn open a live stream of the hosted
	// process's raw PTY output bytes, retrievable via Session.MirrorOutput.
	// Each PTY read chunk is copied and delivered verbatim on that channel as
	// an opaque byte slice — the production surface for byte-mirroring claude's
	// terminal to a local attach head. Default false → zero behaviour change;
	// the reader hot loop is identical for every non-attach session. Composes
	// with RecordTo / MirrorStderr (all sinks see the same bytes).
	//
	// OPAQUE BYTES — DO NOT PARSE: the chunks are for forwarding to a terminal
	// verbatim. The consumer must never inspect, match, or branch on their
	// content; all screen knowledge (anchors, spinners, modal text) stays
	// inside tui-driver. The delivery shape (a channel of []byte, not an
	// io.Reader over the rolling buffer) is deliberately hostile to tokenising.
	//
	// SECURITY: the stream carries every byte claude's terminal renders — the
	// prompt, claude's output, and all tool output, which can include file
	// contents and secrets (the same content RecordTo flags above). The
	// consumer owns the confidentiality of whatever transport it forwards these
	// bytes to (local TTY, socket, or a network link to a remote head). Do not
	// pipe them over an untrusted channel.
	MirrorOutput bool

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
	// buffer is the rolling buffer the reader goroutine appends PTY bytes
	// into. Unexported: consumers read TUI state through Snapshot / QuietFor /
	// LastAppendAt, never the writable buffer handle.
	buffer *Buffer

	// pty is the PTY master side, owned end to end by the Session.
	// Unexported: the public Write seam is gone, so consumers drive input via
	// the typed keystroke methods (AcceptTrust, Answer, SendEsc, Navigate,
	// SendKeys) and DeliverPrompt, never the raw FD.
	pty *os.File

	cmd           *exec.Cmd
	exited        chan struct{} // closed by the cmd.Wait goroutine
	exitErr       error         // populated before exited is closed
	readerDone    chan struct{}
	shutdownGrace time.Duration
	shutdownOnce  sync.Once

	// recCloser is the recording file opened for SpawnOpts.RecordTo, or nil
	// when no recording was requested. Closed by Close after the reader
	// goroutine — the sole writer — has joined, so the close cannot race a
	// mirror write.
	recCloser io.Closer

	// mirrorOut is the raw-output stream returned by MirrorOutput, allocated
	// in Spawn only when SpawnOpts.MirrorOutput is set (nil otherwise). The
	// reader goroutine is its sole sender and sole closer, so no send-on-closed
	// is possible. Set once before any goroutine starts and only read after, so
	// the hot-loop nil check and the accessor need no lock.
	mirrorOut chan []byte
}

// Spawn launches cmd inside a PTY and starts a reader goroutine that
// streams output into Buffer. Callers should set cmd.Args + cmd.Env
// (via EnsureClaudeEnv if driving claude) before calling Spawn.
//
// Returns a *Session on success. Caller MUST defer Close to release the
// PTY and reap the subprocess.
func Spawn(cmd *exec.Cmd, opts SpawnOpts) (*Session, error) {
	// Build the optional mirror sink BEFORE starting the process, so a
	// recording-open failure aborts cleanly with nothing spawned.
	mirror, recCloser, err := buildMirror(opts)
	if err != nil {
		return nil, err
	}

	ptmx, err := StartPTY(cmd)
	if err != nil {
		if recCloser != nil {
			_ = recCloser.Close()
		}
		return nil, err
	}

	grace := opts.ShutdownGrace
	if grace <= 0 {
		grace = DefaultShutdownGrace
	}

	s := &Session{
		buffer:        NewBuffer(opts.BufferCap),
		pty:           ptmx,
		cmd:           cmd,
		exited:        make(chan struct{}),
		readerDone:    make(chan struct{}),
		shutdownGrace: grace,
		recCloser:     recCloser,
	}
	if opts.MirrorOutput {
		s.mirrorOut = make(chan []byte, defaultMirrorOutputBuffer)
	}

	// cmd.Wait observer — populates exitErr then closes exited. The
	// close-broadcast pattern lets Wait() and Close() (and any other
	// caller) all observe the exit without racing each other.
	go func() {
		s.exitErr = cmd.Wait()
		close(s.exited)
	}()

	// PTY reader goroutine — drains the master FD into Buffer, optionally
	// teeing each chunk to the mirror sink (recording and/or stderr) and to the
	// raw-output stream. Sole sender and sole closer of mirrorOut.
	go func() {
		defer close(s.readerDone)
		// LIFO: registered after readerDone so mirrorOut closes *first*. A
		// consumer that calls Wait (which blocks on readerDone) and then drains
		// therefore always observes the stream already closed.
		if s.mirrorOut != nil {
			defer close(s.mirrorOut)
		}
		buf := make([]byte, 4096)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				s.buffer.Append(chunk)
				if mirror != nil {
					_, _ = mirror.Write(chunk)
				}
				if s.mirrorOut != nil {
					// chunk aliases the reused buf; the async consumer needs its
					// own backing array.
					cp := make([]byte, n)
					copy(cp, chunk)
					select {
					case s.mirrorOut <- cp:
					default:
						// Drop-newest: the consumer is slower than the PTY. A
						// dropped chunk is a transient screen glitch that claude's
						// next full repaint heals; dropping (not blocking) keeps
						// the shared reader off a full channel so state detection
						// never freezes and Close never wedges. NEVER log cp — it
						// carries claude's full screen content (file contents,
						// secrets); a debug log would leak it.
					}
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	return s, nil
}

// buildMirror assembles the optional PTY-mirror sink from opts. Returns the
// io.Writer the reader goroutine tees every chunk to (nil when no sink is
// configured) and the io.Closer for any file the session opened (nil unless
// RecordTo opened one). The recorder and stderr sinks are combined with
// io.MultiWriter when both are present.
//
// The returned writer never escapes the package: it lives only inside the
// reader goroutine. A consumer configures sinks by value (a path, a bool), so
// the raw byte tap cannot be reached or reused.
func buildMirror(opts SpawnOpts) (io.Writer, io.Closer, error) {
	var sinks []io.Writer
	var recCloser io.Closer

	if opts.RecordTo != "" {
		f, err := os.OpenFile(opts.RecordTo, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("tuidriver: open recording %s: %w", opts.RecordTo, err)
		}
		rec := NewCastRecorder(f, int(DefaultPtyCols), int(DefaultPtyRows))
		if herr := rec.WriteHeader(); herr != nil {
			_ = f.Close()
			return nil, nil, fmt.Errorf("tuidriver: recording header: %w", herr)
		}
		sinks = append(sinks, rec)
		recCloser = f
	}
	if opts.MirrorStderr {
		sinks = append(sinks, os.Stderr)
	}

	switch len(sinks) {
	case 0:
		return nil, recCloser, nil
	case 1:
		return sinks[0], recCloser, nil
	default:
		return io.MultiWriter(sinks...), recCloser, nil
	}
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
	_, err := s.pty.Write(bracketedPaste(text))
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
		if _, err := s.pty.Write([]byte{text[i]}); err != nil {
			return err
		}
		sleepFn(PromptInterByteDelay)
	}
	sleepFn(PromptCommitSettle)
	if _, err := s.pty.Write([]byte("\r")); err != nil {
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
	if _, err := s.pty.Write([]byte{0x15}); err != nil {
		return err
	}
	sleepFn(ClearLineSettle)
	return nil
}

// Snapshot returns a copy of the rolling PTY buffer's current contents — the
// latest TUI rendering. Feed it to the state-detection predicates (IsIdle,
// IsThinking, DetectModalClass, HasTrustModal, …). This is the read seam:
// consumers inspect TUI state through it and never hold the writable buffer.
func (s *Session) Snapshot() []byte { return s.buffer.Snapshot() }

// QuietFor returns the time since the last PTY byte was appended — the
// PTY-heartbeat signal the watchdog's quiet arm reads. Returns 0 before the
// first byte arrives.
func (s *Session) QuietFor() time.Duration { return s.buffer.QuietFor() }

// LastAppendAt returns the timestamp of the most recent PTY append, or the zero
// value if nothing has arrived yet.
func (s *Session) LastAppendAt() time.Time { return s.buffer.LastAppendAt() }

// MirrorOutput returns the live receive-only stream of the hosted process's
// raw PTY output bytes. Each received []byte is an independent copy of one PTY
// read chunk, delivered verbatim — the production surface for byte-mirroring
// claude's terminal to a local attach head. This is a tap on the live PTY read,
// not a reader over the rolling buffer.
//
// OPAQUE BYTES — DO NOT PARSE: forward the chunks verbatim and never look
// inside them. The consumer must not inspect, match, or branch on their
// content; all screen knowledge (anchors, spinners, modal text) stays inside
// tui-driver. See SpawnOpts.MirrorOutput for the full opaque-bytes-no-parse and
// SECURITY contract (the bytes can include file contents and secrets).
//
// Returns nil when SpawnOpts.MirrorOutput was false — ranging a nil channel
// blocks forever, so only call this when you enabled the stream. The reader
// goroutine closes the channel on shutdown (Close, or natural process exit); a
// consumer that calls Wait and then drains always observes it already closed.
// Slow-consumer policy: under backpressure chunks are dropped (drop-newest, a
// fixed-capacity buffer) rather than blocking the session — the mirror is
// best-effort and a dropped chunk heals on claude's next full repaint.
func (s *Session) MirrorOutput() <-chan []byte { return s.mirrorOut }

// Wait blocks until both the underlying process has exited AND the PTY
// reader goroutine has drained the final bytes from the PTY master FD.
// Returns the exit error (or nil for clean exit). Safe to call from
// multiple goroutines and before/after Close.
//
// After Wait returns, any bytes the reader wrote into the rolling buffer or
// into the RecordTo recording before the subprocess exited are guaranteed
// visible to the caller (happens-before via the readerDone channel close).
// This is the synchronisation point for inspecting Snapshot() without racing
// the reader.
func (s *Session) Wait() error {
	<-s.exited
	<-s.readerDone
	return s.exitErr
}

// Close shuts the session down cleanly:
//
//  1. SIGTERM the process.
//  2. Wait up to ShutdownGrace for the process to exit.
//  3. If still alive, SIGKILL and wait for the exit.
//  4. Close the PTY file.
//  5. Wait for the reader goroutine to finish (PTY close unblocks it).
//  6. Close the recording file, if one was opened for SpawnOpts.RecordTo.
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
		_ = s.pty.Close()
		<-s.readerDone
		// Close the recording file only after the reader goroutine — the sole
		// writer to it — has joined, so the close cannot race a mirror write.
		if s.recCloser != nil {
			_ = s.recCloser.Close()
		}
	})
	return s.exitErr
}

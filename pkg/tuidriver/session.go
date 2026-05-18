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

package tuidriver

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitReady(t *testing.T) {
	idle := []byte(idleGlyphTest + " ") // ❯ present, no spinner → IsIdle

	tests := []struct {
		name string
		snap []byte
		want Readiness
	}{
		{
			name: "clean idle",
			snap: idle,
			want: Readiness{Idle: true},
		},
		{
			// Spaced form: since #163 HasTrustModal matches the rendered grid, so
			// the on-screen "Quick safety check" header is what sets TrustModal
			// (consistent with DetectModalClass). #219: the header alone no longer
			// suffices — the fixture is now the full dialog shape (header plus a
			// pointer-marked option row), as trust-folder-snapshot.bin renders it.
			// The option row's ❯ also supplies the idle glyph, so Idle stays true.
			name: "trust modal at idle",
			snap: []byte("Quick safety check: Is this a project you created or one you trust?\r\n" +
				"❯ 1. Yes, I trust this folder\r\n" +
				"  2. No, I selected this folder by mistake\r\n" +
				"(Esc to cancel)\r\n"),
			want: Readiness{Idle: true, TrustModal: true},
		},
		{
			name: "mcp failure banner at idle",
			snap: append([]byte("2 MCP servers failed "), idle...),
			want: Readiness{Idle: true, McpFailure: true, FailedMcpCount: 2},
		},
		{
			name: "network failure at idle",
			snap: append([]byte("FailedToOpenSocket "), idle...),
			want: Readiness{Idle: true, NetworkFailure: true},
		},
		{
			// A recognized modal that no other Readiness field surfaces (here a
			// permission prompt) sets UnknownModal. Spaced form: since #152
			// DetectModalClass matches the rendered grid, so the on-screen
			// "Do you want to proceed" is what classifies as Permission.
			name: "unrecognized modal at idle",
			snap: append([]byte("Do you want to proceed"), idle...),
			want: Readiness{Idle: true, UnknownModal: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Session{buffer: NewBuffer(0)}
			s.buffer.Append(tt.snap)
			got, err := s.WaitReady(context.Background())
			if err != nil {
				t.Fatalf("WaitReady: %v", err)
			}
			if got != tt.want {
				t.Errorf("WaitReady = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestWaitReadyContextCancelled is the AC #3 "process alive, cancelled" guard:
// a directly-constructed Session has a nil exited channel, so the exit arm is
// inert and WaitReady still returns the ctx cause when it never reaches idle.
func TestWaitReadyContextCancelled(t *testing.T) {
	s := &Session{buffer: NewBuffer(0)} // never idle
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	got, err := s.WaitReady(ctx)
	if err == nil {
		t.Fatal("WaitReady = nil error, want ctx error")
	}
	if (got != Readiness{}) {
		t.Errorf("WaitReady = %+v on error, want zero Readiness", got)
	}
}

// TestWaitReadyProcessExited: a session whose process has exited (exited
// closed, exitErr set) and that never reaches idle must return promptly with a
// *ProcessExitedError carrying the exit cause — not poll until the ctx timeout.
func TestWaitReadyProcessExited(t *testing.T) {
	exitErr := errors.New("exit status 1")
	exited := make(chan struct{})
	close(exited)
	s := &Session{buffer: NewBuffer(0), exited: exited, exitErr: exitErr} // never idle

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	got, err := s.WaitReady(ctx)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("WaitReady took %v, want prompt return well within ctx timeout", elapsed)
	}

	var pee *ProcessExitedError
	if !errors.As(err, &pee) {
		t.Fatalf("WaitReady err = %v, want *ProcessExitedError", err)
	}
	if !errors.Is(err, exitErr) {
		t.Errorf("WaitReady err does not wrap exitErr %v", exitErr)
	}
	if pee.Err != exitErr {
		t.Errorf("ProcessExitedError.Err = %v, want %v", pee.Err, exitErr)
	}
	if (got != Readiness{}) {
		t.Errorf("WaitReady = %+v on error, want zero Readiness", got)
	}
}

// TestWaitReadyProcessExitedCleanExit: a clean exit-before-ready (exitErr nil)
// still surfaces a *ProcessExitedError, with a nil cause.
func TestWaitReadyProcessExitedCleanExit(t *testing.T) {
	exited := make(chan struct{})
	close(exited)
	s := &Session{buffer: NewBuffer(0), exited: exited} // never idle, exitErr nil

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := s.WaitReady(ctx)
	var pee *ProcessExitedError
	if !errors.As(err, &pee) {
		t.Fatalf("WaitReady err = %v, want *ProcessExitedError", err)
	}
	if pee.Err != nil {
		t.Errorf("ProcessExitedError.Err = %v, want nil on clean exit", pee.Err)
	}
}

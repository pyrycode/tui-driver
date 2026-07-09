package tuidriver

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitReady(t *testing.T) {
	idle := []byte(idleGlyphTest + " ") // ❯ present, no spinner → IsIdle

	// loadIdleFixture loads a real fixture and guards that it renders idle. A
	// non-idle fixture would make WaitReady block until the ctx deadline, so this
	// fails loudly at load time instead of as an opaque test timeout.
	loadIdleFixture := func(t *testing.T, name string) []byte {
		snap := loadFixture(t, name)
		if !IsIdle(snap) {
			t.Fatalf("fixture %s is not IsIdle; WaitReady would block on it", name)
		}
		return snap
	}

	tests := []struct {
		name string
		snap []byte
		// want is the expected Readiness on a nil-error return. wantErr selects
		// the #173 loud-error path instead, with wantClass the modal class the
		// UnexpectedModalError must carry.
		want      Readiness
		wantErr   bool
		wantClass ModalClass
	}{
		{
			name: "clean idle",
			snap: idle,
			want: Readiness{Idle: true},
		},
		{
			// mcp-empty is the `/mcp` "No MCP servers configured" line echoed on an
			// otherwise idle screen (#223): DetectModalClass is Unknown and no
			// selection shape is present, so it is clean-ready, not an unexpected
			// modal. Guards a regression that would re-classify the echo as a modal.
			name: "mcp-empty echo at idle is clean-ready",
			snap: loadIdleFixture(t, "mcp-empty-snapshot.bin"),
			want: Readiness{Idle: true},
		},
		{
			// AC3: trust stays report-only, the driver owns the trust decision.
			// Spaced form: since #163 HasTrustModal matches the rendered grid, so
			// the on-screen "Quick safety check" header sets TrustModal. #219: the
			// header alone no longer suffices — the fixture is the full dialog shape
			// (header plus a pointer-marked option row). The option row's ❯ also
			// supplies the idle glyph, so Idle stays true.
			name: "trust modal at idle stays report-only (synthetic)",
			snap: []byte("Quick safety check: Is this a project you created or one you trust?\r\n" +
				"❯ 1. Yes, I trust this folder\r\n" +
				"  2. No, I selected this folder by mistake\r\n" +
				"(Esc to cancel)\r\n"),
			want: Readiness{Idle: true, TrustModal: true},
		},
		{
			name: "trust modal at idle stays report-only (real fixture)",
			snap: loadIdleFixture(t, "trust-folder-snapshot.bin"),
			want: Readiness{Idle: true, TrustModal: true},
		},
		{
			// AC3: the MCP-failure banner is a status line, not a modal class, so it
			// does not trip the gate — it stays a report-only flag.
			name: "mcp failure banner at idle stays report-only",
			snap: append([]byte("2 MCP servers failed "), idle...),
			want: Readiness{Idle: true, McpFailure: true, FailedMcpCount: 2},
		},
		{
			// AC3: #220's network anchor ("Unable to connect to API") is a status
			// line, advisory only — claude retries it itself — so it stays a flag.
			name: "network failure at idle stays report-only",
			snap: append([]byte("Unable to connect to API (ConnectionRefused) "), idle...),
			want: Readiness{Idle: true, NetworkFailure: true},
		},
		{
			// AC2/AC5, #173 inversion: a recognized modal that is not part of a
			// clean startup no longer rides as an advisory flag — it fails loudly so
			// a consumer reading only the error never types its first prompt into it.
			name:      "unexpected permission modal at idle fails loudly (real fixture)",
			snap:      loadIdleFixture(t, "permission-snapshot.bin"),
			wantErr:   true,
			wantClass: ModalClassPermission,
		},
		{
			// Synthetic permission overlay in the bottom region (#152/#153). #242:
			// the prompt phrase alone no longer suffices — the fixture is the full
			// dialog shape (prompt plus a pointer-marked numbered option row region-
			// scoped below it), so it classifies as Permission. The option row's ❯
			// also supplies the idle glyph, so the screen still reads idle.
			name: "unexpected permission modal at idle fails loudly (synthetic)",
			snap: []byte("Do you want to proceed?\r\n" +
				"❯ 1. Yes\r\n" +
				"  2. No\r\n" +
				"(Esc to cancel)\r\n"),
			wantErr:   true,
			wantClass: ModalClassPermission,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Session{buffer: NewBuffer(0)}
			s.buffer.Append(tt.snap)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			got, err := s.WaitReady(ctx)
			if tt.wantErr {
				var ume *UnexpectedModalError
				if !errors.As(err, &ume) {
					t.Fatalf("WaitReady err = %v, want *UnexpectedModalError", err)
				}
				if ume.Class != tt.wantClass {
					t.Errorf("UnexpectedModalError.Class = %q, want %q", ume.Class, tt.wantClass)
				}
				if (got != Readiness{}) {
					t.Errorf("WaitReady = %+v on error, want zero Readiness", got)
				}
				return
			}
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

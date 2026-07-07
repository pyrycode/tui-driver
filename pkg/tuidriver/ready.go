package tuidriver

import "context"

// Readiness reports what claude's TUI is showing once it first reaches idle:
// the input prompt plus any benign status banner or the trust modal. WaitReady
// returns it so the consumer — not the library — owns the policy. For example:
// abort on a trust modal, continue past an MCP-failure banner, abort on a
// network failure. The library detects; the driver decides.
//
// A modal that is unexpected before the first prompt is NOT reported here as a
// flag: WaitReady returns an *UnexpectedModalError for it instead (#173), so a
// consumer that reads only the error halts rather than typing its first prompt
// into the dialog. Readiness therefore only ever carries the benign conditions.
type Readiness struct {
	// Idle is true on any nil-error WaitReady return: claude reached the input
	// prompt. False only accompanies a non-nil error.
	Idle bool

	// TrustModal is true when claude's trust-folder modal ("Quick safety
	// check") is up at idle. Pre-marking the workdir trusted avoids it.
	TrustModal bool

	// McpFailure is true when claude's "N MCP server(s) failed" status banner
	// is present at idle. Typically non-fatal: an ambient MCP server failing
	// must not abort the run.
	McpFailure bool

	// FailedMcpCount is the N from the MCP-failure banner, 0 when McpFailure is
	// false.
	FailedMcpCount int

	// NetworkFailure is true when claude's network-unreachable status line
	// ("Unable to connect to API") is present in the status region at idle: the
	// claude API is unreachable. Advisory; claude retries it itself.
	NetworkFailure bool
}

// ProcessExitedError reports that the session's child process exited before
// WaitReady observed idle. Err is the process's exit status (from cmd.Wait),
// or nil on a clean exit-before-ready. Match with errors.As; the underlying
// cause is available via the Err field or errors.Is (Unwrap).
type ProcessExitedError struct {
	Err error
}

func (e *ProcessExitedError) Error() string {
	if e.Err != nil {
		return "tuidriver: process exited before ready: " + e.Err.Error()
	}
	return "tuidriver: process exited before ready"
}

func (e *ProcessExitedError) Unwrap() error { return e.Err }

// UnexpectedModalError reports that WaitReady reached idle but the screen shows
// a blocking modal the driver did not expect before its first prompt: a
// recognized modal class other than the benign trust folder, or (since #224) a
// novel dialog whose selection shape is recognized even though its class is not.
// WaitReady returns it instead of a nil-error Readiness so a consumer that reads
// only the top-line signal — in Go, the error — halts rather than typing its
// first prompt into the dialog. This inverts the pre-#173 permissive default,
// under which such a modal rode as an ignorable advisory flag.
//
// Class is the DetectModalClass result that tripped the gate, a value from the
// fixed ModalClass const set (ModalClassUnknown when a novel dialog is caught by
// shape alone). It is never attacker-controlled free text. Match with
// errors.As; a driver that wants to dismiss the modal can recover Class and act.
// There is no wrapped cause, so no Unwrap.
type UnexpectedModalError struct {
	Class ModalClass
}

func (e *UnexpectedModalError) Error() string {
	if e.Class == ModalClassUnknown {
		return "tuidriver: unexpected dialog at startup"
	}
	return "tuidriver: unexpected modal at startup: " + string(e.Class)
}

// WaitReady waits for claude's TUI to reach idle, then classifies the post-idle
// snapshot for the blocking conditions a driver must decide on. Absorbs the
// consumer's WaitUntil(IsIdle) plus the trust / mcp / network one-shot checks
// into a single call. For the benign conditions it only reports, leaving the
// policy to the driver; for a modal that is unexpected before the first prompt
// it fails loudly (see below).
//
// The error is: the context cause on cancellation (the standard
// operator-shutdown collapse); a *ProcessExitedError when the child process
// exits before idle; or, since #173, an *UnexpectedModalError when idle is
// reached but a recognized-but-unexpected modal (or a novel dialog, #224) is up.
// It is nil otherwise. The returned Readiness is the zero value on any non-nil
// error.
func (s *Session) WaitReady(ctx context.Context) (Readiness, error) {
	if err := s.waitUntilOrExit(ctx, func() bool { return IsIdle(s.Snapshot()) }); err != nil {
		return Readiness{}, err
	}
	// Render the post-idle snapshot once and share the one Grid across every
	// blocking-condition check (#225), instead of each predicate rebuilding its
	// own grid from the same bytes.
	snap := s.Snapshot()
	g := NewGrid(snap, 0, 0)
	// #173: invert the pre-first-prompt permissive default. A recognized modal
	// that is not part of a clean startup — or a novel selection dialog (#224) —
	// fails loudly here instead of riding as an advisory flag the consumer may
	// ignore, so a driver that reads only the error never types its first prompt
	// into a dialog. The benign conditions below (trust / mcp-failure / network)
	// stay report-only flags on a nil-error Readiness, per the driver-decides
	// contract.
	if isUnexpectedStartupModalGrid(g, snap) {
		return Readiness{}, &UnexpectedModalError{Class: detectModalClassWithGrid(g, snap)}
	}
	return Readiness{
		Idle:           true,
		TrustModal:     gridHasTrustDialog(g),
		McpFailure:     mcpBannerMatchInRegion(g) != nil,
		FailedMcpCount: mcpCountFromGrid(g),
		NetworkFailure: hasNetworkFailureGrid(g),
	}, nil
}

// isUnexpectedStartupModal reports whether snap shows a modal WaitReady must fail
// loudly on before the first prompt (#173). The trust modal is benign and
// surfaced by Readiness.TrustModal, so it stays false here. Any other recognized
// class is true. ModalClassUnknown is the common no-modal idle screen and stays
// false, UNLESS a novel dialog shape is present (#224): a dialog whose class is
// not recognized but whose selection shape is, which still blocks input.
func isUnexpectedStartupModal(snap []byte) bool {
	return isUnexpectedStartupModalGrid(NewGrid(snap, 0, 0), snap)
}

// isUnexpectedStartupModalGrid is isUnexpectedStartupModal over a Grid the caller
// already rendered. WaitReady threads its single post-idle grid here (#225), so
// the check reuses the same render as the class detection it wraps rather than
// building two more grids (DetectModalClass + the selection-dialog probe).
func isUnexpectedStartupModalGrid(g *Grid, snap []byte) bool {
	switch detectModalClassWithGrid(g, snap) {
	case ModalClassTrustFolder:
		return false
	case ModalClassUnknown:
		return gridHasSelectionDialog(g)
	default:
		return true
	}
}

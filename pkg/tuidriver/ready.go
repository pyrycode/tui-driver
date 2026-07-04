package tuidriver

import "context"

// Readiness reports what claude's TUI is showing once it first reaches idle:
// the input prompt plus any blocking modal or status banner. WaitReady returns
// it so the consumer — not the library — owns the policy. For example: abort on
// a trust modal, continue past an MCP-failure banner, abort on a network
// failure. The library detects; the driver decides.
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

	// NetworkFailure is true when a network-unreachable anchor
	// (FailedToOpenSocket) is present at idle: the claude API is unreachable.
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

// WaitReady waits for claude's TUI to reach idle, then classifies the post-idle
// snapshot for the blocking conditions a driver must decide on. It only
// reports; it never acts on what it finds. Absorbs the consumer's
// WaitUntil(IsIdle) plus the trust / mcp / network one-shot checks into a
// single call.
//
// The error is the context cause on cancellation (the standard
// operator-shutdown collapse) or a *ProcessExitedError when the child process
// exits before idle, and nil otherwise; the returned Readiness is the zero
// value on a non-nil error.
func (s *Session) WaitReady(ctx context.Context) (Readiness, error) {
	if err := s.waitUntilOrExit(ctx, func() bool { return IsIdle(s.Snapshot()) }); err != nil {
		return Readiness{}, err
	}
	snap := s.Snapshot()
	return Readiness{
		Idle:           true,
		TrustModal:     HasTrustModal(snap),
		McpFailure:     HasMcpFailureBanner(snap),
		FailedMcpCount: FailedMcpCount(snap),
		NetworkFailure: HasNetworkFailure(snap),
	}, nil
}

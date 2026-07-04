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

	// UnknownModal is true when a recognized modal class is up at idle that no
	// other Readiness field already surfaces — any DetectModalClass result
	// except ModalClassUnknown (the common no-modal idle screen) and the trust
	// modal (surfaced by TrustModal). It catches a startup dialog the consumer
	// would otherwise type its first prompt into. Detecting a genuinely novel,
	// unclassified dialog is out of scope: this axis rides on the existing
	// class set.
	UnknownModal bool
}

// WaitReady waits for claude's TUI to reach idle, then classifies the post-idle
// snapshot for the blocking conditions a driver must decide on. It only
// reports; it never acts on what it finds. Absorbs the consumer's
// WaitUntil(IsIdle) plus the trust / mcp / network one-shot checks into a
// single call.
//
// The error is WaitUntil's context cause on cancellation (the standard
// operator-shutdown collapse) and nil otherwise; the returned Readiness is the
// zero value on a non-nil error.
func (s *Session) WaitReady(ctx context.Context) (Readiness, error) {
	if err := WaitUntil(ctx, func() bool { return IsIdle(s.Snapshot()) }); err != nil {
		return Readiness{}, err
	}
	snap := s.Snapshot()
	return Readiness{
		Idle:           true,
		TrustModal:     HasTrustModal(snap),
		McpFailure:     HasMcpFailureBanner(snap),
		FailedMcpCount: FailedMcpCount(snap),
		NetworkFailure: HasNetworkFailure(snap),
		UnknownModal:   isUnknownModal(snap),
	}, nil
}

// isUnknownModal reports whether snap shows a recognized modal class that no
// other Readiness field already surfaces. ModalClassUnknown is the common
// no-modal idle screen, and the trust modal is surfaced by Readiness.TrustModal;
// both are excluded so the normal ready path and a trust-only screen stay false.
func isUnknownModal(snap []byte) bool {
	switch DetectModalClass(snap) {
	case ModalClassUnknown, ModalClassTrustFolder:
		return false
	default:
		return true
	}
}

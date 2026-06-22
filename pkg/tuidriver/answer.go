package tuidriver

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// Dismissal re-read bounds. claude re-renders within a few hundred ms of a
// committing keystroke, so DefaultAnswerConfirmTimeout gives generous headroom
// while answerConfirmPoll matches deliver.go's promptCommitPoll cadence.
const (
	// DefaultAnswerConfirmTimeout bounds the dismissal re-read when
	// AnswerModalOpts.ConfirmTimeout is 0.
	DefaultAnswerConfirmTimeout = 2 * time.Second
	answerConfirmPoll           = 150 * time.Millisecond
)

// AnswerModalOpts configures Session.AnswerModal.
type AnswerModalOpts struct {
	// Class is the modal's class as returned by ParseModalContent /
	// DetectModalClass. It selects the per-class keystroke dispatch and is the
	// class the dismissal re-read confirms gone. Only ModalClassPermission and
	// ModalClassTrustFolder are supported today.
	Class ModalClass

	// Choice is the chosen option's Index — claude's rendered 1-based number
	// (ModalOption.Index). The keystroke selects this option. Routing on Index,
	// never the prompt-/tool-influenceable Label, is the #146 security contract.
	Choice int

	// ConfirmTimeout bounds the dismissal re-read poll. 0 picks
	// DefaultAnswerConfirmTimeout.
	ConfirmTimeout time.Duration
}

// AnswerModal sends the keystroke that selects opts.Choice for opts.Class's
// modal, then re-reads the screen to confirm the modal dismissed. Returns nil
// once the modal is no longer detected; returns an error if the keystroke could
// not be sent, the class is unsupported, the choice is invalid, or the modal is
// still present after ConfirmTimeout (so the caller can retry or degrade rather
// than assume success). Mirrors DeliverPrompt's deliver→re-read→confirm
// structure and its injectable seams.
//
// The answer routes only on the abstract Class + Choice — never claude's screen
// text or raw key bytes. The permission decision (which option, who may answer,
// timeout/audit) stays in the consumer (ADR 025); this owns only the
// screen↔keystroke mechanics.
func (s *Session) AnswerModal(ctx context.Context, opts AnswerModalOpts) error {
	return answerModal(opts, answerDeps{
		answer: s.Answer,
		dismissed: func(timeout time.Duration) bool {
			return modalDismissed(ctx, opts.Class, s.Snapshot, timeout)
		},
	})
}

// answerDeps are the seams answerModal drives. AnswerModal wires the real
// session methods; tests inject fakes to script the keystroke + dismissal
// outcome without a live PTY. Mirrors deliverDeps.
//
// Today only answer is needed (both supported classes are number-select). A
// future non-number-select class adds its own primitive seam (e.g. navigate,
// sendEsc) when it is fixtured — not before (see answerModal's per-class switch).
type answerDeps struct {
	answer    func(choice string) error        // → s.Answer (number-select keystroke)
	dismissed func(timeout time.Duration) bool // → modalDismissed over s.Snapshot
}

// answerModal dispatches the per-class keystroke for opts then confirms the
// modal dismissed via deps.dismissed. The pure driver the tests call directly
// with fakes.
func answerModal(opts AnswerModalOpts, deps answerDeps) error {
	timeout := opts.ConfirmTimeout
	if timeout <= 0 {
		timeout = DefaultAnswerConfirmTimeout
	}

	switch opts.Class {
	case ModalClassPermission, ModalClassTrustFolder:
		// Number-select strategy: the chosen Index IS the keystroke. For
		// Choice == 1 this is byte-identical to AcceptTrust's "1\r", and it also
		// honours Choice == 2 (decline) which AcceptTrust cannot. A future
		// non-number-select class (y/n, arrow+enter) adds its own case here with
		// a different keystroke strategy (and its seam to answerDeps) — but only
		// once it has a fixtured reader. Do NOT pre-build unfixtured classes
		// (#146 follow-up: destructive / plan-confirm deferred until fixtured).
		if opts.Choice < 1 {
			return fmt.Errorf("tuidriver: AnswerModal: choice %d is invalid (Index is 1-based)", opts.Choice)
		}
		if err := deps.answer(strconv.Itoa(opts.Choice)); err != nil {
			return fmt.Errorf("tuidriver: AnswerModal: send keystroke for choice %d: %w", opts.Choice, err)
		}
	default:
		// Send nothing: a number-select keystroke must not reach a class that
		// uses a different selection strategy.
		return fmt.Errorf("tuidriver: AnswerModal: unsupported modal class %q", opts.Class)
	}

	if deps.dismissed(timeout) {
		return nil
	}
	return fmt.Errorf("tuidriver: AnswerModal: %s modal still present after answering choice %d", opts.Class, opts.Choice)
}

// modalDismissed polls the screen until the answered modal class is no longer
// detected (the dismissal signal), the timeout elapses, or ctx is cancelled. It
// only observes — never writes. snapshot is the injectable re-read source.
//
// The dismissal signal is class-level, not content-level: DetectModalClass reads
// claude's structural anchors (cheap, no Render), not the prompt-/tool-
// influenceable option text, so a hostile screen cannot spoof "dismissed". Any
// non-matching screen — idle, or a different modal that popped after the answer
// landed — counts as dismissed: the class the operator answered is gone. Mirrors
// promptDidCommit's poll structure exactly.
func modalDismissed(ctx context.Context, class ModalClass, snapshot func() []byte, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tk := time.NewTicker(answerConfirmPoll)
	defer tk.Stop()
	for {
		if DetectModalClass(snapshot()) != class {
			return true
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

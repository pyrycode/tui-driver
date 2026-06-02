package tuidriver

// ArrowKey identifies one of the four cursor-arrow keys for Navigate. The
// wire form is the CSI sequence claude's TUI expects: ESC [ A/B/C/D.
type ArrowKey int

const (
	ArrowUp ArrowKey = iota
	ArrowDown
	ArrowLeft
	ArrowRight
)

// bytes returns the CSI escape sequence for k. An unknown value returns nil,
// so Navigate writes nothing rather than a malformed sequence.
func (k ArrowKey) bytes() []byte {
	switch k {
	case ArrowUp:
		return []byte{0x1b, '[', 'A'}
	case ArrowDown:
		return []byte{0x1b, '[', 'B'}
	case ArrowLeft:
		return []byte{0x1b, '[', 'D'}
	case ArrowRight:
		return []byte{0x1b, '[', 'C'}
	default:
		return nil
	}
}

// writeRaw is the single internal PTY-write path that every typed-keystroke
// method and the prompt-delivery helpers funnel through. Unexported on
// purpose: with the public Write seam gone, a consumer cannot inject arbitrary
// bytes — only the named intents below and the constrained SendKeys hatch.
func (s *Session) writeRaw(p []byte) error {
	_, err := s.pty.Write(p)
	return err
}

// AcceptTrust answers claude's trust-folder modal ("Quick safety check: Is
// this a project you created or one you trust?"): the "1" option, meaning
// "Yes, I trust this folder", plus the \r commit. Pre-marking the workdir
// trusted is the better path; this is for spikes that drive the modal live.
func (s *Session) AcceptTrust() error {
	return s.writeRaw([]byte("1\r"))
}

// Answer sends a modal choice followed by the \r commit. choice is the literal
// option token claude renders, e.g. "1", "2", or "y". Pass "" to send a bare
// \r (accept the default). To dismiss a modal instead of answering, use
// SendEsc.
func (s *Session) Answer(choice string) error {
	return s.writeRaw([]byte(choice + "\r"))
}

// SendEsc sends a single ESC (0x1b): cancel an in-flight turn, or dismiss a
// modal or picker.
func (s *Session) SendEsc() error {
	return s.writeRaw([]byte{0x1b})
}

// Navigate moves a picker or modal selection by one cursor-arrow keystroke.
func (s *Session) Navigate(k ArrowKey) error {
	return s.writeRaw(k.bytes())
}

// SendKeys writes raw key bytes to the PTY. It is the deliberately narrow
// escape hatch for spike binaries that need a keystroke the named methods
// above do not cover: the "/" picker-open trigger, a down-arrow-then-commit
// combo ("\x1b[B\r"), a double-ESC, or a Ctrl-C. It is NOT a general write
// seam and NOT for prompt delivery — use DeliverPrompt for prompts. Not
// intended for production drivers.
func (s *Session) SendKeys(keys string) error {
	return s.writeRaw([]byte(keys))
}

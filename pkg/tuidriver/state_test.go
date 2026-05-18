package tuidriver

import "testing"

func TestIsIdleBareInputPrompt(t *testing.T) {
	// ❯ alone → idle.
	in := []byte("\xe2\x9d\xaf Try \"refactor <filepath>\"")
	if !IsIdle(in) {
		t.Errorf("IsIdle(bare input) = false, want true")
	}
}

func TestIsIdleSpinnerActiveNotIdle(t *testing.T) {
	// ❯ + ✻ → thinking, not idle.
	in := []byte("\xe2\x9c\xbb Baked for 2s\n\xe2\x9d\xaf [redrawn input line]")
	if IsIdle(in) {
		t.Errorf("IsIdle(spinner+input) = true, want false")
	}
}

func TestIsIdleNoInputPromptNotIdle(t *testing.T) {
	// claude not yet at first prompt → no ❯ → not idle.
	in := []byte("welcome banner without input line")
	if IsIdle(in) {
		t.Errorf("IsIdle(no prompt) = true, want false")
	}
}

func TestIsIdleEmpty(t *testing.T) {
	if IsIdle(nil) {
		t.Errorf("IsIdle(nil) = true, want false")
	}
	if IsIdle([]byte{}) {
		t.Errorf("IsIdle([]byte{}) = true, want false")
	}
}

func TestIsIdleStripsANSIBeforeChecking(t *testing.T) {
	// Glyphs wrapped in CSI — strip should expose them.
	in := []byte("\x1b[38;5;246m\xe2\x9d\xaf\x1b[39m hint text")
	if !IsIdle(in) {
		t.Errorf("IsIdle(ANSI-wrapped ❯) = false, want true")
	}
}

func TestIsThinkingPositive(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"class A — verb + counter", []byte("\xe2\x9c\xbb Baked for 2s")},
		{"class B — verb + ellipsis", []byte("\xe2\x9c\xbb Channeling…")},
		{"class C — verb + tokens", []byte("\xe2\x9c\xbb Actualizing… (2s · ↓1 tokens)")},
		{"CSI-wrapped", []byte("\x1b[38;5;174m\xe2\x9c\xbb\x1b[39m Thinking for 1s")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !IsThinking(tc.in) {
				t.Errorf("IsThinking(%q) = false, want true", tc.in)
			}
		})
	}
}

func TestIsThinkingNegative(t *testing.T) {
	if IsThinking(nil) {
		t.Errorf("IsThinking(nil) = true, want false")
	}
	if IsThinking([]byte("idle prompt no spinner")) {
		t.Errorf("IsThinking(no spinner) = true, want false")
	}
}

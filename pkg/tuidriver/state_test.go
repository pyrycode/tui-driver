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

func TestParseSpinnerTokensClassC(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want int
	}{
		{
			"single-digit",
			[]byte("\xe2\x9c\xbb Actualizing… (2s · ↓1 tokens)"),
			1,
		},
		{
			"multi-digit",
			[]byte("\xe2\x9c\xbb Embellishing… (3s · ↓247 tokens)"),
			247,
		},
		{
			"singular token (1 token, not tokens)",
			[]byte("\xe2\x9c\xbb Generating… (1s · ↓1 token)"),
			1,
		},
		{
			"CSI-wrapped (live spinner rendering)",
			[]byte("\x1b[38;5;174m\xe2\x9c\xbb\x1b[39m Cooked… (4s · ↓512 tokens)"),
			512,
		},
		{
			"multiple matches in buffer — return last",
			[]byte("✻ … (1s · ↓50 tokens)... later ✻ … (3s · ↓200 tokens)"),
			200,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseSpinnerTokens(tc.in)
			if !ok {
				t.Fatalf("ParseSpinnerTokens(%q) returned ok=false", tc.in)
			}
			if got != tc.want {
				t.Errorf("ParseSpinnerTokens(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseSpinnerTokensClassAB(t *testing.T) {
	// Classes A (`✻ Baked for 2s`) and B (`✻ Channeling…`) don't include
	// token counts. Parser should return ok=false.
	cases := [][]byte{
		[]byte("\xe2\x9c\xbb Baked for 2s"),
		[]byte("\xe2\x9c\xbb Channeling…"),
		[]byte("\xe2\x9c\xbb Sautéed for 5s"),
	}
	for _, in := range cases {
		if n, ok := ParseSpinnerTokens(in); ok {
			t.Errorf("ParseSpinnerTokens(%q) = (%d, true), want (0, false)", in, n)
		}
	}
}

func TestParseSpinnerTokensNegative(t *testing.T) {
	if _, ok := ParseSpinnerTokens(nil); ok {
		t.Errorf("ParseSpinnerTokens(nil) returned ok=true")
	}
	if _, ok := ParseSpinnerTokens([]byte("idle ❯ no spinner")); ok {
		t.Errorf("ParseSpinnerTokens(idle) returned ok=true")
	}
}

func TestParseSpinnerTokensIgnoresUnrelatedTokensText(t *testing.T) {
	// Text mentioning "tokens" without the ↓ arrow should NOT match.
	if _, ok := ParseSpinnerTokens([]byte("CLAUDE.md: 9 tokens")); ok {
		t.Errorf("matched unrelated 'tokens' text")
	}
}

func TestParseSpinnerClassAMatches(t *testing.T) {
	cases := []struct {
		name     string
		in       []byte
		wantVerb string
		wantSecs int
	}{
		{
			"class A — single-word verb",
			[]byte("\xe2\x9c\xbb Baked for 2s"),
			"Baked",
			2,
		},
		{
			"class A — two-word verb",
			[]byte("\xe2\x9c\xbb Quick witted for 13s"),
			"Quick witted",
			13,
		},
		{
			"class A — minutes + seconds",
			[]byte("\xe2\x9c\xbb Baked for 2m 5s"),
			"Baked",
			125,
		},
		{
			"class A — ANSI-wrapped spinner glyph",
			[]byte("\x1b[2K\x1b[1G\xe2\x9c\xbb Baked for 2s"),
			"Baked",
			2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verb, secs, ok := ParseSpinner(tc.in)
			if !ok {
				t.Fatalf("ParseSpinner(%q) returned ok=false", tc.in)
			}
			if verb != tc.wantVerb {
				t.Errorf("verb = %q, want %q", verb, tc.wantVerb)
			}
			if secs != tc.wantSecs {
				t.Errorf("totalSeconds = %d, want %d", secs, tc.wantSecs)
			}
		})
	}
}

func TestParseSpinnerNonClassA(t *testing.T) {
	// Classes B (`✻ Channeling…`), C (`✻ Verb… (Ns · ↓N tokens)`), and
	// snapshots without the spinner glyph all return ok=false. The
	// regex requires the literal `for` keyword that only class A emits.
	cases := []struct {
		name string
		in   []byte
	}{
		{"class B — ellipsis only", []byte("\xe2\x9c\xbb Channeling…")},
		{"class C — parens + bullet", []byte("\xe2\x9c\xbb Actualizing… (2s · ↓1 tokens)")},
		{"no spinner glyph", []byte("\xe2\x9d\xaf ready")},
		{"empty snapshot", []byte("")},
		{"nil snapshot", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verb, secs, ok := ParseSpinner(tc.in)
			if ok {
				t.Errorf("ParseSpinner(%q) = (%q, %d, true), want (\"\", 0, false)", tc.in, verb, secs)
			}
			if verb != "" || secs != 0 {
				t.Errorf("ParseSpinner(%q) returned non-zero (%q, %d) with ok=false", tc.in, verb, secs)
			}
		})
	}
}

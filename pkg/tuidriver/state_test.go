package tuidriver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gridRows joins rows with "\r\n" so vt10x renders them as fresh rows. A bare
// "\n" is line-feed-only through the emulator and renders a staircase, not
// separate rows (codebase/150.md fixture invariant).
func gridRows(rows ...string) []byte {
	return []byte(strings.Join(rows, "\r\n"))
}

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
		// claude 2.1.199 renders the live thinking spinner with a new glyph
		// ✳ (U+2733) instead of ✻ (U+273B), e.g.
		// `✳ Shenaniganing… (113 tokens · thought for 53s)`. Verified from a
		// real PTY recording of a wedged agent run (tui-driver #152, 2026-07-04).
		{"2.1.199 ✳ glyph", []byte("\xe2\x9c\xb3 Shenaniganing… (113 tokens · thought for 53s)")},
		{"2.1.199 ✳ CSI-wrapped", []byte("\x1b[38;2;153;153;153m\xe2\x9c\xb3\x1b[39m Distilling… (5s)")},
		// claude's spinner is an ANIMATION cycling through five sparkle glyphs,
		// not one: ✻ ✳ ✢ ✶ ✽. A corpus scan of 843 PTY recordings (2026-07-04)
		// found ✢ (U+2722) the second-most-common frame, with ✶ (U+2736) and ✽
		// (U+273D) also unrecognised. Each frame alone must read as thinking.
		{"spinner frame ✢", []byte("\xe2\x9c\xa2 Vibing… (3s)")},
		{"spinner frame ✶", []byte("\xe2\x9c\xb6 Waddling… (12s · ↓ 40 tokens)")},
		{"spinner frame ✽", []byte("\xe2\x9c\xbd Sautéing… (1m 4s)")},
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

func TestIsIdleSpinnerGlyphInTranscriptNotThinking(t *testing.T) {
	// AC #3a — a literal ✻ printed as transcript content (rendered markdown /
	// tool output) high above the overlay must NOT forge "thinking" while a
	// real idle input line sits at the bottom. Region-scoping ignores the
	// stale glyph; whole-buffer substring matching would report thinking.
	snap := gridRows(
		"✻ Baked for 2s",                        // literal spinner glyph in transcript output
		"(the line above is rendered markdown,", // ... well above the status overlay
		"not the live status spinner)",
		"transcript line",
		"transcript line",
		"transcript line",
		"transcript line",
		"transcript line",
		"❯ ready for the next prompt", // the real idle input line, at the bottom
		"  ? for shortcuts",
	)
	if !IsIdle(snap) {
		t.Errorf("IsIdle(spinner-in-transcript) = false, want true")
	}
	if IsThinking(snap) {
		t.Errorf("IsThinking(spinner-in-transcript) = true, want false")
	}
	// Contrast: a raw substring match over the same bytes forges thinking.
	if !strings.Contains(string(snap), string(SpinnerGlyph)) {
		t.Fatalf("fixture no longer contains ✻ — the forgery contrast is void")
	}
}

func TestIsIdleScrolledPromptGlyphNotIdle(t *testing.T) {
	// AC #3b — a stale ❯ from a previous turn's input line, now scrolled up
	// out of the status region while a turn is running, must NOT forge "idle".
	// The bottom rows carry mid-turn transcript, no ❯. Whole-buffer substring
	// matching would report idle and let the consumer write into a live turn.
	snap := gridRows(
		"❯ a previous turn's prompt, now scrolled up", // stale ❯ high in history
		"assistant is still working on that turn",
		"transcript line",
		"transcript line",
		"transcript line",
		"transcript line",
		"transcript line",
		"transcript line",
		"tool result: still streaming output", // bottom rows: no ❯, no ✻
		"more streaming output",
	)
	if IsIdle(snap) {
		t.Errorf("IsIdle(scrolled-prompt) = true, want false")
	}
	// Contrast: a raw substring match over the same bytes forges idle.
	if !strings.Contains(string(snap), string(IdleGlyph)) {
		t.Fatalf("fixture no longer contains ❯ — the forgery contrast is void")
	}
}

func TestIsThinkingRealisticLayoutPinsRegion(t *testing.T) {
	// Pins the lower bound of statusRegionRows. Mirrors a real thinking frame:
	// the spinner renders adjacent above the redrawn input box, landing at
	// ~row -5 from the bottom (box border/hint below the ❯ line). The region
	// must be large enough to reach it — this fails RED if statusRegionRows is
	// sized too small.
	snap := gridRows(
		"transcript line",
		"transcript line",
		"assistant working on the turn",
		"✻ Simmering… (7s)", // spinner, adjacent above the input box (-5)
		"╭────────────────╮", // input box top border (-4)
		"❯", // redrawn input line beneath the spinner (-3)
		"╰────────────────╯", // input box bottom border (-2)
		"  ? for shortcuts", // hint bar (-1)
	)
	if !IsThinking(snap) {
		t.Errorf("IsThinking(realistic thinking layout) = false, want true")
	}
	if IsIdle(snap) {
		t.Errorf("IsIdle(realistic thinking layout) = true, want false")
	}
}

func TestIsThinkingInterruptHintDrivesBusy(t *testing.T) {
	// AC #3 — the "esc to interrupt" hint alone, with NO spinner glyph anywhere,
	// classifies busy through the same region-scoped grid path as the spinner.
	// This is the second, independent busy anchor: a change to claude's spinner
	// glyph set can no longer silently flip the busy check to idle mid-turn.
	snap := gridRows(
		"transcript line",
		"assistant working on the turn",
		"esc to interrupt", // the interrupt hint in the status region, no ✻
		"╭────────────────╮", // input box top border
		"❯", // input line redrawn beneath the running turn (-3)
		"╰────────────────╯", // input box bottom border
		"  ? for shortcuts", // hint bar (-1)
	)
	if !IsThinking(snap) {
		t.Errorf("IsThinking(interrupt-hint layout) = false, want true")
	}
	// ❯ is present (redrawn beneath the running turn) yet the hint alone must
	// still flip busy — this is the load-bearing assertion: the hint, not the
	// spinner, is doing the work.
	if IsIdle(snap) {
		t.Errorf("IsIdle(interrupt-hint layout) = true, want false")
	}
	if strings.Contains(string(snap), string(SpinnerGlyph)) {
		t.Fatalf("fixture unexpectedly contains ✻ — the second-anchor test is void")
	}
}

func TestIsIdleInterruptHintInTranscriptNotThinking(t *testing.T) {
	// AC #4 — the literal string "esc to interrupt" printed as transcript
	// content (a hostile prompt or tool result can print it) high above the
	// overlay must NOT forge "thinking" while a real idle input line sits at
	// the bottom. Mirrors TestIsIdleSpinnerGlyphInTranscriptNotThinking: the
	// new anchor is region-scoped, so only a whole-buffer substring path would
	// forge busy here.
	snap := gridRows(
		"a tool result printed the literal words esc to interrupt", // forged hint, high in history
		"(the line above is transcript body, not the live status)",
		"transcript line",
		"transcript line",
		"transcript line",
		"transcript line",
		"transcript line",
		"transcript line",
		"❯ ready for the next prompt", // the real idle input line, at the bottom
		"  ? for shortcuts",
	)
	if !IsIdle(snap) {
		t.Errorf("IsIdle(hint-in-transcript) = false, want true")
	}
	if IsThinking(snap) {
		t.Errorf("IsThinking(hint-in-transcript) = true, want false")
	}
	// Contrast: a raw substring match over the same bytes forges thinking; only
	// region-scoping gets it right.
	if !strings.Contains(string(snap), InterruptHint) {
		t.Fatalf("fixture no longer contains %q — the forgery contrast is void", InterruptHint)
	}
}

func TestIsThinkingPickerCapturesNotBusy(t *testing.T) {
	// The full-phrase anchor must not false-positive on benign in-region
	// content. Both committed slash-picker captures render "…without
	// interrupting the main conversation" (the /btw description) inside the
	// 6-row status region — a shortened anchor ("interrupt", "to interrupt")
	// would flip them to busy. The full phrase "esc to interrupt" does not
	// occur there, so they stay not-busy. Pins the full-phrase decision against
	// a future shortening of InterruptHint.
	for _, name := range []string{"picker-snapshot.bin", "picker-truecolor-snapshot.bin"} {
		snap, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("read fixture %s: %v", name, err)
		}
		if IsThinking(snap) {
			t.Errorf("IsThinking(%s) = true, want false", name)
		}
	}
}

func TestIsIdleRealCaptureUnchanged(t *testing.T) {
	// AC #4 — a committed real capture that carries a genuine on-screen input
	// line (❯ at row -3) classifies idle, unchanged from the whole-buffer era.
	snap, err := os.ReadFile(filepath.Join("testdata", "mcp-empty-snapshot.bin"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if !IsIdle(snap) {
		t.Errorf("IsIdle(mcp-empty-snapshot.bin) = false, want true")
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

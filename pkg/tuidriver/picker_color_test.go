package tuidriver

import "testing"

func TestXterm256ToRGB(t *testing.T) {
	cases := []struct {
		name string
		idx  uint8
		want rgb
	}{
		// Base-16 region (system + bright ANSI).
		{"index 0 black", 0, rgb{0, 0, 0}},
		{"index 15 bright white", 15, rgb{0xff, 0xff, 0xff}},
		// Color-cube corners and a known shade.
		{"index 16 cube origin (black)", 16, rgb{0, 0, 0}},
		{"index 196 cube pure red", 196, rgb{255, 0, 0}},
		{"index 231 cube pure white", 231, rgb{255, 255, 255}},
		// Grayscale ramp endpoints.
		{"index 232 gray min", 232, rgb{8, 8, 8}},
		{"index 255 gray max", 255, rgb{238, 238, 238}},
		// Empirical picker shades.
		{"index 153 (legacy highlight)", 153, rgb{175, 215, 255}},
		{"index 246 (legacy normal)", 246, rgb{148, 148, 148}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := xterm256ToRGB(tc.idx)
			if got != tc.want {
				t.Errorf("xterm256ToRGB(%d) = %+v, want %+v", tc.idx, got, tc.want)
			}
		})
	}
}

func TestParseForegroundSGR(t *testing.T) {
	type want struct {
		color    rgb
		consumed int
		isFg     bool
		isReset  bool
	}
	cases := []struct {
		name string
		in   []byte
		want want
	}{
		{
			"indexed fg color 153",
			[]byte("\x1b[38;5;153mrest"),
			want{rgb{175, 215, 255}, len("\x1b[38;5;153m"), true, false},
		},
		{
			"truecolor fg 177,185,249",
			[]byte("\x1b[38;2;177;185;249mrest"),
			want{rgb{177, 185, 249}, len("\x1b[38;2;177;185;249m"), true, false},
		},
		{
			"truecolor with out-of-range clamps",
			[]byte("\x1b[38;2;999;0;-5mrest"),
			want{rgb{255, 0, 0}, len("\x1b[38;2;999;0;-5m"), true, false},
		},
		{
			"fg reset (39)",
			[]byte("\x1b[39mrest"),
			want{rgb{}, len("\x1b[39m"), false, true},
		},
		{
			"unrelated style SGR (bold)",
			[]byte("\x1b[1mrest"),
			want{rgb{}, len("\x1b[1m"), false, false},
		},
		{
			"unrelated CSI cursor-forward",
			[]byte("\x1b[43Crest"),
			want{rgb{}, len("\x1b[43C"), false, false},
		},
		{
			"not a CSI prefix",
			[]byte("rest"),
			want{rgb{}, 0, false, false},
		},
		{
			"CSI with no terminating final byte",
			[]byte("\x1b[38;5;153"),
			want{rgb{}, 0, false, false},
		},
		{
			// ECMA-48 empty-param-defaults-to-zero rule: `38;5;` parses as
			// `38;5;0` (palette index 0 = black). Documented here as the
			// walker's contract — black is harmless because it's not in
			// pickerHighlightedRGBs.
			"empty trailing param parses as 0 per SGR spec",
			[]byte("\x1b[38;5;mrest"),
			want{rgb{0, 0, 0}, len("\x1b[38;5;m"), true, false},
		},
		{
			"unrecognised fg shape (hypothetical 38;6 encoding)",
			[]byte("\x1b[38;6;1;2;3mrest"),
			want{rgb{}, len("\x1b[38;6;1;2;3m"), false, false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			color, consumed, isFg, isReset := parseForegroundSGR(tc.in)
			got := want{color, consumed, isFg, isReset}
			if got != tc.want {
				t.Errorf("parseForegroundSGR(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestRGBIsHighlighted(t *testing.T) {
	cases := []struct {
		name string
		c    rgb
		want bool
	}{
		{"legacy indexed highlight", rgb{175, 215, 255}, true},
		{"truecolor highlight", rgb{177, 185, 249}, true},
		{"normal gray", rgb{153, 153, 153}, false},
		{"zero (unknown/default)", rgb{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rgbIsHighlighted(tc.c); got != tc.want {
				t.Errorf("rgbIsHighlighted(%+v) = %v, want %v", tc.c, got, tc.want)
			}
		})
	}
}

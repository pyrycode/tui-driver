package tuidriver

import "strconv"

// rgb is a package-internal 24-bit color triplet. Used by the picker
// row-finder to classify highlight state by parsed color rather than by
// captured SGR string (ticket #90). Not exported — callers interact with
// PickerItem.Highlighted, never with raw colors.
type rgb struct{ R, G, B uint8 }

// xterm256Base16 is the system + bright ANSI subset of the xterm-256
// palette. The remaining 240 entries (cube + grayscale ramp) are
// computed arithmetically in xterm256ToRGB; only 0–15 need a literal
// because their values are historical accidents, not formulas.
var xterm256Base16 = [16]rgb{
	{0x00, 0x00, 0x00}, // 0  black
	{0x80, 0x00, 0x00}, // 1  red
	{0x00, 0x80, 0x00}, // 2  green
	{0x80, 0x80, 0x00}, // 3  yellow
	{0x00, 0x00, 0x80}, // 4  blue
	{0x80, 0x00, 0x80}, // 5  magenta
	{0x00, 0x80, 0x80}, // 6  cyan
	{0xc0, 0xc0, 0xc0}, // 7  white
	{0x80, 0x80, 0x80}, // 8  bright black
	{0xff, 0x00, 0x00}, // 9  bright red
	{0x00, 0xff, 0x00}, // 10 bright green
	{0xff, 0xff, 0x00}, // 11 bright yellow
	{0x00, 0x00, 0xff}, // 12 bright blue
	{0xff, 0x00, 0xff}, // 13 bright magenta
	{0x00, 0xff, 0xff}, // 14 bright cyan
	{0xff, 0xff, 0xff}, // 15 bright white
}

// xterm256CubeLevels maps each 0–5 component of a 6×6×6 cube index to its
// RGB level. The pattern (0, 95, 135, 175, 215, 255) is xterm's standard.
var xterm256CubeLevels = [6]uint8{0, 95, 135, 175, 215, 255}

// xterm256ToRGB returns the RGB triplet for a given xterm-256 palette
// index. Region rules:
//
//   - 0–15:   fixed lookup table (system + bright ANSI).
//   - 16–231: 6×6×6 color cube. n = 16 + 36*r + 6*g + b, each component
//     ∈ {0..5} mapping via xterm256CubeLevels.
//   - 232–255: grayscale ramp. gray = 8 + 10*(n-232), applied to R/G/B.
func xterm256ToRGB(n uint8) rgb {
	if n < 16 {
		return xterm256Base16[n]
	}
	if n < 232 {
		i := int(n) - 16
		r := xterm256CubeLevels[i/36]
		g := xterm256CubeLevels[(i/6)%6]
		b := xterm256CubeLevels[i%6]
		return rgb{r, g, b}
	}
	gray := uint8(8 + 10*(int(n)-232))
	return rgb{gray, gray, gray}
}

// pickerHighlightedRGBs is the set of foreground RGB values claude paints
// the highlighted picker row in. claude has shipped at least two distinct
// shades during the indexed→truecolor migration; future palette tweaks
// extend this set rather than touching the row anchor or classifier.
//
//   - {175, 215, 255}: xterm-256 index 153 (older indexed-color renderer).
//   - {177, 185, 249}: claude 2.1.150 truecolor renderer.
var pickerHighlightedRGBs = []rgb{
	{175, 215, 255},
	{177, 185, 249},
}

// rgbIsHighlighted reports whether c matches any of the known highlighted
// shades. Equality, not nearness — claude paints deterministic constants
// per render; we extend the set rather than fuzzy-matching.
func rgbIsHighlighted(c rgb) bool {
	for _, h := range pickerHighlightedRGBs {
		if c == h {
			return true
		}
	}
	return false
}

// parseForegroundSGR recognises a foreground-color or color-reset CSI
// sequence at the start of b. Returns:
//
//   - color:    the parsed RGB (meaningful only when isFg==true)
//   - consumed: byte length of the recognised sequence including ESC `[`
//     and the trailing `m`; 0 if b does not start with ESC `[`.
//   - isFg:     true when the sequence sets a foreground color (38;5;N or
//     38;2;R;G;B). Mutually exclusive with isReset.
//   - isReset:  true when the sequence clears foreground to default (39).
//
// Any other CSI sequence (style toggles, cursor movement, etc.) is
// recognised — consumed reflects its length — but both isFg and isReset
// are false, telling the walker to advance past it without changing color
// state. Malformed shapes (missing parameters, non-numeric tokens) return
// consumed reflecting whatever the scanner ate up to the final byte; the
// walker treats them as no-ops.
func parseForegroundSGR(b []byte) (color rgb, consumed int, isFg, isReset bool) {
	if len(b) < 2 || b[0] != 0x1b || b[1] != '[' {
		return rgb{}, 0, false, false
	}
	// Find the final byte ([a-zA-Z]) terminating the CSI.
	end := -1
	for i := 2; i < len(b); i++ {
		c := b[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			end = i
			break
		}
	}
	if end < 0 {
		return rgb{}, 0, false, false
	}
	consumed = end + 1
	// Only the SGR final byte 'm' carries color information; everything
	// else (movement, mode toggles) is a no-op for the color walker.
	if b[end] != 'm' {
		return rgb{}, consumed, false, false
	}
	params := parseSGRParams(b[2:end])
	switch {
	case len(params) >= 5 && params[0] == 38 && params[1] == 2:
		return rgb{clampByte(params[2]), clampByte(params[3]), clampByte(params[4])}, consumed, true, false
	case len(params) >= 3 && params[0] == 38 && params[1] == 5:
		return xterm256ToRGB(clampByte(params[2])), consumed, true, false
	case len(params) >= 1 && params[0] == 39:
		return rgb{}, consumed, false, true
	}
	return rgb{}, consumed, false, false
}

// parseSGRParams splits a semicolon-separated parameter list into ints.
// Empty tokens (`;;`) become 0, matching SGR conventions. Tokens that
// fail to parse are dropped — the caller treats a too-short slice as a
// malformed sequence and no-ops.
func parseSGRParams(s []byte) []int {
	if len(s) == 0 {
		return nil
	}
	var out []int
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ';' {
			tok := s[start:i]
			start = i + 1
			if len(tok) == 0 {
				out = append(out, 0)
				continue
			}
			n, err := strconv.Atoi(string(tok))
			if err != nil {
				continue
			}
			out = append(out, n)
		}
	}
	return out
}

// clampByte squashes an SGR parameter into the uint8 range. SGR truecolor
// components are spec'd 0–255, but malformed inputs (e.g. 999) are clamped
// rather than wrapping — surprises in production are worse than a slight
// color mismatch.
func clampByte(n int) uint8 {
	if n < 0 {
		return 0
	}
	if n > 255 {
		return 255
	}
	return uint8(n)
}

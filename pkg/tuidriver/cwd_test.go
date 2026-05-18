package tuidriver

import "testing"

func TestEncodeCwd(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"pure alnum", "abc123XYZ", "abc123XYZ"},
		{"single slash", "/", "-"},
		{"path with slashes", "/Users/me/code", "-Users-me-code"},
		{"adjacent specials produce adjacent hyphens", ") [", "---"},
		{"dot and space mapped", "v1.2 beta", "v1-2-beta"},
		{"underscore is non-alnum", "snake_case", "snake-case"},
		{"unicode bytes mapped per-byte to hyphen", "café", "caf--"},
		{
			"loop 2 B-4 reference case",
			"/private/tmp/encode test (with) [brackets] & amp+plus_under",
			"-private-tmp-encode-test--with---brackets----amp-plus-under",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EncodeCwd(tc.in)
			if got != tc.want {
				t.Errorf("EncodeCwd(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

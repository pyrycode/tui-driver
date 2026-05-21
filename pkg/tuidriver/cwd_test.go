package tuidriver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncodeCwd(t *testing.T) {
	// Cases below do not name on-disk directories (with the exception of "/"),
	// so they exercise both the byte-transform and the post-#57 fallback
	// contract: canonicalisePath returns ok=false → encode input as-passed.
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"pure alnum", "abc123XYZ", "abc123XYZ"},
		{"single slash", "/", "-"},
		// Rewritten in #57: original input was "/Users/me/code", which is
		// filesystem-dependent (may exist on a developer's Mac). The new
		// input is reliably non-existent so the test stays deterministic
		// across machines while still exercising the path-separator
		// byte-transform.
		{"path with slashes (non-existent → fallback)", "/tui-driver-test-fixture-does-not-exist/me/code", "-tui-driver-test-fixture-does-not-exist-me-code"},
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

// byteTransform applies the post-canonicalisation byte-by-byte hyphen
// mapping used by EncodeCwd. Tests use it to compute expected outputs
// from a canonicalised path without re-implementing the rule.
func byteTransform(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func TestEncodeCwd_RealpathHappyPath(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "workdir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Compute the expected output by canonicalising via EvalSymlinks
	// — on darwin t.TempDir() lives under /var which is a symlink to
	// /private/var, so the canonical form differs from the literal dir.
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	want := byteTransform(canonical)
	got := EncodeCwd(dir)
	if got != want {
		t.Errorf("EncodeCwd(%q) = %q, want %q (canonical=%q)", dir, got, want, canonical)
	}
}

func TestEncodeCwd_NonexistentPath(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "this-does-not-exist")
	want := byteTransform(p)
	got := EncodeCwd(p)
	if got != want {
		t.Errorf("EncodeCwd(%q) = %q, want %q (fallback should encode input as-passed)", p, got, want)
	}
}

func TestEncodeCwd_SymlinkResolution(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	viaLink := EncodeCwd(link)
	viaTarget := EncodeCwd(target)
	if viaLink != viaTarget {
		t.Errorf("EncodeCwd(link=%q) = %q, want %q (= EncodeCwd(target))", link, viaLink, viaTarget)
	}
}

func TestEncodeCwd_CaseCanonicalisation(t *testing.T) {
	tmp := t.TempDir()
	// Probe the filesystem for case-insensitivity. If a differently-cased
	// stat of an existing dir fails, the FS is case-sensitive and the
	// assertion below would be vacuous (the fallback would fire on ENOENT).
	probe := filepath.Join(tmp, "CaseProbe")
	if err := os.MkdirAll(probe, 0o755); err != nil {
		t.Fatalf("mkdir probe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "caseprobe")); err != nil {
		t.Skip("case-sensitive filesystem — assertion would be vacuous")
	}

	dir := filepath.Join(tmp, "Foo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir Foo: %v", err)
	}
	lowered := filepath.Join(tmp, "foo")
	got := EncodeCwd(lowered)
	if !strings.Contains(got, "-Foo") {
		t.Errorf("EncodeCwd(%q) = %q, want output to contain canonical-case %q", lowered, got, "-Foo")
	}
	if strings.Contains(got, "-foo") {
		t.Errorf("EncodeCwd(%q) = %q, want output NOT to contain lowercased %q", lowered, got, "-foo")
	}
}

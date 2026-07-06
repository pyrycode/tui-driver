package main

import (
	"reflect"
	"testing"
)

// TestReferenceTransforms pins the three claude-free cwd-encoding reference
// transforms to the #206 golden leaf `Työ😀` (observed against real claude
// 2.1.199, 2026-07-06). The astral char `😀` (a surrogate pair) makes all three
// rules distinct in one fixture — per-byte emits one hyphen per UTF-8 byte,
// per-utf16 one per UTF-16 code unit, per-rune one per Unicode character — so
// these rows are the deterministic `make check` proof that the leaf
// discriminates the candidates and that EncodeCwd's target rule (per-utf16)
// yields `Ty---`. This is the safety net #206's code-review SHOULD-FIX called
// for, folded here per docs/knowledge/codebase/206.md § Follow-ups.
func TestReferenceTransforms(t *testing.T) {
	tests := []struct {
		name      string
		transform func(string) string
		in        string
		want      string
	}{
		// ö = U+00F6 (2 UTF-8 bytes / 1 UTF-16 unit / 1 rune);
		// 😀 = U+1F600 (4 bytes / 2 units / 1 rune) — the astral discriminator.
		{"perByte astral golden", perByte, nonASCIILeaf, "Ty------"},
		{"perUTF16 astral golden", perUTF16, nonASCIILeaf, "Ty---"},
		{"perRune astral golden", perRune, nonASCIILeaf, "Ty--"},
		// ASCII fast path: all three agree and pass bytes through untouched.
		{"perByte ascii passthrough", perByte, "abc123", "abc123"},
		{"perUTF16 ascii passthrough", perUTF16, "abc123", "abc123"},
		{"perRune ascii passthrough", perRune, "abc123", "abc123"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.transform(tc.in); got != tc.want {
				t.Errorf("%s(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
			}
		})
	}
}

// TestDeriveRule pins the leaf-suffix rule derivation. With the astral golden
// leaf, per-utf16 and per-rune produce different suffixes, so a projects-dir
// ending in per-utf16's `-Ty---` matches exactly one candidate. The synthetic
// dir prefix avoids hardcoding the recorded run's random temp suffix —
// deriveRule matches on the leaf suffix only, so any portable prefix is fine
// and the assertion stays machine-independent.
func TestDeriveRule(t *testing.T) {
	tests := []struct {
		name        string
		observedDir string
		wantRule    string
		wantMatched []string
	}{
		{
			// The #206 golden: dir ends in per-utf16's "-Ty---".
			name:        "astral golden -> per-utf16 only",
			observedDir: "-tmp-probe-Ty---",
			wantRule:    "per-utf16-code-unit",
			wantMatched: []string{"per-utf16-code-unit"},
		},
		{
			// per-rune suffix "-Ty--" matches only the per-unicode-character rule.
			name:        "per-rune suffix -> per-unicode-character only",
			observedDir: "-tmp-probe-Ty--",
			wantRule:    "per-unicode-character",
			wantMatched: []string{"per-unicode-character"},
		},
		{
			// per-byte suffix "-Ty------" matches only the per-byte rule.
			name:        "per-byte suffix -> per-byte only",
			observedDir: "-tmp-probe-Ty------",
			wantRule:    "per-byte",
			wantMatched: []string{"per-byte"},
		},
		{
			// No candidate leaf encoding is a suffix (raw un-encoded leaf) ->
			// unknown, nil matches — claude did something unmodelled.
			name:        "unmodelled suffix -> unknown",
			observedDir: "-tmp-probe-Työ😀",
			wantRule:    "unknown",
			wantMatched: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotRule, gotMatched := deriveRule(tc.observedDir, nonASCIILeaf)
			if gotRule != tc.wantRule {
				t.Errorf("deriveRule(%q, %q) rule = %q, want %q", tc.observedDir, nonASCIILeaf, gotRule, tc.wantRule)
			}
			if !reflect.DeepEqual(gotMatched, tc.wantMatched) {
				t.Errorf("deriveRule(%q, %q) matched = %v, want %v", tc.observedDir, nonASCIILeaf, gotMatched, tc.wantMatched)
			}
		})
	}
}

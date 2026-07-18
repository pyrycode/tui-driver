package tuidriver

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	snap, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return snap
}

func TestParseModalContentNoFalsePositive(t *testing.T) {
	cases := []struct {
		name string
		snap []byte
	}{
		{"nil", nil},
		{"idle bytes", []byte("idle TUI bytes")},
		{"slash-picker fixture", loadFixture(t, "picker-snapshot.bin")},
		{"mcp fixture", loadFixture(t, "mcp-snapshot.bin")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseModalContent(tc.snap); got != nil {
				t.Errorf("ParseModalContent(%s) = %+v, want nil", tc.name, got)
			}
		})
	}
}

func TestParseModalContentPermission(t *testing.T) {
	got := ParseModalContent(loadFixture(t, "permission-snapshot.bin"))
	if got == nil {
		t.Fatal("ParseModalContent(permission) = nil, want non-nil")
	}
	want := &ModalContent{
		Class:  ModalClassPermission,
		Title:  "Bash command",
		Prompt: "Do you want to proceed?",
		Options: []ModalOption{
			{Index: 1, Label: "Yes"},
			{Index: 2, Label: "Yes, allow reading from tmp/ from this project"},
			{Index: 3, Label: "No"},
		},
		Default: 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseModalContent(permission) =\n  %+v\nwant\n  %+v", got, want)
	}
}

// #295: extraction must work on a Write-tool permission dialog, not only the
// Bash "proceed" form. permission-write-snapshot.bin is a real claude 2.1.199
// Write prompt captured live at 120x40. Its prompt line carries no "proceed",
// so before the tool-independent anchor the whole modal classified Unknown and a
// remote client got neither a modal_shown nor this typed content.
func TestParseModalContentPermissionWrite(t *testing.T) {
	got := ParseModalContent(loadFixture(t, "permission-write-snapshot.bin"))
	if got == nil {
		t.Fatal("ParseModalContent(permission-write) = nil, want non-nil")
	}
	want := &ModalContent{
		Class:  ModalClassPermission,
		Title:  "Create file",
		Prompt: "Do you want to create probe.txt?",
		Options: []ModalOption{
			{Index: 1, Label: "Yes"},
			{Index: 2, Label: "Yes, allow all edits during this session (shift+tab)"},
			{Index: 3, Label: "No"},
		},
		Default: 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseModalContent(permission-write) =\n  %+v\nwant\n  %+v", got, want)
	}
}

func TestParseModalContentTrustFolder(t *testing.T) {
	got := ParseModalContent(loadFixture(t, "trust-folder-snapshot.bin"))
	if got == nil {
		t.Fatal("ParseModalContent(trust-folder) = nil, want non-nil")
	}
	if got.Class != ModalClassTrustFolder {
		t.Errorf("Class = %q, want %q", got.Class, ModalClassTrustFolder)
	}
	if got.Title != "Quick safety check" {
		t.Errorf("Title = %q, want %q", got.Title, "Quick safety check")
	}
	if got.Prompt != "Is this a project you created or one you trust?" {
		t.Errorf("Prompt = %q, want %q", got.Prompt, "Is this a project you created or one you trust?")
	}
	// Option-2 wording is unconfirmed (constructed fixture — see spec Open
	// Question 1); assert option 1, the count, and the default only.
	if len(got.Options) < 2 {
		t.Fatalf("len(Options) = %d, want >= 2", len(got.Options))
	}
	if want := (ModalOption{Index: 1, Label: "Yes, I trust this folder"}); got.Options[0] != want {
		t.Errorf("Options[0] = %+v, want %+v", got.Options[0], want)
	}
	if got.Default != 1 {
		t.Errorf("Default = %d, want 1", got.Default)
	}
}

// Labels must be neutral display text: no leading "N.", no ❯ marker, no
// box-drawing rune.
func TestParseModalContentLabelsAreClean(t *testing.T) {
	for _, fixture := range []string{"permission-snapshot.bin", "trust-folder-snapshot.bin"} {
		t.Run(fixture, func(t *testing.T) {
			mc := ParseModalContent(loadFixture(t, fixture))
			if mc == nil {
				t.Fatalf("ParseModalContent(%s) = nil", fixture)
			}
			for _, opt := range mc.Options {
				for _, r := range []rune{'❯', '─', '⎿'} {
					if bytes.ContainsRune([]byte(opt.Label), r) {
						t.Errorf("Label %q contains forbidden rune %q", opt.Label, r)
					}
				}
				if len(opt.Label) > 0 && opt.Label[0] >= '0' && opt.Label[0] <= '9' {
					t.Errorf("Label %q starts with a digit (leading N. not stripped)", opt.Label)
				}
			}
		})
	}
}

// The serialized push shape must carry no ANSI / terminal control bytes and
// no claude screen-marker glyph — only neutral field values (AC2).
func TestModalContentSerializationNeutral(t *testing.T) {
	for _, fixture := range []string{"permission-snapshot.bin", "trust-folder-snapshot.bin"} {
		t.Run(fixture, func(t *testing.T) {
			mc := ParseModalContent(loadFixture(t, fixture))
			if mc == nil {
				t.Fatalf("ParseModalContent(%s) = nil", fixture)
			}
			out, err := json.Marshal(mc)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			if bytes.IndexByte(out, 0x1b) >= 0 {
				t.Error("serialized output contains ESC (0x1b)")
			}
			if bytes.IndexByte(out, 0x07) >= 0 {
				t.Error("serialized output contains BEL (0x07)")
			}
			for _, r := range []rune{'❯', '─', '⎿'} {
				if bytes.ContainsRune(out, r) {
					t.Errorf("serialized output contains forbidden glyph %q", r)
				}
			}
			// Known neutral values round-trip.
			if !bytes.Contains(out, []byte(`"default":1`)) {
				t.Errorf("serialized output missing \"default\":1: %s", out)
			}
		})
	}
	// Class value renders as the neutral enum string.
	perm := ParseModalContent(loadFixture(t, "permission-snapshot.bin"))
	out, _ := json.Marshal(perm)
	if !bytes.Contains(out, []byte(`"class":"permission"`)) {
		t.Errorf("serialized permission missing \"class\":\"permission\": %s", out)
	}
}

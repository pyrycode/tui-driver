package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseSnapshotResults(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   map[string]any
	}{
		{
			name: "all match",
			stdout: "SNAPSHOT picker match\n" +
				"SNAPSHOT mcp match\n" +
				"SNAPSHOT agents match\n",
			want: map[string]any{
				"snapshots": []map[string]any{
					{"file": "pkg/tuidriver/testdata/picker-snapshot.bin", "result": "match"},
					{"file": "pkg/tuidriver/testdata/mcp-snapshot.bin", "result": "match"},
					{"file": "pkg/tuidriver/testdata/agents-snapshot.bin", "result": "match"},
				},
			},
		},
		{
			name: "mixed match and diff",
			stdout: "SNAPSHOT picker match\n" +
				"SNAPSHOT mcp diff\n" +
				"SNAPSHOT agents match\n",
			want: map[string]any{
				"snapshots": []map[string]any{
					{"file": "pkg/tuidriver/testdata/picker-snapshot.bin", "result": "match"},
					{"file": "pkg/tuidriver/testdata/mcp-snapshot.bin", "result": "diff"},
					{"file": "pkg/tuidriver/testdata/agents-snapshot.bin", "result": "match"},
				},
			},
		},
		{
			name:   "empty stdout",
			stdout: "",
			want:   nil,
		},
		{
			name:   "no SNAPSHOT lines amid noise",
			stdout: "some other log line\nanother line\n",
			want:   nil,
		},
		{
			name: "noisy stdout with SNAPSHOT lines interspersed",
			stdout: "2026-05-19 14:00:00 starting\n" +
				"SNAPSHOT picker match\n" +
				"intermediate log\n" +
				"SNAPSHOT mcp diff\n" +
				"final log line\n" +
				"SNAPSHOT agents match\n" +
				"done\n",
			want: map[string]any{
				"snapshots": []map[string]any{
					{"file": "pkg/tuidriver/testdata/picker-snapshot.bin", "result": "match"},
					{"file": "pkg/tuidriver/testdata/mcp-snapshot.bin", "result": "diff"},
					{"file": "pkg/tuidriver/testdata/agents-snapshot.bin", "result": "match"},
				},
			},
		},
		{
			name:   "partial output (timeout mid-run)",
			stdout: "SNAPSHOT picker match\nSNAPSHOT mcp match\n",
			want: map[string]any{
				"snapshots": []map[string]any{
					{"file": "pkg/tuidriver/testdata/picker-snapshot.bin", "result": "match"},
					{"file": "pkg/tuidriver/testdata/mcp-snapshot.bin", "result": "match"},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSnapshotResults(tc.stdout, "")
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseSnapshotResults(...) = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestParseClaudeVersion(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "canonical with parenthetical", raw: "2.1.144 (Claude Code)", want: "2.1.144"},
		{name: "whitespace padding", raw: "  2.1.144  ", want: "2.1.144"},
		{name: "bare token", raw: "2.1.144", want: "2.1.144"},
		{name: "empty", raw: "", want: ""},
		{name: "whitespace only", raw: "   ", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseClaudeVersion(tc.raw)
			if got != tc.want {
				t.Errorf("parseClaudeVersion(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseLockFile(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		want        lockFile
		wantErr     bool
		errContains string
	}{
		{
			name: "canonical",
			content: "# header comment\n" +
				"version=2.1.144\n" +
				"\n" +
				"flag=--session-id\n" +
				"flag=--permission-mode bypassPermissions\n",
			want: lockFile{
				Version: "2.1.144",
				Flags:   []string{"--session-id", "--permission-mode bypassPermissions"},
			},
		},
		{
			name: "comments and blank lines tolerated",
			content: "\n" +
				"# first comment\n" +
				"   # indented comment\n" +
				"\n" +
				"version=2.1.144\n" +
				"\n" +
				"# flags below\n" +
				"flag=--session-id\n",
			want: lockFile{
				Version: "2.1.144",
				Flags:   []string{"--session-id"},
			},
		},
		{
			name:    "whitespace around key and value trimmed",
			content: "  version =  2.1.144  \n  flag = --session-id  \n",
			want: lockFile{
				Version: "2.1.144",
				Flags:   []string{"--session-id"},
			},
		},
		{
			name:        "missing version",
			content:     "flag=--session-id\n",
			wantErr:     true,
			errContains: "missing required version",
		},
		{
			name:        "duplicate version line",
			content:     "version=2.1.144\nversion=2.1.145\n",
			wantErr:     true,
			errContains: "line 2",
		},
		{
			name:        "unknown key",
			content:     "version=2.1.144\ncolor=red\n",
			wantErr:     true,
			errContains: "line 2",
		},
		{
			name:        "empty flag value",
			content:     "version=2.1.144\nflag=\n",
			wantErr:     true,
			errContains: "empty flag value",
		},
		{
			name:    "version only, no flags",
			content: "version=2.1.144\n",
			want: lockFile{
				Version: "2.1.144",
				Flags:   nil,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "claude-version.lock")
			if err := os.WriteFile(path, []byte(tc.content), 0644); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			got, err := parseLockFile(path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseLockFile(%q): expected error, got nil (result %#v)", tc.name, got)
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLockFile: unexpected error %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseLockFile = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestParseLockFile_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := parseLockFile(filepath.Join(dir, "does-not-exist.lock"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist error, got %v", err)
	}
}

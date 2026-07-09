package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestBuildChecksExcludesSnapshotDrift pins #252's retirement of the last
// snapshot-drift fixture: the runner's check list must contain no
// snapshot-drift entry (by name or by the deleted e2e-snapshot-check binary).
// A deterministic, claude-free guard against an accidental re-add.
func TestBuildChecksExcludesSnapshotDrift(t *testing.T) {
	checks := buildChecks(func(context.Context) (string, map[string]any) { return "pass", nil })
	for _, c := range checks {
		if c.Name == "snapshot-drift" || c.Binary == "e2e-snapshot-check" {
			t.Errorf("buildChecks returned a retired snapshot-drift check: %+v", c)
		}
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
				"flag=--permission-mode\n" +
				"value=bypassPermissions\n",
			want: lockFile{
				Version: "2.1.144",
				Flags:   []string{"--session-id", "--permission-mode"},
				Values:  []string{"bypassPermissions"},
			},
		},
		{
			name:    "value key parses alongside flag key",
			content: "version=2.1.144\nflag=--session-id\nvalue=acceptEdits\n",
			want: lockFile{
				Version: "2.1.144",
				Flags:   []string{"--session-id"},
				Values:  []string{"acceptEdits"},
			},
		},
		{
			name:        "empty value value rejected",
			content:     "version=2.1.144\nvalue=\n",
			wantErr:     true,
			errContains: "empty value value",
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

func TestEvaluateClaudeVersionLock(t *testing.T) {
	canonicalLF := lockFile{
		Version: "2.1.144",
		Flags:   []string{"--session-id", "--permission-mode", "--model", "--effort"},
		Values:  []string{"bypassPermissions"},
	}
	const allFlagsHelp = "  --session-id\n  --permission-mode\n  --model\n  --effort\n  bypassPermissions\n"
	helpMissingSessionID := "  --permission-mode\n  --model\n  --effort\n  bypassPermissions\n"
	helpMissingBypass := "  --session-id\n  --permission-mode\n  --model\n  --effort\n"
	helpMissingBoth := "  --permission-mode\n  --model\n  --effort\n"

	tests := []struct {
		name             string
		lf               lockFile
		installed        string
		helpOut          string
		wantStatus       string
		wantMissingFlags []string
		wantMissingVals  []string
	}{
		{
			name:       "canonical match",
			lf:         canonicalLF,
			installed:  "2.1.144",
			helpOut:    allFlagsHelp,
			wantStatus: "pass",
		},
		{
			name:       "patch drift above lock",
			lf:         canonicalLF,
			installed:  "2.1.145",
			helpOut:    allFlagsHelp,
			wantStatus: "pass",
		},
		{
			name:       "patch drift below lock",
			lf:         canonicalLF,
			installed:  "2.1.143",
			helpOut:    allFlagsHelp,
			wantStatus: "pass",
		},
		{
			name:       "minor drift above lock",
			lf:         canonicalLF,
			installed:  "2.2.0",
			helpOut:    allFlagsHelp,
			wantStatus: "pass",
		},
		{
			name:             "missing flag",
			lf:               canonicalLF,
			installed:        "2.1.144",
			helpOut:          helpMissingSessionID,
			wantStatus:       "fail",
			wantMissingFlags: []string{"--session-id"},
		},
		{
			name:            "missing value",
			lf:              canonicalLF,
			installed:       "2.1.144",
			helpOut:         helpMissingBypass,
			wantStatus:      "fail",
			wantMissingVals: []string{"bypassPermissions"},
		},
		{
			name:             "both missing",
			lf:               canonicalLF,
			installed:        "2.1.144",
			helpOut:          helpMissingBoth,
			wantStatus:       "fail",
			wantMissingFlags: []string{"--session-id"},
			wantMissingVals:  []string{"bypassPermissions"},
		},
		{
			name:       "empty flags and values",
			lf:         lockFile{Version: "2.1.144"},
			installed:  "2.1.144",
			helpOut:    "irrelevant content",
			wantStatus: "pass",
		},
		{
			name:             "drift above lock plus missing flag",
			lf:               canonicalLF,
			installed:        "2.1.145",
			helpOut:          helpMissingSessionID,
			wantStatus:       "fail",
			wantMissingFlags: []string{"--session-id"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, extra := evaluateClaudeVersionLock(tc.lf, tc.installed, tc.helpOut)
			if status != tc.wantStatus {
				t.Errorf("status = %q, want %q", status, tc.wantStatus)
			}

			gotInstalled, ok := extra["installed_version"].(string)
			if !ok {
				t.Fatalf("installed_version type = %T, want string", extra["installed_version"])
			}
			if gotInstalled != tc.installed {
				t.Errorf("installed_version = %q, want %q", gotInstalled, tc.installed)
			}

			gotExpected, ok := extra["expected_version"].(string)
			if !ok {
				t.Fatalf("expected_version type = %T, want string", extra["expected_version"])
			}
			if gotExpected != tc.lf.Version {
				t.Errorf("expected_version = %q, want %q", gotExpected, tc.lf.Version)
			}

			wantMF := tc.wantMissingFlags
			if wantMF == nil {
				wantMF = []string{}
			}
			gotMF, ok := extra["missing_flags"].([]string)
			if !ok {
				t.Fatalf("missing_flags type = %T, want []string", extra["missing_flags"])
			}
			if !reflect.DeepEqual(gotMF, wantMF) {
				t.Errorf("missing_flags = %#v, want %#v", gotMF, wantMF)
			}

			wantMV := tc.wantMissingVals
			if wantMV == nil {
				wantMV = []string{}
			}
			gotMV, ok := extra["missing_values"].([]string)
			if !ok {
				t.Fatalf("missing_values type = %T, want []string", extra["missing_values"])
			}
			if !reflect.DeepEqual(gotMV, wantMV) {
				t.Errorf("missing_values = %#v, want %#v", gotMV, wantMV)
			}
		})
	}
}

func TestGateFailed(t *testing.T) {
	tests := []struct {
		name      string
		nonGating bool
		status    string
		want      bool
	}{
		{name: "gating pass", nonGating: false, status: "pass", want: false},
		{name: "gating fail", nonGating: false, status: "fail", want: true},
		{name: "gating timeout", nonGating: false, status: "timeout", want: true},
		{name: "non-gating pass", nonGating: true, status: "pass", want: false},
		{name: "non-gating fail does not gate", nonGating: true, status: "fail", want: false},
		{name: "non-gating timeout does not gate", nonGating: true, status: "timeout", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := gateFailed(Check{NonGating: tc.nonGating}, tc.status); got != tc.want {
				t.Errorf("gateFailed(NonGating=%t, %q) = %t, want %t", tc.nonGating, tc.status, got, tc.want)
			}
		})
	}
}

package main

import (
	"reflect"
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

package tuidriver

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBuildMirror(t *testing.T) {
	t.Run("no sinks", func(t *testing.T) {
		w, c, err := buildMirror(SpawnOpts{})
		if err != nil {
			t.Fatalf("buildMirror: %v", err)
		}
		if w != nil || c != nil {
			t.Errorf("want (nil,nil), got (%v,%v)", w, c)
		}
	})

	t.Run("stderr only", func(t *testing.T) {
		w, c, err := buildMirror(SpawnOpts{MirrorStderr: true})
		if err != nil {
			t.Fatalf("buildMirror: %v", err)
		}
		if w == nil {
			t.Error("want non-nil writer for MirrorStderr")
		}
		if c != nil {
			t.Error("want nil closer for MirrorStderr (no file opened)")
		}
	})

	t.Run("record to opens and headers a file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "rec.cast")
		w, c, err := buildMirror(SpawnOpts{RecordTo: path})
		if err != nil {
			t.Fatalf("buildMirror: %v", err)
		}
		if w == nil || c == nil {
			t.Fatalf("want non-nil writer and closer, got (%v,%v)", w, c)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read cast: %v", err)
		}
		if !bytes.Contains(data, []byte(`"version":2`)) {
			t.Errorf("cast missing asciinema header; got %q", data)
		}
		// 0600 owner-only.
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("cast perm = %o, want 600", perm)
		}
	})

	t.Run("record + stderr compose", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "rec.cast")
		w, c, err := buildMirror(SpawnOpts{RecordTo: path, MirrorStderr: true})
		if err != nil {
			t.Fatalf("buildMirror: %v", err)
		}
		if w == nil || c == nil {
			t.Errorf("want non-nil writer and closer, got (%v,%v)", w, c)
		}
		if c != nil {
			_ = c.Close()
		}
	})

	t.Run("record to existing file fails (O_EXCL)", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "rec.cast")
		if err := os.WriteFile(path, []byte("pre"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := buildMirror(SpawnOpts{RecordTo: path}); err == nil {
			t.Error("buildMirror = nil error, want O_EXCL failure on existing file")
		}
	})

	t.Run("record to nonexistent dir fails", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "no-such-dir", "rec.cast")
		if _, _, err := buildMirror(SpawnOpts{RecordTo: path}); err == nil {
			t.Error("buildMirror = nil error, want open failure for missing dir")
		}
	})
}

// TestSpawnRecordToWritesCast drives RecordTo end to end: spawn a process,
// let it write, and confirm the .cast captured the output framed as an
// asciinema recording.
func TestSpawnRecordToWritesCast(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY tests skipped on Windows")
	}
	path := filepath.Join(t.TempDir(), "session.cast")
	cmd := exec.Command("echo", "recorded-output")
	s, err := Spawn(cmd, SpawnOpts{RecordTo: path})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Logf("Close returned %v (acceptable)", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cast: %v", err)
	}
	if !bytes.Contains(data, []byte(`"version":2`)) {
		t.Errorf("cast missing header; got %q", data)
	}
	if !bytes.Contains(data, []byte("recorded-output")) {
		t.Errorf("cast missing recorded output; got %q", data)
	}
}

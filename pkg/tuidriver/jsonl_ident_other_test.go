//go:build !windows

package tuidriver

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStatIdentity pins the Unix generation-identity extractor: distinct
// files yield different identities, the same file stat'd twice yields an
// equal one, and identities are known on Unix.
func TestStatIdentity(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a")
	pathB := filepath.Join(dir, "b")
	if err := os.WriteFile(pathA, []byte("a"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(pathB, []byte("b"), 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}
	statOrFatal := func(path string) os.FileInfo {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		return fi
	}
	idA := statIdentity(statOrFatal(pathA))
	idA2 := statIdentity(statOrFatal(pathA))
	idB := statIdentity(statOrFatal(pathB))

	if !idA.known {
		t.Errorf("statIdentity(a).known = false, want true on Unix")
	}
	if !idA.sameGeneration(idA2) {
		t.Errorf("same file stat'd twice: sameGeneration = false, want true (%+v vs %+v)", idA, idA2)
	}
	if idA.sameGeneration(idB) {
		t.Errorf("distinct files: sameGeneration = true, want false (%+v vs %+v)", idA, idB)
	}
}

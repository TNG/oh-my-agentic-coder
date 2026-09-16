package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSecurityRuntimeDirServeNotAdoptedFromForeignDir verifies that
// createRuntimeDirServe places the runtime dir outside shared /tmp and
// creates a fresh directory with only the expected subdirs.
func TestSecurityRuntimeDirServeNotAdoptedFromForeignDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("HOME", t.TempDir())
	serverRoot := t.TempDir()

	dir, err := createRuntimeDirServe(serverRoot)
	if err != nil {
		t.Fatalf("createRuntimeDirServe: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	// The directory must not be a direct child of bare /tmp (shared system
	// temp root). See TestSecurityRuntimeDirNotAdoptedFromForeignDir for
	// the rationale.
	for _, sharedRoot := range []string{"/tmp", "/private/tmp"} {
		rel, relErr := filepath.Rel(sharedRoot, dir)
		if relErr == nil && len(rel) > 0 && rel[0] != '.' {
			if filepath.Dir(rel) == "." {
				t.Errorf("serve runtime dir %s is a direct child of shared temp root %s", dir, sharedRoot)
			}
		}
	}

	// A fresh serve runtime dir must contain exactly logs/ — no foreign files.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("fresh serve runtime dir %s has %d entries (want 1: logs): %v", dir, len(entries), entries)
	}
}

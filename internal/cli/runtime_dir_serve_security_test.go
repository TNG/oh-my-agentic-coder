//go:build vuln

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSecurityRuntimeDirServeNotAdoptedFromForeignDir is
// TestSecurityRuntimeDirNotAdoptedFromForeignDir for createRuntimeDirServe,
// the serve-mode twin — same Stat/RemoveAll/MkdirAll shape, entirely
// untested before this.
func TestSecurityRuntimeDirServeNotAdoptedFromForeignDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	serverRoot := t.TempDir()

	clean, err := createRuntimeDirServe(serverRoot)
	if err != nil {
		t.Fatalf("control: createRuntimeDirServe: %v", err)
	}
	if err := os.RemoveAll(clean); err != nil {
		t.Fatalf("control cleanup: %v", err)
	}

	victimSub := filepath.Join(clean, "victim")
	if err := os.MkdirAll(victimSub, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(victimSub, "secret")
	if err := os.WriteFile(marker, []byte("planted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(victimSub, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(victimSub, 0o755) })

	got, err := createRuntimeDirServe(serverRoot)
	if err == nil {
		if _, statErr := os.Stat(marker); statErr == nil {
			t.Errorf("createRuntimeDirServe returned %s successfully even though a pre-existing entry could not be removed: the planted file %s survived", got, marker)
		}
	}
}

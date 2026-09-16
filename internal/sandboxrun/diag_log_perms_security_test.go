package sandboxrun

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSecurityDiagnosticsLogIsNotWorldReadable asserts that the sandbox
// diagnostics log (and its parent directory) are created private to the
// owner, not world-readable.
//
// openDiagFile creates the parent with 0o755 and the file with 0o644.
// Per-connection allow/deny history — including which hosts and ports the
// agent tried to reach — is then readable by any local user, and a
// pre-existing world-readable file is never tightened on open.
func TestSecurityDiagnosticsLogIsNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "sandbox.log")

	f, err := openDiagFile(path)
	if err != nil {
		t.Fatalf("openDiagFile: %v", err)
	}
	defer f.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	// Control: the file was actually created (not silently skipped), so a
	// no-op fix wouldn't make this test pass by accident.
	if fi.Size() != 0 {
		t.Fatalf("control: unexpected pre-existing content: the fixture is broken, not the security property")
	}

	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("sandbox.log created with mode %04o: group/other can read the sandbox's per-connection allow/deny history", fi.Mode().Perm())
	}

	parentFI, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat parent: %v", err)
	}
	if parentFI.Mode().Perm()&0o077 != 0 {
		t.Errorf("diagnostics log directory created with mode %04o: group/other can traverse and list it", parentFI.Mode().Perm())
	}
}

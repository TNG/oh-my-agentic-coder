package sandboxprofile

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSecuritySymlinkRootGrantRejected asserts that a filesystem.allow
// entry that is a symlink to "/" is rejected by Profile.Validate. The
// existing literal "/" check sees only the alias spelling, so the
// blanket root grant prohibition is defeated by any symlink whose target
// is the filesystem root. The control grants a symlink to an ordinary
// directory, which must pass validation.
func TestSecuritySymlinkRootGrantRejected(t *testing.T) {
	tmpDir := t.TempDir()

	rootLink := filepath.Join(tmpDir, "root-link")
	if err := os.Symlink("/", rootLink); err != nil {
		t.Fatalf("symlink to /: %v", err)
	}

	ordinaryDir := t.TempDir()
	ordinaryLink := filepath.Join(tmpDir, "ordinary-link")
	if err := os.Symlink(ordinaryDir, ordinaryLink); err != nil {
		t.Fatalf("symlink to ordinary dir: %v", err)
	}

	// Control: a symlink to an ordinary directory is accepted.
	controlProf := &Profile{
		Filesystem: Filesystem{Allow: []string{ordinaryLink}},
		Workdir:    Workdir{Access: AccessNone},
		Network:    Network{Mode: ModeBlocked},
	}
	if err := controlProf.Validate(); err != nil {
		t.Fatalf("control: validate rejected a symlink to an ordinary directory: %v", err)
	}

	// A symlink to "/" must be rejected just like a literal "/" entry.
	prof := &Profile{
		Filesystem: Filesystem{Allow: []string{rootLink}},
		Workdir:    Workdir{Access: AccessNone},
		Network:    Network{Mode: ModeBlocked},
	}
	if err := prof.Validate(); err == nil {
		t.Errorf("Profile.Validate accepted a filesystem.allow entry that is a symlink to /; " +
			"the blanket root grant check sees only the alias spelling")
	}
}

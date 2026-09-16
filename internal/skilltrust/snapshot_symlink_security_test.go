package skilltrust

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecuritySnapshotContainsNoEscapingSymlink asserts that a symlink whose
// containment check passes at the SOURCE tree cannot resolve outside the
// destination snapshot, once relocated there.
//
// copyInTreeSymlink validates containment against the source directory
// (EvalSymlinks + containedIn on srcReal) but recreates the link text
// verbatim in the snapshot. A link of the form "../"*N + <absolute path
// back into the source tree> resolves in-tree from the source (any number
// of leading ".." clamps at "/" and Go's path resolution matches the
// kernel's), passing the check — but the snapshot lives at a different
// depth under the store, so the identical link text resolves through the
// snapshot into the agent-writable workdir instead.
func TestSecuritySnapshotContainsNoEscapingSymlink(t *testing.T) {
	isolate(t)

	workdir := t.TempDir()
	srcTree := filepath.Join(workdir, ".opencode", "skills", "s")
	if err := os.MkdirAll(srcTree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcTree, "code.py"), []byte("print('approved')\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	link := strings.Repeat("../", 64) + srcTree + "/code.py"
	if err := os.Symlink(link, filepath.Join(srcTree, "entry.py")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	// Control: an ordinary in-tree relative symlink is preserved and
	// resolves within the snapshot (the case skilltrust_test.go's
	// TestSnapshotDoesNotBakeEscapingSymlink already pins), so a fix that
	// drops every symlink wouldn't make this test pass for the wrong
	// reason.
	if err := os.Symlink("code.py", filepath.Join(srcTree, "alias.py")); err != nil {
		t.Fatal(err)
	}
	// Use the real bundle hash so snapshot() verification passes.
	hash := bundleHash(t, srcTree)
	controlSnap, err := snapshot("control", hash, srcTree)
	if err != nil {
		t.Fatalf("control: snapshot: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(controlSnap, "alias.py")); err != nil {
		t.Fatalf("control: an ordinary in-tree relative symlink did not survive snapshotting: %v", err)
	}

	if err := Approve("s", hash, srcTree); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	snapDir, ok := SnapshotPath("s", hash)
	if !ok {
		t.Fatal("snapshot missing")
	}

	snapLink := filepath.Join(snapDir, "entry.py")
	resolved, err := filepath.EvalSymlinks(snapLink)
	if err != nil {
		return // refused entirely also satisfies the property
	}
	if strings.HasPrefix(resolved, workdir) {
		t.Errorf("a symlink that passed containment against the SOURCE tree resolves, from inside the SNAPSHOT, back into the agent-writable workdir %s (resolved: %s): "+
			"the approved snapshot's entry point can be repointed post-approval by editing the workdir file this link ultimately targets", workdir, resolved)
	}
}

package opencodestate

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSecurityPlantedProjectRecordYieldsNoWorktree asserts that a
// storage/project JSON record naming the user's own home directory (or the
// filesystem root, or an ancestor of it) does not come back from
// Worktrees() as a directory to grant.
//
// The caller turns returned directories into read+write sandbox grants.
func TestSecurityPlantedProjectRecordYieldsNoWorktree(t *testing.T) {
	home := testDesktopHome(t)
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, ".local", "share", "opencode", "storage", "project")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Control: a legitimate project directory outside $HOME still comes
	// back, so a fix that empties Worktrees() entirely wouldn't make this
	// test pass for the wrong reason. (A project inside $HOME would
	// collapse into the planted $HOME entry itself, which is the exact
	// bug under test — so the control has to live elsewhere.)
	legit, _ := filepath.EvalSymlinks(t.TempDir())
	writeProject(t, stateDir, "legit", legit)
	writeDesktopPinnedProject(t, home, legit)

	writeProject(t, stateDir, "planted", home)

	worktrees, _, err := Worktrees(0)
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, wt := range worktrees {
		if wt == legit {
			found = true
		}
	}
	if !found {
		t.Fatalf("control: the legitimate project directory %s was not returned (%v): the fixture is broken, not the security property", legit, worktrees)
	}

	for _, wt := range worktrees {
		if wt == home {
			t.Errorf("Worktrees() returned the user's home directory %s from a planted storage/project record: "+
				"the caller turns every returned worktree into a --allow (read+write) sandbox grant", home)
		}
	}
}

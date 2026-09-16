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
// Worktrees() only rejects "", "/", non-absolute paths, and duplicates; a
// record naming $HOME passes every check and $HOME exists and is a
// directory, so it comes back as an ordinary worktree. The caller
// (`omac serve --for-opencode-desktop`) turns every returned worktree into
// a `--allow <dir>` (read+write) sandbox grant — one JSON file dropped in
// ~/.local/share/opencode/storage/project/ is enough to turn the next
// activation into a full-home grant.
func TestSecurityPlantedProjectRecordYieldsNoWorktree(t *testing.T) {
	home := t.TempDir()
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
	legit := t.TempDir()
	writeProject(t, stateDir, "legit", legit)

	writeProject(t, stateDir, "planted", home)

	worktrees, _, err := Worktrees()
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

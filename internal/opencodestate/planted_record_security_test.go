package opencodestate

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Agent-writable storage records never become grant sources; the Desktop
// control keeps a source that must still work.
func TestSecurityPlantedProjectRecordYieldsNoWorktree(t *testing.T) {
	home := securityFakeHome(t)
	stateDir := filepath.Join(home, ".local", "share", "opencode", "storage", "project")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	outside := securityOutOfHomeDir(t)
	writeProject(t, stateDir, "planted", home)
	writeProject(t, stateDir, "outside", outside)

	control := securityOutOfHomeDir(t)
	securityWriteDesktopProjects(t, home, control)

	worktrees, _, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(worktrees, control) {
		t.Fatalf("control: Desktop record %q was not returned: %v", control, worktrees)
	}
	for _, wt := range worktrees {
		if wt == home || wt == outside {
			t.Errorf("agent-writable storage record %q was returned as a grant source: %v", wt, worktrees)
		}
	}
}

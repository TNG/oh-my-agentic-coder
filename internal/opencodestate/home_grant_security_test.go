package opencodestate

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Storage records are agent-writable and must not become grant sources, even
// when the recorded path would otherwise pass the path filters.
func TestSecurityPlantedRecordRejectsStrictHomeDescendant(t *testing.T) {
	home := securityOutOfHomeDir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")

	stateDir := filepath.Join(home, ".local", "share", "opencode", "storage", "project")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	inHome := filepath.Join(home, "Documents")
	if err := os.MkdirAll(inHome, 0o755); err != nil {
		t.Fatal(err)
	}

	outside := securityOutOfHomeDir(t)
	writeProject(t, stateDir, "planted", inHome)
	writeProject(t, stateDir, "outside", outside)

	control := securityOutOfHomeDir(t)
	securityWriteDesktopProjects(t, home, control)

	worktrees, _, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(worktrees, control) {
		t.Errorf("control: Desktop record %q was not returned: %v", control, worktrees)
	}
	for _, worktree := range []string{inHome, outside} {
		if slices.Contains(worktrees, worktree) {
			t.Errorf("agent-writable storage record %q was returned as a grant source: %v", worktree, worktrees)
		}
	}
}

// A trusted source does not make an in-home path safe to grant.
func TestSecurityDesktopInHomeWorktreeRejected(t *testing.T) {
	home := securityOutOfHomeDir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")

	inHome := filepath.Join(home, "Documents")
	if err := os.MkdirAll(inHome, 0o755); err != nil {
		t.Fatal(err)
	}
	control := securityOutOfHomeDir(t)
	securityWriteDesktopProjects(t, home, inHome, control)

	worktrees, skipped, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(worktrees, inHome) {
		t.Errorf("in-home Desktop record %q was returned as a grant source: %v", inHome, worktrees)
	}
	if !slices.Contains(skipped, SkippedWorktree{Path: inHome, Reason: SkipUntrusted}) {
		t.Errorf("in-home Desktop record %q was not surfaced in skipped as untrusted: %v", inHome, skipped)
	}
	if !slices.Contains(worktrees, control) {
		t.Errorf("control: out-of-home Desktop record %q was not returned: %v", control, worktrees)
	}
}

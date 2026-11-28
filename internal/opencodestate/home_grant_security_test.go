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
	home := securityFakeHome(t)

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
	home := securityFakeHome(t)

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

// The record spelling is outside home, but the filesystem object it names is
// not; the resolved location must decide the grant.
func TestSecurityDesktopSymlinkIntoHomeRejected(t *testing.T) {
	home := securityFakeHome(t)

	inHome := filepath.Join(home, "Documents")
	if err := os.MkdirAll(inHome, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(securityOutOfHomeDir(t), "link")
	if err := os.Symlink(inHome, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	control := securityOutOfHomeDir(t)
	securityWriteDesktopProjects(t, home, link, control)

	worktrees, skipped, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(worktrees, link) {
		t.Errorf("out-of-home symlink to an in-home directory %q was returned as a grant source: %v", link, worktrees)
	}
	if !slices.Contains(skipped, SkippedWorktree{Path: link, Reason: SkipUntrusted}) {
		t.Errorf("out-of-home symlink to an in-home directory %q was not surfaced as untrusted: %v", link, skipped)
	}
	if !slices.Contains(worktrees, control) {
		t.Errorf("control: out-of-home Desktop record %q was not returned: %v", control, worktrees)
	}
}

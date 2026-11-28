package opencodestate

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestSecurityPlantedRecordRejectsStrictHomeDescendant asserts that a project
// record harvested from the agent-writable storage tree cannot name any
// directory inside the user's home directory. Files under that tree are
// themselves writable by the confined process, so accepting an in-home path
// from them would let the process steer a later grant at a path where DAC
// offers no protection. The rejected record must surface in `skipped` so the
// caller can log it, while a legitimate out-of-home record from the same
// source still resolves.
func TestSecurityPlantedRecordRejectsStrictHomeDescendant(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")

	stateDir := filepath.Join(home, ".local", "share", "opencode", "storage", "project")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A strict descendant of $HOME: the broad-worktree gate (which rejects
	// only $HOME and its ancestors) does not cover it, so the source-aware
	// check is what has to catch it.
	inHome := filepath.Join(home, "Documents")
	if err := os.MkdirAll(inHome, 0o755); err != nil {
		t.Fatal(err)
	}

	// Control: a record outside $HOME from the same source must keep
	// resolving, so a fix that drops the source entirely can't satisfy this
	// test for the wrong reason.
	legit := t.TempDir()
	writeProject(t, stateDir, "planted", inHome)
	writeProject(t, stateDir, "legit", legit)

	worktrees, skipped, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(worktrees, legit) {
		t.Errorf("legitimate out-of-home record %q was not returned: %v", legit, worktrees)
	}
	if slices.Contains(worktrees, inHome) {
		t.Errorf("in-home record %q was returned from an agent-writable source: %v", inHome, worktrees)
	}
	if !slices.Contains(skipped, SkippedWorktree{Path: inHome, Reason: SkipUntrusted}) {
		t.Errorf("in-home record %q was not surfaced in skipped as untrusted: %v", inHome, skipped)
	}
}

// TestSecurityDesktopInHomeWorktreeRejected asserts that the source of a
// record does not make an in-home path safe to grant. A Desktop project
// record pointing inside the user's home must be rejected like any other
// in-home record; an out-of-home Desktop record still resolves so a fix
// cannot pass by ignoring the trusted source entirely.
func TestSecurityDesktopInHomeWorktreeRejected(t *testing.T) {
	home := t.TempDir()
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

package opencodestate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestSecurityPlantedRecordRejectsStrictHomeDescendant asserts that a project
// record harvested from the agent-writable storage tree cannot name any
// directory, whether inside or outside HOME. Only Desktop authorizes grants.
func TestSecurityPlantedRecordRejectsStrictHomeDescendant(t *testing.T) {
	home := testDesktopHome(t)
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

	legit, _ := filepath.EvalSymlinks(t.TempDir())
	writeProject(t, stateDir, "planted", inHome)
	writeProject(t, stateDir, "legit", legit)
	writeDesktopPinnedProject(t, home, legit)

	worktrees, skipped, err := Worktrees(0)
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

// TestSecurityDesktopWorktreeInHomeStillHonored pins the trust distinction
// between sources. The Desktop store lives outside the sandbox's writable
// tree, so an in-home project recorded there must still be honored even
// though the same path planted through the agent-writable storage tree must
// be rejected. Asserting both halves keeps the test honest about which source
// contributed the grant.
func TestSecurityDesktopWorktreeInHomeStillHonored(t *testing.T) {
	home := testDesktopHome(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")

	stateDir := filepath.Join(home, ".local", "share", "opencode", "storage", "project")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	inHome := filepath.Join(home, "Documents")
	if err := os.MkdirAll(inHome, 0o755); err != nil {
		t.Fatal(err)
	}

	// Same in-home path through both sources: the storage copy must be
	// rejected, the Desktop copy must still grant.
	writeProject(t, stateDir, "planted", inHome)
	writeDesktopPinnedProject(t, home, inHome)

	worktrees, skipped, err := Worktrees(0)
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(worktrees, inHome) {
		t.Errorf("in-home Desktop record %q was not honored: %v", inHome, worktrees)
	}
	if len(skipped) != 0 {
		t.Errorf("authorized path should not be logged as skipped: %v", skipped)
	}
}

// writeDesktopPinnedProject writes an opencode.global.dat entry naming worktree
// as a pinned project under the Desktop app's data dir, mirroring the real
// double-encoded on-disk shape.
func writeDesktopPinnedProject(t *testing.T, home string, worktrees ...string) {
	t.Helper()
	var records []projectRecord
	for _, worktree := range worktrees {
		records = append(records, projectRecord{ID: "d", Worktree: worktree})
	}
	inner, _ := json.Marshal(map[string]any{"value": records})
	top := map[string]any{"globalSync.project": string(inner)}
	data, err := json.Marshal(top)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "Library", "Application Support", "ai.opencode.desktop")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.global.dat"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

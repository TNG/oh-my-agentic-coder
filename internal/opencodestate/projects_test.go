package opencodestate

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeProject(t *testing.T, dir, id, worktree string) {
	t.Helper()
	data := `{"id": "` + id + `", "worktree": "` + worktree + `", "vcs": "git"}`
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWorktrees(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, ".local", "share", "opencode", "storage", "project")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	existing1 := filepath.Join(home, "proj-a")
	existing2 := filepath.Join(home, "proj-b")
	for _, d := range []string{existing1, existing2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stale := filepath.Join(home, "gone")

	writeProject(t, stateDir, "aaa", existing1)
	writeProject(t, stateDir, "bbb", existing2)
	writeProject(t, stateDir, "ccc", existing2) // duplicate worktree
	writeProject(t, stateDir, "ddd", stale)     // doesn't exist
	writeProject(t, stateDir, "global", "/")    // pseudo-project
	writeProject(t, stateDir, "rel", "relative/path")
	// Garbage file must not break parsing.
	if err := os.WriteFile(filepath.Join(stateDir, "junk.json"), []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	worktrees, skipped, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(worktrees, []string{existing1, existing2}) {
		t.Errorf("worktrees = %v", worktrees)
	}
	if !slices.Equal(skipped, []string{stale}) {
		t.Errorf("skipped = %v", skipped)
	}
}

func TestWorktreesNoState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	worktrees, skipped, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if len(worktrees) != 0 || len(skipped) != 0 {
		t.Errorf("expected empty: %v %v", worktrees, skipped)
	}
}

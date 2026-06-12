package opencodestate

import (
	"encoding/json"
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
	nested := filepath.Join(existing1, "sub", "module") // inside proj-a
	prefixSib := filepath.Join(home, "proj-a-suffix")   // shares prefix, NOT nested
	for _, d := range []string{existing1, existing2, nested, prefixSib} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stale := filepath.Join(home, "gone")

	writeProject(t, stateDir, "aaa", existing1)
	writeProject(t, stateDir, "bbb", existing2)
	writeProject(t, stateDir, "ccc", existing2) // duplicate worktree
	writeProject(t, stateDir, "ddd", stale)     // doesn't exist
	writeProject(t, stateDir, "eee", nested)    // nested inside existing1 -> collapsed
	writeProject(t, stateDir, "fff", prefixSib) // prefix sibling -> kept
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
	if !slices.Equal(worktrees, []string{existing1, prefixSib, existing2}) {
		t.Errorf("worktrees = %v (nested must collapse into parent, prefix sibling must stay)", worktrees)
	}
	if !slices.Equal(skipped, []string{stale}) {
		t.Errorf("skipped = %v", skipped)
	}
}

func TestCollapseNested(t *testing.T) {
	got := collapseNested([]string{
		"/a/b/c",
		"/a/b",
		"/a/bc", // prefix sibling of /a/b — must survive
		"/d",
		"/a/b/x/y",
	})
	if !slices.Equal(got, []string{"/a/b", "/a/bc", "/d"}) {
		t.Errorf("collapseNested = %v", got)
	}
}

func TestDesktopWorktrees(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")

	proj := filepath.Join(home, "desktop-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	// Shape captured from a real opencode.global.dat: the
	// globalSync.project value is a JSON-encoded *string* of
	// {"value":[{...,"worktree":...}]}.
	inner := `{"value":[{"id":"abc","worktree":"` + proj + `","vcs":"git"},{"id":"global","worktree":"/"}]}`
	top := map[string]any{
		"model":              "{}",
		"globalSync.project": inner,
	}
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

	worktrees, skipped, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(worktrees, []string{proj}) {
		t.Errorf("worktrees = %v, want [%s]", worktrees, proj)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v", skipped)
	}
}

func TestDesktopOpenTabsPickedUp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")

	pinned := filepath.Join(home, "pinned-proj")
	openTab := filepath.Join(home, "open-tab-proj") // open tab, NOT in globalSync.project
	for _, d := range []string{pinned, openTab} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	syncInner := `{"value":[{"id":"a","worktree":"` + pinned + `"}]}`
	pageInner := `{"lastProjectSession":{"` + openTab + `":{"directory":"` + openTab + `","id":"ses_x","at":1}}}`
	top := map[string]any{
		"globalSync.project": syncInner, // double-encoded string
		"layout.page":        pageInner, // double-encoded string
	}
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

	worktrees, _, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(worktrees, []string{openTab, pinned}) { // sorted
		t.Errorf("worktrees = %v, want both pinned and open-tab dirs", worktrees)
	}
}

func TestDesktopWorktreesPlainObject(t *testing.T) {
	// Defensive: also accept the non-double-encoded form.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	proj := filepath.Join(home, "p2")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"globalSync.project": {"value":[{"id":"x","worktree":"` + proj + `"}]}}`
	dir := filepath.Join(home, ".local", "share", "ai.opencode.desktop.beta")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.global.dat"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	worktrees, _, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(worktrees, []string{proj}) {
		t.Errorf("worktrees = %v", worktrees)
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

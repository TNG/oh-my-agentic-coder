package opencodestate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

func writeProject(t *testing.T, dir, id, worktree string) {
	t.Helper()
	data := `{"id": "` + id + `", "worktree": "` + worktree + `", "vcs": "git"}`
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopStateDir(t *testing.T) {
	// clearXDG makes a subtest hermetic against the runner's own XDG env.
	clearXDG := func(t *testing.T, home string) {
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("XDG_DATA_HOME", "")
	}

	t.Run("prefers the macOS Application Support dir when it exists", func(t *testing.T) {
		home := t.TempDir()
		clearXDG(t, home)
		want := filepath.Join(home, "Library", "Application Support", "ai.opencode.desktop")
		if err := os.MkdirAll(want, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := DesktopStateDir(); got != want {
			t.Errorf("DesktopStateDir() = %q, want %q", got, want)
		}
	})

	t.Run("finds the Linux ~/.config dir when it exists", func(t *testing.T) {
		home := t.TempDir()
		clearXDG(t, home)
		want := filepath.Join(home, ".config", "ai.opencode.desktop")
		if err := os.MkdirAll(want, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := DesktopStateDir(); got != want {
			t.Errorf("DesktopStateDir() = %q, want %q", got, want)
		}
	})

	t.Run("honors XDG_CONFIG_HOME when set", func(t *testing.T) {
		home := t.TempDir()
		clearXDG(t, home)
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		want := filepath.Join(xdg, "ai.opencode.desktop")
		if err := os.MkdirAll(want, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := DesktopStateDir(); got != want {
			t.Errorf("DesktopStateDir() = %q, want %q", got, want)
		}
	})

	t.Run("falls back to beta when only beta exists", func(t *testing.T) {
		home := t.TempDir()
		clearXDG(t, home)
		want := filepath.Join(home, ".config", "ai.opencode.desktop.beta")
		if err := os.MkdirAll(want, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := DesktopStateDir(); got != want {
			t.Errorf("DesktopStateDir() = %q, want %q", got, want)
		}
	})

	t.Run("returns a platform default when none exists", func(t *testing.T) {
		home := t.TempDir()
		clearXDG(t, home)
		want := defaultDesktopStateDir(home) // darwin: App Support; linux: ~/.config
		got := DesktopStateDir()
		if got != want {
			t.Errorf("DesktopStateDir() = %q, want platform default %q", got, want)
		}
		if filepath.Base(got) != "ai.opencode.desktop" || !strings.HasPrefix(got, home) {
			t.Errorf("DesktopStateDir() = %q, want basename ai.opencode.desktop under HOME %q", got, home)
		}
	})
}

func TestWorktrees(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, ".local", "share", "opencode", "storage", "project")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Legitimate storage records must live outside $HOME: records from
	// the agent-writable storage tree that name an in-home path are
	// rejected (the confined process can forge them and DAC offers no
	// protection inside $HOME). In-home paths are exercised by the
	// security tests in home_grant_security_test.go.
	base := t.TempDir()
	existing1 := filepath.Join(base, "proj-a")
	existing2 := filepath.Join(base, "proj-b")
	nested := filepath.Join(existing1, "sub", "module") // inside proj-a
	prefixSib := filepath.Join(base, "proj-a-suffix")   // shares prefix, NOT nested
	for _, d := range []string{existing1, existing2, nested, prefixSib} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stale := filepath.Join(base, "gone") // doesn't exist

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
	want := []string{existing1, prefixSib, existing2}
	sort.Strings(want)
	if !slices.Equal(worktrees, want) {
		t.Errorf("worktrees = %v, want %v (nested must collapse into parent, prefix sibling must stay)", worktrees, want)
	}
	if !slices.Equal(skipped, []SkippedWorktree{{Path: stale, Reason: SkipStale}}) {
		t.Errorf("skipped = %v, want [{%s stale}]", skipped, stale)
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

// A Desktop project folder that contains one of Desktop's own stores (here
// a dotfiles project at ~/.config) is skipped with a notice instead of
// making the sandbox refuse to start; the other projects still work.
func TestWorktreesSkipsFolderContainingDesktopStore(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	config := filepath.Join(home, ".config")
	nested := filepath.Join(config, "nvim")
	project := filepath.Join(home, "work", "p")
	link := filepath.Join(home, "dots")
	for _, d := range []string{nested, project} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(config, link); err != nil {
		t.Fatal(err)
	}
	records := `{"value":[{"worktree":"` + config + `"},{"worktree":"` + nested + `"},{"worktree":"` + project + `"},{"worktree":"` + link + `"}]}`
	data, err := json.Marshal(map[string]any{"globalSync.project": records})
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(config, "ai.opencode.desktop")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "opencode.global.dat"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	worktrees, skipped, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{nested, project}; !slices.Equal(worktrees, want) {
		t.Errorf("worktrees = %v, want %v", worktrees, want)
	}
	for _, p := range []string{config, link} {
		if !slices.Contains(skipped, SkippedWorktree{Path: p, Reason: SkipDesktopStore}) {
			t.Errorf("%s not skipped as a Desktop store folder: %v", p, skipped)
		}
	}
}

// Discovery and the sandbox protection share one list of Desktop dirs, with
// no duplicates, covering the stable and beta app in every location.
func TestDesktopStorePathsListsEachDirOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share")) // same as the default
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "cfg", "ai.opencode.desktop"))

	dirs := desktopDirs(home)
	want := []string{
		filepath.Join(home, "Library", "Application Support", "ai.opencode.desktop"),
		filepath.Join(home, "cfg", "ai.opencode.desktop"),
		filepath.Join(home, ".config", "ai.opencode.desktop"),
		filepath.Join(home, ".local", "share", "ai.opencode.desktop"),
		filepath.Join(home, "Library", "Application Support", "ai.opencode.desktop.beta"),
		filepath.Join(home, "cfg", "ai.opencode.desktop.beta"),
		filepath.Join(home, ".config", "ai.opencode.desktop.beta"),
		filepath.Join(home, ".local", "share", "ai.opencode.desktop.beta"),
	}
	if !slices.Equal(dirs, want) {
		t.Errorf("desktopDirs = %v\nwant %v", dirs, want)
	}

	paths := DesktopStorePaths()
	if len(paths) != len(want)*5 {
		t.Errorf("DesktopStorePaths has %d entries, want %d (5 files in each dir, XDG_STATE_HOME already listed): %v", len(paths), len(want)*5, paths)
	}
	for _, p := range []string{
		filepath.Join(home, ".local", "share", "ai.opencode.desktop.beta", "drafts.sqlite-wal"),
		filepath.Join(home, "cfg", "ai.opencode.desktop", "opencode.global.dat"),
	} {
		if !slices.Contains(paths, p) {
			t.Errorf("DesktopStorePaths misses %s", p)
		}
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	if !slices.Contains(DesktopStorePaths(), filepath.Join(home, "state", "drafts.sqlite")) {
		t.Error("DesktopStorePaths misses the XDG_STATE_HOME store")
	}
}

// Without a home directory, the $XDG_STATE_HOME stores are still listed.
func TestDesktopStorePathsWithoutHome(t *testing.T) {
	state := t.TempDir()
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", state)
	paths := DesktopStorePaths()
	if len(paths) != 5 || !slices.Contains(paths, filepath.Join(state, "opencode.global.dat")) {
		t.Errorf("DesktopStorePaths = %v, want the 5 stores in %s", paths, state)
	}
	t.Setenv("XDG_STATE_HOME", "")
	if paths := DesktopStorePaths(); len(paths) != 0 {
		t.Errorf("DesktopStorePaths = %v, want none without HOME and XDG_STATE_HOME", paths)
	}
}

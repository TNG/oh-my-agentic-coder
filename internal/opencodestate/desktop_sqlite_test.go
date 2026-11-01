package opencodestate

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writeDesktopSQLite creates a drafts.sqlite with the `state` table of
// OpenCode Desktop 1.18+ (schema copied from a real install) and stores
// rows under name 'opencode.global.dat'.
func writeDesktopSQLite(t *testing.T, dir string, rows map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "drafts.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE `state` (`name` text NOT NULL, `key` text NOT NULL, `value` text NOT NULL, `updated_at` integer NOT NULL, CONSTRAINT `state_pk` PRIMARY KEY(`name`, `key`))"); err != nil {
		t.Fatal(err)
	}
	for k, v := range rows {
		if _, err := db.Exec("INSERT INTO state VALUES ('opencode.global.dat', ?, ?, 0)", k, v); err != nil {
			t.Fatal(err)
		}
	}
	// A row of another store must be ignored.
	if _, err := db.Exec(`INSERT INTO state VALUES ('default.dat', 'globalSync.project', '{"value":[{"worktree":"/must/not/appear"}]}', 0)`); err != nil {
		t.Fatal(err)
	}
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// Desktop 1.18+ dropped the opencode.global.dat file and keeps the same
// keys in drafts.sqlite. Pinned projects, open tabs of the built-in
// server, and the projects + open tabs of an extra local server (the
// omac sandbox on :4095) must all be found; a remote server's are not.
func TestDesktopSQLiteWorktrees(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	pinned := filepath.Join(home, "pinned")
	openTab := filepath.Join(home, "open-tab")
	sandboxProj := filepath.Join(home, "sandbox-proj")
	sandboxTab := filepath.Join(home, "sandbox-tab")
	remote := filepath.Join(home, "remote-proj") // exists locally, but belongs to a remote server
	mkdirs(t, pinned, openTab, sandboxProj, sandboxTab, remote)

	sandbox := "http://localhost:4095"
	writeDesktopSQLite(t, filepath.Join(home, "Library", "Application Support", "ai.opencode.desktop"), map[string]string{
		"globalSync.project": `{"value":[{"id":"a","worktree":"` + pinned + `"},{"id":"global","worktree":"/"}]}`,
		"layout.page":        `{"lastProjectSession":{"` + openTab + `":{"directory":"` + openTab + `","id":"s","at":1}}}`,
		"server": `{"list":[{"type":"http","http":{"url":"` + sandbox + `"},"displayName":"Sandbox"}],` +
			`"projects":{"` + sandbox + `":[{"worktree":"` + sandboxProj + `"}],"https://build-box:4096":[{"worktree":"` + remote + `"}]}}`,
		sandbox + "\x00layout.page":             `{"lastProjectSession":{"` + sandboxTab + `":{"directory":"` + sandboxTab + `"}}}`,
		"https://build-box:4096\x00layout.page": `{"lastProjectSession":{"` + remote + `":{"directory":"` + remote + `"}}}`,
		sandbox + "\x00layout":                  `{"sidebar":{"opened":true}}`,
		"model":                                 `{}`,
	})

	worktrees, skipped, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{openTab, pinned, sandboxProj, sandboxTab}
	if !slices.Equal(worktrees, want) {
		t.Errorf("worktrees = %v, want %v", worktrees, want)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v", skipped)
	}
}

// On Linux, Electron's userData is ~/.config/<id>; drafts.sqlite there
// must be found too.
func TestDesktopSQLiteLinuxConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	proj := filepath.Join(home, "p")
	mkdirs(t, proj)
	writeDesktopSQLite(t, filepath.Join(home, ".config", "ai.opencode.desktop"), map[string]string{
		"globalSync.project": `{"value":[{"id":"a","worktree":"` + proj + `"}]}`,
	})
	worktrees, _, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(worktrees, []string{proj}) {
		t.Errorf("worktrees = %v, want [%s]", worktrees, proj)
	}
}

// A drafts.sqlite without the expected table (or garbage) is ignored.
func TestDesktopSQLiteUnexpectedSchema(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "drafts.sqlite"), []byte("not a db"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := desktopSQLiteWorktrees(filepath.Join(dir, "drafts.sqlite")); got != nil {
		t.Errorf("garbage db: got %v", got)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "other.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE document (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if got := desktopSQLiteWorktrees(filepath.Join(dir, "other.sqlite")); got != nil {
		t.Errorf("no state table: got %v", got)
	}
}

func TestIsLocalServer(t *testing.T) {
	for s, want := range map[string]bool{
		"local":                  true,
		"sidecar":                true,
		"http://localhost:4095":  true,
		"http://127.0.0.1:4096":  true,
		"http://[::1]:4096":      true,
		"https://build-box:4096": false,
		"http://10.0.0.5:4095":   false,
		"ssh://localhost":        false,
		"":                       false,
	} {
		if got := isLocalServer(s); got != want {
			t.Errorf("isLocalServer(%q) = %v, want %v", s, got, want)
		}
	}
}

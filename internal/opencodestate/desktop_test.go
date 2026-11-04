package opencodestate

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testDesktopHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	for _, name := range []string{"XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME"} {
		t.Setenv(name, "")
	}
	return home
}

func TestDesktopSQLiteScopesAndVerifiesIDs(t *testing.T) {
	home := testDesktopHome(t)
	state := filepath.Join(home, ".config", "ai.opencode.desktop")
	t.Setenv("XDG_STATE_HOME", state)
	paths := map[string]string{}
	for _, name := range []string{"pinned", "tab", "by-id", "other-server", "forged", "outside"} {
		paths[name] = filepath.Join(home, name)
		if name == "outside" {
			paths[name], _ = filepath.EvalSymlinks(t.TempDir())
		}
		if err := os.MkdirAll(paths[name], 0700); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha1.Sum([]byte("directory:" + paths["by-id"]))
	id := hex.EncodeToString(sum[:])
	projects := testSQLite(t, filepath.Join(home, ".local", "share", "opencode", "opencode.db"), "CREATE TABLE project(id TEXT, worktree TEXT)")
	for _, path := range []string{paths["by-id"], paths["forged"], paths["outside"]} {
		if _, err := projects.Exec("INSERT INTO project VALUES (?, ?)", id, path); err != nil {
			t.Fatal(err)
		}
	}
	store := testSQLite(t, filepath.Join(state, "drafts.sqlite"), "PRAGMA journal_mode=WAL; CREATE TABLE state(name TEXT, key TEXT, value TEXT)")
	values := map[string]any{
		"server": map[string]any{
			"projects":       map[string]any{"http://localhost:4095": []map[string]string{{"worktree": paths["pinned"]}}, "http://localhost:4999": []map[string]string{{"worktree": paths["other-server"]}}},
			"usedProjectIDs": map[string][]string{"http://localhost:4095": {id}},
		},
		"http://127.0.0.1:4095\x00layout.page": map[string]any{"lastProjectSession": map[string]any{paths["tab"]: map[string]string{"directory": paths["tab"]}}},
		"model":                                "not JSON and not our concern",
	}
	for key, value := range values {
		data, _ := json.Marshal(value)
		if key == "model" {
			data = []byte(strings.Repeat("not JSON", metadataMaxBytes/4))
		}
		if _, err := store.Exec("INSERT INTO state VALUES ('opencode.global.dat', ?, ?)", key, string(data)); err != nil {
			t.Fatal(err)
		}
	}
	for _, port := range []int{4095, 4999, 0} {
		got, _, err := Worktrees(port)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{}
		if port == 4095 {
			want = []string{paths["by-id"], paths["pinned"], paths["tab"]}
		}
		if port == 4999 {
			want = []string{paths["other-server"]}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("port %d: got %v, want %v", port, got, want)
		}
	}
}

func TestDesktopSQLiteRejectsUnsafeSources(t *testing.T) {
	home := testDesktopHome(t)
	for _, schema := range []string{
		"CREATE TABLE unrelated(value TEXT)", // Desktop 1.18 has no state table.
		"CREATE VIEW state AS SELECT 'opencode.global.dat' AS name, 'server' AS key, '{}' AS value",
		"CREATE VIEW state AS WITH RECURSIVE t(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM t) SELECT 'opencode.global.dat' AS name, 'server' AS key, '{}' AS value FROM t",
	} {
		path := filepath.Join(t.TempDir(), "drafts.sqlite")
		testSQLite(t, path, schema)
		start := time.Now()
		top, _ := readDesktopStore(path)
		if len(top) != 0 {
			t.Fatal("unexpected records")
		}
		if time.Since(start) > 3*time.Second {
			t.Fatal("unbounded store query")
		}
	}
	path := filepath.Join(home, "oversized.sqlite")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(sqliteMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := readDesktopStore(path); err == nil {
		t.Fatal("oversized source accepted")
	}
}

func TestDiscoveryRejectsNonregularSources(t *testing.T) {
	home := testDesktopHome(t)
	for _, name := range []string{"drafts.sqlite", "drafts.sqlite-wal", "project.json"} {
		path := filepath.Join(home, name)
		if err := syscall.Mkfifo(path, 0600); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if _, err := readRegular(path, metadataMaxBytes); err == nil {
			t.Fatal("FIFO accepted")
		}
		if time.Since(start) > time.Second {
			t.Fatal("blocked on FIFO")
		}
	}
	dir := filepath.Join(home, ".local", "share", "opencode", "storage", "project")
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(dir, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Worktrees(0); err != nil {
		t.Fatal(err)
	}
}

func TestProjectIDProof(t *testing.T) {
	home := testDesktopHome(t)
	path := filepath.Join(home, "repo")
	git := filepath.Join(path, ".git")
	if err := os.MkdirAll(git, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum([]byte("git-remote:github.com/TNG/test"))
	id := hex.EncodeToString(sum[:])
	for _, remote := range []string{"https://GitHub.com/TNG/test.git", "git@github.com:TNG/test.git", "ssh://git@github.com/TNG/test/"} {
		if err := os.WriteFile(filepath.Join(git, "config"), []byte("[remote \"origin\"]\n url = "+remote+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if !idMatchesWorktree(id, path) {
			t.Fatalf("remote %s rejected", remote)
		}
	}
	if idMatchesWorktree("ffffffffffffffffffffffffffffffffffffffff", path) {
		t.Fatal("forged repository ID accepted")
	}
	cached := "0123456789012345678901234567890123456789"
	if err := os.WriteFile(filepath.Join(git, "opencode"), []byte(cached), 0600); err != nil {
		t.Fatal(err)
	}
	if !idMatchesWorktree(cached, path) {
		t.Fatal("cached ID rejected")
	}
	admin := filepath.Join(git, "worktrees", "linked")
	if err := os.MkdirAll(admin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte("../.."), 0600); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(home, "linked")
	if err := os.Mkdir(linked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: ../repo/.git/worktrees/linked"), 0600); err != nil {
		t.Fatal(err)
	}
	if !idMatchesWorktree(cached, linked) {
		t.Fatal("linked worktree ID rejected")
	}
}

func TestDesktopPathDoesNotAuthorizeSymlinkTarget(t *testing.T) {
	home := testDesktopHome(t)
	target := filepath.Join(home, "other-repository")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "opened-project")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	writeDesktopPinnedProject(t, home, link)
	got, _, err := Worktrees(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("symlink rerouted grant to %v", got)
	}
}

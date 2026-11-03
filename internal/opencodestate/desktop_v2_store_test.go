package opencodestate

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The `server` row and the per-window rows below have the shape OpenCode
// Desktop 2.2.5 wrote on Linux (captured from a real run):
//   - the built-in server is keyed "local" in lastProjectID/usedProjectIDs
//     and "sidecar" in the window's tabs;
//   - a project OpenCode cannot derive an ID for (a git repo without
//     commits) is recorded as "global";
//   - open tabs live in separate rows named opencode.window.<uuid>.dat,
//     not in opencode.global.dat.
func TestDesktopV2StoreShape(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	plain := filepath.Join(home, "proj-plain")     // no git: directory hash
	repo := filepath.Join(home, "proj-git")        // origin remote hash
	otherPort := filepath.Join(home, "other-port") // opened on another loopback server
	tabOnly := filepath.Join(home, "tab-only")     // only named by a window tab
	mkdirs(t, plain, repo, otherPort, tabOnly)
	writeFile(t, filepath.Join(repo, ".git", "config"), "[remote \"origin\"]\n\turl = git@github.com:TNG/demo-repo.git\n")

	plainID := sha1Hex("directory:" + plain)
	repoID := sha1Hex("git-remote:github.com/TNG/demo-repo")
	homeID := sha1Hex("directory:" + home)
	otherID := sha1Hex("directory:" + otherPort)

	sandbox := "http://localhost:4095"
	desktop := filepath.Join(home, ".config", "ai.opencode.desktop")
	writeDesktopSQLite(t, desktop, map[string]string{
		"server": `{"list":[{"type":"http","http":{"url":"` + sandbox + `","password":"pw"},"displayName":"omac sandbox"}],` +
			`"hidden":{},"lastProjectID":{"local":"` + repoID + `","` + sandbox + `":"global"},` +
			`"usedProjectIDs":{"local":["` + plainID + `","` + homeID + `","` + repoID + `"],"` + sandbox + `":["global"],"http://localhost:3000":["` + otherID + `"]},` +
			`"hostnames":{"sidecar":"host","` + sandbox + `":"host"},"projects":{}}`,
	})
	db, err := sql.Open("sqlite", filepath.Join(desktop, "drafts.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO state VALUES ('opencode.window.a30abf10.dat', 'tabs', ?, 0)`,
		`[{"type":"draft","server":"sidecar","directory":"`+tabOnly+`"}]`); err != nil {
		t.Fatal(err)
	}
	writeOpenCodeDB(t, home, [][2]string{
		{plainID, plain}, {repoID, repo}, {homeID, home}, {otherID, otherPort}, {"global", filepath.Join(home, "no-commits")},
	})

	got, _, err := WorktreesForPort(4095)
	if err != nil {
		t.Fatal(err)
	}
	// $HOME itself is verified by its ID but still too broad; "global" and
	// the window tab are not sources; the other port is out of scope.
	if want := []string{repo, plain}; !slices.Equal(sorted(got), sorted(want)) {
		t.Errorf("WorktreesForPort(4095) = %v, want %v", got, want)
	}

	all, _, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{otherPort, plain, repo}; !slices.Equal(sorted(all), sorted(want)) {
		t.Errorf("Worktrees() = %v, want %v", all, want)
	}
}

func sorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// Desktop keeps many keys omac never reads. A value there that is not JSON or
// is larger than omac's 1 MiB bound must not make omac drop the project
// records next to it.
func TestDesktopStoreIgnoresUnrelatedRows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	proj := filepath.Join(home, "proj")
	mkdirs(t, proj)
	id := sha1Hex("directory:" + proj)
	desktop := filepath.Join(home, ".config", "ai.opencode.desktop")
	writeDesktopSQLite(t, desktop, map[string]string{
		"server": `{"usedProjectIDs":{"local":["` + id + `"]}}`,
	})
	db, err := sql.Open("sqlite", filepath.Join(desktop, "drafts.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for key, value := range map[string]string{
		"command.catalog.v1":  "not json at all",
		"prompt.history":      strings.Repeat("x", 2<<20),
		"somebody.layout.pag": `{broken`,
	} {
		if _, err := db.Exec(`INSERT INTO state VALUES ('opencode.global.dat', ?, ?, 0)`, key, value); err != nil {
			t.Fatal(err)
		}
	}
	writeOpenCodeDB(t, home, [][2]string{{id, proj}})

	got, skipped, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{proj}) {
		t.Errorf("worktrees = %v, want [%s]; unrelated rows must not invalidate the store", got, proj)
	}
	for _, s := range skipped {
		if s.Reason == SkipInvalidSource {
			t.Errorf("store reported invalid because of an unrelated row: %v", s)
		}
	}
}

// Desktop 1.18.x keeps its store in opencode.global.dat and its drafts.sqlite
// has no `state` table. That is another format, not an invalid source, so omac
// must not warn about it on every start; a database that is really broken
// still must.
func TestDesktopSQLiteWithoutStateTableIsNotInvalid(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	desktop := filepath.Join(home, ".config", "ai.opencode.desktop")
	mkdirs(t, desktop)
	db, err := sql.Open("sqlite", filepath.Join(desktop, "drafts.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"CREATE TABLE document (key TEXT PRIMARY KEY, value TEXT)", "CREATE TABLE blob (key TEXT PRIMARY KEY, value BLOB)"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	invalid := func() bool {
		_, skipped, err := Worktrees()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range skipped {
			if s.Reason == SkipInvalidSource {
				return true
			}
		}
		return false
	}
	if invalid() {
		t.Error("a valid drafts.sqlite without a state table was reported as an invalid source")
	}
	writeFile(t, filepath.Join(desktop, "drafts.sqlite"), "this is not a database")
	if !invalid() {
		t.Error("a corrupt drafts.sqlite must still be reported")
	}
}

func TestRelevantGlobalKey(t *testing.T) {
	for key, want := range map[string]bool{
		"server":                                 true,
		"globalSync.project":                     true,
		"layout.page":                            true,
		"http://localhost:4095\x00layout.page":   true,
		"\x00layout.page":                        true,
		"command.catalog.v1":                     false,
		"layout.pages":                           false,
		"xlayout.page":                           false, // suffix without the NUL separator
		"http://localhost:4095layout.page":       false,
		"http://localhost:4095\x00layout.page.x": false,
		"Server":                                 false, // keys are case-sensitive
		"":                                       false,
	} {
		if got := relevantGlobalKey(key); got != want {
			t.Errorf("relevantGlobalKey(%q) = %v, want %v", key, got, want)
		}
	}
}

// Without a known server port (a harness that declares none) only the
// built-in Desktop server may name projects.
func TestWorktreesForPortWithoutPort(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	builtin := filepath.Join(home, "builtin")
	served := filepath.Join(home, "served")
	mkdirs(t, builtin, served)
	builtinID, servedID := sha1Hex("directory:"+builtin), sha1Hex("directory:"+served)
	writeDesktopSQLite(t, filepath.Join(home, ".config", "ai.opencode.desktop"), map[string]string{
		"server": `{"usedProjectIDs":{"local":["` + builtinID + `"],"http://localhost:4096":["` + servedID + `"]}}`,
	})
	writeOpenCodeDB(t, home, [][2]string{{builtinID, builtin}, {servedID, served}})
	for port, want := range map[int][]string{0: {builtin}, -1: {builtin}, 4096: {builtin, served}, 3000: {builtin}} {
		got, _, err := WorktreesForPort(port)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(sorted(got), sorted(want)) {
			t.Errorf("WorktreesForPort(%d) = %v, want %v", port, got, want)
		}
	}
}

// The file form has the same unrelated keys. They must not count against
// the record bound or invalidate the file.
func TestDesktopFileIgnoresUnrelatedKeys(t *testing.T) {
	home := t.TempDir()
	var b strings.Builder
	b.WriteString(`{"server":{"usedProjectIDs":{"local":["x"]}}`)
	for i := 0; i <= sourceMaxRecords; i++ { // more unrelated keys than the record bound
		fmt.Fprintf(&b, `,"unrelated.%d":%d`, i, i)
	}
	b.WriteString("}")
	path := filepath.Join(home, "opencode.global.dat")
	writeFile(t, path, b.String())
	got, err := readDesktopFile(path)
	if err != nil || len(got) != 1 {
		t.Fatalf("readDesktopFile = %d keys, %v; want the one relevant key", len(got), err)
	}
}

// A long key of a row omac does not read must not trip SQLite's string length
// limit and invalidate the store.
func TestDesktopSQLiteLongUnrelatedKey(t *testing.T) {
	home := t.TempDir()
	desktop := filepath.Join(home, "desktop")
	writeDesktopSQLite(t, desktop, map[string]string{"server": `{}`})
	db, err := sql.Open("sqlite", filepath.Join(desktop, "drafts.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, n := range []int{100 << 10, 700 << 10} {
		if _, err := db.Exec(`INSERT INTO state VALUES ('opencode.global.dat', ?, '{}', 0)`, strings.Repeat("k", n)); err != nil {
			t.Fatal(err)
		}
	}
	if got := readGlobalStoreSQLite(filepath.Join(desktop, "drafts.sqlite")); len(got) != 1 {
		t.Errorf("long unrelated keys invalidated the store: %d keys", len(got))
	}
}

// opencode.db is written by the sandbox. A copy without its project table is
// not "another format": it stays reported, so the sandbox cannot silence the
// warning by dropping the table.
func TestMissingProjectTableStaysInvalid(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	dir := filepath.Join(home, ".local", "share", "opencode")
	mkdirs(t, dir)
	db, err := sql.Open("sqlite", filepath.Join(dir, "opencode.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE other (x int)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, skipped, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(skipped, func(s SkippedWorktree) bool { return s.Reason == SkipInvalidSource }) {
		t.Errorf("opencode.db without a project table was not reported: %v", skipped)
	}
}

// KNOWN LIMIT, pinned on purpose: the port scope covers the Desktop store.
// OpenCode's project database has no notion of a server, so a folder outside
// $HOME that is listed there is granted even if only another server opened it.
func TestPortScopeDoesNotCoverOutOfHomeRecords(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	outside := t.TempDir() // not under home
	id := sha1Hex("directory:" + outside)
	writeDesktopSQLite(t, filepath.Join(home, ".config", "ai.opencode.desktop"), map[string]string{
		"server": `{"usedProjectIDs":{"http://localhost:3000":["` + id + `"]}}`,
	})
	writeOpenCodeDB(t, home, [][2]string{{id, outside}})
	got, _, err := WorktreesForPort(4095)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, outside) {
		t.Errorf("expected the out-of-home record to be granted (documented limit); got %v", got)
	}
}

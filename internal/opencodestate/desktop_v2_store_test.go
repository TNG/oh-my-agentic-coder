package opencodestate

import (
	"database/sql"
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

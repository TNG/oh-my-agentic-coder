package opencodestate

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writeOpenCodeDB creates ~/.local/share/opencode/opencode.db with a
// minimal project table holding the given id -> worktree rows.
func writeOpenCodeDB(t *testing.T, home string, rows [][2]string) {
	t.Helper()
	dir := filepath.Join(home, ".local", "share", "opencode")
	mkdirs(t, dir)
	db, err := sql.Open("sqlite", filepath.Join(dir, "opencode.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE `project` (`id` text PRIMARY KEY, `worktree` text NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := db.Exec("INSERT INTO project VALUES (?, ?)", r[0], r[1]); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	mkdirs(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Desktop 1.18+ keeps only project IDs for the omac server. The ID of
// a folder OpenCode opened must turn into a grant, even inside $HOME;
// a forged opencode.db row that pairs a trusted ID with another folder
// and an ID recorded for a remote server must not.
func TestDesktopProjectIDsResolved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	plain := filepath.Join(home, "plain")          // no git: directory hash
	repo := filepath.Join(home, "repo")            // git: origin remote hash
	victim := filepath.Join(home, "secrets")       // target of the forged row
	remoteOnly := filepath.Join(home, "remote-ws") // ID only known to a remote server
	mkdirs(t, plain, repo, victim, remoteOnly)
	writeFile(t, filepath.Join(repo, ".git", "config"), "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = git@github.com:TNG/demo.git\n")

	plainID := sha1Hex("directory:" + plain)
	repoID := sha1Hex("git-remote:github.com/TNG/demo")
	forgedID := sha1Hex("directory:" + filepath.Join(home, "gone"))
	remoteID := sha1Hex("directory:" + remoteOnly)

	sandbox := "http://localhost:4095"
	writeDesktopSQLite(t, filepath.Join(home, "Library", "Application Support", "ai.opencode.desktop"), map[string]string{
		"server": `{"list":[],"projects":{},` +
			`"usedProjectIDs":{"` + sandbox + `":["` + plainID + `","` + forgedID + `","global"],"https://build-box:4096":["` + remoteID + `"]},` +
			`"lastProjectID":{"` + sandbox + `":"` + repoID + `"}}`,
	})
	writeOpenCodeDB(t, home, [][2]string{
		{plainID, plain},
		{repoID, repo},
		{forgedID, victim},
		{remoteID, remoteOnly},
	})

	worktrees, _, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{plain, repo}; !slices.Equal(worktrees, want) {
		t.Errorf("worktrees = %v, want %v", worktrees, want)
	}
}

func TestIdMatchesWorktree(t *testing.T) {
	base := t.TempDir()

	plain := filepath.Join(base, "plain")
	mkdirs(t, plain)

	https := filepath.Join(base, "https")
	writeFile(t, filepath.Join(https, ".git", "config"), "[remote \"upstream\"]\n\turl = https://example.com/other.git\n[remote \"origin\"]\n\turl = https://GitHub.com/TNG/demo.git/\n")

	cached := filepath.Join(base, "cached")
	writeFile(t, filepath.Join(cached, ".git", "opencode"), "cafe\n")

	// Linked worktree: .git is a file; the ID lives in the main repo's
	// common dir.
	main := filepath.Join(base, "main")
	writeFile(t, filepath.Join(main, ".git", "opencode"), "beef\n")
	linked := filepath.Join(base, "linked")
	writeFile(t, filepath.Join(main, ".git", "worktrees", "linked", "commondir"), "../..\n")
	writeFile(t, filepath.Join(linked, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "linked")+"\n")

	link := filepath.Join(base, "link")
	if err := os.Symlink(plain, link); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		id   string
		wt   string
		want bool
	}{
		{"directory hash", sha1Hex("directory:" + plain), plain, true},
		{"directory hash of another folder", sha1Hex("directory:" + https), plain, false},
		{"origin remote, https form", sha1Hex("git-remote:github.com/TNG/demo"), https, true},
		{"non-origin remote is not used", sha1Hex("git-remote:example.com/other"), https, false},
		{"cached ID file", "cafe", cached, true},
		{"cached ID file mismatch", "dead", cached, false},
		{"linked worktree", "beef", linked, true},
		{"global ID", "global", plain, false},
		{"empty ID", "", plain, false},
		{"symlink in place of the folder", sha1Hex("directory:" + link), link, false},
		{"unclean path", sha1Hex("directory:" + plain + "/"), plain + "/", false},
		{"relative path", sha1Hex("directory:plain"), "plain", false},
		{"missing folder", sha1Hex("directory:" + filepath.Join(base, "nope")), filepath.Join(base, "nope"), false},
	} {
		if got := idMatchesWorktree(tc.id, tc.wt); got != tc.want {
			t.Errorf("%s: idMatchesWorktree(%q, %q) = %v, want %v", tc.name, tc.id, tc.wt, got, tc.want)
		}
	}
}

func TestNormalizeRemote(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:TNG/oh-my-agentic-coder.git":    "github.com/TNG/oh-my-agentic-coder",
		"https://github.com/TNG/oh-my-agentic-coder":    "github.com/TNG/oh-my-agentic-coder",
		"https://GitHub.com/TNG/x.git/":                 "github.com/TNG/x",
		"ssh://git@gitlab.example.com:2222/group/x.git": "gitlab.example.com/group/x",
		"host.example:path/repo":                        "host.example/path/repo",
		"file:///srv/repo.git":                          "",
		"/srv/repo.git":                                 "",
		"":                                              "",
	} {
		if got := normalizeRemote(in); got != want {
			t.Errorf("normalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

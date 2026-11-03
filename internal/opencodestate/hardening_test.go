package opencodestate

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if handled, code := prepareSQLiteTestWorker(os.Args[1:]); handled {
		os.Exit(code)
	}
	if handled, code := DispatchSQLiteReader(os.Args[1:]); handled {
		os.Exit(code)
	}
	// macOS's /var alias must not turn fixture IDs into noncanonical IDs.
	if canonical, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		_ = os.Setenv("TMPDIR", canonical)
	}
	for _, key := range []string{"XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME"} {
		_ = os.Unsetenv(key)
	}
	os.Exit(m.Run())
}

func hardeningHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{"XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME"} {
		t.Setenv(key, "")
	}
	return home
}

func fixtureSQL(t *testing.T, path, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

func TestHardeningSQLiteBounds(t *testing.T) {
	for _, table := range []string{"project", "state"} {
		for _, bad := range []string{"oversized row", "too many rows", "invalid row", "view", "recursive view", "virtual table"} {
			t.Run(table+"/"+bad, func(t *testing.T) {
				home := hardeningHome(t)
				var path string
				if table == "project" {
					writeOpenCodeDB(t, home, [][2]string{{"good", "/good"}})
					path = filepath.Join(home, ".local/share/opencode/opencode.db")
				} else {
					writeDesktopSQLite(t, home, map[string]string{"server": `{}`})
					path = filepath.Join(home, "drafts.sqlite")
				}
				var statement string
				switch bad {
				case "oversized row":
					if table == "project" {
						statement = `INSERT INTO project VALUES ('bad', printf('%.*c', 1048577, 'x'))`
					} else {
						statement = `INSERT INTO state VALUES ('opencode.global.dat', 'layout.page', printf('%.*c', 1048577, 'x'), 0)`
					}
				case "too many rows":
					if table == "project" {
						statement = `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<10000) INSERT INTO project SELECT CAST(x AS TEXT), '/extra' FROM n`
					} else {
						statement = `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<10000) INSERT INTO state SELECT 'opencode.global.dat', CAST(x AS TEXT) || char(0) || 'layout.page', '{}', 0 FROM n`
					}
				case "invalid row":
					if table == "project" {
						statement = `INSERT INTO project VALUES ('bad', x'ff')`
					} else {
						statement = `INSERT INTO state VALUES ('opencode.global.dat', 'layout.page', '{not-json', 0)`
					}
				case "view":
					statement = fmt.Sprintf(`ALTER TABLE %s RENAME TO backing; CREATE VIEW %s AS SELECT * FROM backing`, table, table)
				case "recursive view":
					statement = fmt.Sprintf(`DROP TABLE %s; CREATE VIEW %s AS WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n) SELECT x AS id, '/bad' AS worktree, 'opencode.global.dat' AS name, x AS key, '{}' AS value FROM n`, table, table)
				case "virtual table":
					if table == "project" {
						statement = `DROP TABLE project; CREATE VIRTUAL TABLE project USING fts5(id, worktree); INSERT INTO project VALUES ('good', '/good')`
					} else {
						statement = `DROP TABLE state; CREATE VIRTUAL TABLE state USING fts5(name, key, value); INSERT INTO state VALUES ('opencode.global.dat', 'server', '{}')`
					}
				}
				fixtureSQL(t, path, statement)
				start := time.Now()
				if table == "project" {
					if got := dbProjectRecords(path); len(got) != 0 {
						t.Errorf("invalid source returned %d records", len(got))
					}
				} else if got := readGlobalStoreSQLite(path); len(got) != 0 {
					t.Errorf("invalid source returned %d records", len(got))
				}
				if elapsed := time.Since(start); elapsed > 2*time.Second {
					t.Errorf("read exceeded one-second budget with scheduling margin: %s", elapsed)
				}
			})
		}
	}
}

func TestHardeningSQLiteEscapedPaths(t *testing.T) {
	home := hardeningHome(t)
	writeOpenCodeDB(t, home, [][2]string{{"good", "/good"}})
	project := filepath.Join(home, "project?#%.sqlite")
	if err := os.Rename(filepath.Join(home, ".local/share/opencode/opencode.db"), project); err != nil {
		t.Fatal(err)
	}
	if got := dbProjectRecords(project); len(got) != 1 || got[0].Worktree != "/good" {
		t.Errorf("escaped project path returned %v", got)
	}
	writeDesktopSQLite(t, home, map[string]string{"server": `{}`})
	desktop := filepath.Join(home, "desktop?#%.sqlite")
	if err := os.Rename(filepath.Join(home, "drafts.sqlite"), desktop); err != nil {
		t.Fatal(err)
	}
	if got := readGlobalStoreSQLite(desktop); len(got) != 1 {
		t.Errorf("escaped Desktop path returned %v", got)
	}
}

func TestHardeningJSONSourceLimits(t *testing.T) {
	for _, bad := range []string{"oversized", "records", "malformed", "nested malformed"} {
		t.Run(bad, func(t *testing.T) {
			home := hardeningHome(t)
			badProject := filepath.Join(home, "must-not-grant")
			goodProject := filepath.Join(home, "independent")
			mkdirs(t, badProject, goodProject)
			top := map[string]any{"globalSync.project": map[string]any{"value": []projectRecord{{Worktree: badProject}}}}
			switch bad {
			case "oversized":
				top["padding"] = strings.Repeat("x", 1048576)
			case "records":
				top["globalSync.project"] = map[string]any{"value": make([]projectRecord, 10001)}
			case "nested malformed":
				top["server"] = "{broken-private-record"
			}
			data, _ := json.Marshal(top)
			if bad == "malformed" {
				data = []byte("{broken-private-record")
			}
			base := filepath.Join(home, "Library/Application Support/ai.opencode.desktop")
			writeFile(t, filepath.Join(base, "opencode.global.dat"), string(data))
			writeDesktopSQLite(t, filepath.Join(home, ".config/ai.opencode.desktop.beta"), map[string]string{
				"globalSync.project": `{"value":[{"worktree":"` + goodProject + `"}]}`,
			})
			got, skipped, err := Worktrees()
			if err != nil || !slices.Equal(got, []string{goodProject}) {
				t.Errorf("source did not fail independently: grants=%v err=%v", got, err)
			}
			if len(skipped) == 0 {
				t.Error("invalid source produced no diagnostic")
			}
			for _, diagnostic := range skipped {
				if strings.Contains(fmt.Sprint(diagnostic), "private-record") {
					t.Error("diagnostic leaked record contents")
				}
			}
		})
	}
}

func TestHardeningStorageLimits(t *testing.T) {
	for _, bad := range []string{"oversized", "records", "malformed", "null"} {
		t.Run(bad, func(t *testing.T) {
			home := hardeningHome(t)
			storage := filepath.Join(home, ".local/share/opencode/storage/project")
			mkdirs(t, storage)
			project := t.TempDir()
			writeProject(t, storage, "good", project)
			switch bad {
			case "oversized":
				writeFile(t, filepath.Join(storage, "bad.json"), `{"padding":"`+strings.Repeat("x", 1048576)+`"}`)
			case "records":
				for i := 0; i < 10000; i++ {
					writeProject(t, storage, fmt.Sprint(i), project)
				}
			case "malformed":
				writeFile(t, filepath.Join(storage, "bad.json"), "{private-record")
			case "null":
				writeFile(t, filepath.Join(storage, "bad.json"), "null")
			}
			if got := projectRecords(); len(got) != 0 {
				t.Errorf("ID lookup retained %d records from invalid source", len(got))
			}
			got, skipped, err := Worktrees()
			if err != nil || len(got) != 0 || len(skipped) == 0 {
				t.Errorf("invalid initial harvest: grants=%v diagnostics=%v err=%v", got, skipped, err)
			}
		})
	}
}

func TestHardeningMetadataKinds(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo"} {
		for _, source := range []string{"desktop JSON", "storage JSON", "Desktop SQLite", "project SQLite", "git config"} {
			t.Run(source+"/"+kind, func(t *testing.T) {
				home := hardeningHome(t)
				path := filepath.Join(home, "metadata")
				target := filepath.Join(home, "target")
				writeFile(t, target, `{"id":"bad","worktree":"/bad","globalSync.project":{"value":[{"worktree":"/bad"}]}}`)
				switch source {
				case "storage JSON":
					path = filepath.Join(home, ".local/share/opencode/storage/project/bad.json")
					mkdirs(t, filepath.Dir(path))
				case "Desktop SQLite":
					writeDesktopSQLite(t, home, map[string]string{"server": `{}`})
					target = filepath.Join(home, "drafts.sqlite")
				case "project SQLite":
					writeOpenCodeDB(t, home, [][2]string{{"good", "/good"}})
					target = filepath.Join(home, ".local/share/opencode/opencode.db")
				case "git config":
					writeFile(t, target, "[remote \"origin\"]\nurl = https://example.com/repo.git\n")
				}
				if kind == "symlink" {
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				} else if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
				done := make(chan bool, 1)
				go func() {
					switch source {
					case "desktop JSON":
						done <- len(readGlobalStoreFile(path)) == 0
					case "storage JSON":
						done <- len(projectRecords()) == 0
					case "Desktop SQLite":
						done <- len(readGlobalStoreSQLite(path)) == 0
					case "project SQLite":
						done <- len(dbProjectRecords(path)) == 0
					case "git config":
						done <- originURL(path) == ""
					}
				}()
				select {
				case rejected := <-done:
					if !rejected {
						t.Error("non-regular metadata was read")
					}
				case <-time.After(1500 * time.Millisecond):
					// Release an old blocking reader so a RED run leaves no goroutine behind.
					f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
					if err == nil {
						_, _ = f.Write([]byte("{}"))
						_ = f.Close()
						<-done
					}
					t.Error("metadata read blocked")
				}
			})
		}
	}
}

func TestHardeningCanonicalTargets(t *testing.T) {
	home := hardeningHome(t)
	storage := filepath.Join(home, ".local/share/opencode/storage/project")
	mkdirs(t, storage)
	inside := filepath.Join(home, "private")
	mkdirs(t, inside)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(inside, alias); err != nil {
		t.Fatal(err)
	}
	writeProject(t, storage, "forged", alias)
	got, skipped, err := Worktrees()
	if err != nil || len(got) != 0 || !slices.Contains(skipped, SkippedWorktree{Path: inside, Reason: SkipUntrusted}) {
		t.Errorf("alias bypassed home filtering: %v %v %v", got, skipped, err)
	}
	if idMatchesWorktree(sha1Hex("directory:"+alias), alias) {
		t.Error("directory ID accepted noncanonical spelling")
	}
	if !idMatchesWorktree(sha1Hex("directory:"+inside), alias) {
		t.Error("canonical directory ID did not resolve alias")
	}
	writeDesktopPinnedProject(t, home, alias)
	got, _, err = Worktrees()
	if err != nil || !slices.Equal(got, []string{inside}) {
		t.Errorf("trusted path did not produce canonical grant: %v %v", got, err)
	}
}

func TestHardeningCanonicalRootAlias(t *testing.T) {
	home := hardeningHome(t)
	storage := filepath.Join(home, ".local/share/opencode/storage/project")
	mkdirs(t, storage)
	project := t.TempDir()
	alias := filepath.Join(t.TempDir(), "root-alias")
	if err := os.Symlink("/", alias); err != nil {
		t.Fatal(err)
	}
	writeProject(t, storage, "root", alias)
	writeProject(t, storage, "good", project)
	got, skipped, err := Worktrees()
	if err != nil || !slices.Equal(got, []string{project}) || !slices.Contains(skipped, SkippedWorktree{Path: "/", Reason: SkipUntrusted}) {
		t.Errorf("root alias escaped broad-path filtering: %v %v %v", got, skipped, err)
	}
	if !rejectsForHome("/outside", "/", originAgentWritable) {
		t.Error("a root HOME must treat every directory as in-home")
	}
}

func TestHardeningGitMetadata(t *testing.T) {
	for _, bad := range []string{"oversized config", "symlink config", "symlink cache", "symlink gitdir", "symlink commondir", "empty gitdir"} {
		t.Run(bad, func(t *testing.T) {
			base := t.TempDir()
			repo := filepath.Join(base, "repo")
			config := `[remote "origin"]` + "\nurl = https://github.com/TNG/demo.git\n"
			writeFile(t, filepath.Join(repo, ".git/config"), config)
			id := sha1Hex("git-remote:github.com/TNG/demo")
			switch bad {
			case "oversized config":
				writeFile(t, filepath.Join(repo, ".git/config"), config+strings.Repeat("#", 1048576))
			case "symlink config":
				writeFile(t, filepath.Join(base, "config"), config)
				if err := os.Remove(filepath.Join(repo, ".git/config")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(base, "config"), filepath.Join(repo, ".git/config")); err != nil {
					t.Fatal(err)
				}
			case "symlink cache":
				id = "cached"
				writeFile(t, filepath.Join(base, "cached"), id)
				if err := os.Symlink(filepath.Join(base, "cached"), filepath.Join(repo, ".git/opencode")); err != nil {
					t.Fatal(err)
				}
			case "symlink gitdir":
				if err := os.Rename(filepath.Join(repo, ".git"), filepath.Join(base, "git")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(base, "git"), filepath.Join(repo, ".git")); err != nil {
					t.Fatal(err)
				}
			case "symlink commondir":
				writeFile(t, filepath.Join(base, "common"), filepath.Join(repo, ".git"))
				if err := os.Symlink(filepath.Join(base, "common"), filepath.Join(repo, ".git/commondir")); err != nil {
					t.Fatal(err)
				}
			case "empty gitdir":
				if err := os.Rename(filepath.Join(repo, ".git"), filepath.Join(repo, "metadata")); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(repo, ".git"), "gitdir: \n")
				writeFile(t, filepath.Join(repo, "opencode"), id)
			}
			if idMatchesWorktree(id, repo) {
				t.Error("unsafe Git metadata authorized a project")
			}
		})
	}
	for _, clone := range []string{"one", "two"} {
		repo := filepath.Join(t.TempDir(), clone)
		writeFile(t, filepath.Join(repo, ".git/config"), "[remote \"origin\"]\nurl = git@github.com:TNG/demo.git\n")
		if !idMatchesWorktree(sha1Hex("git-remote:github.com/TNG/demo"), repo) {
			t.Error("same-repository clone equivalence was lost")
		}
	}
}

func TestDesktopStorePaths(t *testing.T) {
	home := hardeningHome(t)
	config := filepath.Join(home, "config[*]$literal")
	data := filepath.Join(home, "data?#%")
	state := filepath.Join(home, "state[*]$literal")
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("XDG_STATE_HOME", state)
	paths := DesktopStorePaths()
	seen := map[string]bool{}
	for _, path := range paths {
		if !filepath.IsAbs(path) || seen[path] {
			t.Errorf("not a unique absolute literal path: %q", path)
		}
		seen[path] = true
	}
	bases := []string{state}
	for _, app := range []string{"ai.opencode.desktop", "ai.opencode.desktop.beta"} {
		for _, root := range []string{filepath.Join(home, "Library/Application Support"), filepath.Join(home, ".local/share"), filepath.Join(home, ".config"), config, data} {
			bases = append(bases, filepath.Join(root, app))
		}
	}
	for _, base := range bases {
		for _, name := range []string{"opencode.global.dat", "drafts.sqlite", "drafts.sqlite-wal", "drafts.sqlite-shm", "drafts.sqlite-journal"} {
			if !seen[filepath.Join(base, name)] {
				t.Errorf("missing protection path: %s", filepath.Join(base, name))
			}
		}
	}
	// The custom state directory is also a discovery source, not just a deny.
	project := filepath.Join(home, "project")
	mkdirs(t, project)
	writeDesktopSQLite(t, state, map[string]string{"globalSync.project": `{"value":[{"worktree":"` + project + `"}]}`})
	got, _, err := Worktrees()
	if err != nil || !slices.Equal(got, []string{project}) {
		t.Errorf("explicit XDG_STATE_HOME was not consulted: %v %v", got, err)
	}
}

func TestHardeningExactBounds(t *testing.T) {
	home := hardeningHome(t)
	path := filepath.Join(home, "opencode.global.dat")
	data := `{"layout.page":{"padding":"` + strings.Repeat("x", 1048576-len(`{"layout.page":{"padding":""}}`)) + `"}}`
	writeFile(t, path, data)
	if got := readGlobalStoreFile(path); len(got) != 1 {
		t.Error("JSON file at exactly 1 MiB was rejected")
	}
	writeOpenCodeDB(t, home, nil)
	projectDB := filepath.Join(home, ".local/share/opencode/opencode.db")
	fixtureSQL(t, projectDB, `INSERT INTO project VALUES ('good', printf('%.*c', 1048572, 'x'))`)
	if got := dbProjectRecords(projectDB); len(got) != 1 {
		t.Error("project row at exactly 1 MiB was rejected")
	}
	writeDesktopSQLite(t, home, map[string]string{"server": `{"padding":"` + strings.Repeat("x", 1048576-len("server")-len(`{"padding":""}`)) + `"}`})
	if got := readGlobalStoreSQLite(filepath.Join(home, "drafts.sqlite")); len(got) != 1 {
		t.Error("Desktop row at exactly 1 MiB was rejected")
	}
}

func TestHardeningSQLiteLocked(t *testing.T) {
	for _, table := range []string{"project", "state"} {
		t.Run(table, func(t *testing.T) {
			home := hardeningHome(t)
			path := filepath.Join(home, "drafts.sqlite")
			if table == "project" {
				writeOpenCodeDB(t, home, [][2]string{{"good", "/good"}})
				path = filepath.Join(home, ".local/share/opencode/opencode.db")
			} else {
				writeDesktopSQLite(t, home, map[string]string{"server": `{}`})
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec("BEGIN EXCLUSIVE"); err != nil {
				t.Fatal(err)
			}
			defer db.Exec("ROLLBACK")
			start := time.Now()
			if table == "project" {
				if got := dbProjectRecords(path); len(got) != 0 {
					t.Error("locked project source returned records")
				}
			} else if got := readGlobalStoreSQLite(path); len(got) != 0 {
				t.Error("locked Desktop source returned records")
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Errorf("locked read exceeded budget with scheduling margin: %s", elapsed)
			}
		})
	}
}

func TestHardeningDBInitialHarvestWithoutCLI(t *testing.T) {
	home := hardeningHome(t)
	t.Setenv("PATH", "")
	project := filepath.Join(t.TempDir(), "project\nwith newline")
	mkdirs(t, project)
	writeOpenCodeDB(t, home, [][2]string{{"good", project}})
	got, skipped, err := Worktrees()
	if err != nil || len(skipped) != 0 || !slices.Equal(got, []string{project}) {
		t.Errorf("initial DB harvest still relies on CLI/line splitting: %v %v %v", got, skipped, err)
	}
}

func TestHardeningSQLiteReadOnlyWAL(t *testing.T) {
	for _, table := range []string{"project", "state"} {
		t.Run(table, func(t *testing.T) {
			home := hardeningHome(t)
			path := filepath.Join(home, "drafts.sqlite")
			insert := `INSERT INTO state VALUES ('opencode.global.dat', 'layout.page', '{}', 0)`
			if table == "project" {
				writeOpenCodeDB(t, home, [][2]string{{"good", "/good"}})
				path = filepath.Join(home, ".local/share/opencode/opencode.db")
				insert = `INSERT INTO project VALUES ('wal', '/wal')`
			} else {
				writeDesktopSQLite(t, home, map[string]string{"server": `{}`})
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; " + insert); err != nil {
				t.Fatal(err)
			}
			snapshots := map[string][]byte{}
			for _, file := range []string{path, path + "-wal"} {
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				snapshots[file] = data
			}
			if table == "project" {
				if got := dbProjectRecords(path); len(got) != 2 {
					t.Error("read-only project discovery missed a live WAL row")
				}
			} else if got := readGlobalStoreSQLite(path); len(got) != 2 {
				t.Error("read-only Desktop discovery missed a live WAL row")
			}
			for file, before := range snapshots {
				after, err := os.ReadFile(file)
				if err != nil || !bytes.Equal(before, after) {
					t.Errorf("discovery modified database/WAL file %s: %v", file, err)
				}
			}
		})
	}
}

func TestSQLiteGeneratedValueLimitedBeforeProjection(t *testing.T) {
	home := hardeningHome(t)
	path := filepath.Join(home, "generated.sqlite")
	// The generated value is deliberately small (2 MiB), not an OOM test.
	// Returning only length(worktree) bypasses Go-side/projection-size checks:
	// SQLite itself must reject constructing the oversized intermediate value.
	fixtureSQL(t, path, `CREATE TABLE project (id TEXT, worktree TEXT GENERATED ALWAYS AS (printf('%.*c', 2097152, 'x')) VIRTUAL); INSERT INTO project(id) VALUES ('good')`)
	err := readSQLiteRows(path, "project", `SELECT id, CAST(length(worktree) AS TEXT) FROM project WHERE ? > 0 AND ? > 0 LIMIT ?`, func(string, string) error {
		t.Error("oversized generated expression was evaluated without an engine limit")
		return nil
	})
	if err == nil {
		t.Error("generated value above the engine limit did not reject the source")
	}
}

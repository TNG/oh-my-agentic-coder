package opencodestate

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func testSQLite(t *testing.T, path, schema string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	uri := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSQLiteDiscoveryReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state?#%.db")
	db := testSQLite(t, path, `PRAGMA journal_mode=WAL; CREATE TABLE project(id TEXT, worktree TEXT); INSERT INTO project VALUES ('one', '/projects/one')`)
	before, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := readSQLite(path, "project", "SELECT id, worktree FROM project LIMIT 10001")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !slices.Equal(rows[0], []string{"one", "/projects/one"}) {
		t.Fatalf("rows = %v", rows)
	}
	after, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(before) != len(after) {
		t.Fatal("reader changed host database files")
	}
	// Keep the writer live: the inserted row is in its WAL, not the main DB.
	if _, err := db.Exec("INSERT INTO project VALUES ('two', '/projects/two')"); err != nil {
		t.Fatal(err)
	}
	rows, err = readSQLite(path, "project", "SELECT id, worktree FROM project LIMIT 10001")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows after writer commit = %v, error = %v", rows, err)
	}
}

func TestSQLiteDiscoveryRejectsUnsafeInput(t *testing.T) {
	for _, schema := range []string{
		"CREATE VIEW project AS SELECT 'one' AS id, '/projects/one' AS worktree",
		"CREATE VIEW project AS WITH RECURSIVE t(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM t) SELECT 'one' AS id, '/projects/one' AS worktree FROM t",
	} {
		path := filepath.Join(t.TempDir(), "view.db")
		testSQLite(t, path, schema)
		if _, err := readSQLite(path, "project", "SELECT id, worktree FROM project LIMIT 10001"); err == nil {
			t.Fatal("view accepted")
		}
	}
	path := filepath.Join(t.TempDir(), "fifo.db")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := readSQLite(path, "project", "SELECT id, worktree FROM project"); err == nil {
		t.Fatal("FIFO accepted")
	}
	if time.Since(start) > time.Second {
		t.Fatal("blocked on FIFO")
	}
	large := filepath.Join(t.TempDir(), "large.db")
	f, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(sqliteMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := readSQLite(large, "project", "SELECT id, worktree FROM project"); err == nil {
		t.Fatal("oversized source accepted")
	}
}

func TestSQLiteDiscoveryBoundsQuery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "project.db")
	testSQLite(t, path, `CREATE TABLE project(id TEXT, worktree TEXT);
 INSERT INTO project WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x < 10001) SELECT x, '/project' FROM n`)
	if rows, err := readSQLite(path, "project", "SELECT id, worktree FROM project LIMIT 10001"); err == nil || len(rows) != 0 {
		t.Fatal("row limit returned partial authority")
	}
	start := time.Now()
	_, err := readSQLite(path, "project", `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x < 100000000) SELECT 'id', sum(x) FROM n`)
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("query deadline: %v", err)
	}
}

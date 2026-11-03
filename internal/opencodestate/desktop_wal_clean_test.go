package opencodestate

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// Desktop closes drafts.sqlite cleanly when the user quits it: SQLite then
// deletes the -wal and -shm files but the database stays in WAL mode.
func TestDesktopSQLiteCleanWALClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "drafts.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"PRAGMA journal_mode=WAL",
		"CREATE TABLE `state` (`name` text NOT NULL, `key` text NOT NULL, `value` text NOT NULL, `updated_at` integer NOT NULL, PRIMARY KEY(`name`, `key`))",
		`INSERT INTO state VALUES ('opencode.global.dat', 'server', '{"usedProjectIDs":{"local":["x"]}}', 0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			t.Fatalf("fixture still has %s; the clean-close state was not reproduced", suffix)
		}
	}
	got, err := readDesktopSQLite(path)
	if err != nil || len(got) != 1 {
		t.Fatalf("clean WAL database: got %v, err %v; want its one row", got, err)
	}
}

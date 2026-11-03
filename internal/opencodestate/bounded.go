package opencodestate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	metadataMaxBytes = 1 << 20
	sourceMaxRecords = 10000
)

// Errors deliberately contain neither database errors nor record contents.
var errInvalidSource = errors.New("invalid or oversized discovery source")

func openRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, errInvalidSource
	}
	return f, nil
}

func readMetadata(path string) ([]byte, error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() > metadataMaxBytes {
		return nil, errInvalidSource
	}
	data, err := io.ReadAll(io.LimitReader(f, metadataMaxBytes+1))
	if err != nil || len(data) > metadataMaxBytes || !utf8.Valid(data) {
		return nil, errInvalidSource
	}
	return data, nil
}

func readStorageProjectRecords(dir string) ([]projectRecord, error) {
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Bound directory allocation too, including irrelevant entries.
	entries, err := f.ReadDir(sourceMaxRecords + 1)
	if (err != nil && err != io.EOF) || len(entries) > sourceMaxRecords {
		return nil, errInvalidSource
	}
	var out []projectRecord
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := readMetadata(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, errInvalidSource
		}
		var rec projectRecord
		if json.Unmarshal(data, &rec) != nil || strings.TrimSpace(string(data)) == "null" {
			return nil, errInvalidSource
		}
		out = append(out, rec)
	}
	return out, nil
}

// readSQLiteRows is worker-local. Startup callers use readSQLiteSource so
// context-free driver initialization is bounded by the parent's process
// deadline too. Queries are internal constants, never state-file contents.
func readSQLiteRows(path, table, query string, consume func(string, string) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := checkSQLiteFiles(path); err != nil {
		return err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return errInvalidSource
	}
	uri := url.URL{Scheme: "file", Path: absolute}
	params := url.Values{"mode": {"ro"}}
	uri.RawQuery = params.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return errInvalidSource
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return errInvalidSource
	}
	defer conn.Close()
	// SQLite also limits whole encoded records, including headers/unused
	// columns. Allow bounded encoding overhead while CASE enforces the exact
	// 1 MiB metadata payload rule. Limit expressions before schema/row queries.
	if _, err := sqlite.Limit(conn, sqlite3.SQLITE_LIMIT_LENGTH, metadataMaxBytes+64*1024); err != nil {
		return errInvalidSource
	}
	if _, err := sqlite.Limit(conn, sqlite3.SQLITE_LIMIT_SQL_LENGTH, metadataMaxBytes); err != nil {
		return errInvalidSource
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout=500"); err != nil {
		return errInvalidSource
	}
	var kind string
	var rootPage int
	switch err := conn.QueryRowContext(ctx, `SELECT type, rootpage FROM sqlite_master WHERE name = ?`, table).Scan(&kind, &rootPage); {
	case errors.Is(err, sql.ErrNoRows) && table == "state":
		// A valid database without a `state` table is another format, not a bad
		// source: Desktop 1.18.x keeps its store in files and its drafts.sqlite
		// has no such table. (opencode.db always has `project`, so a missing
		// one stays invalid: the sandbox can write that file.)
		return fs.ErrNotExist
	case err != nil || kind != "table" || rootPage <= 0:
		return errInvalidSource
	}
	rows, err := conn.QueryContext(ctx, query, metadataMaxBytes, metadataMaxBytes, sourceMaxRecords+1)
	if err != nil {
		return errInvalidSource
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		var first, second sql.NullString
		if count > sourceMaxRecords || rows.Scan(&first, &second) != nil || !first.Valid || !second.Valid ||
			!utf8.ValidString(first.String) || !utf8.ValidString(second.String) {
			return errInvalidSource
		}
		if consume(first.String, second.String) != nil {
			return errInvalidSource
		}
	}
	if rows.Err() != nil || ctx.Err() != nil {
		return errInvalidSource
	}
	return nil
}

func readDBProjectRecords(path string) ([]projectRecord, error) {
	var out []projectRecord
	err := readSQLiteSource(path, "project", func(id, worktree string) error {
		out = append(out, projectRecord{ID: id, Worktree: worktree})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func readDesktopSQLite(path string) (map[string]json.RawMessage, error) {
	top := map[string]json.RawMessage{}
	err := readSQLiteSource(path, "state", func(key, value string) error {
		if !relevantGlobalKey(key) { // the SQL prefilter is a superset ("%layout.page")
			return nil
		}
		if !json.Valid([]byte(value)) {
			return errInvalidSource
		}
		if _, duplicate := top[key]; duplicate {
			return errInvalidSource
		}
		top[key] = json.RawMessage(value)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := validateGlobalStore(top); err != nil {
		return nil, err
	}
	return top, nil
}

func readDesktopFile(path string) (map[string]json.RawMessage, error) {
	data, err := readMetadata(path)
	if err != nil {
		return nil, err
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil || top == nil {
		return nil, errInvalidSource
	}
	top = relevantGlobalKeys(top)
	if err := validateGlobalStore(top); err != nil {
		return nil, err
	}
	return top, nil
}

// relevantGlobalKey reports whether omac reads the store key at all. The
// store holds dozens of unrelated keys (command catalogs, plugin state, ...)
// that Desktop may grow or change; one oversized or non-JSON value there must
// not make omac drop the project records next to it.
func relevantGlobalKey(key string) bool {
	switch key {
	case "server", "globalSync.project", "layout.page":
		return true
	}
	return strings.HasSuffix(key, serverScopeSep+"layout.page")
}

func relevantGlobalKeys(top map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(top))
	for key, raw := range top {
		if relevantGlobalKey(key) {
			out[key] = raw
		}
	}
	return out
}

// Each Desktop row can contain many projects, tabs, or IDs. Bound those
// records as well as SQL rows, and validate all relevant rows before harvest.
func validateGlobalStore(top map[string]json.RawMessage) error {
	if len(top) > sourceMaxRecords {
		return errInvalidSource
	}
	count := 0
	for key, raw := range top {
		if len(raw) > metadataMaxBytes || !json.Valid(raw) {
			return errInvalidSource
		}
		value := unwrapValue(raw)
		switch {
		case key == "globalSync.project":
			var sync struct {
				Value []projectRecord `json:"value"`
			}
			if json.Unmarshal(value, &sync) != nil {
				return errInvalidSource
			}
			count += len(sync.Value)
		case key == "server":
			var server struct {
				Projects map[string][]projectRecord `json:"projects"`
				Used     map[string][]string        `json:"usedProjectIDs"`
				Last     map[string]string          `json:"lastProjectID"`
			}
			if json.Unmarshal(value, &server) != nil {
				return errInvalidSource
			}
			for _, projects := range server.Projects {
				count += len(projects)
			}
			for _, ids := range server.Used {
				count += len(ids)
			}
			count += len(server.Last)
		case key == "layout.page" || strings.HasSuffix(key, serverScopeSep+"layout.page"):
			var page struct {
				Sessions map[string]struct {
					Directory string `json:"directory"`
				} `json:"lastProjectSession"`
			}
			if json.Unmarshal(value, &page) != nil {
				return errInvalidSource
			}
			count += len(page.Sessions)
		}
		if count > sourceMaxRecords {
			return errInvalidSource
		}
	}
	return nil
}

func sourceDiagnostic(path string, err error) []SkippedWorktree {
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return []SkippedWorktree{{Path: path, Reason: SkipInvalidSource}}
}

func readProjectSources() ([]projectRecord, []SkippedWorktree, error) {
	dir, err := storageProjectDir()
	if err != nil {
		return nil, nil, err
	}
	storage, storageErr := readStorageProjectRecords(dir)
	dbPath := filepath.Join(filepath.Dir(filepath.Dir(dir)), "opencode.db")
	db, dbErr := readDBProjectRecords(dbPath)
	diagnostics := append(sourceDiagnostic(dir, storageErr), sourceDiagnostic(dbPath, dbErr)...)
	return append(storage, db...), diagnostics, nil
}

func canonicalWorktree(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", errInvalidSource
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(resolved)
	if err != nil || !fi.IsDir() {
		return "", errInvalidSource
	}
	return resolved, nil
}

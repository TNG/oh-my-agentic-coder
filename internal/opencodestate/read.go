package opencodestate

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	metadataMaxBytes = 1 << 20
	sqliteMaxBytes   = 512 << 20
	resultMaxBytes   = 8 << 20
	maxRecords       = 10000
)

var errSource = errors.New("invalid or oversized OpenCode state")

func openRegular(path string, limit int64) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > limit {
		f.Close()
		return nil, errSource
	}
	return f, nil
}

func readRegular(path string, limit int64) ([]byte, error) {
	f, err := openRegular(path, limit)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errSource
	}
	return data, nil
}

// Read a private bounded copy. SQLite never opens attacker-controlled paths,
// follows a swapped FIFO, or creates/updates the host database's WAL/SHM files.
// A concurrent Desktop checkpoint may make the copy inconsistent; fail closed.
func sqliteSnapshot(source string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "omac-discovery-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	target := filepath.Join(dir, "state.db")
	remaining := int64(sqliteMaxBytes)
	for _, suffix := range []string{"", "-wal", "-journal"} {
		f, err := openRegular(source+suffix, remaining)
		if suffix != "" && os.IsNotExist(err) {
			continue
		}
		if err != nil {
			cleanup()
			return "", nil, err
		}
		out, err := os.OpenFile(target+suffix, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			f.Close()
			cleanup()
			return "", nil, err
		}
		n, copyErr := io.Copy(out, io.LimitReader(f, remaining+1))
		f.Close()
		closeErr := out.Close()
		if copyErr != nil || closeErr != nil || n > remaining {
			cleanup()
			return "", nil, errSource
		}
		remaining -= n
	}
	return target, cleanup, nil
}

func readSQLite(path, table, query string) ([][]string, error) {
	snapshot, cleanup, err := sqliteSnapshot(path)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	uri := url.URL{Scheme: "file", Path: snapshot, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, errSource
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, errSource
	}
	defer conn.Close()
	if _, err := sqlite.Limit(conn, sqlite3.SQLITE_LIMIT_LENGTH, metadataMaxBytes+65536); err != nil {
		return nil, errSource
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout=100"); err != nil {
		return nil, errSource
	}
	var kind string
	var root int
	err = conn.QueryRowContext(ctx, "SELECT type, rootpage FROM sqlite_master WHERE name = ?", table).Scan(&kind, &root)
	if errors.Is(err, sql.ErrNoRows) && table == "state" {
		return nil, nil
	} // Desktop 1.18's other SQLite format.
	if err != nil || kind != "table" || root <= 0 {
		return nil, errSource
	}
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return nil, errSource
	}
	defer rows.Close()
	var out [][]string
	bytes := 0
	for rows.Next() {
		var a, b string
		if rows.Scan(&a, &b) != nil {
			return nil, errSource
		}
		bytes += len(a) + len(b)
		if len(out) >= maxRecords || bytes > resultMaxBytes {
			return nil, errSource
		}
		out = append(out, []string{a, b})
	}
	if rows.Err() != nil || ctx.Err() != nil {
		return nil, errSource
	}
	return out, nil
}

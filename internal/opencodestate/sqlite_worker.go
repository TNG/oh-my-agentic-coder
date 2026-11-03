package opencodestate

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"
	"unicode/utf8"
)

const sqliteWorkerFlag = "--omac-internal-sqlite-reader-v1"
const sqliteProtocolMagic = "OSR1"

// sqliteExitNoTable is the worker's exit status for a database that opened
// fine but has no table of the requested kind: another format, not an
// invalid source.
const sqliteExitNoTable = 3

// DispatchSQLiteReader is a private executable entry point. Call it before
// launcher setup; it never reads configuration, credentials, or arbitrary SQL.
func DispatchSQLiteReader(args []string) (handled bool, code int) {
	if len(args) == 0 || args[0] != sqliteWorkerFlag {
		return false, 0
	}
	if len(args) != 3 || !filepath.IsAbs(args[2]) {
		return true, 2
	}
	query := sqliteSourceQuery(args[1])
	if query == "" || installSQLiteWorkerGuard() != nil {
		return true, 2
	}
	out := bufio.NewWriterSize(os.Stdout, 64*1024)
	if _, err := io.WriteString(out, sqliteProtocolMagic); err != nil {
		return true, 1
	}
	if err := readSQLiteRows(args[2], args[1], query, func(first, second string) error {
		return writeSQLiteRow(out, first, second)
	}); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return true, sqliteExitNoTable
		}
		return true, 1
	}
	if err := out.WriteByte(0); err != nil {
		return true, 1
	}
	if err := out.Flush(); err != nil {
		return true, 1
	}
	return true, 0
}

func sqliteSourceQuery(kind string) string {
	switch kind {
	case "project":
		return `SELECT
 CASE WHEN typeof(id) = 'text' AND typeof(worktree) = 'text'
 AND length(CAST(id AS BLOB)) + length(CAST(worktree AS BLOB)) <= ? THEN id END,
 CASE WHEN typeof(id) = 'text' AND typeof(worktree) = 'text'
 AND length(CAST(id AS BLOB)) + length(CAST(worktree AS BLOB)) <= ? THEN worktree END
 FROM project LIMIT ?`
	case "state":
		return `SELECT
 CASE WHEN typeof(key) = 'text' AND typeof(value) = 'text'
 AND length(CAST(key AS BLOB)) + length(CAST(value AS BLOB)) <= ? THEN key END,
 CASE WHEN typeof(key) = 'text' AND typeof(value) = 'text'
 AND length(CAST(key AS BLOB)) + length(CAST(value AS BLOB)) <= ? THEN value END
 FROM state WHERE name = 'opencode.global.dat'
 AND (key IN ('server', 'globalSync.project', 'layout.page') OR hex(key) LIKE '%006C61796F75742E70616765') LIMIT ?`
	default:
		return ""
	}
}

func checkSQLiteFiles(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		// Do not open/close these in the parent: POSIX close() can release
		// locks held by another SQLite connection in the same process.
		// The worker guards the actual open; this is only a cheap missing/
		// static-invalid-source check that cannot block on a FIFO.
		fi, err := os.Lstat(path + suffix)
		if err != nil {
			if suffix != "" && os.IsNotExist(err) {
				continue
			}
			return err
		}
		if !fi.Mode().IsRegular() {
			return errInvalidSource
		}
	}
	return nil
}

func readSQLiteSource(path, kind string, consume func(string, string) error) error {
	return readSQLiteSourceContext(context.Background(), path, kind, consume)
}

func readSQLiteSourceContext(parent context.Context, path, kind string, consume func(string, string) error) error {
	ctx, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()
	if sqliteSourceQuery(kind) == "" {
		return errInvalidSource
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := checkSQLiteFiles(path); err != nil {
		return err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return errInvalidSource
	}
	executable, err := os.Executable()
	if err != nil {
		return errInvalidSource
	}
	cmd := exec.CommandContext(ctx, executable, sqliteWorkerFlag, kind, absolute)
	// No ambient credentials, provider keys, facade tokens, or test controls.
	// The fixed race-runtime setting removes its exit-only sleep, so even a
	// race-instrumented executable can honor the one-second process budget.
	cmd.Env = []string{"TMPDIR=" + os.TempDir(), "GORACE=atexit_sleep_ms=0"}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return errInvalidSource
	}
	defer out.Close()
	if err := cmd.Start(); err != nil {
		return errInvalidSource
	}
	decodeErr := decodeSQLiteRows(out, func(first, second string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return consume(first, second)
	})
	if decodeErr != nil || ctx.Err() != nil {
		_ = cmd.Process.Kill()
		_ = out.Close()
	}
	// Always reap, including malformed output, early decoder failure, and kill.
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) && exitErr.ExitCode() == sqliteExitNoTable {
		return fs.ErrNotExist
	}
	if decodeErr != nil || waitErr != nil {
		return errInvalidSource
	}
	return nil
}

func writeSQLiteRow(w io.Writer, first, second string) error {
	if len(first)+len(second) > metadataMaxBytes || !utf8.ValidString(first) || !utf8.ValidString(second) {
		return errInvalidSource
	}
	var header [9]byte
	header[0] = 1
	binary.BigEndian.PutUint32(header[1:5], uint32(len(first)))
	binary.BigEndian.PutUint32(header[5:9], uint32(len(second)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if _, err := io.WriteString(w, first); err != nil {
		return err
	}
	_, err := io.WriteString(w, second)
	return err
}

func decodeSQLiteRows(r io.Reader, consume func(string, string) error) error {
	input := bufio.NewReader(r)
	var magic [4]byte
	if _, err := io.ReadFull(input, magic[:]); err != nil || string(magic[:]) != sqliteProtocolMagic {
		return errInvalidSource
	}
	for count := 0; ; count++ {
		kind, err := input.ReadByte()
		if err != nil {
			return errInvalidSource
		}
		if kind == 0 {
			if _, err := input.ReadByte(); err != io.EOF {
				return errInvalidSource
			}
			return nil
		}
		if kind != 1 || count == sourceMaxRecords {
			return errInvalidSource
		}
		var sizes [8]byte
		if _, err := io.ReadFull(input, sizes[:]); err != nil {
			return errInvalidSource
		}
		first := uint64(binary.BigEndian.Uint32(sizes[:4]))
		second := uint64(binary.BigEndian.Uint32(sizes[4:]))
		if first+second > metadataMaxBytes {
			return errInvalidSource
		}
		data := make([]byte, int(first+second))
		if _, err := io.ReadFull(input, data); err != nil || !utf8.Valid(data[:first]) || !utf8.Valid(data[first:]) {
			return errInvalidSource
		}
		if err := consume(string(data[:first]), string(data[first:])); err != nil {
			return err
		}
	}
}

//go:build darwin || linux

package opencodestate

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Test-only syscall interposition runs inside the actual reader child, after
// its prechecks and guard flag selection but immediately before native open.
var testSQLiteVFS sqlite3.Tsqlite3_vfs
var testSQLiteVFSAddress uintptr
var testSQLiteOriginalOpen uintptr
var testSQLiteOpenCallback any = sqliteTestOpen
var testSQLitePath, testSQLiteMode string
var testSQLiteReplaced bool

func installSQLiteTestOpen(path, mode string) error {
	testSQLitePath, testSQLiteMode = path, mode
	tls := libc.NewTLS()
	defer tls.Close()
	testSQLiteVFSAddress = sqlite3.Xsqlite3_vfs_find(tls, 0)
	if testSQLiteVFSAddress == 0 {
		return errInvalidSource
	}
	libc.Xmemcpy(tls, uintptr(unsafe.Pointer(&testSQLiteVFS)), testSQLiteVFSAddress, sqlite3.Tsize_t(unsafe.Sizeof(testSQLiteVFS)))
	name, err := libc.CString("open")
	if err != nil {
		return err
	}
	defer libc.Xfree(tls, name)
	get := testSQLiteVFS.FxGetSystemCall
	testSQLiteOriginalOpen = (*(*func(*libc.TLS, uintptr, uintptr) uintptr)(unsafe.Pointer(&get)))(tls, testSQLiteVFSAddress, name)
	return setSQLiteTestOpen(tls, name, (*[2]uintptr)(unsafe.Pointer(&testSQLiteOpenCallback))[1])
}

func setSQLiteTestOpen(tls *libc.TLS, name, callback uintptr) error {
	set := testSQLiteVFS.FxSetSystemCall
	if (*(*func(*libc.TLS, uintptr, uintptr, uintptr) int32)(unsafe.Pointer(&set)))(tls, testSQLiteVFSAddress, name, callback) != sqlite3.SQLITE_OK {
		return errInvalidSource
	}
	return nil
}

func sqliteTestOpen(tls *libc.TLS, path uintptr, flags, mode int32) int32 {
	name := libc.GoString(path)
	if name == "/dev/urandom" && testSQLiteMode == "deny-entropy" {
		_ = os.WriteFile(testSQLitePath+".test-entropy", []byte("attempted"), 0o600)
		return -1
	}
	if name == testSQLitePath+testSQLiteMode && !testSQLiteReplaced {
		testSQLiteReplaced = true
		if err := os.Rename(name, name+".before"); err != nil {
			panic(err)
		}
		if err := syscall.Mkfifo(name, 0o400); err != nil {
			panic(err)
		}
		if err := os.WriteFile(testSQLitePath+".test-armed", []byte(strconv.Itoa(int(flags))), 0o600); err != nil {
			panic(err)
		}
	}
	original := testSQLiteOriginalOpen
	return (*(*func(*libc.TLS, uintptr, int32, int32) int32)(unsafe.Pointer(&original)))(tls, path, flags, mode)
}

func prepareSQLiteReplacement(path, mode string) error {
	return installSQLiteTestOpen(path, mode)
}

func TestSQLiteReplacementAtOpen(t *testing.T) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		t.Run("db"+suffix, func(t *testing.T) {
			home := hardeningHome(t)
			writeOpenCodeDB(t, home, [][2]string{{"good", "/good"}})
			path := filepath.Join(home, ".local/share/opencode/opencode.db")
			if suffix == "-wal" || suffix == "-shm" {
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; INSERT INTO project VALUES ('wal', '/wal')"); err != nil {
					t.Fatal(err)
				}
			}
			if suffix == "-journal" {
				data := make([]byte, 512)
				data[0] = 1
				if err := os.WriteFile(path+suffix, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			writeFile(t, path+".test-control", "replace:"+suffix)
			err := readSQLiteSource(path, "project", func(string, string) error {
				t.Error("replaced metadata produced a row")
				return nil
			})
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("actual metadata guard did not fail before the parent deadline: %v", err)
			}
			data, err := os.ReadFile(path + ".test-armed")
			if err != nil {
				t.Fatalf("actual open replacement was not exercised: %v", err)
			}
			flags, err := strconv.Atoi(string(data))
			if err != nil || flags&syscall.O_NONBLOCK == 0 || flags&syscall.O_NOFOLLOW == 0 {
				t.Errorf("actual open lacked nonblocking/no-follow flags: %d %v", flags, err)
			}
		})
	}
}

func runSQLiteEntropyContract(path string) int {
	if installSQLiteWorkerGuard() != nil {
		return 1
	}
	tls := libc.NewTLS()
	defer tls.Close()
	for _, file := range []string{"/dev/urandom", "/dev/zero", filepath.Dir(path)} {
		name, err := libc.CString(file)
		if err != nil {
			return 1
		}
		fd := workerSQLiteOpen(tls, name, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
		libc.Xfree(tls, name)
		if fd >= 0 {
			_ = syscall.Close(int(fd))
			return 1 // No device or directory may masquerade as metadata.
		}
	}
	if workerEntropyOpens != 1 {
		return 1 // Exactly the successful native RNG self-test, not metadata.
	}
	if err := os.WriteFile(path+".test-entropy", []byte("native RNG verified; metadata devices rejected"), 0o600); err != nil {
		return 1
	}
	_, _ = os.Stdout.Write([]byte(sqliteProtocolMagic + "\x00"))
	return 0
}

func TestSQLiteParentHooksAndEntropyUnchanged(t *testing.T) {
	if workerNativeOpen != 0 || workerEntropyOpens != 0 {
		t.Fatal("worker I/O guard was installed in the parent process")
	}
	home := hardeningHome(t)
	writeOpenCodeDB(t, home, [][2]string{{"good", "/good"}})
	path := filepath.Join(home, ".local/share/opencode/opencode.db")
	tls := libc.NewTLS()
	defer tls.Close()
	if err := installSQLiteTestOpen(path, "observe-only"); err != nil {
		t.Fatal(err)
	}
	name, err := libc.CString("open")
	if err != nil {
		t.Fatal(err)
	}
	defer libc.Xfree(tls, name)
	defer setSQLiteTestOpen(tls, name, testSQLiteOriginalOpen)
	get := testSQLiteVFS.FxGetSystemCall
	read := func() uintptr {
		return (*(*func(*libc.TLS, uintptr, uintptr) uintptr)(unsafe.Pointer(&get)))(tls, testSQLiteVFSAddress, name)
	}
	before := read()
	if got := dbProjectRecords(path); len(got) != 1 {
		t.Fatal("healthy worker failed")
	}
	if read() != before {
		t.Fatal("worker changed parent SQLite syscall hook")
	}
	if workerNativeOpen != 0 || workerEntropyOpens != 0 {
		t.Fatal("discovery initialized a worker I/O guard in its parent")
	}
	var random [32]byte
	// Use C-owned storage rather than passing a Go stack address across the ABI.
	buffer, err := libc.CString(string(random[:]))
	if err != nil {
		t.Fatal(err)
	}
	defer libc.Xfree(tls, buffer)
	fn := testSQLiteVFS.FxRandomness
	if n := (*(*func(*libc.TLS, uintptr, int32, uintptr) int32)(unsafe.Pointer(&fn)))(tls, testSQLiteVFSAddress, int32(len(random)), buffer); n != int32(len(random)) {
		t.Fatalf("parent native entropy fell back to time/PID: %d", n)
	}
}

func TestSQLiteWorkerEntropyException(t *testing.T) {
	for _, mode := range []string{"entropy-contract", "deny-entropy"} {
		t.Run(mode, func(t *testing.T) {
			home := hardeningHome(t)
			writeOpenCodeDB(t, home, [][2]string{{"good", "/good"}})
			path := filepath.Join(home, ".local/share/opencode/opencode.db")
			writeFile(t, path+".test-control", mode)
			err := readSQLiteSource(path, "project", func(string, string) error {
				t.Error("entropy test emitted a source row")
				return nil
			})
			if (mode == "entropy-contract") != (err == nil) {
				t.Fatalf("entropy contract/fail-closed result: %v", err)
			}
			if _, err := os.ReadFile(path + ".test-entropy"); err != nil {
				t.Fatal("entropy branch was not exercised")
			}
		})
	}
}

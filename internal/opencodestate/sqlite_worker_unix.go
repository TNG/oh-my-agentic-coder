//go:build darwin || linux

package opencodestate

import (
	"strings"
	"syscall"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// These globals are initialized only by the private worker dispatcher, never
// by package initialization or startup discovery in the parent process.
var workerNativeOpen uintptr
var workerOpenCallback any = workerSQLiteOpen
var workerInvalidErrno = int32(syscall.EINVAL)
var workerEntropyStat syscall.Stat_t
var workerEntropyOpens int

func installSQLiteWorkerGuard() error {
	entropy, err := openWorkerEntropy()
	if err != nil {
		return errInvalidSource
	}
	workerEntropyStat = entropy
	tls := libc.NewTLS()
	defer tls.Close()
	if sqlite3.Xsqlite3_initialize(tls) != sqlite3.SQLITE_OK {
		return errInvalidSource
	}
	vfs := sqlite3.Xsqlite3_vfs_find(tls, 0)
	if vfs == 0 {
		return errInvalidSource
	}
	var native sqlite3.Tsqlite3_vfs
	size := int(unsafe.Sizeof(native))
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&native)), size), libc.GoBytes(vfs, size))
	if native.FiVersion < 3 || !strings.HasPrefix(libc.GoString(native.FzName), "unix") || native.FxGetSystemCall == 0 || native.FxSetSystemCall == 0 {
		return errInvalidSource
	}
	name, err := libc.CString("open")
	if err != nil {
		return errInvalidSource
	}
	defer libc.Xfree(tls, name)
	get := native.FxGetSystemCall
	workerNativeOpen = (*(*func(*libc.TLS, uintptr, uintptr) uintptr)(unsafe.Pointer(&get)))(tls, vfs, name)
	if workerNativeOpen == 0 {
		return errInvalidSource
	}
	callback := (*[2]uintptr)(unsafe.Pointer(&workerOpenCallback))[1]
	set := native.FxSetSystemCall
	if (*(*func(*libc.TLS, uintptr, uintptr, uintptr) int32)(unsafe.Pointer(&set)))(tls, vfs, name, callback) != sqlite3.SQLITE_OK {
		return errInvalidSource
	}
	// Native metadata opens (db/wal/journal/shm) include O_NOFOLLOW; native
	// entropy opens do not. Verify the identity-checked exception actually works
	// with this SQLite build, instead of silently accepting its time/PID fallback.
	random := libc.Xmalloc(tls, 32)
	if random == 0 {
		return errInvalidSource
	}
	defer libc.Xfree(tls, random)
	before := workerEntropyOpens
	fn := native.FxRandomness
	if fn == 0 || (*(*func(*libc.TLS, uintptr, int32, uintptr) int32)(unsafe.Pointer(&fn)))(tls, vfs, 32, random) != 32 || workerEntropyOpens != before+1 {
		return errInvalidSource
	}
	return nil
}

func openWorkerEntropy() (syscall.Stat_t, error) {
	var stat syscall.Stat_t
	fd, err := syscall.Open("/dev/urandom", syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return stat, err
	}
	defer syscall.Close(fd)
	if err := syscall.Fstat(fd, &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFCHR {
		return stat, errInvalidSource
	}
	return stat, nil
}

func workerSQLiteOpen(tls *libc.TLS, path uintptr, flags, mode int32) int32 {
	// The only device exception is SQLite's own read-only entropy open. In
	// particular a metadata open redirected to /dev/urandom has O_NOFOLLOW
	// already set by the Unix VFS and cannot use this exception.
	entropy := flags&syscall.O_NOFOLLOW == 0 && flags&syscall.O_ACCMODE == syscall.O_RDONLY && flags&(syscall.O_CREAT|syscall.O_TRUNC) == 0 && libc.GoString(path) == "/dev/urandom"
	open := workerNativeOpen
	fd := (*(*func(*libc.TLS, uintptr, int32, int32) int32)(unsafe.Pointer(&open)))(tls, path, flags|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, mode)
	if fd < 0 {
		return fd
	}
	var stat syscall.Stat_t
	err := syscall.Fstat(int(fd), &stat)
	kind := stat.Mode & syscall.S_IFMT
	if err == nil && kind == syscall.S_IFREG {
		return fd
	}
	if err == nil && entropy && kind == syscall.S_IFCHR && stat.Dev == workerEntropyStat.Dev && stat.Ino == workerEntropyStat.Ino && stat.Rdev == workerEntropyStat.Rdev {
		workerEntropyOpens++
		return fd
	}
	_ = syscall.Close(int(fd))
	libc.Xmemcpy(tls, libc.X__errno_location(tls), uintptr(unsafe.Pointer(&workerInvalidErrno)), 4)
	return -1
}

package toolcache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

var (
	errActiveLock = errors.New("cache lock is active")
	errUnsafeLock = errors.New("unsafe cache lock path")
)

func hasExactLockPermissions(mode uint32) bool {
	return mode&0o7777 == 0o600
}

func rawFileModePermissions(mode os.FileMode) uint32 {
	permissions := uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		permissions |= 0o4000
	}
	if mode&os.ModeSetgid != 0 {
		permissions |= 0o2000
	}
	if mode&os.ModeSticky != 0 {
		permissions |= 0o1000
	}
	return permissions
}

// lockDirOpenHook lets the regression test replace .locks after validation.
var lockDirOpenHook func()

// lockFileOpenHook lets the regression test replace a lock after validation.
var lockFileOpenHook func()

// lockModeCheckHook lets the regression test add a link before lock mutation.
var lockModeCheckHook func()

// lockFinalCheckHook lets the regression test change the lock after flocking.
var lockFinalCheckHook func()

func acquireLock(root, digest string, operation int) (*os.File, error) {
	file, path, err := openLockFile(root, digest)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), operation); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errActiveLock
		}
		return nil, fmt.Errorf("lock cache scope: %w", err)
	}
	if err := validateLockFile(file, path); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func acquireRootLock(root *os.Root, digest string, operation int) (*os.File, error) {
	if !isDigest(digest) {
		return nil, fmt.Errorf("%w: invalid digest %q", errUnsafeLock, digest)
	}
	if _, err := ensureRootPrivateDir(root, "."); err != nil {
		return nil, err
	}
	expectedLocks, err := ensureRootPrivateDir(root, ".locks")
	if err != nil {
		return nil, err
	}
	if lockDirOpenHook != nil {
		lockDirOpenHook()
	}
	locks, err := root.OpenRoot(".locks")
	if err != nil {
		return nil, fmt.Errorf("open cache lock directory: %w", err)
	}
	defer locks.Close()
	if err := validateRootDirectory(locks, &expectedLocks); err != nil {
		return nil, err
	}

	lockName := digest + ".lock"
	var expectedLock *syscall.Stat_t
	openFlags := os.O_RDWR
	if info, err := locks.Lstat(lockName); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: %q is not a regular file", errUnsafeLock, lockName)
		}
		mode := rawFileModePermissions(info.Mode())
		if !hasExactLockPermissions(mode) {
			return nil, fmt.Errorf("%w: lock permissions %#o", errUnsafeLock, mode)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, fmt.Errorf("%w: stat lock path", errUnsafeLock)
		}
		expected := *stat
		expectedLock = &expected
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect cache lock: %w", err)
	} else {
		openFlags |= os.O_CREATE | os.O_EXCL
	}
	if lockFileOpenHook != nil {
		lockFileOpenHook()
	}

	file, err := locks.OpenFile(lockName, openFlags, 0o600)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EISDIR) || errors.Is(err, os.ErrExist) || errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q", errUnsafeLock, lockName)
		}
		return nil, fmt.Errorf("open cache lock: %w", err)
	}
	descriptor, err := lockDescriptorStat(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if expectedLock != nil && (descriptor.Dev != expectedLock.Dev || descriptor.Ino != expectedLock.Ino) {
		_ = file.Close()
		return nil, fmt.Errorf("%w: lock inode changed", errUnsafeLock)
	}
	if err := validateRootLockFile(file, locks, lockName); err != nil {
		_ = file.Close()
		return nil, err
	}
	if lockModeCheckHook != nil {
		lockModeCheckHook()
	}
	if err := validateRootLockFile(file, locks, lockName); err != nil {
		_ = file.Close()
		return nil, err
	}
	descriptor, err = lockDescriptorStat(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !hasExactLockPermissions(uint32(descriptor.Mode)) {
		_ = file.Close()
		return nil, fmt.Errorf("%w: lock permissions %#o", errUnsafeLock, uint32(descriptor.Mode)&0o7777)
	}
	if err := syscall.Flock(int(file.Fd()), operation); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errActiveLock
		}
		return nil, fmt.Errorf("lock cache scope: %w", err)
	}
	if lockFinalCheckHook != nil {
		lockFinalCheckHook()
	}
	if err := validateRootLockFile(file, locks, lockName); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, err
	}
	descriptor, err = lockDescriptorStat(file)
	if err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, err
	}
	if !hasExactLockPermissions(uint32(descriptor.Mode)) {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, fmt.Errorf("%w: lock permissions %#o", errUnsafeLock, uint32(descriptor.Mode)&0o7777)
	}
	return file, nil
}

func openLockFile(root, digest string) (*os.File, string, error) {
	if !isDigest(digest) {
		return nil, "", fmt.Errorf("%w: invalid digest %q", errUnsafeLock, digest)
	}
	if err := ensurePrivateDir(root); err != nil {
		return nil, "", err
	}
	locks := filepath.Join(root, ".locks")
	if err := ensurePrivateDir(locks); err != nil {
		return nil, "", err
	}
	path := filepath.Join(locks, digest+".lock")
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, "", fmt.Errorf("%w: %q is not a regular file", errUnsafeLock, path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("inspect cache lock: %w", err)
	}

	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EISDIR) {
			return nil, "", fmt.Errorf("%w: %q", errUnsafeLock, path)
		}
		return nil, "", fmt.Errorf("open cache lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if _, err := lockDescriptorStat(file); err != nil {
		_ = file.Close()
		return nil, "", err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, "", fmt.Errorf("chmod cache lock: %w", err)
	}
	if err := validateLockFile(file, path); err != nil {
		_ = file.Close()
		return nil, "", err
	}
	return file, path, nil
}

func validateLockFile(file *os.File, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: stat lock path: %v", errUnsafeLock, err)
	}
	return validateLockInfo(file, info, path)
}

func validateRootLockFile(file *os.File, locks *os.Root, name string) error {
	info, err := locks.Lstat(name)
	if err != nil {
		return fmt.Errorf("%w: stat lock path: %v", errUnsafeLock, err)
	}
	return validateLockInfo(file, info, name)
}

func validateLockInfo(file *os.File, info os.FileInfo, path string) error {
	descriptor, err := lockDescriptorStat(file)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %q is not a regular file", errUnsafeLock, path)
	}
	pathStat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || descriptor.Dev != pathStat.Dev || descriptor.Ino != pathStat.Ino {
		return fmt.Errorf("%w: lock inode changed", errUnsafeLock)
	}
	return nil
}

func ensureRootPrivateDir(root *os.Root, name string) (syscall.Stat_t, error) {
	info, err := root.Lstat(name)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return syscall.Stat_t{}, err
		}
		if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return syscall.Stat_t{}, err
		}
		info, err = root.Lstat(name)
		if err != nil {
			return syscall.Stat_t{}, err
		}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return syscall.Stat_t{}, fmt.Errorf("%w: cache directory %q is a symlink", errUnsafeLock, name)
	}
	if !info.IsDir() {
		return syscall.Stat_t{}, fmt.Errorf("%w: cache path %q is not a directory", errUnsafeLock, name)
	}
	expected, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return syscall.Stat_t{}, fmt.Errorf("%w: stat cache directory %q", errUnsafeLock, name)
	}
	directory, err := root.Open(name)
	if err != nil {
		return syscall.Stat_t{}, err
	}
	actual, statErr := directoryDescriptorStat(directory)
	if statErr != nil {
		_ = directory.Close()
		return syscall.Stat_t{}, statErr
	}
	if !sameDirectory(expected, &actual) {
		_ = directory.Close()
		return syscall.Stat_t{}, fmt.Errorf("%w: cache directory %q changed", errUnsafeLock, name)
	}
	if err := directory.Chmod(0o700); err != nil {
		_ = directory.Close()
		return syscall.Stat_t{}, fmt.Errorf("chmod cache directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return syscall.Stat_t{}, fmt.Errorf("close cache directory: %w", err)
	}
	return actual, nil
}

func validateRootDirectory(root *os.Root, expected *syscall.Stat_t) error {
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open cache directory descriptor: %w", err)
	}
	actual, statErr := directoryDescriptorStat(directory)
	closeErr := directory.Close()
	if statErr != nil {
		return statErr
	}
	if closeErr != nil {
		return fmt.Errorf("close cache directory descriptor: %w", closeErr)
	}
	if !sameDirectory(expected, &actual) {
		return fmt.Errorf("%w: lock directory changed", errUnsafeLock)
	}
	return nil
}

func directoryDescriptorStat(directory *os.File) (syscall.Stat_t, error) {
	var descriptor syscall.Stat_t
	if err := syscall.Fstat(int(directory.Fd()), &descriptor); err != nil {
		return syscall.Stat_t{}, fmt.Errorf("stat cache directory descriptor: %w", err)
	}
	if descriptor.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return syscall.Stat_t{}, fmt.Errorf("%w: cache directory descriptor is not a directory", errUnsafeLock)
	}
	return descriptor, nil
}

func lockDescriptorStat(file *os.File) (syscall.Stat_t, error) {
	var descriptor syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &descriptor); err != nil {
		return syscall.Stat_t{}, fmt.Errorf("stat cache lock descriptor: %w", err)
	}
	if descriptor.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return syscall.Stat_t{}, fmt.Errorf("%w: lock descriptor is not a regular file", errUnsafeLock)
	}
	if descriptor.Nlink != 1 {
		return syscall.Stat_t{}, fmt.Errorf("%w: lock file has %d links", errUnsafeLock, descriptor.Nlink)
	}
	return descriptor, nil
}

func releaseLock(file *os.File) error {
	if file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}

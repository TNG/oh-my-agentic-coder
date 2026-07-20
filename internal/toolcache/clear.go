package toolcache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

type ClearStatus string

const (
	ClearRemoved ClearStatus = "removed"
	ClearActive  ClearStatus = "active"
	ClearSkipped ClearStatus = "skipped"
)

type ClearResult struct {
	Path   string
	Status ClearStatus
	Reason string
}

var digestName = regexp.MustCompile(`^[0-9a-f]{64}$`)

var errScopeChanged = errors.New("cache scope changed during removal")

// scopeDeleteHook lets the regression test replace a scope after its descriptor is open.
var scopeDeleteHook func()

// rootDeleteHook lets the regression test replace the root before cleanup acquires its lock.
var rootDeleteHook func()

// rootOpenHook and leafOpenHook synchronize replacement regression tests.
var rootOpenHook func()
var leafOpenHook func()

type trustedRoot struct {
	root *os.Root
	path string
	stat syscall.Stat_t
}

type scopeIdentity struct {
	root syscall.Stat_t
	leaf syscall.Stat_t
}

func ClearWorkdir(path string) (ClearResult, error) {
	scope, err := DescribePersistent(DomainWorkdir, path)
	if err != nil {
		return ClearResult{}, err
	}
	return clearScope(filepath.Dir(scope.Dir), scope.Digest, scope.Dir)
}

func ClearAll() ([]ClearResult, error) {
	root, err := persistentRoot()
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect cache root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("unsafe cache root %q", root)
	}
	expectedRoot, ok := directoryStat(info)
	if !ok {
		return nil, fmt.Errorf("unsafe cache root %q", root)
	}
	trusted, err := openTrustedRoot(root, &expectedRoot)
	if errors.Is(err, errScopeChanged) {
		return nil, fmt.Errorf("cache root changed while opening %q", root)
	}
	if err != nil {
		return nil, err
	}
	defer trusted.root.Close()

	entries, err := rootEntries(trusted.root)
	if err != nil {
		return nil, fmt.Errorf("read cache root: %w", err)
	}
	results := make([]ClearResult, 0, len(entries))
	for _, entry := range entries {
		if !isDigest(entry.Name()) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		leafInfo, err := trusted.root.Lstat(entry.Name())
		if err != nil {
			results = append(results, ClearResult{Path: path, Status: ClearSkipped, Reason: "scope changed"})
			continue
		}
		leaf, ok := directoryStat(leafInfo)
		if !ok {
			results = append(results, ClearResult{Path: path, Status: ClearSkipped, Reason: "scope is unsafe"})
			continue
		}
		result, err := clearTrustedScope(trusted, entry.Name(), path, &leaf)
		results = append(results, result)
		if err != nil {
			return results, err
		}
	}
	return results, nil
}

func clearScope(root, digest, path string) (ClearResult, error) {
	result := ClearResult{Path: path}
	identity, reason, err := validateScopeDirectory(root, path)
	if err != nil {
		return result, err
	} else if reason != "" {
		result.Status = ClearSkipped
		result.Reason = reason
		return result, nil
	}

	trusted, err := openTrustedRoot(root, &identity.root)
	if errors.Is(err, errScopeChanged) {
		result.Status = ClearSkipped
		result.Reason = "cache root changed"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer trusted.root.Close()
	return clearTrustedScope(trusted, digest, path, &identity.leaf)
}

func clearTrustedScope(trusted *trustedRoot, digest, path string, expectedLeaf *syscall.Stat_t) (ClearResult, error) {
	result := ClearResult{Path: path}
	if err := verifyTrustedRoot(trusted); errors.Is(err, errScopeChanged) {
		result.Status = ClearSkipped
		result.Reason = "cache root changed"
		return result, nil
	} else if err != nil {
		return result, err
	}

	if rootDeleteHook != nil {
		rootDeleteHook()
	}
	lock, err := acquireRootLock(trusted.root, digest, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, errActiveLock) {
		result.Status = ClearActive
		result.Reason = "scope lock is held"
		return result, nil
	}
	if errors.Is(err, errUnsafeLock) {
		result.Status = ClearSkipped
		result.Reason = "lock path is unsafe"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer releaseLock(lock)

	if err := verifyTrustedRoot(trusted); errors.Is(err, errScopeChanged) {
		result.Status = ClearSkipped
		result.Reason = "cache root changed"
		return result, nil
	} else if err != nil {
		return result, err
	}
	if leafOpenHook != nil {
		leafOpenHook()
	}
	if err := removeScopeDirectory(trusted.root, digest, expectedLeaf); errors.Is(err, errScopeChanged) {
		result.Status = ClearSkipped
		result.Reason = "scope directory changed"
		return result, nil
	} else if err != nil {
		return result, fmt.Errorf("remove cache scope %q: %w", path, err)
	}
	if err := verifyTrustedRoot(trusted); errors.Is(err, errScopeChanged) {
		result.Status = ClearSkipped
		result.Reason = "cache root changed"
		return result, nil
	} else if err != nil {
		return result, err
	}
	result.Status = ClearRemoved
	result.Reason = "inactive scope removed"
	return result, nil
}

func removeScopeDirectory(root *os.Root, name string, expected *syscall.Stat_t) error {
	scope, scopeStat, err := openRootDirectory(root, name, expected)
	if err != nil {
		return err
	}
	defer scope.Close()
	if scopeDeleteHook != nil {
		scopeDeleteHook()
	}
	if err := removeRootContents(scope); err != nil {
		return err
	}
	if err := verifyRootDirectory(root, name, &scopeStat); err != nil {
		return err
	}
	if err := root.Remove(name); err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ENOTEMPTY) {
			return errScopeChanged
		}
		return fmt.Errorf("remove cache scope directory: %w", err)
	}
	return nil
}

func removeRootContents(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open cache directory: %w", err)
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil {
		return fmt.Errorf("read cache directory: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close cache directory: %w", closeErr)
	}
	for _, entry := range entries {
		name := entry.Name()
		info, err := root.Lstat(name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("stat cache entry: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove cache entry: %w", err)
			}
			continue
		}

		childStat, ok := directoryStat(info)
		if !ok {
			return errScopeChanged
		}
		child, childStat, err := openRootDirectory(root, name, &childStat)
		if err != nil {
			return err
		}
		err = removeRootContents(child)
		closeErr := child.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return fmt.Errorf("close cache directory: %w", closeErr)
		}
		if err := verifyRootDirectory(root, name, &childStat); err != nil {
			return err
		}
		if err := root.Remove(name); err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ENOTEMPTY) {
				return errScopeChanged
			}
			return fmt.Errorf("remove cache directory: %w", err)
		}
	}
	return nil
}

func openTrustedRoot(path string, expected *syscall.Stat_t) (*trustedRoot, error) {
	if rootOpenHook != nil {
		rootOpenHook()
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open cache root: %w", err)
	}
	directory, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("open cache root descriptor: %w", err)
	}
	var actual syscall.Stat_t
	statErr := syscall.Fstat(int(directory.Fd()), &actual)
	closeErr := directory.Close()
	if statErr != nil {
		_ = root.Close()
		return nil, fmt.Errorf("stat cache root descriptor: %w", statErr)
	}
	if closeErr != nil {
		_ = root.Close()
		return nil, fmt.Errorf("close cache root descriptor: %w", closeErr)
	}
	if !sameDirectory(expected, &actual) {
		_ = root.Close()
		return nil, errScopeChanged
	}
	return &trustedRoot{root: root, path: path, stat: actual}, nil
}

func verifyTrustedRoot(trusted *trustedRoot) error {
	info, err := os.Lstat(trusted.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errScopeChanged
		}
		return fmt.Errorf("re-stat cache root: %w", err)
	}
	current, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSymlink != 0 || !sameDirectory(&trusted.stat, current) {
		return errScopeChanged
	}
	return nil
}

func openRootDirectory(parent *os.Root, name string, expected *syscall.Stat_t) (*os.Root, syscall.Stat_t, error) {
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, syscall.Stat_t{}, errScopeChanged
	}
	directory, err := child.Open(".")
	if err != nil {
		_ = child.Close()
		return nil, syscall.Stat_t{}, fmt.Errorf("open cache directory descriptor: %w", err)
	}
	var actual syscall.Stat_t
	statErr := syscall.Fstat(int(directory.Fd()), &actual)
	closeErr := directory.Close()
	if statErr != nil {
		_ = child.Close()
		return nil, syscall.Stat_t{}, fmt.Errorf("stat cache directory descriptor: %w", statErr)
	}
	if closeErr != nil {
		_ = child.Close()
		return nil, syscall.Stat_t{}, fmt.Errorf("close cache directory descriptor: %w", closeErr)
	}
	if !sameDirectory(expected, &actual) {
		_ = child.Close()
		return nil, syscall.Stat_t{}, errScopeChanged
	}
	return child, actual, nil
}

func verifyRootDirectory(parent *os.Root, name string, expected *syscall.Stat_t) error {
	info, err := parent.Lstat(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errScopeChanged
		}
		return fmt.Errorf("re-stat cache directory: %w", err)
	}
	current, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSymlink != 0 || !sameDirectory(expected, current) {
		return errScopeChanged
	}
	return nil
}

func sameDirectory(first, second *syscall.Stat_t) bool {
	return first.Mode&syscall.S_IFMT == syscall.S_IFDIR && second.Mode&syscall.S_IFMT == syscall.S_IFDIR && first.Dev == second.Dev && first.Ino == second.Ino
}

func validateScopeDirectory(root, path string) (scopeIdentity, string, error) {
	rootInfo, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return scopeIdentity{}, "cache root does not exist", nil
	}
	if err != nil {
		return scopeIdentity{}, "", fmt.Errorf("inspect cache root: %w", err)
	}
	rootStat, ok := directoryStat(rootInfo)
	if !ok {
		return scopeIdentity{}, "cache root is unsafe", nil
	}
	leafInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return scopeIdentity{}, "scope does not exist", nil
	}
	if err != nil {
		return scopeIdentity{}, "", fmt.Errorf("inspect cache scope: %w", err)
	}
	leafStat, ok := directoryStat(leafInfo)
	if !ok {
		if leafInfo.Mode()&os.ModeSymlink != 0 {
			return scopeIdentity{}, "scope is a symlink", nil
		}
		return scopeIdentity{}, "scope is not a directory", nil
	}
	return scopeIdentity{root: rootStat, leaf: leafStat}, "", nil
}

func directoryStat(info os.FileInfo) (syscall.Stat_t, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return syscall.Stat_t{}, false
	}
	return *stat, true
}

func rootEntries(root *os.Root) ([]os.DirEntry, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil {
		return nil, readErr
	}
	return entries, closeErr
}

func persistentRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "omac"), nil
}

func isDigest(name string) bool {
	return digestName.MatchString(name)
}

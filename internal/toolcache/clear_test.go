package toolcache

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestToolcacheLockHelper(t *testing.T) {
	if os.Getenv("TOOLCACHE_LOCK_HELPER") != "1" {
		return
	}

	path := os.Args[len(os.Args)-1]
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		os.Exit(2)
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		os.Exit(1)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func TestHasExactLockPermissions(t *testing.T) {
	tests := []struct {
		name string
		mode uint32
		want bool
	}{
		{name: "owner read-write only", mode: 0o600, want: true},
		{name: "world readable", mode: 0o644, want: false},
		{name: "owner read only", mode: 0o400, want: false},
		{name: "setuid", mode: 0o4600, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hasExactLockPermissions(test.mode); got != test.want {
				t.Errorf("hasExactLockPermissions(%#o) = %t, want %t", test.mode, got, test.want)
			}
		})
	}
}

func TestRawFileModePermissions(t *testing.T) {
	tests := []struct {
		name string
		mode os.FileMode
		want uint32
	}{
		{name: "owner read-write only", mode: 0o600, want: 0o600},
		{name: "setuid", mode: 0o600 | os.ModeSetuid, want: 0o4600},
		{name: "setgid", mode: 0o600 | os.ModeSetgid, want: 0o2600},
		{name: "sticky", mode: 0o600 | os.ModeSticky, want: 0o1600},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := rawFileModePermissions(test.mode); got != test.want {
				t.Errorf("rawFileModePermissions(%#o) = %#o, want %#o", test.mode, got, test.want)
			}
		})
	}
}

func TestPreparePersistentHoldsSharedLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()

	scope, err := PreparePersistent(DomainWorkdir, workdir)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(home, ".cache", "omac", ".locks", scope.Digest+".lock")
	if info, err := os.Stat(lockPath); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("lock permissions = %#o, want 0600", got)
	}
	assertPrivateDir(t, filepath.Dir(lockPath))

	if lockAttempt(t, lockPath) == nil {
		t.Fatal("exclusive lock succeeded while persistent scope remained open")
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lockAttempt(t, lockPath); err != nil {
		t.Fatalf("exclusive lock after Scope.Close() failed: %v", err)
	}

	t.Run("rejects symlinked lock file", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		workdir := t.TempDir()
		described, err := DescribePersistent(DomainWorkdir, workdir)
		if err != nil {
			t.Fatal(err)
		}
		locks := filepath.Join(home, ".cache", "omac", ".locks")
		if err := os.MkdirAll(locks, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "outside.lock")
		if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(locks, described.Digest+".lock")); err != nil {
			t.Fatal(err)
		}

		if _, err := PreparePersistent(DomainWorkdir, workdir); err == nil {
			t.Fatal("PreparePersistent() succeeded with a symlinked lock file")
		}
	})

	t.Run("rejects non-regular lock file", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		workdir := t.TempDir()
		described, err := DescribePersistent(DomainWorkdir, workdir)
		if err != nil {
			t.Fatal(err)
		}
		locks := filepath.Join(home, ".cache", "omac", ".locks")
		if err := os.MkdirAll(locks, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(locks, described.Digest+".lock"), 0o700); err != nil {
			t.Fatal(err)
		}

		if _, err := PreparePersistent(DomainWorkdir, workdir); err == nil {
			t.Fatal("PreparePersistent() succeeded with a directory lock path")
		}
	})

	t.Run("rejects hard-linked lock file before chmod", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		workdir := t.TempDir()
		described, err := DescribePersistent(DomainWorkdir, workdir)
		if err != nil {
			t.Fatal(err)
		}
		locks := filepath.Join(home, ".cache", "omac", ".locks")
		if err := os.MkdirAll(locks, 0o700); err != nil {
			t.Fatal(err)
		}
		external := filepath.Join(t.TempDir(), "external.lock")
		if err := os.WriteFile(external, []byte("external"), 0o644); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(external)
		if err != nil {
			t.Fatal(err)
		}
		mode := info.Mode().Perm()
		if err := os.Link(external, filepath.Join(locks, described.Digest+".lock")); err != nil {
			t.Fatal(err)
		}

		if _, err := PreparePersistent(DomainWorkdir, workdir); err == nil {
			t.Fatal("PreparePersistent() succeeded with a hard-linked lock file")
		}
		if info, err := os.Stat(external); err != nil {
			t.Fatal(err)
		} else if got := info.Mode().Perm(); got != mode {
			t.Errorf("external hard-link permissions = %#o, want %#o", got, mode)
		}
	})

	t.Run("rejects replaced lock inode", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		root := filepath.Join(home, ".cache", "omac")
		digest := strings.Repeat("a", 64)
		file, path, err := openLockFile(root, digest)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := validateLockFile(file, path); err == nil {
			t.Fatal("validateLockFile() accepted a replaced lock inode")
		}
	})
}

func TestClearWorkdirRemovesInactiveScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	scope, err := PreparePersistent(DomainWorkdir, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope.Dir, "cache-data"), []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := ClearWorkdir(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != ClearRemoved {
		t.Errorf("status = %q, want %q", result.Status, ClearRemoved)
	}
	if _, err := os.Lstat(scope.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("scope directory still exists or lstat failed: %v", err)
	}
}

func TestClearWorkdirRefusesActiveScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	scope, err := PreparePersistent(DomainWorkdir, workdir)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	marker := filepath.Join(scope.Dir, "cache-data")
	if err := os.WriteFile(marker, []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := ClearWorkdir(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != ClearActive {
		t.Errorf("status = %q, want %q", result.Status, ClearActive)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("active scope data was removed or could not be statted: %v", err)
	}
}

func TestClearWorkdirRejectsExistingLockReplacementBeforeOpen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	scope, err := PreparePersistent(DomainWorkdir, workdir)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	marker := filepath.Join(scope.Dir, "cache-data")
	if err := os.WriteFile(marker, []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(filepath.Dir(scope.Dir), ".locks", scope.Digest+".lock")
	replacement := lockPath + ".replacement"
	if err := os.WriteFile(replacement, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	var swapErr error
	oldHook := lockFileOpenHook
	lockFileOpenHook = func() {
		swapErr = os.Rename(replacement, lockPath)
	}
	t.Cleanup(func() { lockFileOpenHook = oldHook })

	result, err := ClearWorkdir(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if swapErr != nil {
		t.Fatal(swapErr)
	}
	if result.Status != ClearSkipped {
		t.Errorf("status = %q, want %q", result.Status, ClearSkipped)
	}
	if result.Reason != "lock path is unsafe" {
		t.Errorf("reason = %q, want %q", result.Reason, "lock path is unsafe")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("active scope data was removed or could not be statted: %v", err)
	}
}

func TestClearWorkdirRejectsConcurrentLockCreationBeforeOpen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	scope, err := PreparePersistent(DomainWorkdir, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(scope.Dir, "cache-data")
	if err := os.WriteFile(marker, []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(filepath.Dir(scope.Dir), ".locks", scope.Digest+".lock")
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	var createErr error
	oldHook := lockFileOpenHook
	lockFileOpenHook = func() {
		if err := os.WriteFile(lockPath, []byte("concurrent-lock"), 0o600); err != nil {
			createErr = err
			return
		}
		createErr = os.Chmod(lockPath, 0o600)
	}
	t.Cleanup(func() { lockFileOpenHook = oldHook })

	result, err := ClearWorkdir(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if createErr != nil {
		t.Fatal(createErr)
	}
	if result.Status != ClearSkipped {
		t.Errorf("status = %q, want %q", result.Status, ClearSkipped)
	}
	if result.Reason != "lock path is unsafe" {
		t.Errorf("reason = %q, want %q", result.Reason, "lock path is unsafe")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("scope data was removed or could not be statted: %v", err)
	}
}

func TestClearWorkdirRejectsLockModeChangedAfterFlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	scope, err := PreparePersistent(DomainWorkdir, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(scope.Dir, "cache-data")
	if err := os.WriteFile(marker, []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(filepath.Dir(scope.Dir), ".locks", scope.Digest+".lock")
	var chmodErr error
	oldHook := lockFinalCheckHook
	lockFinalCheckHook = func() {
		chmodErr = os.Chmod(lockPath, 0o644)
	}
	t.Cleanup(func() { lockFinalCheckHook = oldHook })

	result, err := ClearWorkdir(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if result.Status != ClearSkipped {
		t.Errorf("status = %q, want %q", result.Status, ClearSkipped)
	}
	if result.Reason != "lock path is unsafe" {
		t.Errorf("reason = %q, want %q", result.Reason, "lock path is unsafe")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("scope data was removed or could not be statted: %v", err)
	}
}

func TestClearAllRemovesOnlyDigestDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	scope, err := PreparePersistent(DomainWorkdir, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(scope.Dir)
	malformed := filepath.Join(root, "not-a-cache-digest")
	if err := os.Mkdir(malformed, 0o700); err != nil {
		t.Fatal(err)
	}

	results, err := ClearAll()
	if err != nil {
		t.Fatal(err)
	}
	if resultStatus(results, scope.Dir) != ClearRemoved {
		t.Errorf("scope status = %q, want %q", resultStatus(results, scope.Dir), ClearRemoved)
	}
	if _, err := os.Stat(malformed); err != nil {
		t.Errorf("malformed root entry was removed or could not be statted: %v", err)
	}
}

func TestClearAllSkipsActiveAndUnsafeEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	inactive, err := PreparePersistent(DomainWorkdir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inactive.Dir, "inactive"), []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := inactive.Close(); err != nil {
		t.Fatal(err)
	}
	active, err := PreparePersistent(DomainWorkdir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	activeMarker := filepath.Join(active.Dir, "active")
	if err := os.WriteFile(activeMarker, []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := filepath.Dir(inactive.Dir)
	for _, name := range []string{"not-a-cache-digest", strings.Repeat("g", 64)} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	outsideMarker := filepath.Join(outside, "outside")
	if err := os.WriteFile(outsideMarker, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(root, strings.Repeat("b", 64))
	if err := os.Symlink(outside, symlinkPath); err != nil {
		t.Fatal(err)
	}
	regularPath := filepath.Join(root, strings.Repeat("c", 64))
	if err := os.WriteFile(regularPath, []byte("regular"), 0o600); err != nil {
		t.Fatal(err)
	}

	results, err := ClearAll()
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]ClearStatus{
		inactive.Dir: ClearRemoved,
		active.Dir:   ClearActive,
		symlinkPath:  ClearSkipped,
		regularPath:  ClearSkipped,
	} {
		if got := resultStatus(results, path); got != want {
			t.Errorf("result for %q = %q, want %q", path, got, want)
		}
	}
	if _, err := os.Stat(outsideMarker); err != nil {
		t.Errorf("outside symlink target was removed or could not be statted: %v", err)
	}
	if _, err := os.Stat(activeMarker); err != nil {
		t.Errorf("active scope data was removed or could not be statted: %v", err)
	}
	if _, err := os.Lstat(regularPath); err != nil {
		t.Errorf("regular-file cache entry was removed or could not be lstat'ed: %v", err)
	}
}

func TestClearDoesNotFollowSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	described, err := DescribePersistent(DomainWorkdir, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(described.Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	marker := filepath.Join(outside, "outside")
	if err := os.WriteFile(marker, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, described.Dir); err != nil {
		t.Fatal(err)
	}

	result, err := ClearWorkdir(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != ClearSkipped {
		t.Errorf("status = %q, want %q", result.Status, ClearSkipped)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("outside symlink target was removed or could not be statted: %v", err)
	}
}

func TestClearWorkdirRefusesReplacedScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	scope, err := PreparePersistent(DomainWorkdir, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope.Dir, "original"), []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}

	root := filepath.Dir(scope.Dir)
	replacement := filepath.Join(root, "replacement")
	if err := os.Mkdir(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	replacementMarker := filepath.Join(replacement, "must-survive")
	if err := os.WriteFile(replacementMarker, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	displaced := filepath.Join(root, "displaced")
	hookEntered := make(chan struct{})
	replacementDone := make(chan struct{})
	replaceErr := make(chan error, 1)
	oldHook := scopeDeleteHook
	scopeDeleteHook = func() {
		close(hookEntered)
		<-replacementDone
	}
	t.Cleanup(func() { scopeDeleteHook = oldHook })
	go func() {
		<-hookEntered
		if err := os.Rename(scope.Dir, displaced); err != nil {
			replaceErr <- err
			close(replacementDone)
			return
		}
		replaceErr <- os.Rename(replacement, scope.Dir)
		close(replacementDone)
	}()

	result, err := ClearWorkdir(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-replaceErr; err != nil {
		t.Fatal(err)
	}
	if result.Status != ClearSkipped {
		t.Errorf("status = %q, want %q", result.Status, ClearSkipped)
	}
	if _, err := os.Stat(filepath.Join(scope.Dir, filepath.Base(replacementMarker))); err != nil {
		t.Errorf("replacement scope was recursively removed or could not be statted: %v", err)
	}
}

func TestClearWorkdirRefusesReplacedRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	scope, err := PreparePersistent(DomainWorkdir, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}

	root := filepath.Dir(scope.Dir)
	parent := filepath.Dir(root)
	replacement := filepath.Join(parent, "replacement-root")
	if err := os.Mkdir(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	replacementScope := filepath.Join(replacement, scope.Digest)
	if err := os.Mkdir(replacementScope, 0o700); err != nil {
		t.Fatal(err)
	}
	replacementMarker := filepath.Join(replacementScope, "must-survive")
	if err := os.WriteFile(replacementMarker, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	displaced := filepath.Join(parent, "displaced-root")
	hookEntered := make(chan struct{})
	replacementDone := make(chan struct{})
	replaceErr := make(chan error, 1)
	oldHook := rootOpenHook
	rootOpenHook = func() {
		close(hookEntered)
		<-replacementDone
	}
	t.Cleanup(func() { rootOpenHook = oldHook })
	go func() {
		<-hookEntered
		if err := os.Rename(root, displaced); err != nil {
			replaceErr <- err
			close(replacementDone)
			return
		}
		replaceErr <- os.Rename(replacement, root)
		close(replacementDone)
	}()

	result, err := ClearWorkdir(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-replaceErr; err != nil {
		t.Fatal(err)
	}
	if result.Status != ClearSkipped {
		t.Errorf("status = %q, want %q", result.Status, ClearSkipped)
	}
	if _, err := os.Stat(filepath.Join(root, scope.Digest, filepath.Base(replacementMarker))); err != nil {
		t.Errorf("replacement root scope was removed or could not be statted: %v", err)
	}
}

func TestClearWorkdirDoesNotTouchReplacementRootWhileAcquiringLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	scope, err := PreparePersistent(DomainWorkdir, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}

	root := filepath.Dir(scope.Dir)
	parent := filepath.Dir(root)
	replacement := filepath.Join(parent, "replacement-root-before-lock")
	locks := filepath.Join(replacement, ".locks")
	if err := os.MkdirAll(locks, 0o700); err != nil {
		t.Fatal(err)
	}
	lockName := scope.Digest + ".lock"
	replacementLock := filepath.Join(locks, lockName)
	if err := os.WriteFile(replacementLock, []byte("replacement-marker"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(replacementLock, 0o644); err != nil {
		t.Fatal(err)
	}

	displaced := filepath.Join(parent, "displaced-root-before-lock")
	var replaceErr error
	oldHook := rootDeleteHook
	rootDeleteHook = func() {
		if err := os.Rename(root, displaced); err != nil {
			replaceErr = err
			return
		}
		replaceErr = os.Rename(replacement, root)
	}
	t.Cleanup(func() { rootDeleteHook = oldHook })

	result, err := ClearWorkdir(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if replaceErr != nil {
		t.Fatal(replaceErr)
	}
	if result.Status != ClearSkipped {
		t.Errorf("status = %q, want %q", result.Status, ClearSkipped)
	}
	if result.Reason != "cache root changed" {
		t.Errorf("reason = %q, want %q", result.Reason, "cache root changed")
	}
	replacementLock = filepath.Join(root, ".locks", lockName)
	if info, err := os.Stat(replacementLock); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("replacement lock permissions = %#o, want 0644", got)
	}
	if content, err := os.ReadFile(replacementLock); err != nil {
		t.Fatal(err)
	} else if got := string(content); got != "replacement-marker" {
		t.Errorf("replacement lock content = %q, want %q", got, "replacement-marker")
	}
}

func TestClearWorkdirDoesNotFollowSwappedLockDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	scope, err := PreparePersistent(DomainWorkdir, workdir)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()

	root := filepath.Dir(scope.Dir)
	locks := filepath.Join(root, ".locks")
	lockName := scope.Digest + ".lock"
	alternateLock := filepath.Join(root, lockName)
	if err := os.WriteFile(alternateLock, []byte("alternate-marker"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(alternateLock, 0o644); err != nil {
		t.Fatal(err)
	}

	displacedLocks := filepath.Join(root, ".locks-displaced")
	var swapErr error
	oldHook := lockDirOpenHook
	lockDirOpenHook = func() {
		if err := os.Rename(locks, displacedLocks); err != nil {
			swapErr = err
			return
		}
		swapErr = os.Symlink(".", locks)
	}
	t.Cleanup(func() { lockDirOpenHook = oldHook })

	result, err := ClearWorkdir(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if swapErr != nil {
		t.Fatal(swapErr)
	}
	if result.Status != ClearSkipped {
		t.Errorf("status = %q, want %q", result.Status, ClearSkipped)
	}
	if info, err := os.Stat(alternateLock); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("alternate lock permissions = %#o, want 0644", got)
	}
	if content, err := os.ReadFile(alternateLock); err != nil {
		t.Fatal(err)
	} else if got := string(content); got != "alternate-marker" {
		t.Errorf("alternate lock content = %q, want %q", got, "alternate-marker")
	}
	if _, err := os.Stat(scope.Dir); err != nil {
		t.Errorf("active scope was removed or could not be statted: %v", err)
	}
}

func TestClearWorkdirDoesNotTouchSwappedLockTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	scope, err := PreparePersistent(DomainWorkdir, workdir)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()

	root := filepath.Dir(scope.Dir)
	locks := filepath.Join(root, ".locks")
	lockPath := filepath.Join(locks, scope.Digest+".lock")
	targetName := "alternate.lock"
	targetPath := filepath.Join(locks, targetName)
	if err := os.WriteFile(targetPath, []byte("alternate-marker"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(targetPath, 0o644); err != nil {
		t.Fatal(err)
	}

	var swapErr error
	oldHook := lockFileOpenHook
	lockFileOpenHook = func() {
		if err := os.Remove(lockPath); err != nil {
			swapErr = err
			return
		}
		swapErr = os.Symlink(targetName, lockPath)
	}
	t.Cleanup(func() { lockFileOpenHook = oldHook })

	result, err := ClearWorkdir(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if swapErr != nil {
		t.Fatal(swapErr)
	}
	if result.Status != ClearSkipped {
		t.Errorf("status = %q, want %q", result.Status, ClearSkipped)
	}
	if info, err := os.Stat(targetPath); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("alternate lock permissions = %#o, want 0644", got)
	}
	if content, err := os.ReadFile(targetPath); err != nil {
		t.Fatal(err)
	} else if got := string(content); got != "alternate-marker" {
		t.Errorf("alternate lock content = %q, want %q", got, "alternate-marker")
	}
	if _, err := os.Stat(scope.Dir); err != nil {
		t.Errorf("active scope was removed or could not be statted: %v", err)
	}
}

func TestClearWorkdirDoesNotTouchHardLinkedLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	scope, err := PreparePersistent(DomainWorkdir, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}

	root := filepath.Dir(scope.Dir)
	lockPath := filepath.Join(root, ".locks", scope.Digest+".lock")
	if err := os.WriteFile(lockPath, []byte("lock-marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lockPath, 0o600); err != nil {
		t.Fatal(err)
	}
	externalLink := filepath.Join(root, "external.lock")

	var linkErr error
	oldHook := lockModeCheckHook
	lockModeCheckHook = func() {
		linkErr = os.Link(lockPath, externalLink)
	}
	t.Cleanup(func() { lockModeCheckHook = oldHook })

	result, err := ClearWorkdir(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if linkErr != nil {
		t.Fatal(linkErr)
	}
	if result.Status != ClearSkipped {
		t.Errorf("status = %q, want %q", result.Status, ClearSkipped)
	}
	if info, err := os.Stat(externalLink); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("external link permissions = %#o, want 0600", got)
	}
	if content, err := os.ReadFile(externalLink); err != nil {
		t.Fatal(err)
	} else if got := string(content); got != "lock-marker" {
		t.Errorf("external link content = %q, want %q", got, "lock-marker")
	}
	if _, err := os.Stat(scope.Dir); err != nil {
		t.Errorf("scope was removed or could not be statted: %v", err)
	}
}

func TestClearWorkdirRejectsLockCreatedWithInsecureMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	scope, err := PreparePersistent(DomainWorkdir, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(scope.Dir, "must-survive")
	if err := os.WriteFile(marker, []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(filepath.Dir(scope.Dir), ".locks", scope.Digest+".lock")
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	var createErr error
	oldHook := lockFileOpenHook
	lockFileOpenHook = func() {
		if err := os.WriteFile(lockPath, []byte("late-lock"), 0o644); err != nil {
			createErr = err
			return
		}
		createErr = os.Chmod(lockPath, 0o644)
	}
	t.Cleanup(func() { lockFileOpenHook = oldHook })

	result, err := ClearWorkdir(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if createErr != nil {
		t.Fatal(createErr)
	}
	if result.Status != ClearSkipped {
		t.Errorf("status = %q, want %q", result.Status, ClearSkipped)
	}
	if result.Reason != "lock path is unsafe" {
		t.Errorf("reason = %q, want %q", result.Reason, "lock path is unsafe")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("scope was removed or could not be statted: %v", err)
	}
}

func TestClearWorkdirRejectsInaccessibleLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	scope, err := PreparePersistent(DomainWorkdir, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(scope.Dir, "must-survive")
	if err := os.WriteFile(marker, []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(filepath.Dir(scope.Dir), ".locks", scope.Digest+".lock")
	if err := os.Chmod(lockPath, 0o400); err != nil {
		t.Fatal(err)
	}

	result, err := ClearWorkdir(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != ClearSkipped {
		t.Errorf("status = %q, want %q", result.Status, ClearSkipped)
	}
	if result.Reason != "lock path is unsafe" {
		t.Errorf("reason = %q, want %q", result.Reason, "lock path is unsafe")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("scope was removed or could not be statted: %v", err)
	}
}

func TestClearWorkdirRefusesLeafReplacementBeforeOpen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	scope, err := PreparePersistent(DomainWorkdir, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}

	root := filepath.Dir(scope.Dir)
	replacement := filepath.Join(root, "replacement-before-open")
	if err := os.Mkdir(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(replacement, "must-survive")
	if err := os.WriteFile(marker, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	displaced := filepath.Join(root, "displaced-before-open")
	hookEntered := make(chan struct{})
	done := make(chan struct{})
	replaceErr := make(chan error, 1)
	oldHook := leafOpenHook
	leafOpenHook = func() {
		close(hookEntered)
		<-done
	}
	t.Cleanup(func() { leafOpenHook = oldHook })
	go func() {
		<-hookEntered
		if err := os.Rename(scope.Dir, displaced); err != nil {
			replaceErr <- err
			close(done)
			return
		}
		replaceErr <- os.Rename(replacement, scope.Dir)
		close(done)
	}()

	result, err := ClearWorkdir(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-replaceErr; err != nil {
		t.Fatal(err)
	}
	if result.Status != ClearSkipped {
		t.Errorf("status = %q, want %q", result.Status, ClearSkipped)
	}
	if _, err := os.Stat(filepath.Join(scope.Dir, filepath.Base(marker))); err != nil {
		t.Errorf("replacement scope was removed or could not be statted: %v", err)
	}
}

func TestClearAllRefusesRootReplacementBeforeOpen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	scope, err := PreparePersistent(DomainWorkdir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}

	root := filepath.Dir(scope.Dir)
	parent := filepath.Dir(root)
	replacement := filepath.Join(parent, "replacement-root-before-open")
	if err := os.Mkdir(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	replacementScope := filepath.Join(replacement, scope.Digest)
	if err := os.Mkdir(replacementScope, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(replacementScope, "must-survive")
	if err := os.WriteFile(marker, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	displaced := filepath.Join(parent, "displaced-root-before-open")
	hookEntered := make(chan struct{})
	done := make(chan struct{})
	replaceErr := make(chan error, 1)
	oldHook := rootOpenHook
	rootOpenHook = func() {
		close(hookEntered)
		<-done
	}
	t.Cleanup(func() { rootOpenHook = oldHook })
	go func() {
		<-hookEntered
		if err := os.Rename(root, displaced); err != nil {
			replaceErr <- err
			close(done)
			return
		}
		replaceErr <- os.Rename(replacement, root)
		close(done)
	}()

	if _, err := ClearAll(); err == nil {
		t.Fatal("ClearAll() succeeded after root replacement before descriptor open")
	}
	if err := <-replaceErr; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, scope.Digest, filepath.Base(marker))); err != nil {
		t.Errorf("replacement root scope was removed or could not be statted: %v", err)
	}
}

func lockAttempt(t *testing.T, path string) error {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestToolcacheLockHelper$", "--", path)
	command.Env = append(os.Environ(), "TOOLCACHE_LOCK_HELPER=1")
	return command.Run()
}

func resultStatus(results []ClearResult, path string) ClearStatus {
	for _, result := range results {
		if result.Path == path {
			return result.Status
		}
	}
	return ""
}

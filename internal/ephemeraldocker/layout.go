package ephemeraldocker

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// scopeDirName carves the feature's state out of the resolved omac
	// cache scope; the scope's sharing and clearing policy governs it.
	scopeDirName = "ephemeral-docker"
	imageFileDir = "ephemeral-docker"
	imageFilePat = "alpine-cloud.qcow2"
	markerName   = "session.json"
	lockName     = "lock"
	limaYAMLName = "lima.yaml"

	// vmPrefix marks every limactl instance this feature owns. The sweep
	// and the QEMU reaper match on it plus the session id; foreign lima
	// instances (colima etc.) never match.
	vmPrefix = "omac-eph-"

	// symlinkPrefix marks the short LIMA_HOME aliases under /tmp.
	symlinkPrefix = "omac-eph-docker-"

	// The physical state lives deep inside the cache scope, but macOS
	// rejects unix sockets whose path is >= 104 bytes and lima builds
	// guestagent control sockets as <LIMA_HOME>/<vm>/ssh.sock.<nonce>.
	// A cache-scope path (~/.cache/omac/<64 hex>) can never fit, so lima
	// receives this short alias while the state stays in the scope.
	symlinkDir = "/tmp"

	maxUnixSocketPath = 104
	sshNonceAllowance = 20
)

// SessionMarker is the on-disk identity of one session's VM unit. The
// sweep reads it to recognize and reap the unit after a crash.
type SessionMarker struct {
	VMName   string    `json:"vm_name"`
	HostPort int       `json:"host_port"`
	Symlink  string    `json:"symlink"`
	Created  time.Time `json:"created"`
}

// Layout is the session's on-disk footprint: the private LIMA_HOME
// (physically inside the cache scope, exposed to lima via a short /tmp
// symlink), its marker, and its liveness lock.
type Layout struct {
	// Root is the feature's directory inside the cache scope
	// (<cache>/ephemeral-docker), shared with previous sessions.
	Root string
	// ImageFile is the digest-pinned base image shared across sessions
	// of this cache scope.
	ImageFile string
	// Dir is the per-session private LIMA_HOME.
	Dir string
	// VMName is the limactl instance name (omac-eph-<hex>).
	VMName string
	// HostPort is the deterministic loopback port forwarding to the
	// guest docker endpoint.
	HostPort int
	// SymlinkPath is the short alias lima sees as LIMA_HOME.
	SymlinkPath string
	// MarkerPath is the session.json marker file.
	MarkerPath string
	// LockPath is the flock-held liveness file.
	LockPath string
	// LimaYAML is where the generated config is written.
	LimaYAML string

	lockFile *os.File
}

// NewLayout creates the session directory inside the cache scope, writes
// the marker, takes the liveness lock, and installs the short /tmp
// symlink. Callers hold the returned layout for the session lifetime and
// call Release to drop the lock (the sweep then treats the unit as
// orphaned once the process is gone).
func NewLayout(cacheDir, workdir string, isFree func(int) bool) (*Layout, error) {
	if cacheDir == "" {
		return nil, errors.New("ephemeral-docker: cache dir required")
	}
	port, err := HostPortFor(workdir, isFree)
	if err != nil {
		return nil, err
	}
	var sessID [6]byte
	if _, err := rand.Read(sessID[:]); err != nil {
		return nil, fmt.Errorf("ephemeral-docker: session id: %w", err)
	}
	var vmID [4]byte
	if _, err := rand.Read(vmID[:]); err != nil {
		return nil, fmt.Errorf("ephemeral-docker: vm id: %w", err)
	}
	sess := hex.EncodeToString(sessID[:])
	lay := &Layout{
		Root:        filepath.Join(cacheDir, scopeDirName),
		ImageFile:   filepath.Join(cacheDir, imageFileDir, imageFilePat),
		Dir:         filepath.Join(cacheDir, scopeDirName, sess),
		VMName:      vmPrefix + hex.EncodeToString(vmID[:]),
		HostPort:    port,
		SymlinkPath: filepath.Join(symlinkDir, symlinkPrefix+sess),
		MarkerPath:  "",
		LockPath:    "",
		LimaYAML:    "",
	}
	lay.MarkerPath = filepath.Join(lay.Dir, markerName)
	lay.LockPath = filepath.Join(lay.Dir, lockName)
	lay.LimaYAML = filepath.Join(lay.Dir, limaYAMLName)

	// Guard the unix-socket path limit before anything boots: lima builds
	// <LIMA_HOME>/<vm>/ssh.sock.<nonce> and macOS rejects >= 104 bytes.
	probe := lay.SymlinkPath + string(filepath.Separator) + lay.VMName + "/ssh.sock."
	if len(probe)+sshNonceAllowance >= maxUnixSocketPath {
		return nil, fmt.Errorf("ephemeral-docker: LIMA_HOME alias %s too long for lima unix sockets", lay.SymlinkPath)
	}

	if err := os.MkdirAll(lay.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("ephemeral-docker: session dir: %w", err)
	}
	// Liveness lock: held for the session's lifetime; a crashed parent
	// drops it implicitly, which is what makes the sweep safe.
	lock, err := os.OpenFile(lay.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("ephemeral-docker: lock file: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("ephemeral-docker: session lock held elsewhere: %w", err)
	}
	lay.lockFile = lock

	// Replace any leftover at the alias path (a stale dangling symlink
	// with the same random name), then point it at the session dir.
	if err := os.Remove(lay.SymlinkPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = lock.Close()
		return nil, fmt.Errorf("ephemeral-docker: clear stale alias %s: %w", lay.SymlinkPath, err)
	}
	if err := os.Symlink(lay.Dir, lay.SymlinkPath); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("ephemeral-docker: LIMA_HOME alias: %w", err)
	}

	marker := SessionMarker{
		VMName:   lay.VMName,
		HostPort: lay.HostPort,
		Symlink:  lay.SymlinkPath,
		Created:  time.Now().UTC(),
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("ephemeral-docker: marshal marker: %w", err)
	}
	if err := os.WriteFile(lay.MarkerPath, raw, 0o600); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("ephemeral-docker: write marker: %w", err)
	}
	return lay, nil
}

// Release drops the session liveness lock. It does not remove any state —
// teardown does. Safe to call multiple times.
func (l *Layout) Release() error {
	if l == nil || l.lockFile == nil {
		return nil
	}
	err := syscall.Flock(int(l.lockFile.Fd()), syscall.LOCK_UN)
	cerr := l.lockFile.Close()
	l.lockFile = nil
	if err != nil {
		return err
	}
	return cerr
}

// tryLock attempts a non-blocking exclusive flock on path. It reports
// whether the lock was acquired; a live session's lock is never
// acquirable, so acquiring means "no one is holding it". The returned
// cleanup func unlocks and closes; it is nil when not acquired.
func tryLock(path string) (bool, func()) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0o600)
	if err != nil {
		// A missing lock file means the unit never finished layout (or
		// was already reaped): treat as acquirable so the sweep cleans
		// it up.
		return true, func() {}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return false, nil
	}
	return true, func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

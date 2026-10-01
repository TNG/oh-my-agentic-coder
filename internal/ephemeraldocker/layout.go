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

	// shortHomePrefix marks the feature's LIMA_HOME directories under
	// /tmp.
	shortHomePrefix = "omac-eph-docker-"

	// The session's bookkeeping lives deep inside the cache scope, but
	// lima builds guestagent control sockets as
	// <LIMA_HOME>/<vm>/ssh.sock.<nonce> and rejects paths >= 104 bytes.
	// A cache-scope path (~/.cache/omac/<64 hex>) can never fit, and
	// lima resolves symlinks before that check, so an alias cannot hide
	// the depth: lima needs a real, short directory.
	shortHomeDir = "/tmp"

	maxUnixSocketPath = 104
	sshNonceAllowance = 20
)

// SessionMarker is the on-disk identity of one session's VM unit. The
// sweep reads it to recognize and reap the unit after a crash.
type SessionMarker struct {
	VMName   string    `json:"vm_name"`
	HostPort int       `json:"host_port"`
	Created  time.Time `json:"created"`
}

// Layout is the session's on-disk footprint: the bookkeeping dir in the
// cache scope (marker, lock, lima.yaml), the short real LIMA_HOME under
// /tmp that lima needs for its unix sockets, and the liveness lock.
type Layout struct {
	// Root is the feature's directory inside the cache scope
	// (<cache>/ephemeral-docker), shared with previous sessions.
	Root string
	// ImageFile is the digest-pinned base image shared across sessions
	// of this cache scope.
	ImageFile string
	// Dir is the per-session bookkeeping dir in the cache scope.
	Dir string
	// VMName is the limactl instance name (omac-eph-<hex>).
	VMName string
	// HostPort is the deterministic loopback port forwarding to the
	// guest docker endpoint.
	HostPort int
	// LimaHome is the short real LIMA_HOME under /tmp that lima
	// receives; the VM's instance data lives there, not in the scope.
	LimaHome string
	// MarkerPath is the session.json marker file.
	MarkerPath string
	// LockPath is the flock-held liveness file.
	LockPath string
	// LimaYAML is where the generated config is written.
	LimaYAML string

	lockFile *os.File
}

// NewLayout creates the session's bookkeeping dir inside the cache
// scope, writes the marker, takes the liveness lock, and creates the
// short real LIMA_HOME under /tmp. Callers hold the returned layout for
// the session lifetime and call Release to drop the lock (the sweep then
// treats the unit as orphaned once the process is gone).
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
		LimaHome:    filepath.Join(shortHomeDir, shortHomePrefix+sess),
		MarkerPath:  "",
		LockPath:    "",
		LimaYAML:    "",
	}
	lay.MarkerPath = filepath.Join(lay.Dir, markerName)
	lay.LockPath = filepath.Join(lay.Dir, lockName)
	lay.LimaYAML = filepath.Join(lay.Dir, limaYAMLName)

	// Guard the unix-socket path limit before anything boots: lima builds
	// <LIMA_HOME>/<vm>/ssh.sock.<nonce> and rejects paths >= 104 bytes.
	probe := lay.LimaHome + string(filepath.Separator) + lay.VMName + "/ssh.sock."
	if len(probe)+sshNonceAllowance >= maxUnixSocketPath {
		return nil, fmt.Errorf("ephemeral-docker: LIMA_HOME %s too long for lima unix sockets", lay.LimaHome)
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

	// Replace any leftover at the home path (a stale unit with the same
	// random name), then create the short real LIMA_HOME.
	if err := os.RemoveAll(lay.LimaHome); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = lock.Close()
		return nil, fmt.Errorf("ephemeral-docker: clear stale LIMA_HOME %s: %w", lay.LimaHome, err)
	}
	if err := os.MkdirAll(lay.LimaHome, 0o700); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("ephemeral-docker: LIMA_HOME: %w", err)
	}

	marker := SessionMarker{
		VMName:   lay.VMName,
		HostPort: lay.HostPort,
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

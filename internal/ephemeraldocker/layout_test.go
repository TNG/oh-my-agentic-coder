package ephemeraldocker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLayoutCreatesMarkedSessionDir(t *testing.T) {
	cache := t.TempDir()
	lay, err := NewLayout(cache, "/Users/me/work/proj", func(int) bool { return true })
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	defer lay.Release()

	if !strings.HasPrefix(lay.Dir, filepath.Join(cache, "ephemeral-docker")) {
		t.Errorf("session dir %s must live under the cache scope", lay.Dir)
	}
	// Symlink must point at the session dir and carry the marker prefix.
	target, err := os.Readlink(lay.SymlinkPath)
	if err != nil {
		t.Fatalf("symlink missing: %v", err)
	}
	if target != lay.Dir {
		t.Errorf("symlink -> %s, want %s", target, lay.Dir)
	}
	if !strings.HasPrefix(filepath.Base(lay.SymlinkPath), "omac-eph-docker-") {
		t.Errorf("symlink %s must carry the omac-eph-docker marker", lay.SymlinkPath)
	}
	// VM name must be recognizable and short enough for QEMU argv matching.
	if !strings.HasPrefix(lay.VMName, "omac-eph-") {
		t.Errorf("VM name %q must carry the omac-eph marker", lay.VMName)
	}

	// Marker file must round-trip the sweep-critical fields.
	raw, err := os.ReadFile(lay.MarkerPath)
	if err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	var m SessionMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("marker not JSON: %v", err)
	}
	if m.VMName != lay.VMName || m.HostPort != lay.HostPort || m.Symlink != lay.SymlinkPath {
		t.Errorf("marker drifted: %+v", m)
	}
}

func TestLayoutSocketPathsStayUnderUnixLimit(t *testing.T) {
	// Lima builds guestagent control sockets as
	// <LIMA_HOME>/<vm>/ssh.sock.<nonce>; macOS rejects paths >= 104 bytes.
	// The short /tmp symlink is the workaround; guard the invariant.
	cache := t.TempDir()
	lay, err := NewLayout(cache, "/Users/me/work/proj", func(int) bool { return true })
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	defer lay.Release()
	// Lima binds its control sockets via the short alias, not the deep
	// physical cache-scope path; only the alias-based string counts
	// against the 104-byte unix socket limit.
	probe := filepath.Join(lay.SymlinkPath, lay.VMName, "ssh.sock.0123456789abcdef")
	if len(probe) >= 104 {
		t.Errorf("socket-relevant path too long (%d): %s", len(probe), probe)
	}
	if !strings.Contains(probe, "omac-eph-docker-") {
		t.Errorf("unexpected alias shape: %s", probe)
	}
}

func TestLayoutLockGuardsOrphanDetection(t *testing.T) {
	cache := t.TempDir()
	lay, err := NewLayout(cache, "/Users/me/work/proj", func(int) bool { return true })
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if acquired, _ := tryLock(lay.LockPath); acquired {
		t.Error("the live session's lock must not be acquirable by the sweep")
	}
	lay.Release()
	acquired, cleanup := tryLock(lay.LockPath)
	if !acquired {
		t.Fatal("after Release the session must look orphaned to the sweep")
	}
	cleanup()
}

func TestLayoutReleaseIdempotent(t *testing.T) {
	cache := t.TempDir()
	lay, err := NewLayout(cache, "/Users/me/work/proj", func(int) bool { return true })
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if err := lay.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := lay.Release(); err != nil {
		t.Fatalf("second Release must be a no-op: %v", err)
	}
	if acquired, cleanup := tryLock(lay.LockPath); !acquired {
		t.Error("lock must be free after Release")
	} else {
		cleanup()
	}
}

func TestLayoutPortDeterministicForWorktree(t *testing.T) {
	cache := t.TempDir()
	a, err := NewLayout(cache, "/Users/me/work/proj", func(int) bool { return true })
	if err != nil {
		t.Fatalf("NewLayout A: %v", err)
	}
	defer a.Release()
	b, err := NewLayout(cache, "/Users/me/work/proj", func(int) bool { return true })
	if err != nil {
		t.Fatalf("NewLayout B: %v", err)
	}
	defer b.Release()
	if a.HostPort != b.HostPort {
		t.Errorf("same worktree resolved different ports: %d vs %d", a.HostPort, b.HostPort)
	}
}

func TestLayoutRejectsEmptyCacheDir(t *testing.T) {
	if _, err := NewLayout("", "/w", func(int) bool { return true }); err == nil {
		t.Fatal("empty cache dir must be rejected")
	}
}

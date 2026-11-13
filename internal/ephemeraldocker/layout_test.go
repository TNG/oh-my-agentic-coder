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
	t.Cleanup(func() { _ = os.RemoveAll(lay.LimaHome) })
	defer lay.Release()

	if !strings.HasPrefix(lay.Dir, filepath.Join(cache, "ephemeral-docker")) {
		t.Errorf("session dir %s must live under the cache scope", lay.Dir)
	}
	// LIMA_HOME must be a real directory under /tmp carrying the marker
	// prefix; lima resolves symlinks, so an alias could not hide the
	// deep cache-scope path.
	info, err := os.Lstat(lay.LimaHome)
	if err != nil {
		t.Fatalf("LIMA_HOME missing: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("LIMA_HOME %s must be a real directory", lay.LimaHome)
	}
	if !strings.HasPrefix(filepath.Base(lay.LimaHome), "omac-eph-docker-") {
		t.Errorf("LIMA_HOME %s must carry the omac-eph-docker marker", lay.LimaHome)
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
	if m.VMName != lay.VMName || m.HostPort != lay.HostPort {
		t.Errorf("marker drifted: %+v", m)
	}
}

func TestLayoutSocketPathsStayUnderUnixLimit(t *testing.T) {
	// Lima builds guestagent control sockets as
	// <LIMA_HOME>/<vm>/ssh.sock.<nonce> and rejects paths >= 104 bytes,
	// resolving symlinks first; hence the short real home under /tmp.
	// Guard the invariant.
	cache := t.TempDir()
	lay, err := NewLayout(cache, "/Users/me/work/proj", func(int) bool { return true })
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(lay.LimaHome) })
	defer lay.Release()
	probe := filepath.Join(lay.LimaHome, lay.VMName, "ssh.sock.0123456789abcdef")
	if len(probe) >= 104 {
		t.Errorf("socket-relevant path too long (%d): %s", len(probe), probe)
	}
	if !strings.Contains(probe, "omac-eph-docker-") {
		t.Errorf("unexpected LIMA_HOME shape: %s", probe)
	}
}

func TestLayoutLockGuardsOrphanDetection(t *testing.T) {
	cache := t.TempDir()
	lay, err := NewLayout(cache, "/Users/me/work/proj", func(int) bool { return true })
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(lay.LimaHome) })
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
	t.Cleanup(func() { _ = os.RemoveAll(lay.LimaHome) })
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
	t.Cleanup(func() { _ = os.RemoveAll(a.LimaHome) })
	defer a.Release()
	b, err := NewLayout(cache, "/Users/me/work/proj", func(int) bool { return true })
	if err != nil {
		t.Fatalf("NewLayout B: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(b.LimaHome) })
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

//go:build linux

package sandboxrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/tngtech/oh-my-agentic-coder/internal/toolcache"
)

// TestIntegrationCacheScopeIsolation proves the sandbox grants only the
// selected cache leaf and denies the host-global cache root, sibling
// scopes, and the .locks directory.
//
// The test creates host markers under a uniquely named directory beneath
// the real user home (not under /tmp or $TMPDIR, which the Linux baseline
// grants WRITE — that blanket temp-write would mask the read-only/denied
// assertions). HOME is redirected to that directory so toolcache computes
// its cache root inside it.
func TestIntegrationCacheScopeIsolation(t *testing.T) {
	requireBwrap(t)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	testRoot, err := os.MkdirTemp(home, ".omac-cache-integ-")
	if err != nil {
		t.Skipf("cannot create test dir under home: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(testRoot) })
	t.Setenv("HOME", testRoot)

	// Two "workdirs" whose persistent cache scopes land under
	// testRoot/.cache/omac/<digest>.
	wd1 := filepath.Join(testRoot, "workdir1")
	if err := os.MkdirAll(wd1, 0o755); err != nil {
		t.Fatal(err)
	}
	wd2 := filepath.Join(testRoot, "workdir2")
	if err := os.MkdirAll(wd2, 0o755); err != nil {
		t.Fatal(err)
	}
	scope1, err := toolcache.DescribePersistent(toolcache.DomainWorkdir, wd1)
	if err != nil {
		t.Fatal(err)
	}
	scope2, err := toolcache.DescribePersistent(toolcache.DomainWorkdir, wd2)
	if err != nil {
		t.Fatal(err)
	}
	if scope1.Digest == scope2.Digest {
		t.Fatal("scope digests collided")
	}
	cacheRoot := filepath.Dir(scope1.Dir)
	locksDir := filepath.Join(cacheRoot, ".locks")

	// Create all directories and host markers.
	for _, d := range []string{scope1.Dir, scope2.Dir, locksDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	hostGlobalMarker := filepath.Join(cacheRoot, "host-global-marker")
	if err := os.WriteFile(hostGlobalMarker, []byte("host-global-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	siblingMarker := filepath.Join(scope2.Dir, "sibling-secret")
	if err := os.WriteFile(siblingMarker, []byte("sibling-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockMarker := filepath.Join(locksDir, scope1.Digest+".lock")
	if err := os.WriteFile(lockMarker, []byte("lock-marker"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Grant only scope1.Dir (the selected cache leaf) as writable.
	workdir := t.TempDir()
	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{
			Allow: []string{scope1.Dir},
		},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, workdir, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}

	// --- Assertion: host global cache marker is unreadable ---
	out, code := runBwrapped(t, g, "/bin/sh", "-c", "cat "+hostGlobalMarker+" 2>&1")
	if code == 0 {
		t.Errorf("host global cache marker should be unreadable, got: %s", out)
	}
	if strings.Contains(out, "host-global-secret") {
		t.Errorf("SECURITY: host global cache marker leaked: %s", out)
	}

	// --- Assertion: selected cache leaf is writable ---
	probe := filepath.Join(scope1.Dir, "probe.txt")
	out, code = runBwrapped(t, g, "/bin/sh", "-c", "echo writable > "+probe+" && cat "+probe)
	if code != 0 {
		t.Errorf("selected cache leaf should be writable, exit=%d out=%s", code, out)
	}
	if !strings.Contains(out, "writable") {
		t.Errorf("selected cache leaf write/read failed: %s", out)
	}

	// --- Assertion: sibling scope leaf is unreadable ---
	out, code = runBwrapped(t, g, "/bin/sh", "-c", "cat "+siblingMarker+" 2>&1")
	if code == 0 {
		t.Errorf("sibling scope leaf should be unreadable, got: %s", out)
	}
	if strings.Contains(out, "sibling-secret") {
		t.Errorf("SECURITY: sibling scope marker leaked: %s", out)
	}

	// --- Assertion: sibling scope leaf is unwritable ---
	siblingWrite := filepath.Join(scope2.Dir, "evil")
	_, code = runBwrapped(t, g, "/bin/sh", "-c", "echo pwn > "+siblingWrite)
	if code == 0 {
		t.Error("SECURITY: sibling scope leaf should be unwritable")
	}

	// --- Assertion: parent ~/.cache/omac is not granted (host entries invisible) ---
	// The tmpfs parent shows only the bound entry, never host siblings.
	out, code = runBwrapped(t, g, "/bin/sh", "-c", "ls -A "+cacheRoot)
	if code != 0 {
		t.Errorf("listing cache root failed (exit %d): %s", code, out)
	}
	if strings.Contains(out, scope2.Digest) {
		t.Errorf("SECURITY: sibling scope visible in cache root listing: %s", out)
	}
	if strings.Contains(out, "host-global-marker") {
		t.Errorf("SECURITY: host global marker visible in cache root listing: %s", out)
	}
	if strings.Contains(out, ".locks") {
		t.Errorf("SECURITY: .locks visible in cache root listing: %s", out)
	}

	// --- Assertion: .locks directory is not granted ---
	_, code = runBwrapped(t, g, "/bin/sh", "-c", "cat "+lockMarker)
	if code == 0 {
		t.Error("SECURITY: .locks lock file should be unreadable")
	}
	_, code = runBwrapped(t, g, "/bin/sh", "-c", "echo evil > "+filepath.Join(locksDir, "evil"))
	if code == 0 {
		t.Error("SECURITY: .locks directory should be unwritable")
	}

	// --- Assertion: host global marker unchanged ---
	data, err := os.ReadFile(hostGlobalMarker)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "host-global-secret" {
		t.Errorf("host global marker was modified: %q", string(data))
	}
}

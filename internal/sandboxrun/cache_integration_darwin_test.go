//go:build darwin

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
// the real user home (not under /tmp or $TMPDIR, which the macOS baseline
// grants WRITE — that blanket temp-write would mask the denied
// assertions). HOME is redirected to that directory so toolcache computes
// its cache root inside it.
//
// NOTE: on a macOS host without a working Seatbelt profile (e.g. the omac
// sandbox cannot apply in this environment), this test may fail or skip
// locally — it must pass in CI.
func TestIntegrationCacheScopeIsolation(t *testing.T) {
	// Skip when the macOS Seatbelt sandbox is unavailable in this
	// environment (e.g. nested sandboxes, restricted test runners).
	// CI runners have a working sandbox-exec; this skip only fires
	// locally so a missing backend does not produce a false failure.
	if err := CheckPlatform(); err != nil {
		t.Skipf("macOS Seatbelt sandbox unavailable: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	testRoot, err := os.MkdirTemp(home, ".omac-cache-integ-")
	if err != nil {
		t.Skipf("cannot create test dir under home (home not writable in this environment): %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(testRoot) })
	t.Setenv("HOME", testRoot)

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
	if err := os.WriteFile(siblingMarker, []byte("sibling-leaked"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockMarker := filepath.Join(locksDir, scope1.Digest+".lock")
	if err := os.WriteFile(lockMarker, []byte("lock-marker"), 0o600); err != nil {
		t.Fatal(err)
	}

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

	// host global cache marker: unreadable and unchanged.
	out, code := runSandboxed(t, g, "/bin/sh", "-c", "cat "+hostGlobalMarker+" 2>&1")
	if code == 0 {
		t.Errorf("host global cache marker should be unreadable, got: %s", out)
	}
	if strings.Contains(out, "host-global-secret") {
		t.Errorf("SECURITY: host global cache marker leaked: %s", out)
	}

	// selected cache leaf: writable.
	probe := filepath.Join(scope1.Dir, "probe.txt")
	out, code = runSandboxed(t, g, "/bin/sh", "-c", "echo writable > "+probe+" && cat "+probe)
	if code != 0 {
		t.Errorf("selected cache leaf should be writable, exit=%d out=%s", code, out)
	}
	if !strings.Contains(out, "writable") {
		t.Errorf("selected cache leaf write/read failed: %s", out)
	}

	// sibling scope leaf: unreadable.
	out, code = runSandboxed(t, g, "/bin/sh", "-c", "cat "+siblingMarker+" 2>&1")
	if code == 0 {
		t.Errorf("sibling scope leaf should be unreadable, got: %s", out)
	}
	if strings.Contains(out, "sibling-leaked") {
		t.Errorf("SECURITY: sibling scope marker leaked: %s", out)
	}

	// sibling scope leaf: unwritable.
	siblingWrite := filepath.Join(scope2.Dir, "evil")
	_, code = runSandboxed(t, g, "/bin/sh", "-c", "echo pwn > "+siblingWrite)
	if code == 0 {
		t.Error("SECURITY: sibling scope leaf should be unwritable")
	}

	// parent ~/.cache/omac and .locks: not granted. On macOS Seatbelt,
	// listing the parent may be denied entirely (stronger than Linux's
	// "visible but only the bound entry") — both are acceptable.
	out, code = runSandboxed(t, g, "/bin/sh", "-c", "ls -A "+cacheRoot+" 2>&1")
	if code == 0 {
		if strings.Contains(out, scope2.Digest) {
			t.Errorf("SECURITY: sibling scope visible in cache root listing: %s", out)
		}
		if strings.Contains(out, "host-global-marker") {
			t.Errorf("SECURITY: host global marker visible in cache root listing: %s", out)
		}
		if strings.Contains(out, ".locks") {
			t.Errorf("SECURITY: .locks visible in cache root listing: %s", out)
		}
	}

	_, code = runSandboxed(t, g, "/bin/sh", "-c", "cat "+lockMarker)
	if code == 0 {
		t.Error("SECURITY: .locks lock file should be unreadable")
	}
	_, code = runSandboxed(t, g, "/bin/sh", "-c", "echo evil > "+filepath.Join(locksDir, "evil"))
	if code == 0 {
		t.Error("SECURITY: .locks directory should be unwritable")
	}

	data, err := os.ReadFile(hostGlobalMarker)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "host-global-secret" {
		t.Errorf("host global marker was modified: %q", string(data))
	}
}

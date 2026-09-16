package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/toolcache"
)

// The tool cache shared between sessions.
//
// Hard-coded cache locations are denied by the sandbox, so omac redirects
// every tool's cache — GOCACHE, GOMODCACHE, CARGO_HOME, NPM_CONFIG_CACHE,
// PIP_CACHE_DIR, XDG_CACHE_HOME — into a directory it grants the confined
// agent read-write. Without it every session would re-download and rebuild
// from scratch, so the cache is a feature and sharing it is the point.
//
// What the sharing costs depends on its reach, and the default reach is every
// session on the machine. The default scope resolves to one directory,
// independent of workdir, and each session mounts it writable. A cache is
// executable input: compiler object files, downloaded crates and wheels, an
// npm tree that a postinstall script runs. So whatever one session writes
// there, the next session compiles into its build or runs outright.
//
// That makes the cache a channel between sandboxes that are otherwise
// separate, and the sandboxes are not equally trusted: profiles differ per
// project, and one session may hold grants another does not. The weakest
// session on the machine decides what every later session executes, and
// nothing about the transfer is visible — no prompt, no approval, no record.
//
// Narrowing the default is one answer, making the cache read-only to the
// sandbox with a host-side populate step is another. Both satisfy the test
// below, which only asks that two unrelated projects not write into one
// bucket by default.

// TestSecurityDefaultToolCacheNotSharedAcrossWorkdirs asserts that, with no
// configuration, two different workdirs do not get the same writable cache.
func TestSecurityDefaultToolCacheNotSharedAcrossWorkdirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	defaultScope, err := config.CacheConfig{}.Resolve()
	if err != nil {
		t.Fatalf("resolve default cache scope: %v", err)
	}

	projectA, projectB := t.TempDir(), t.TempDir()
	sandboxTmp := t.TempDir()

	cacheFor := func(workdir string) string {
		t.Helper()
		scope, err := prepareLaunchCache(false, false, defaultScope, workdir, "", sandboxTmp)
		if err != nil {
			t.Fatalf("prepare cache for %s: %v", workdir, err)
		}
		if scope == nil {
			t.Fatalf("no cache scope prepared for %s", workdir)
		}
		t.Cleanup(func() { _ = scope.Close() })
		return scope.Dir
	}

	dirA := cacheFor(projectA)
	dirB := cacheFor(projectB)

	// Control: the cache is real, private to the user, and is what the tool
	// environment variables point into. Without it, a resolution that
	// returned nothing usable would satisfy the assertion below while
	// breaking every cached build.
	fi, err := os.Stat(dirA)
	if err != nil || !fi.IsDir() {
		t.Fatalf("the prepared cache %s is not a directory (%v): the fixture is broken, not the security property", dirA, err)
	}
	env := toolcache.Environment(dirA, toolcache.ModePersistent)
	if got := env["GOCACHE"]; got == "" || !strings.HasPrefix(got, dirA+string(filepath.Separator)) {
		t.Fatalf("GOCACHE resolved to %q, which is not inside the prepared cache %s: the fixture is broken", got, dirA)
	}

	if dirA == dirB {
		t.Errorf("two unrelated projects share the writable tool cache %s by default: what one confined session writes there — compiler output, downloaded packages, an npm tree with install scripts — is executed by the next session, whatever grants that one holds", dirA)
	}
}

package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestResolveGrantsDenyScanSkipsToolchainGrant reproduces the launch
// refusal "~/Library/pnpm hit the 200000-entry limit". The default profile
// grants tool-install dirs like the pnpm home read-only so the agent can
// run the tools; they are not where secrets live, so the scan leaves them
// out instead of walking pnpm's store (one file per installed package file).
func TestResolveGrantsDenyScanSkipsToolchainGrant(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	populateLargeTree(t, mkdirAll(t, home, "Library", "pnpm", "store", "v10", "files"))

	p := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Read: []string{"~/Library/pnpm"}},
	}
	if _, err := ResolveGrants(p, t.TempDir(), nil); err != nil {
		t.Fatalf("toolchain grant must not be deny-scanned: %v", err)
	}
}

// TestResolveGrantsDenyScanPrunesToolchainUnderBroadGrant covers a user
// who grants all of ~: the walk prunes the toolchain dirs inside it, but
// still scans everything else, so ~/project/.env stays protected.
func TestResolveGrantsDenyScanPrunesToolchainUnderBroadGrant(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	pnpmHome := filepath.Join(home, ".local", "share", "pnpm")
	populateLargeTree(t, mkdirAll(t, pnpmHome, "store", "v10", "files"))
	toolEnv := filepath.Join(pnpmHome, ".env")
	writeFile(t, toolEnv)
	projectEnv := filepath.Join(mkdirAll(t, home, "project"), ".env")
	writeFile(t, projectEnv)

	p := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Read: []string{"~"}},
	}
	g, err := ResolveGrants(p, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("toolchain dir under a broad grant must be pruned: %v", err)
	}
	if !slices.Contains(g.ProtectedPaths, projectEnv) {
		t.Errorf(".env outside the toolchain dirs not protected: %s (%v)", projectEnv, g.ProtectedPaths)
	}
	if slices.Contains(g.ProtectedPaths, toolEnv) {
		t.Errorf("toolchain dir must be pruned, not descended: %s", toolEnv)
	}
}

func mkdirAll(t *testing.T, elem ...string) string {
	t.Helper()
	dir := filepath.Join(elem...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

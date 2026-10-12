package sandboxrun

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// TestResolveGrantsLogsSkippedToolchainDirs checks that launch says which
// granted dirs the .env scan left out, so a user can tell that a .env
// inside e.g. ~/.nvm is not hidden. A toolchain dir outside every grant
// was never going to be scanned and is not mentioned.
func TestResolveGrantsLogsSkippedToolchainDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	nvm := mkdirAll(t, home, ".nvm")
	mkdirAll(t, home, ".rustup")

	p := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Read: []string{"~/.nvm"}},
	}
	var notices bytes.Buffer
	if _, err := ResolveGrants(p, t.TempDir(), &notices); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notices.String(), "did not search") || !strings.Contains(notices.String(), nvm) {
		t.Errorf("notice must name the skipped toolchain dir %s, got:\n%s", nvm, notices.String())
	}
	if strings.Contains(notices.String(), ".rustup") {
		t.Errorf("ungranted toolchain dir must not be named, got:\n%s", notices.String())
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

package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestResolveGrantsDenyScanSkipsPnpmStore reproduces the launch refusal
// "~/Library/pnpm hit the 200000-entry limit": the default profile grants
// the pnpm home, and its content-addressable store (store/v10/files/...)
// holds one file per package file ever installed. The walk must prune the
// store instead of failing closed on it.
func TestResolveGrantsDenyScanSkipsPnpmStore(t *testing.T) {
	pnpmHome := t.TempDir()
	populateLargeTree(t, mkdirAll(t, pnpmHome, "store", "v10", "files"))
	homeEnv := filepath.Join(pnpmHome, ".env")
	writeFile(t, homeEnv)

	p := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Read: []string{pnpmHome}},
	}
	g, err := ResolveGrants(p, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("pnpm store must be pruned, not scanned to the cap: %v", err)
	}
	if !slices.Contains(g.ProtectedPaths, homeEnv) {
		t.Errorf(".env outside the store not protected: %s (%v)", homeEnv, g.ProtectedPaths)
	}
}

// TestResolveGrantsDenyScanKeepsPlainStoreDir guards the pnpm-store prune
// against matching too broadly: a project folder that happens to be called
// "store" (e.g. src/store in a frontend app) is not a pnpm store, so a
// .env inside it stays protected.
func TestResolveGrantsDenyScanKeepsPlainStoreDir(t *testing.T) {
	wd := t.TempDir()
	env := filepath.Join(mkdirAll(t, wd, "src", "store"), ".env")
	writeFile(t, env)

	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(g.ProtectedPaths, env) {
		t.Errorf(".env in a plain store dir not protected: %s (%v)", env, g.ProtectedPaths)
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

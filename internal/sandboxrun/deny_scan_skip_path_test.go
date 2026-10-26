package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// setDenyScanSkipPaths replaces denyScanSkipPaths for one test, since a
// test cannot create /nix/store.
func setDenyScanSkipPaths(t *testing.T, paths ...string) {
	t.Helper()
	old := denyScanSkipPaths
	denyScanSkipPaths = paths
	t.Cleanup(func() { denyScanSkipPaths = old })
}

// writeEnv creates dir/.env and returns its path.
func writeEnv(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, ".env")
	if err := os.WriteFile(f, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// A granted Nix store larger than the scan limit must not stop launch
// (#394). The store is left out of the scan, so it never hits the limit.
func TestResolveGrantsSkipsLargeNixStoreGrant(t *testing.T) {
	setDenyScanLimit(t, 20000)
	setDenyByName(t, false) // Linux, where hitting the limit refuses launch
	parent := canonicalPath(t.TempDir())
	store := filepath.Join(parent, "nix", "store")
	populateLargeTreeAt(t, store)
	setDenyScanSkipPaths(t, store)
	workdir := t.TempDir()
	workEnv := writeEnv(t, workdir)

	prof := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Read: []string{store}},
		Network:    sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(prof, workdir, nil)
	if err != nil {
		t.Fatalf("granting the Nix store refused launch: %v", err)
	}
	if !slices.Contains(g.ProtectedPaths, workEnv) {
		t.Errorf("workdir .env %s not hidden; the rest of the scan must still run", workEnv)
	}
}

// A broad grant that contains the store (here its parent, like a grant
// of / or /nix) prunes the store but still scans everything beside it.
// A grant that only reaches the store through a symlink, like
// ~/.nix-profile, is skipped as well.
func TestResolveGrantsPrunesNixStoreInsideBroadGrant(t *testing.T) {
	setDenyScanLimit(t, 20000)
	setDenyByName(t, false)
	parent := canonicalPath(t.TempDir())
	store := filepath.Join(parent, "store")
	populateLargeTreeAt(t, store)
	storeEnv := writeEnv(t, filepath.Join(store, "zzz-pkg"))
	besideEnv := writeEnv(t, filepath.Join(parent, "var"))
	setDenyScanSkipPaths(t, store)
	link := filepath.Join(t.TempDir(), "profile")
	if err := os.Symlink(filepath.Join(store, "zzz-pkg"), link); err != nil {
		t.Fatal(err)
	}

	prof := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessNone},
		Filesystem: sandboxprofile.Filesystem{Read: []string{parent, link}},
		Network:    sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(prof, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("a grant containing the Nix store refused launch: %v", err)
	}
	if !slices.Contains(g.ProtectedPaths, besideEnv) {
		t.Errorf(".env beside the store (%s) not hidden", besideEnv)
	}
	for _, p := range g.ProtectedPaths {
		if p == storeEnv || p == filepath.Join(link, ".env") {
			t.Errorf("%s inside the store was scanned; the store must be skipped", p)
		}
	}
}

func populateLargeTreeAt(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	populateLargeTree(t, root)
}

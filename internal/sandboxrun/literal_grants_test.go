package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestDesktopLiteralGrantsDoNotExpandAndAreScanned(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "project$HOME[1]")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, ".env")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := sandboxprofile.ParseFlags([]string{"--allow-literal", dir, "--", "true"})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := sandboxprofile.Merge(&sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessNone},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}, f)
	g, err := ResolveGrants(p, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(g.AllowPaths, dir) {
		t.Fatalf("literal grant missing: %q", g.AllowPaths)
	}
	if !slices.Contains(g.ProtectedPaths, secret) {
		t.Fatalf("literal grant skipped deny scan: %q", g.ProtectedPaths)
	}
}

func TestDesktopLiteralGrantsRejectSymlinkedRoot(t *testing.T) {
	alias := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink("/", alias); err != nil {
		t.Fatal(err)
	}
	if _, err := existingLiteralGrants([]string{alias}); err == nil {
		t.Fatal("literal grant must not bypass the root-grant guard through a symlink")
	}
}

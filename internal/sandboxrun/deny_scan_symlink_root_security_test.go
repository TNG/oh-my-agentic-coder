package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// setupSymlinkedGrantRoot builds a real project tree containing a protected
// basename file, plus a symlink spelling of that tree that a profile can
// grant. It returns the real dir, the symlink spelling, the protected file
// in its real spelling, and the protected file in the granted spelling.
//
// The two regression tests for the symlinked-grant-root deny-scan case share
// this fixture so they assert the same launch-time property from two angles
// (descend at all; emit both spellings) without duplicating tree setup.
func setupSymlinkedGrantRoot(t *testing.T) (realDir, link, envReal, envGranted string) {
	t.Helper()
	realDir = t.TempDir()
	envReal = filepath.Join(realDir, ".env")
	if err := os.WriteFile(envReal, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	workdir := t.TempDir()
	link = filepath.Join(workdir, "linked")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	envGranted = filepath.Join(link, ".env")
	return realDir, link, envReal, envGranted
}

// TestSecuritySymlinkedGrantRootDenyScanDescends asserts that a deny-glob
// scan descends a granted root that is itself a symlink to a directory tree.
// The kernel backends resolve the granted spelling and mount the real tree,
// so the protection scan must walk the same tree the sandbox actually sees;
// otherwise every protected-basename file under the granted symlink is left
// unmasked. The control grants the real spelling of the same tree and must
// keep masking the file, isolating the root-symlink descent as the property
// under test.
func TestSecuritySymlinkedGrantRootDenyScanDescends(t *testing.T) {
	realDir, link, envReal, _ := setupSymlinkedGrantRoot(t)
	workdir := t.TempDir()

	// Control: granting the real spelling masks the protected file, so
	// the fixture's deny glob and tree layout actually produce a match.
	controlProf := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{Read: []string{realDir}, Deny: []string{".env"}},
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessNone},
		Network:    sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	if err := controlProf.Validate(); err != nil {
		t.Fatalf("control validate: %v", err)
	}
	control, err := ResolveGrants(controlProf, workdir, nil)
	if err != nil {
		t.Fatalf("control resolve: %v", err)
	}
	if !slices.Contains(control.ProtectedPaths, envReal) {
		t.Fatalf("control: granting the real spelling did not mask %s; fixture is broken, not the security property: %v",
			envReal, control.ProtectedPaths)
	}

	// Fixed state: granting the symlink spelling must resolve the root and
	// descend the real tree, so the protected file is masked in the real
	// spelling the kernel backends mount.
	prof := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{Read: []string{link}, Deny: []string{".env"}},
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessNone},
		Network:    sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	if err := prof.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	g, err := ResolveGrants(prof, workdir, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !slices.Contains(g.ProtectedPaths, envReal) {
		t.Errorf("granting a symlink spelling of a dir did not descend to mask %s (real spelling): %v",
			envReal, g.ProtectedPaths)
	}
}

// TestSecurityDenyScanEmitsBothSpellingsForSymlinkRoot asserts that a match
// found under a symlinked grant root is emitted in both the real spelling
// (what the facade-side ProtectedPathSet checks) and the granted spelling
// (what the bwrap coveredByAny check matches), so every backend masks the
// path the sandbox actually sees. It also guards the boundary of that fix:
// a symlink INSIDE the granted tree is still not followed, so an out-of-tree
// file reachable only through such a link is not enumerated (the entry cap
// and loop-bypass protection are preserved).
func TestSecurityDenyScanEmitsBothSpellingsForSymlinkRoot(t *testing.T) {
	realDir, link, envReal, envGranted := setupSymlinkedGrantRoot(t)
	workdir := t.TempDir()

	// An interior symlink to an OUT-of-tree dir holding its own protected
	// basename file. The scan must not follow it: enumerating out-of-tree
	// entries through an in-tree link would let a planted link widen the
	// scan past the granted root and could blow the entry cap.
	outTree := t.TempDir()
	outEnv := filepath.Join(outTree, ".env")
	if err := os.WriteFile(outEnv, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	interiorLink := filepath.Join(realDir, "out-link")
	if err := os.Symlink(outTree, interiorLink); err != nil {
		t.Fatalf("interior symlink: %v", err)
	}

	prof := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{Read: []string{link}, Deny: []string{".env"}},
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessNone},
		Network:    sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	if err := prof.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	g, err := ResolveGrants(prof, workdir, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if !slices.Contains(g.ProtectedPaths, envReal) {
		t.Errorf("real spelling %s not masked under a symlinked grant root: %v", envReal, g.ProtectedPaths)
	}
	if !slices.Contains(g.ProtectedPaths, envGranted) {
		t.Errorf("granted spelling %s not masked under a symlinked grant root: %v", envGranted, g.ProtectedPaths)
	}
	if slices.Contains(g.ProtectedPaths, outEnv) {
		t.Errorf("out-of-tree file %s reachable only through an in-tree symlink was enumerated; "+
			"interior symlinks must not be followed: %v", outEnv, g.ProtectedPaths)
	}
}

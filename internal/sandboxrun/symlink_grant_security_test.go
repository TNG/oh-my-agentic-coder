package sandboxrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestSecuritySymlinkGrantCanonicalized asserts that a profile granting a
// symlinked directory root does not leave protected paths under the
// resolved target unmasked. When the grant root is a symlink, the kernel
// backend resolves and mounts the real tree, so the protection machinery
// must account for the resolved spelling — either by canonicalizing the
// grant root before it reaches the masking loop, or by making the
// coverage check resolution-aware. The control grants the real spelling
// of the same tree and must mask the protected file, isolating the
// symlink spelling as the property under test.
func TestSecuritySymlinkGrantCanonicalized(t *testing.T) {
	realDir := t.TempDir()
	secretFile := filepath.Join(realDir, "secret")
	if err := os.WriteFile(secretFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	workdir := t.TempDir()
	link := filepath.Join(workdir, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Control: granting the real spelling masks the protected file, so
	// the fixture's deny entry and tree layout actually produce a mask.
	controlProf := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{
			Allow: []string{realDir},
			Deny:  []string{secretFile},
		},
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessNone},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	if err := controlProf.Validate(); err != nil {
		t.Fatalf("control validate: %v", err)
	}
	control, err := ResolveGrants(controlProf, workdir, nil)
	if err != nil {
		t.Fatalf("control resolve: %v", err)
	}
	controlArgv, err := BuildBwrapArgv(control, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(controlArgv, " "), secretFile) {
		t.Fatalf("control: granting the real spelling did not mask %s; the fixture is broken, not the security property: %s",
			secretFile, strings.Join(controlArgv, " "))
	}

	// Granting the symlink spelling must not bypass masking: the backend
	// mounts the resolved tree, so the protected file must appear in the
	// argv. Either resolution rejects the symlinked grant, or the mask is
	// present.
	prof := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{
			Allow: []string{link},
			Deny:  []string{secretFile},
		},
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessNone},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	if err := prof.Validate(); err != nil {
		return // rejection is an acceptable outcome
	}
	g, err := ResolveGrants(prof, workdir, nil)
	if err != nil {
		return // rejection is an acceptable outcome
	}
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, secretFile) {
		t.Errorf("granting a symlink spelling of a directory did not mask the protected file %s under the resolved target: %s",
			secretFile, joined)
	}
}

// TestSecuritySymlinkGrantMaskPresent asserts that a protected path
// located under the resolved target of a symlinked mount receives a mask
// argument in the bwrap argv. The coverage check must account for the
// resolved spelling of the mount, not just its literal path. The control
// grants the real spelling of the same tree and must mask the protected
// file.
func TestSecuritySymlinkGrantMaskPresent(t *testing.T) {
	realDir := t.TempDir()
	secretFile := filepath.Join(realDir, "secret")
	if err := os.WriteFile(secretFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	workdir := t.TempDir()
	link := filepath.Join(workdir, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Control: the real spelling of the grant root is covered, so the
	// mask is emitted.
	control := &Grants{
		Workdir:        workdir,
		AllowPaths:     []string{realDir},
		ProtectedPaths: []string{secretFile},
	}
	controlArgv, err := BuildBwrapArgv(control, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(controlArgv, " "), secretFile) {
		t.Fatalf("control: the real spelling did not produce a mask for %s; the fixture is broken, not the security property: %s",
			secretFile, strings.Join(controlArgv, " "))
	}

	// The symlink spelling of the grant root covers the protected path
	// after resolution, so the mask must be emitted — at the resolved
	// spelling (pathForms) and at the mount-relative alias spelling
	// (maskDestinations defense-in-depth).
	g := &Grants{
		Workdir:        workdir,
		AllowPaths:     []string{link},
		ProtectedPaths: []string{secretFile},
	}
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, secretFile) {
		t.Errorf("protected path %s under the resolved target of mount %s (symlink to %s) received no mask: %s",
			secretFile, link, realDir, joined)
	}
	aliasSpelling := filepath.Join(link, "secret")
	if !strings.Contains(joined, aliasSpelling) {
		t.Errorf("mount-relative alias spelling %s for protected path under symlinked mount %s received no mask: %s",
			aliasSpelling, link, joined)
	}
}

//go:build vuln

package sandboxrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecurityPrivateTmpfsSurvivesTmpSubpathGrants asserts that a grant set
// whose only /tmp entries are omac's own scoped subpaths still gets a
// private tmpfs over /tmp — not a bind-through to the host's shared one.
//
// BuildBwrapArgv treats ANY grant with the "/tmp" or "/tmp/" prefix as
// evidence that the caller wants /tmp itself bound, and skips the tmpfs
// fallback. But omac itself always grants a handful of narrow /tmp
// subpaths (the per-launch scratch dir, marker dirs, the runtime dir), none
// of which are "give the sandbox the shared host /tmp" — so removing the
// baseline's bare "/tmp" entry (as TestSecurityBaselineDoesNotGrantHostTmp
// requires) does not actually produce a private tmpfs in practice.
func TestSecurityPrivateTmpfsSurvivesTmpSubpathGrants(t *testing.T) {
	g := &Grants{
		Workdir:    "/work",
		AllowPaths: []string{"/work", "/tmp/omac-sandbox-tmp-x"},
	}
	// The grant doesn't need to exist on disk for the tmpGranted check
	// (only -try mounts existence-filter), so this reflects the real
	// launch-time shape without touching the actual host /tmp.
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")

	// Control: with no /tmp-prefixed grant at all, a private tmpfs is
	// emitted, so the mechanism itself works.
	plain := &Grants{Workdir: "/work", AllowPaths: []string{"/work"}}
	plainArgv, err := BuildBwrapArgv(plain, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plainArgv, " "), "--tmpfs /tmp") {
		t.Fatalf("control: no /tmp grant at all did not produce --tmpfs /tmp: the fixture is broken, not the security property")
	}

	if !strings.Contains(joined, "--tmpfs /tmp") {
		t.Errorf("a grant set containing only the scoped subpath /tmp/omac-sandbox-tmp-x (never bare /tmp) produced no --tmpfs /tmp: "+
			"the shared host /tmp stays reachable through any bind covering it, defeating the point of removing /tmp from the baseline. argv: %s", joined)
	}
}

// TestSecurityProtectedMasksEmittedUnderRootGrant asserts that a grant of
// "/" does not silently drop every protected-path mask.
//
// When AllowPaths contains "/", BuildBwrapArgv binds the whole tree first
// and every subsequent per-path mount entry is skipped (rootGranted short-
// circuits the mount map). The masking loop then runs coveredByAny against
// that now-empty mount list, finds no path "covering" any protected entry,
// and masks nothing at all — so a profile combining a blanket filesystem
// grant with declared protected paths gets none of the protection it
// declared.
func TestSecurityProtectedMasksEmittedUnderRootGrant(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Control: the same protected path is masked when the grant is scoped
	// rather than "/", so the masking mechanism itself works and this test
	// is specifically about the root-grant interaction.
	scoped := &Grants{
		Workdir:        home,
		AllowPaths:     []string{home},
		ProtectedPaths: []string{sshDir},
	}
	scopedArgv, err := BuildBwrapArgv(scoped, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(scopedArgv, " "), sshDir) {
		t.Fatalf("control: a protected path was not masked under a scoped grant: the fixture is broken, not the security property")
	}

	root := &Grants{
		Workdir:        home,
		AllowPaths:     []string{"/"},
		ProtectedPaths: []string{sshDir},
	}
	argv, err := BuildBwrapArgv(root, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, sshDir) {
		t.Errorf("a profile granting \"/\" alongside a declared protected path %q produced no mask for it at all: the protection is silently dropped, not just weakened. argv: %s", sshDir, joined)
	}
}

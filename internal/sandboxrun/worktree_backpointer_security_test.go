//go:build vuln

package sandboxrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestSecurityWorktreeRequiresBackPointer asserts that resolving worktree
// grants for a workdir requires the admin dir it names to point back at
// that exact .git file — not just that the admin dir has git's structural
// shape.
//
// resolveWorktreeCommonDir follows <workdir>/.git to an admin dir and
// reads <admin>/commondir, and separately checks the
// <common>/worktrees/<name> layout invariant — but never reads
// <admin>/gitdir, the file real git writes to point back at the one
// worktree that owns that admin dir. Any workdir whose .git names an
// admin dir with legitimate git structure gets that repo's grants, with
// no proof the workdir is the worktree the admin dir belongs to.
func TestSecurityWorktreeRequiresBackPointer(t *testing.T) {
	// A legitimately created linked worktree, as git itself would produce.
	victimWD, victimCommon := makeLinkedWorktree(t)
	victimAdmin := readAdminDir(t, victimWD)

	p := &sandboxprofile.Profile{Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite}}

	// Control: the legitimate worktree does get grants under its own
	// common dir, so the mechanism itself works and a fix that disables
	// worktree grants entirely wouldn't make this test pass by accident.
	victimGrants, err := ResolveGrants(p, victimWD, nil)
	if err != nil {
		t.Fatalf("control: ResolveGrants for the legitimate worktree failed: %v", err)
	}
	if !anyHasPrefix(append(victimGrants.ReadPaths, victimGrants.WritePaths...), victimCommon) {
		t.Fatalf("control: the legitimate worktree got no grant under its own common dir %s: the fixture is broken, not the security property", victimCommon)
	}

	// An unrelated workdir whose .git names the SAME admin dir. It has no
	// worktrees/<name> entry pointing back at it — it is simply reusing a
	// path it does not own.
	attackerWD := filepath.Join(t.TempDir(), "attacker-wd")
	if err := os.MkdirAll(attackerWD, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attackerWD, ".git"), []byte("gitdir: "+victimAdmin+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	attackerGrants, err := ResolveGrants(p, attackerWD, nil)
	if err != nil {
		t.Fatalf("ResolveGrants: %v", err)
	}
	if anyHasPrefix(append(attackerGrants.ReadPaths, attackerGrants.WritePaths...), victimCommon) {
		t.Errorf("an unrelated workdir naming another worktree's admin dir in its .git file received grants under that worktree's common dir %s: "+
			"nothing checks that %s points back at this workdir", victimCommon, filepath.Join(victimAdmin, "gitdir"))
	}
}

func anyHasPrefix(paths []string, prefix string) bool {
	for _, p := range paths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

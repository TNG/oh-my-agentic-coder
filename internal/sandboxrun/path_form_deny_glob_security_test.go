package sandboxrun

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestSecurityPathFormDenyGlobRejected asserts that a filesystem.deny
// entry that combines a path separator with glob metacharacters is not
// silently treated as a literal path. Such an entry must either be
// rejected at validation time with a clear error, or be resolved as a
// glob against the granted roots so the intended files are actually
// protected. A configuration-shaped entry that protects nothing and
// raises no warning is a silent misconfiguration the sandbox must not
// permit.
func TestSecurityPathFormDenyGlobRejected(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "configs", "db.key")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	// An entry that is neither a bare basename glob nor a plain path:
	// it carries both a separator and a metacharacter.
	entry := "configs/*.key"

	prof := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{Deny: []string{entry}},
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network:    sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}

	// One acceptable Outcome: validation rejects the misconfigured entry
	// outright, surfacing the misconfiguration at profile load time.
	if err := prof.Validate(); err != nil {
		return
	}

	// The other acceptable Outcome: the entry is resolved as a glob
	// against the granted roots and the intended file is actually masked.
	// ResolveGrants is the real launch-time entry point, so the protected
	// set it produces is what the kernel backend enforces.
	g, err := ResolveGrants(prof, root, nil)
	if err != nil {
		t.Fatalf("Validate accepted %q but ResolveGrants failed: %v", entry, err)
	}
	for _, p := range g.ProtectedPaths {
		if p == target {
			return
		}
	}
	t.Errorf("deny entry %q is neither rejected at validation nor resolved to protect %s: "+
		"it is a silent no-op", entry, target)
}

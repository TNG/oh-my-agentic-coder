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

	// An entry that is neither a bare basename glob nor a plain path.
	entry := "configs/*.key"

	// One acceptable outcome: validation rejects the entry outright.
	prof := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{Deny: []string{entry}},
	}
	validationErr := prof.Validate()

	// The other acceptable outcome: the entry is resolved as a glob and
	// the intended file appears in the protected set.
	resolved := resolveDenyPaths([]string{entry}, nil, nil, []string{root}, nil)
	covered := false
	for _, p := range resolved {
		if p == target {
			covered = true
		}
	}

	if validationErr == nil && !covered {
		t.Errorf("deny entry %q is neither rejected at validation nor resolved to protect %s: "+
			"it is a silent no-op", entry, target)
	}
}

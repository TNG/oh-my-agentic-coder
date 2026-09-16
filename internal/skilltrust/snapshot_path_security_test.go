//go:build vuln

package skilltrust

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Snapshot location.
//
// A skill's name is not trustworthy input. It comes from the skill's own
// omac.yaml and from the sidecar registry, both of which sit in the
// agent-writable workdir, so whatever the agent writes there is what reaches
// the approval store as a name. The store then builds the snapshot path by
// joining that name into <store>/skills/<name>/<hash>.
//
// That join is a filesystem write driven by an untrusted string, and it runs
// on the host side of the sandbox boundary with the user's full privileges —
// the one place omac deliberately steps outside confinement. A name
// containing ".." therefore does not merely land in a surprising directory:
// it lets confined code choose a host path outside the store and have omac
// create it and fill it with attacker-supplied files.
//
// It also breaks the spawn backstop. IsSnapshotPath allows a sidecar to run
// only from under <store>/skills, on the reasoning that the sandbox cannot
// write there. A snapshot that escapes that root is no longer covered by
// that reasoning.

// isolateUnder points the approval store at <root>/xdg/omac and returns root,
// so a path-traversal escape lands inside the test's own temp tree instead of
// wherever the developer's store happens to sit.
func isolateUnder(t *testing.T) (root string) {
	t.Helper()
	root = t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	return root
}

// under reports whether path is inside root, comparing cleaned absolute
// paths. Deliberately not the package's own containedIn: a test that borrows
// the helper it is checking cannot detect that helper being wrong.
func under(root, path string) bool {
	root = filepath.Clean(root) + string(filepath.Separator)
	return strings.HasPrefix(filepath.Clean(path)+string(filepath.Separator), root)
}

// TestSecurityApproveWritesOnlyInsideTheStore asserts the same property where
// it actually bites: Approve does not just compute the path, it creates the
// directory and copies the skill's files into it.
//
// Separate from the path test above because the fix could plausibly land in
// either place — reject the name, or confine the write — and this test holds
// whichever is chosen. It passes if Approve refuses the name outright.
func TestSecurityApproveWritesOnlyInsideTheStore(t *testing.T) {
	root := isolateUnder(t)
	skills := filepath.Join(dir(), "skills")

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "omac.yaml"), []byte("name: s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sidecar.py"), []byte("# payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := bundleHash(t, src)

	// Control: a well-named skill is approved and frozen inside the store.
	if err := Approve("plain-skill", h, src); err != nil {
		t.Fatalf("Approve of an ordinary skill failed: %v", err)
	}
	if p, ok := SnapshotPath("plain-skill", h); !ok || !under(skills, p) {
		t.Fatalf("ordinary skill was not frozen inside the store (path %s, exists %v)", p, ok)
	}

	const hostile = "../../../evil"
	// An error here is a pass: nothing was written.
	if err := Approve(hostile, h, src); err == nil {
		p, exists := SnapshotPath(hostile, h)
		if exists && !under(skills, p) {
			t.Errorf("Approve(%q) froze the skill at %s, outside the store at %s: confined code chose a host path and omac filled it", hostile, p, skills)
		}
	}

	// Independent of where SnapshotPath points: nothing belonging to the
	// store may appear next to it. Catches a fix that sanitizes the returned
	// path but leaves the write itself unconfined.
	if _, err := os.Stat(filepath.Join(root, "evil")); err == nil {
		t.Errorf("a directory named by the skill appeared at %s, outside the approval store", filepath.Join(root, "evil"))
	}
}

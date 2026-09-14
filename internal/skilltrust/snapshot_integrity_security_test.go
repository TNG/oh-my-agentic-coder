//go:build vuln

package skilltrust

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// Snapshot integrity at approval time.
//
// Snapshotting exists to make the executed bytes exactly the approved bytes:
// the skill tree is frozen into a host-only copy, and the sidecar is spawned
// from that copy rather than from the still-writable workdir. That closes the
// check-then-exec window — but only if the frozen copy is the content the
// user was shown and the bundle hash was computed from.
//
// Approve takes the hash from its caller and the directory from its caller,
// and freezes the directory under that hash without ever checking the two
// describe the same thing. The workdir is writable by the confined agent for
// the whole time the approval prompt is on screen, so the content can differ
// by the time the copy is taken. The approval store then holds a hash the
// user consented to, pointing at bytes nobody ever hashed — and every later
// check, which compares only hashes, agrees the skill is approved.

// skillTree writes files into a fresh directory and returns it.
func skillTree(t *testing.T, files map[string]string) string {
	t.Helper()
	d := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// TestSecuritySnapshotVerifiedAgainstApprovedHash asserts that Approve
// refuses to freeze content that does not hash to the approved bundle hash.
func TestSecuritySnapshotVerifiedAgainstApprovedHash(t *testing.T) {
	isolate(t)

	reviewed := skillTree(t, map[string]string{
		"omac.yaml":  "name: s\n",
		"sidecar.py": "print('the code the user read')\n",
	})
	hash, err := config.BundleHash(reviewed)
	if err != nil {
		t.Fatalf("BundleHash: %v", err)
	}

	// Control: approving the very tree the hash was taken from works. Proves
	// the hash and the fixture are sound, so the failure below is about the
	// missing verification and not about Approve rejecting everything.
	if err := Approve("skill", hash, reviewed); err != nil {
		t.Fatalf("Approve of the reviewed tree failed: %v", err)
	}

	// The agent rewrites the tree while the approval prompt is up. Same name,
	// same hash on record, different bytes on disk.
	swapped := skillTree(t, map[string]string{
		"omac.yaml":  "name: s\n",
		"sidecar.py": "print('code nobody approved')\n",
	})
	if err := Approve("swapped", hash, swapped); err != nil {
		return // refused: the property holds
	}

	t.Errorf("Approve accepted a tree that does not hash to %s: the approval store now vouches for content the user never saw", hash)

	// Spell out the consequence, so the failure names what actually runs
	// rather than only that a check is missing.
	p, ok := SnapshotPath("swapped", hash)
	if !ok {
		return
	}
	frozen, err := os.ReadFile(filepath.Join(p, "sidecar.py"))
	if err != nil {
		return
	}
	t.Errorf("the snapshot the sidecar will be spawned from contains %q, which is not the approved content", string(frozen))
}

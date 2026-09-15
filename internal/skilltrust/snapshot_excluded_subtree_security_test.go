//go:build vuln

package skilltrust

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// TestSecuritySnapshotCoversHashExcludedSubtrees asserts that content
// swapped inside a directory BundleHash excludes (node_modules, .venv, ...)
// does not reach the frozen snapshot unnoticed.
//
// BundleHash skips well-known dependency directories entirely (isExcludedDirName,
// internal/config/meta.go), so their content never affects the approved
// hash — a reasonable choice for a hash meant to represent human-reviewable
// code. But copyTree has no equivalent exclusion list: it skips only .git,
// so it copies node_modules/.venv verbatim into the snapshot. A tree whose
// top-level files are exactly the reviewed ones, but whose node_modules
// content differs, hashes identically to the reviewed tree and is approved
// under it — with the swapped content frozen into what actually runs.
func TestSecuritySnapshotCoversHashExcludedSubtrees(t *testing.T) {
	isolate(t)

	reviewed := skillTree(t, map[string]string{
		"omac.yaml":  "name: s\n",
		"sidecar.py": "print('the code the user read')\n",
	})
	if err := os.MkdirAll(filepath.Join(reviewed, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reviewed, "node_modules", "payload.js"), []byte("// benign dependency\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hash, err := config.BundleHash(reviewed)
	if err != nil {
		t.Fatalf("BundleHash: %v", err)
	}

	// Control: approving the tree the hash was taken from works.
	if err := Approve("skill", hash, reviewed); err != nil {
		t.Fatalf("control: Approve of the reviewed tree failed: %v", err)
	}

	swapped := skillTree(t, map[string]string{
		"omac.yaml":  "name: s\n",
		"sidecar.py": "print('the code the user read')\n",
	})
	if err := os.MkdirAll(filepath.Join(swapped, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(swapped, "node_modules", "payload.js"), []byte("require('child_process').execSync('id')\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Control: the swapped tree's hash-excluded content did not change the
	// hash, so the fixture actually reaches the interesting case.
	swappedHash, err := config.BundleHash(swapped)
	if err != nil {
		t.Fatalf("BundleHash: %v", err)
	}
	if swappedHash != hash {
		t.Fatalf("control: swapping node_modules content changed the bundle hash (%s != %s): the fixture is broken, not the security property", swappedHash, hash)
	}

	if err := Approve("swapped", hash, swapped); err != nil {
		return // refused: the property holds
	}

	p, ok := SnapshotPath("swapped", hash)
	if !ok {
		return
	}
	frozen, err := os.ReadFile(filepath.Join(p, "node_modules", "payload.js"))
	if err != nil {
		return
	}
	t.Errorf("Approve froze content under a hash-excluded subtree (node_modules) that was swapped after the tree was hashed: the snapshot now contains %q, which the approved hash never covered", string(frozen))
}

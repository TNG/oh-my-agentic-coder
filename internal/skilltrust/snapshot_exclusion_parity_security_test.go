package skilltrust

import (
	"os"
	"path/filepath"
	"testing"
)

// Snapshot exclusion parity.
//
// The frozen snapshot is what the sidecar runs; the bundle hash is what the
// user approved. The approval only means something if the two sets are
// identical: config.BundleHash skips well-known runtime-artifact directory
// and file names so the hash represents reviewable content, and copyTree
// must apply the very same skips when it freezes the tree. A file the hash
// ignores must never be frozen, and every frozen file must be one whose
// bytes the hash covers — otherwise the frozen set can change without the
// approved hash changing. The copy step must also never resolve a path
// that stopped being the regular file the walk classified.

// writeTree writes files (slash-separated relative paths) under root.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// frozenRelPaths returns the slash-relative path of every regular file
// under root: the set of bytes a spawned sidecar can actually read.
func frozenRelPaths(t *testing.T, root string) []string {
	t.Helper()
	var rels []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rels = append(rels, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return rels
}

// TestSecuritySnapshotFrozenSetMatchesHashedSet commits a snapshot of a
// tree that mixes reviewed content with hash-excluded names (top level and
// nested in a kept subdirectory) and content under a hash-excluded
// directory, then asserts the frozen set equals the hashed set.
func TestSecuritySnapshotFrozenSetMatchesHashedSet(t *testing.T) {
	isolate(t)

	src := t.TempDir()
	reviewed := map[string]string{
		"omac.yaml":      "name: parity\n",
		"sidecar.py":     "print('reviewed')\n",
		"scripts/run.sh": "echo run\n",
	}
	excludedNames := map[string]string{
		"foo.pyc":           "compiled artifact",
		".DS_Store":         "os artifact",
		"draft.txt~":        "editor artifact",
		"scripts/cache.pyo": "compiled artifact",
		"scripts/notes.swp": "editor artifact",
	}
	writeTree(t, src, reviewed)
	writeTree(t, src, excludedNames)
	writeTree(t, src, map[string]string{"node_modules/dep.js": "module.exports = 1\n"})
	if err := os.Chmod(filepath.Join(src, "scripts", "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	hash := bundleHash(t, src)
	if err := Approve("parity", hash, src); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	snap, ok := SnapshotPath("parity", hash)
	if !ok {
		t.Fatal("snapshot missing after approval")
	}
	// Control: the committed snapshot hashes to the approved value, so a
	// failure below is about set parity, not a broken fixture.
	if got := bundleHash(t, snap); got != hash {
		t.Fatalf("control: snapshot hash %s != approved %s", got, hash)
	}

	frozen := frozenRelPaths(t, snap)
	frozenSet := make(map[string]bool, len(frozen))
	for _, rel := range frozen {
		frozenSet[rel] = true
	}

	t.Run("no hash-excluded name is frozen", func(t *testing.T) {
		excludedBase := make(map[string]bool, len(excludedNames))
		for rel := range excludedNames {
			excludedBase[filepath.Base(rel)] = true
			if frozenSet[rel] {
				t.Errorf("snapshot froze %s, a name the bundle hash never covers", rel)
			}
		}
		// Anywhere under the root, not only at the planted paths.
		for _, rel := range frozen {
			if excludedBase[filepath.Base(rel)] {
				t.Errorf("snapshot froze %s under a name the bundle hash never covers", rel)
			}
		}
		// The excluded-subtree skip keeps working as before.
		if frozenSet["node_modules/dep.js"] {
			t.Error("snapshot froze content under a hash-excluded directory")
		}
	})

	t.Run("every frozen file is covered by the hash", func(t *testing.T) {
		// Parity, not spot checks: whatever regular file the freeze
		// kept must be one the hash covers, so changing its bytes has
		// to change the hash. Files the hash skips do not satisfy that.
		for _, rel := range frozen {
			p := filepath.Join(snap, filepath.FromSlash(rel))
			orig, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			if err := os.WriteFile(p, append(append([]byte(nil), orig...), '\n'), 0o644); err != nil {
				t.Fatalf("mutate %s: %v", rel, err)
			}
			after := bundleHash(t, snap)
			// Restore before reporting so one violation does not cascade.
			if err := os.WriteFile(p, orig, 0o644); err != nil {
				t.Fatalf("restore %s: %v", rel, err)
			}
			if after == hash {
				t.Errorf("mutating frozen file %s leaves the bundle hash unchanged: the snapshot runs bytes the approved hash does not cover", rel)
			}
		}
	})

	t.Run("reviewed content is frozen verbatim", func(t *testing.T) {
		for rel, body := range reviewed {
			if !frozenSet[rel] {
				t.Errorf("hashed file %s is missing from the snapshot", rel)
				continue
			}
			got, err := os.ReadFile(filepath.Join(snap, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			if string(got) != body {
				t.Errorf("frozen %s = %q; want the reviewed %q", rel, got, body)
			}
		}
		// A fix must not change how legitimate content freezes.
		fi, err := os.Stat(filepath.Join(snap, "scripts", "run.sh"))
		if err != nil {
			t.Fatalf("stat run.sh: %v", err)
		}
		if fi.Mode().Perm()&0o100 == 0 {
			t.Errorf("frozen run.sh lost its execute bit (%s)", fi.Mode().Perm())
		}
	})

	t.Run("a path swapped for a symlink is dropped, not read through", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "host.txt")
		if err := os.WriteFile(target, []byte("bytes on the other side of the link\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "entry")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unsupported here: %v", err)
		}
		dst := filepath.Join(t.TempDir(), "frozen-entry")
		// Whether copyFile reports an error is its choice; the frozen
		// set is what matters. The walk classified this path as a
		// regular file, and the copy step must not silently resolve
		// whatever sits there at open time: the file is dropped.
		_ = copyFile(link, dst, 0o644)
		if _, err := os.Lstat(dst); err == nil {
			t.Error("copyFile froze a path that had become a symlink instead of dropping it")
		}
	})
}

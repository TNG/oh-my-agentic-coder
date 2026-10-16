package skilltrust

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// regularFiles walks dir and returns the slash-relative paths of all regular
// files, sorted. Symlinks and special files are skipped: only regular files
// can be executed by the sidecar.
func regularFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// excludedFromHash reports whether removing rel from dir leaves the bundle
// hash unchanged. The exclusion set is determined behaviourally so the test
// does not encode the predicate list and stays correct if it grows.
func excludedFromHash(t *testing.T, dir, rel, fullHash string) bool {
	t.Helper()
	src := filepath.Join(dir, rel)
	stash := filepath.Join(t.TempDir(), rel)
	if err := os.MkdirAll(filepath.Dir(stash), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(src, stash); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(stash, src)
	h, err := config.BundleHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	return h == fullHash
}

func writeExtra(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSecuritySnapshotExcludesUnhashedFiles asserts that files invisible to
// BundleHash do not appear in the frozen snapshot. The snapshot is the exact
// byte set the sidecar executes, so any file the hash skips but the copy
// retains is content the user never approved yet runs with host privileges.
func TestSecuritySnapshotExcludesUnhashedFiles(t *testing.T) {
	isolate(t)

	dir := skillTree(t, map[string]string{
		"omac.yaml":  "name: s\n",
		"sidecar.py": "print('ok')\n",
	})
	writeExtra(t, dir, map[string]string{
		"helper.py":     "def f(): pass\n",
		"sub/mod.py":    "x = 1\n",
		"module.pyc":    "\x00\x00\x00\x00",
		"notes~":        "draft\n",
		".DS_Store":     "junk\n",
		"Thumbs.db":     "junk\n",
		"sub/debug.pyc": "\x00\x00",
	})

	hash, err := config.BundleHash(dir)
	if err != nil {
		t.Fatalf("BundleHash: %v", err)
	}

	if err := Approve("skill", hash, dir); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	snapPath, ok := SnapshotPath("skill", hash)
	if !ok {
		t.Fatal("snapshot not found after Approve")
	}

	for _, rel := range regularFiles(t, dir) {
		if !excludedFromHash(t, dir, rel, hash) {
			continue
		}
		if _, err := os.Stat(filepath.Join(snapPath, rel)); err == nil {
			t.Errorf("file %q is invisible to BundleHash but present in the snapshot", rel)
		}
	}
}

// TestSecuritySnapshotFileSetMatchesHash asserts that the set of regular files
// in the snapshot is exactly the set of files BundleHash covers. Any divergence
// means the executed bytes are not the approved bytes.
func TestSecuritySnapshotFileSetMatchesHash(t *testing.T) {
	isolate(t)

	dir := skillTree(t, map[string]string{
		"omac.yaml":  "name: s\n",
		"sidecar.py": "print('ok')\n",
		"helper.py":  "def f(): pass\n",
	})
	writeExtra(t, dir, map[string]string{
		"sub/mod.py":    "x = 1\n",
		"module.pyc":    "\x00\x00\x00\x00",
		"notes~":        "draft\n",
		".DS_Store":     "junk\n",
		"Thumbs.db":     "junk\n",
		"sub/debug.pyc": "\x00\x00",
	})

	hash, err := config.BundleHash(dir)
	if err != nil {
		t.Fatalf("BundleHash: %v", err)
	}

	if err := Approve("skill", hash, dir); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	snapPath, ok := SnapshotPath("skill", hash)
	if !ok {
		t.Fatal("snapshot not found after Approve")
	}

	snapFiles := regularFiles(t, snapPath)

	var hashVisible []string
	for _, rel := range regularFiles(t, dir) {
		if !excludedFromHash(t, dir, rel, hash) {
			hashVisible = append(hashVisible, rel)
		}
	}
	sort.Strings(hashVisible)

	if !slices.Equal(snapFiles, hashVisible) {
		t.Errorf("snapshot file set does not match BundleHash-visible file set\n"+
			"snapshot:     %v\nhash-visible: %v", snapFiles, hashVisible)
	}
}

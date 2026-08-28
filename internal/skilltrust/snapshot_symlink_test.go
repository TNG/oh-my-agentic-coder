package skilltrust

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSnapshot_SymlinkRootMatchesRealTree guards that snapshotting a skill
// root that is itself a symlink (common for git-versioned skill libraries
// installed as ~/.config/opencode/skills/foo -> ../../library/skills/foo/)
// resolves the root and freezes the real tree, exactly as snapshotting the
// real dir would. copyTree (internal/skilltrust/snapshot.go:135) already
// calls filepath.EvalSymlinks on the root; this test is a regression guard
// for that behavior so a future change can't silently drop it.
//
// Symlink roots are the skilltrust analogue of the config.BundleHash
// symlink-root case (TestBundleHash_SymlinkRoot). If snapshot regressed to
// walking the link itself, the snapshot would be empty (the walk fires
// once with rel == "." and is skipped) and the sidecar spawn would fail
// with connection refused — the exact failure mode BundleHash had.
func TestSnapshot_SymlinkRootMatchesRealTree(t *testing.T) {
	isolate(t)

	// Stage a real skill dir with omac.yaml, a sidecar file, and a nested
	// helper file — the same shape real skills take.
	realDir := t.TempDir()
	files := map[string]string{
		"omac.yaml":       "name: s\n",
		"sidecar.py":      "# server sidecar\n",
		"helpers/util.py": "def f(): return 1\n",
	}
	for rel, body := range files {
		full := filepath.Join(realDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}

	// Create a symlink to the real dir. Skip on platforms without symlink
	// permission, mirroring the pattern at skilltrust_test.go:215-217 and
	// config/bundle_test.go:149-151.
	symlink := filepath.Join(t.TempDir(), "linked-skill")
	if err := os.Symlink(realDir, symlink); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	// Use distinct hashes for the symlink vs real snapshot so they land in
	// different content-addressed dirs (snapshot is keyed by (name, hash)).
	// The FILE CONTENTS must match even though the keys differ.
	snapSym, err := snapshot("s", "sha256:via-symlink", symlink)
	if err != nil {
		t.Fatalf("snapshot(via symlink): %v", err)
	}
	snapReal, err := snapshot("s", "sha256:via-real", realDir)
	if err != nil {
		t.Fatalf("snapshot(via real): %v", err)
	}

	// Assert non-empty: each staged file must be present in the symlink
	// snapshot and identical in body to the original.
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(snapSym, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("snapshot(via symlink) missing %s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("snapshot(via symlink) %s = %q; want %q", rel, got, want)
		}
	}

	// Assert the snapshot contents equal those produced by snapshotting
	// the real tree — guarding the copyTree/BundleHash consistency on
	// symlink roots. Compare the file set body-for-body.
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(snapReal, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("snapshot(via real) missing %s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("snapshot(via real) %s = %q; want %q", rel, got, want)
		}
		// Cross-compare the two snapshots.
		gotSym, _ := os.ReadFile(filepath.Join(snapSym, filepath.FromSlash(rel)))
		if string(gotSym) != string(got) {
			t.Errorf("snapshot mismatch on %s: symlink=%q real=%q", rel, gotSym, got)
		}
	}

	// The symlink snapshot must NOT contain the link itself baked in as a
	// directory entry pointing nowhere (root resolution, not link copy).
	if _, err := os.Lstat(filepath.Join(snapSym, "linked-skill")); err == nil {
		t.Error("symlink snapshot should not contain a baked-in copy of the link entry")
	}
}

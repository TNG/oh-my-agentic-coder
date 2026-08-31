package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageSkill writes a small set of files under dir and returns dir
// itself for convenience. Files are described as a path-to-contents
// map; intermediate directories are created automatically. The map
// keys are relative paths using forward slashes regardless of host
// OS, matching the BundleHash convention.
func stageSkill(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	for rel, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return dir
}

func TestBundleHash_Stable(t *testing.T) {
	a := stageSkill(t, t.TempDir(), map[string]string{
		"omac.yaml":                "name: x\n",
		"sidecar.py":               "# server\n",
		"install/install.macos.sh": "#!/bin/sh\n",
	})
	h1, err := BundleHash(a)
	if err != nil {
		t.Fatalf("BundleHash: %v", err)
	}
	// Calling twice on the same input must produce identical output.
	h2, err := BundleHash(a)
	if err != nil {
		t.Fatalf("BundleHash: %v", err)
	}
	if h1 != h2 {
		t.Errorf("BundleHash not deterministic across calls: %s vs %s", h1, h2)
	}
	if !strings.HasPrefix(h1, "sha256:") {
		t.Errorf("BundleHash result %q must start with sha256:", h1)
	}
}

func TestBundleHash_DifferentContentsDiffer(t *testing.T) {
	a := stageSkill(t, t.TempDir(), map[string]string{"omac.yaml": "name: a\n"})
	b := stageSkill(t, t.TempDir(), map[string]string{"omac.yaml": "name: b\n"})
	h1, _ := BundleHash(a)
	h2, _ := BundleHash(b)
	if h1 == h2 {
		t.Errorf("different omac.yaml contents must yield different hashes; got %s", h1)
	}
}

func TestBundleHash_FilenameMatters(t *testing.T) {
	// Same bytes under a different name => different hash. This proves
	// the path is part of the digest input, not just the file body.
	a := stageSkill(t, t.TempDir(), map[string]string{"foo.py": "x"})
	b := stageSkill(t, t.TempDir(), map[string]string{"bar.py": "x"})
	h1, _ := BundleHash(a)
	h2, _ := BundleHash(b)
	if h1 == h2 {
		t.Errorf("path should be part of the digest; got identical hashes %s", h1)
	}
}

// TestBundleHash_ExcludesRuntimeArtifacts proves that the hash is
// stable across a `pip install` (artifacts appearing in .venv/), a
// `pytest` run (.pytest_cache/), and editor sessions (.DS_Store,
// foo.pyc). The test stages a baseline, hashes it, then drops a
// representative selection of excluded paths next to the real files
// and re-hashes. Both hashes must match.
func TestBundleHash_ExcludesRuntimeArtifacts(t *testing.T) {
	dir := stageSkill(t, t.TempDir(), map[string]string{
		"omac.yaml":  "name: x\n",
		"sidecar.py": "# server\n",
	})
	baseline, err := BundleHash(dir)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	// Drop in a bunch of excluded paths.
	stageSkill(t, dir, map[string]string{
		".DS_Store":                        "thumbs",
		"sidecar.cpython-311.pyc":          "bytecode",
		"__pycache__/sidecar.cpython.pyc":  "more bytecode",
		"node_modules/foo/index.js":        "module.exports = 1",
		".venv/lib/site-packages/x.py":     "import x",
		".pytest_cache/v/cache/lastfailed": "{}",
		".git/HEAD":                        "ref: refs/heads/main\n",
		"target/release/foo":               "binary",
		"dist/foo.tar.gz":                  "tarball",
		".idea/workspace.xml":              "<idea/>",
		"sidecar.py.swp":                   "vim swap",
	})
	withArtifacts, err := BundleHash(dir)
	if err != nil {
		t.Fatalf("withArtifacts: %v", err)
	}
	if baseline != withArtifacts {
		t.Errorf("hash drifted after staging runtime artifacts:\n baseline: %s\n after:    %s", baseline, withArtifacts)
	}
}

func TestBundleHash_IncludesNestedSourceFiles(t *testing.T) {
	dir := stageSkill(t, t.TempDir(), map[string]string{
		"omac.yaml":        "name: x\n",
		"sidecar.py":       "# server\n",
		"helpers/util.py":  "def f(): return 1\n",
		"install/macos.sh": "echo hi\n",
	})
	baseline, _ := BundleHash(dir)
	// Modify a nested file => hash must change.
	if err := os.WriteFile(filepath.Join(dir, "helpers", "util.py"), []byte("def f(): return 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _ := BundleHash(dir)
	if baseline == after {
		t.Errorf("changing helpers/util.py must change the hash; got %s for both", baseline)
	}
}

func TestBundleHash_MissingDir(t *testing.T) {
	_, err := BundleHash(filepath.Join(t.TempDir(), "definitely-not-here"))
	if err == nil {
		t.Fatal("expected error for missing dir")
	}
}

// TestBundleHash_SymlinkRoot guards that a skill root that is itself a
// symlink (common for git-versioned skill libraries installed as
// ~/.config/opencode/skills-> ../../library/skills/) hashes
// the same as its real target. Without resolving the root, WalkDir
// Lstats the root, sees ModeSymlink, fires the callback once with
// rel == "." (skipped), and never descends — yielding the empty
// sha256:e3b0c44… digest.
func TestBundleHash_SymlinkRoot(t *testing.T) {
	realDir := stageSkill(t, t.TempDir(), map[string]string{
		"omac.yaml":  "name: x\n",
		"sidecar.py": "# server\n",
	})
	symlink := filepath.Join(t.TempDir(), "linked-skill")
	if err := os.Symlink(realDir, symlink); err != nil {
		// On platforms without symlink permission, just skip.
		t.Skipf("symlink: %v", err)
	}

	realHash, err := BundleHash(realDir)
	if err != nil {
		t.Fatalf("BundleHash(realDir): %v", err)
	}
	symHash, err := BundleHash(symlink)
	if err != nil {
		t.Fatalf("BundleHash(symlink): %v", err)
	}

	const emptyHash = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if symHash == emptyHash {
		t.Errorf("BundleHash(symlink) is the empty digest %s; symlink root was not resolved/walked", symHash)
	}
	if symHash != realHash {
		t.Errorf("BundleHash must match for symlink root vs real target; got symlink=%s real=%s", symHash, realHash)
	}
}

func TestBundleHash_SkipsSymlinks(t *testing.T) {
	dir := stageSkill(t, t.TempDir(), map[string]string{
		"omac.yaml":  "name: x\n",
		"sidecar.py": "# server\n",
	})
	// Create a symlink that points outside the bundle. Hashing through
	// it would let an attacker swap the target's bytes without altering
	// the bundle's content. We drop symlinks entirely.
	target := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(target, []byte("evil"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "linked.py")); err != nil {
		// On platforms without symlink permission, just skip.
		t.Skipf("symlink: %v", err)
	}
	h, err := BundleHash(dir)
	if err != nil {
		t.Fatalf("BundleHash: %v", err)
	}
	// Compare against a baseline WITHOUT the symlink.
	baselineDir := stageSkill(t, t.TempDir(), map[string]string{
		"omac.yaml":  "name: x\n",
		"sidecar.py": "# server\n",
	})
	baseline, _ := BundleHash(baselineDir)
	if h != baseline {
		t.Errorf("symlink should not affect bundle hash; got %s, want %s", h, baseline)
	}
}

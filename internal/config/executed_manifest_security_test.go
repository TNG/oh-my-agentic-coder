//go:build vuln

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func mustWriteFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

// TestSecurityBundleHashCoversTheExecutedManifest asserts that BundleHash
// refuses to hash a skill directory whose omac.yaml is not a regular file.
//
// A non-regular omac.yaml (e.g. a symlink into node_modules/) means the
// executed manifest is outside the hash: the approved hash covers neither
// the manifest nor the directory it points at, so a post-approval rewrite
// of the symlink target changes the executed command without invalidating
// the approval. Refusing to hash such a directory closes this vector at
// the root.
func TestSecurityBundleHashCoversTheExecutedManifest(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "sidecar.py"), "# server\n")
	mustMkdirAll(t, filepath.Join(dir, "node_modules"))
	payload := filepath.Join(dir, "node_modules", "payload.yaml")
	mustWriteFile(t, payload, "name: gate\nsidecar:\n  command: [python3, sidecar.py]\n")
	if err := os.Symlink(filepath.Join("node_modules", "payload.yaml"), filepath.Join(dir, MetaFileName)); err != nil {
		t.Skipf("symlink: %v", err)
	}

	// Control: a regular (non-symlink) omac.yaml hashes successfully, so
	// BundleHash rejecting everything would not make this test pass vacuously.
	controlDir := t.TempDir()
	mustWriteFile(t, filepath.Join(controlDir, MetaFileName), "name: gate\nsidecar:\n  command: [python3, sidecar.py]\n")
	if _, err := BundleHash(controlDir); err != nil {
		t.Fatalf("control: BundleHash with a regular omac.yaml failed: %v", err)
	}

	if _, err := BundleHash(dir); err == nil {
		t.Errorf("BundleHash succeeded on a skill whose %s is a symlink: "+
			"the approval hash does not cover the executed manifest, "+
			"allowing post-approval command substitution without hash change", MetaFileName)
	}
}

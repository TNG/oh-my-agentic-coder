package profileaudit

import (
	"path/filepath"
	"testing"
)

func TestExtensionSecretPathsNoBaselineOverlap(t *testing.T) {
	base := BaselineSecretPaths()
	baseSet := make(map[string]bool, len(base))
	for _, p := range base {
		baseSet[p] = true
	}
	for _, p := range ExtensionSecretPaths {
		if baseSet[p] {
			t.Errorf("ExtensionSecretPaths entry %q already in BaselineSecretPaths; the extension list must only hold paths NOT in the baseline", p)
		}
	}
}

func TestExtensionSecretPathsNonEmpty(t *testing.T) {
	if len(ExtensionSecretPaths) == 0 {
		t.Fatal("ExtensionSecretPaths is empty; expected the curated list of CLI-tool credential paths")
	}
}

func TestSecretBasenameGlobsValid(t *testing.T) {
	for _, g := range SecretBasenameGlobs {
		if _, err := filepath.Match(g, "probe"); err != nil {
			t.Errorf("SecretBasenameGlobs entry %q is not a valid filepath.Match pattern: %v", g, err)
		}
	}
}

func TestSecretBasenameGlobsNonEmpty(t *testing.T) {
	if len(SecretBasenameGlobs) == 0 {
		t.Fatal("SecretBasenameGlobs is empty; expected at least .env, *.key, etc.")
	}
}

func TestBaselineSecretPathsNonEmpty(t *testing.T) {
	if len(BaselineSecretPaths()) == 0 {
		t.Fatal("BaselineSecretPaths returned empty; PlatformBaseline().ProtectedPaths should always have entries")
	}
}

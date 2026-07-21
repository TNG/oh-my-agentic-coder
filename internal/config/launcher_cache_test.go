package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCacheConfigResolveDefaultsToGlobal(t *testing.T) {
	scope, err := CacheConfig{}.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if scope != CacheScopeGlobal {
		t.Errorf("scope = %q, want %q", scope, CacheScopeGlobal)
	}
	if got := DefaultLauncherConfig().Cache.Scope; got != CacheScopeGlobal {
		t.Errorf("default cache scope = %q, want %q", got, CacheScopeGlobal)
	}
}

func TestValidateCacheScope(t *testing.T) {
	for _, s := range []string{"global", "config", "workdir"} {
		if _, err := ValidateCacheScope(s); err != nil {
			t.Errorf("ValidateCacheScope(%q) = %v, want nil", s, err)
		}
	}
	if _, err := ValidateCacheScope("bogus"); err == nil {
		t.Errorf("ValidateCacheScope(bogus) = nil, want error")
	}
}

func TestLoadLauncherCacheScope(t *testing.T) {
	dir := t.TempDir()
	ocDir := filepath.Join(dir, ".opencode")
	if err := os.MkdirAll(ocDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ocDir, "oh-my-agentic-coder.yaml"),
		[]byte("cache:\n  scope: workdir\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lc, _, err := LoadLauncher(dir)
	if err != nil {
		t.Fatalf("LoadLauncher: %v", err)
	}
	if lc.Cache.Scope != CacheScopeWorkdir {
		t.Errorf("scope = %q, want %q", lc.Cache.Scope, CacheScopeWorkdir)
	}
}

func TestLoadLauncherRejectsInvalidCacheScope(t *testing.T) {
	dir := t.TempDir()
	ocDir := filepath.Join(dir, ".opencode")
	if err := os.MkdirAll(ocDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ocDir, "oh-my-agentic-coder.yaml"),
		[]byte("cache:\n  scope: nonsense\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadLauncher(dir); err == nil {
		t.Fatalf("LoadLauncher accepted invalid cache scope")
	}
}

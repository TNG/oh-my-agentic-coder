package config

import (
	"os"
	"strings"
	"testing"
)

func TestCacheConfigResolveDefaultsToWorkdir(t *testing.T) {
	scope, err := CacheConfig{}.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if scope != CacheScopeWorkdir {
		t.Errorf("scope = %q, want %q", scope, CacheScopeWorkdir)
	}
	if got := DefaultLauncherConfig().Cache.Scope; got != CacheScopeWorkdir {
		t.Errorf("default cache scope = %q, want %q", got, CacheScopeWorkdir)
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
	if err := os.MkdirAll(LocalConfigDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ProjectLauncherConfigPath(dir),
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

func TestLoadLauncherLocalConfigKeepsGlobalCacheScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFile(t, GlobalLauncherConfigPath(), "cache:\n  scope: global\n")
	workdir := t.TempDir()
	writeFile(t, ProjectLauncherConfigPath(workdir), "sandbox:\n  profile_name: \"\"\n")

	lc, _, err := LoadLauncher(workdir)
	if err != nil {
		t.Fatalf("LoadLauncher: %v", err)
	}
	if lc.Cache.Scope != CacheScopeGlobal {
		t.Errorf("scope = %q; a project config that sets no cache scope must not reset the global one", lc.Cache.Scope)
	}
}

func TestLoadLauncherRejectsInvalidCacheScope(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(LocalConfigDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ProjectLauncherConfigPath(dir),
		[]byte("cache:\n  scope: nonsense\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadLauncher(dir); err == nil {
		t.Fatalf("LoadLauncher accepted invalid cache scope")
	}
}

// With both layers present, an invalid scope must be blamed on the file that
// set it: the global file, not the local one the launch also happens to read.
func TestLoadLauncherBlamesInvalidGlobalCacheScope(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	writeFile(t, GlobalLauncherConfigPath(), "cache:\n  scope: nonsense\n")
	writeFile(t, ProjectLauncherConfigPath(workdir), "facade:\n  max_body_bytes: 42\n")

	_, _, err := LoadLauncher(workdir)
	if err == nil {
		t.Fatal("invalid global cache scope accepted")
	}
	if !strings.Contains(err.Error(), GlobalLauncherConfigPath()) {
		t.Errorf("error must name the file that set the scope, got: %v", err)
	}
}

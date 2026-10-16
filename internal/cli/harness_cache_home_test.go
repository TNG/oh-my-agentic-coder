package cli

import (
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// TestInjectHarnessCacheHome pins the #375 copilot extraction-cache
// redirect: COPILOT_CACHE_HOME is injected pointing at the sandbox-granted
// config home, so the standalone SEA binary extracts its bundled package
// there instead of dying with EPERM on the ungranted ~/Library/Caches.
func TestInjectHarnessCacheHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("COPILOT_HOME", "")
	h, ok := config.LookupHarness("copilot")
	if !ok {
		t.Fatal("copilot harness not found")
	}
	extra := map[string]string{}
	injectHarnessCacheHome(h, extra)
	want := filepath.Join(home, ".copilot")
	if got := extra["COPILOT_CACHE_HOME"]; got != want {
		t.Errorf("COPILOT_CACHE_HOME = %q; want the granted config home %q", got, want)
	}
}

func TestInjectHarnessCacheHomeHonorsHomeEnvRedirect(t *testing.T) {
	// A COPILOT_HOME redirect must flow through: ResolvedSandboxDirs grants
	// the redirected config home, so the cache has to move with it.
	redirect := t.TempDir()
	t.Setenv("COPILOT_HOME", redirect)
	h, ok := config.LookupHarness("copilot")
	if !ok {
		t.Fatal("copilot harness not found")
	}
	extra := map[string]string{}
	injectHarnessCacheHome(h, extra)
	if got := extra["COPILOT_CACHE_HOME"]; got != redirect {
		t.Errorf("COPILOT_CACHE_HOME = %q; want the redirected config home %q", got, redirect)
	}
}

func TestInjectHarnessCacheHomeNoopWithoutCacheEnv(t *testing.T) {
	for _, h := range config.AllHarnesses() {
		if h.CacheHomeEnv == "" {
			extra := map[string]string{"KEEP": "v"}
			injectHarnessCacheHome(h, extra)
			if len(extra) != 1 {
				t.Errorf("%s: unexpected injection into %v", h.Name, extra)
			}
		}
	}
}

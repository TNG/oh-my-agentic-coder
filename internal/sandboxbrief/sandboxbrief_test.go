package sandboxbrief

import (
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/toolcache"
)

func TestCacheGuidanceNamesPathModeAndAllVars(t *testing.T) {
	const dir = "/sandbox/cache/dir"
	got := CacheGuidance(dir, dir, toolcache.ModePersistent)
	for _, want := range []string{
		dir,
		"persistent",
		"OMAC_CACHE_DIR",
		"OMAC_XDG_CACHE_DIR",
		"OMAC_CACHE_MODE",
		"XDG_CACHE_HOME",
		"GOCACHE",
		"GOMODCACHE",
		"NPM_CONFIG_CACHE",
		"PIP_CACHE_DIR",
		"CARGO_HOME",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("CacheGuidance missing %q;\ngot:\n%s", want, got)
		}
	}
	if !strings.Contains(got, dir+"/xdg") && !strings.Contains(got, dir+"\\xdg") {
		t.Errorf("CacheGuidance should reference the XDG subdir under %q;\ngot:\n%s", dir, got)
	}
}

func TestCacheGuidanceExplainsHardcodedHostCachesDenied(t *testing.T) {
	got := CacheGuidance("/c", "/c", toolcache.ModeEphemeral)
	if !strings.Contains(strings.ToLower(got), "denied") {
		t.Errorf("CacheGuidance should explain hardcoded host caches are denied;\ngot:\n%s", got)
	}
	if !strings.Contains(strings.ToLower(got), "hardcoded") && !strings.Contains(strings.ToLower(got), "host") {
		t.Errorf("CacheGuidance should mention hardcoded/host caches;\ngot:\n%s", got)
	}
}

func TestDefaultNonEmpty(t *testing.T) {
	got := Default()
	if strings.TrimSpace(got) == "" {
		t.Fatal("Default() returned empty briefing")
	}
	if !strings.Contains(got, "omac sandbox") {
		t.Errorf("Default() missing expected phrase 'omac sandbox'; got:\n%s", got)
	}
}

func TestResolveUsesOverrideWhenSet(t *testing.T) {
	const custom = "custom briefing text"
	if got := Resolve(custom); got != custom {
		t.Errorf("Resolve(%q) = %q; want the override", custom, got)
	}
}

func TestResolveFallsBackToDefault(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t "} {
		if got := Resolve(in); got != Default() {
			t.Errorf("Resolve(%q) = %q; want Default()", in, got)
		}
	}
}

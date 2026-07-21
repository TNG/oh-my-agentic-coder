package netproxy

import (
	"strings"
	"testing"
)

func TestIsPackageRegistry(t *testing.T) {
	for _, tt := range []struct {
		host string
		want bool
	}{
		{"registry.npmjs.org", true},
		{"registry.npmjs.org.", true}, // trailing dot
		{"files.pythonhosted.org", true},
		{"crates.io", true},
		{"proxy.golang.org", true},
		{"npm.pkg.github.com", true},
		{"registry.corp.example.com", true}, // private, "registry" label
		{"npm.internal.example", true},      // private, "npm" label
		{"api.anthropic.com", false},
		{"models.dev", false},
		{"github.com", false},
		{"example.com", false},
	} {
		if got := isPackageRegistry(tt.host); got != tt.want {
			t.Errorf("isPackageRegistry(%q) = %v; want %v", tt.host, got, tt.want)
		}
	}
}

func TestRegistryDenyHintFiresOnlyForRegistries(t *testing.T) {
	if got := registryDenyHint("api.anthropic.com", "not in allowlist"); got != "" {
		t.Errorf("hint should be empty for a non-registry host; got %q", got)
	}
	if got := registryDenyHint("registry.npmjs.org", "not in allowlist"); got == "" {
		t.Fatal("hint should be non-empty for a registry host")
	}
}

// TestRegistryDenyHintTailorsByReason checks the remedy matches why the request
// was denied — the distinction that tells a user whether a prompt could even
// have been shown.
func TestRegistryDenyHintTailorsByReason(t *testing.T) {
	for _, tt := range []struct {
		reason string
		want   string
	}{
		{"prompt unavailable: on_unavailable=deny", "no interactive network prompt"},
		{"not in allowlist", "network prompt is disabled"},
		{"prompt:deny", "denied at the network prompt"},
		{"deny_domain", "matches a deny rule"},
	} {
		got := registryDenyHint("registry.npmjs.org", tt.reason)
		if !strings.Contains(got, tt.want) {
			t.Errorf("reason %q: hint missing %q;\ngot:\n%s", tt.reason, tt.want, got)
		}
	}
}

func TestRegistryUpstreamHintFiresOnlyForRegistries(t *testing.T) {
	if got := registryUpstreamHint("github.com"); got != "" {
		t.Errorf("upstream hint should be empty for a non-registry host; got %q", got)
	}
	got := registryUpstreamHint("registry.npmjs.org")
	if !strings.Contains(got, "VPN") {
		t.Errorf("upstream hint should mention VPN/reachability;\ngot:\n%s", got)
	}
}

// TestDenyBodyAppendsRegistryHint confirms the hint reaches the body the client
// receives on a policy denial (the elegant single hook point).
func TestDenyBodyAppendsRegistryHint(t *testing.T) {
	body := denyBody("registry.npmjs.org", "prompt unavailable: on_unavailable=deny")
	if !strings.Contains(body, "looks like a package registry") {
		t.Errorf("denyBody should append the registry hint;\ngot:\n%s", body)
	}
	plain := denyBody("api.anthropic.com", "prompt unavailable: on_unavailable=deny")
	if strings.Contains(plain, "looks like a package registry") {
		t.Errorf("denyBody should not append the registry hint for a non-registry host;\ngot:\n%s", plain)
	}
}

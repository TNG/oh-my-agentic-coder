package keychain

import "testing"

// TestScopedServiceNaming locks the keychain service-name scheme so the
// write side (register/secrets set) and the read side (start/serve) can
// never silently disagree about where a secret lives.
func TestScopedServiceNaming(t *testing.T) {
	cases := []struct {
		scope, skill, want string
	}{
		{"", "slack", "omac/slack"},                         // unscoped / global
		{"abc123", "slack", "omac/abc123/slack"},            // workdir-scoped
		{DefaultsScope, "slack", "omac/__defaults__/slack"}, // remembered defaults
	}
	for _, c := range cases {
		if got := ScopedService(c.scope, c.skill); got != c.want {
			t.Errorf("ScopedService(%q,%q) = %q, want %q", c.scope, c.skill, got, c.want)
		}
	}
}

// TestWorkdirIDDeterministicAndDistinct ensures the workdir-id used as the
// secret scope is stable per path and distinct across paths.
func TestWorkdirIDDeterministicAndDistinct(t *testing.T) {
	a1 := WorkdirID("/Users/me/projects/acme")
	a2 := WorkdirID("/Users/me/projects/acme")
	b := WorkdirID("/Users/me/clients/acme")
	if a1 != a2 {
		t.Errorf("WorkdirID not deterministic: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Error("WorkdirID collided for different paths sharing a basename")
	}
	if a1 == "" {
		t.Error("WorkdirID returned empty")
	}
}

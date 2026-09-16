package keychain

import "testing"

// TestSecurityScopedServiceNamesDoNotCollide asserts that ScopedService
// produces a unique identifier per (scope, skillName) pair — a skill name
// containing a path separator cannot forge another skill's keychain
// service string.
//
// ScopedService builds "omac/<scope>/<skill>" by string concatenation with
// no escaping. A skill named "a/b" with no scope produces "omac/a/b",
// identical to skill "b" scoped to "a" — so an unscoped skill can read or
// overwrite a scoped skill's secrets, and vice versa, if either name is
// attacker-influenced (skill names come from a workdir's own directory
// listing before any approval).
func TestSecurityScopedServiceNamesDoNotCollide(t *testing.T) {
	unscoped := ScopedService("", "a/b")
	scoped := ScopedService("a", "b")

	// Control: two genuinely different (scope, skill) pairs with no
	// separator involved produce different services, so the function does
	// distinguish inputs in the ordinary case.
	if ScopedService("x", "y") == ScopedService("x", "z") {
		t.Fatalf("control: ScopedService did not distinguish different skill names under the same scope: the fixture is broken, not the security property")
	}

	if unscoped == scoped {
		t.Errorf("ScopedService(%q, %q) == ScopedService(%q, %q) == %q: an unscoped skill named %q collides with a skill named %q scoped to %q",
			"", "a/b", "a", "b", unscoped, "a/b", "b", "a")
	}
}

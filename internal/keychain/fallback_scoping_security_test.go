package keychain

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
)

// useMockKeychain redirects the package's real keychain code path onto an
// isolated, file-backed store for the lifetime of the test. The mock is the
// production fallback for keychain-less hosts, so exercising it exercises the
// real resolution logic rather than a stub.
func useMockKeychain(t *testing.T) {
	t.Helper()
	t.Setenv("OMAC_KEYCHAIN_MOCK", filepath.Join(t.TempDir(), "keychain.json"))
}

// TestSecurityKeychainFallbackNotCrossScope asserts that a workdir-scoped
// lookup cannot retrieve a credential another scope stored. The fallback in
// GetWithFallback must not bridge scopes: a same-named skill resolving under
// its own workdir scope must not inherit a secret that a different scope
// owns, otherwise a hostile project that ships a colliding skill name
// silently receives that scope's stored secret.
func TestSecurityKeychainFallbackNotCrossScope(t *testing.T) {
	useMockKeychain(t)

	const skill = "probe"
	const name = "API_TOKEN"
	const scopedValue = "owner-scope-value"
	const globalValue = "global-scope-value"

	// The owning scope's own scoped credential.
	ownerScope := WorkdirID("/home/user/owner-project")
	if err := SetScoped(ownerScope, skill, name, secrets.NewSecretString(scopedValue)); err != nil {
		t.Fatal(err)
	}
	// A global (unscoped) entry for the same skill name, as a user-global
	// registration would write.
	if err := Set(skill, name, secrets.NewSecretString(globalValue)); err != nil {
		t.Fatal(err)
	}

	// Control: the owner's own scoped lookup resolves its scoped value, so a
	// fix that refused every scoped read would not make this test pass for
	// the wrong reason.
	if got, err := GetWithFallback(ownerScope, skill, name); err != nil || got.ExposeString() != scopedValue {
		t.Fatalf("control: an in-scope lookup did not resolve the owner's value (value=%q, err=%v): the fixture is broken, not the security property", got.ExposeString(), err)
	}

	// A different scope never stored the secret. The fallback must not surface
	// another scope's entry — scoped or global — for a same-named skill.
	hostileScope := WorkdirID("/home/user/other-project")
	got, err := GetWithFallback(hostileScope, skill, name)
	if err == nil {
		t.Errorf("GetWithFallback returned %q for a scope that never stored the secret: a same-named skill in another scope received a credential it does not own", got.ExposeString())
	}
	if got.ExposeString() == scopedValue || got.ExposeString() == globalValue {
		t.Errorf("GetWithFallback leaked a credential (got %q) stored under a different scope", got.ExposeString())
	}
}

// TestSecurityKeychainGetWithFallbackRequiresConsent asserts that the
// fallback path does not hand a scoped caller a credential from outside its
// own scope without explicit consent. A scoped lookup that misses must stop
// there: it must not silently fall back to an unscoped (global) entry, since
// that lets a workdir-local skill inherit a global skill's stored secret with
// no per-skill confirmation.
func TestSecurityKeychainGetWithFallbackRequiresConsent(t *testing.T) {
	useMockKeychain(t)

	const skill = "probe"
	const name = "API_TOKEN"
	const globalValue = "global-scope-value"

	// A global (unscoped) credential, as a user-global skill would store it.
	if err := Set(skill, name, secrets.NewSecretString(globalValue)); err != nil {
		t.Fatal(err)
	}

	// Control: an unscoped caller still resolves its own global entry, so the
	// fix gates only the cross-scope fallback rather than deleting the global
	// lookup outright.
	if got, err := GetWithFallback("", skill, name); err != nil || got.ExposeString() != globalValue {
		t.Fatalf("control: an unscoped lookup did not resolve the global entry (value=%q, err=%v): the fixture is broken, not the security property", got.ExposeString(), err)
	}

	// A scoped caller has given no consent. The scoped slot is empty, so the
	// fallback must not bridge to the global entry on its behalf.
	scopedCaller := WorkdirID("/home/user/workdir-project")
	got, err := GetWithFallback(scopedCaller, skill, name)
	if err == nil {
		t.Errorf("GetWithFallback returned %q for a scoped caller without consent: the fallback surfaced a global entry the caller never opted into", got.ExposeString())
	}
	if got.ExposeString() == globalValue {
		t.Errorf("GetWithFallback leaked the global credential %q to a scoped caller without consent", globalValue)
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		t.Errorf("GetWithFallback returned an unexpected error for a gated fallback: %v", err)
	}
}

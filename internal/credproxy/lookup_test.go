package credproxy

import (
	"errors"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
)

// fakeLookup is a test CredentialLookup backed by a map. A missing key
// returns ErrCredentialMissing (mirroring keychain.ErrNotFound mapping
// in KeychainLookup).
func fakeLookup(store map[string]string) CredentialLookup {
	return func(alias string) (secrets.Secret, error) {
		v, ok := store[alias]
		if !ok {
			return secrets.Secret{}, ErrCredentialMissing
		}
		return secrets.NewSecretString(v), nil
	}
}

// isSandboxKeychainBlock reports whether err is the macOS-sandbox signal
// that the keychain subprocess was denied (go-keyring shells out to the
// `security` CLI on macOS; the omac sandbox denies it with SIGPRIV,
// surfacing as "exit status 155"). Used only by tests that need to
// skip when the real keychain is sandbox-blocked.
func isSandboxKeychainBlock(err error) bool {
	return strings.Contains(err.Error(), "exit status 155") ||
		strings.Contains(err.Error(), "exit status 126")
}

// TestLookupRegistries_JoinsAliasUpstreamCredential asserts criterion 1:
// the manifest declares (alias, upstream) non-secretly and the credential
// is looked up by alias — it is NOT present in the manifest.
func TestLookupRegistries_JoinsAliasUpstreamCredential(t *testing.T) {
	manifest := []buildmanifest.RegistryEntry{
		{Alias: "internal", Upstream: "https://maven.internal.example/repo"},
	}
	store := map[string]string{"internal": "alice:s3cr3t"}
	regs, err := LookupRegistries(manifest, []string{"internal"}, fakeLookup(store))
	if err != nil {
		t.Fatalf("LookupRegistries: %v", err)
	}
	if len(regs) != 1 {
		t.Fatalf("got %d registries, want 1", len(regs))
	}
	if regs[0].Alias != "internal" {
		t.Errorf("Alias = %q, want internal", regs[0].Alias)
	}
	if regs[0].Upstream != "https://maven.internal.example/repo" {
		t.Errorf("Upstream = %q", regs[0].Upstream)
	}
	if regs[0].Credential.ExposeString() != "alice:s3cr3t" {
		t.Errorf("Credential = %q", regs[0].Credential.ExposeString())
	}
}

// TestLookupRegistries_NoApprovedReturnsNil asserts the common case: no
// approved registries → nil, no error (the caller skips the credential
// proxy).
func TestLookupRegistries_NoApprovedReturnsNil(t *testing.T) {
	regs, err := LookupRegistries(nil, nil, fakeLookup(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if regs != nil {
		t.Errorf("got %v, want nil", regs)
	}
}

// TestLookupRegistries_MissingCredentialDenial asserts criterion 7: an
// approved alias with no keychain credential yields a
// *RegistryCredentialError naming the alias — never the credential.
func TestLookupRegistries_MissingCredentialDenial(t *testing.T) {
	manifest := []buildmanifest.RegistryEntry{
		{Alias: "internal", Upstream: "https://maven.internal.example/repo"},
	}
	_, err := LookupRegistries(manifest, []string{"internal"}, fakeLookup(nil))
	if err == nil {
		t.Fatal("expected error for missing credential")
	}
	var rce *RegistryCredentialError
	if !errors.As(err, &rce) {
		t.Fatalf("error = %T, want *RegistryCredentialError", err)
	}
	if rce.Alias != "internal" {
		t.Errorf("Alias = %q, want internal", rce.Alias)
	}
	msg := rce.Render()
	if !strings.Contains(msg, "internal") {
		t.Errorf("diagnostic must name the alias: %s", msg)
	}
	if strings.Contains(msg, "s3cr3t") {
		t.Errorf("diagnostic must not contain any credential value: %s", msg)
	}
}

// TestLookupRegistries_UnapprovedManifestAliasSkipped asserts an alias
// in the manifest but NOT in the approved set is skipped (the gate is
// the authority on what is approved for the session).
func TestLookupRegistries_UnapprovedManifestAliasSkipped(t *testing.T) {
	manifest := []buildmanifest.RegistryEntry{
		{Alias: "internal", Upstream: "https://maven.internal.example/repo"},
		{Alias: "other", Upstream: "https://maven.other.example/repo"},
	}
	store := map[string]string{
		"internal": "alice:s3cr3t",
		"other":    "bob:hunter2",
	}
	// Only "internal" is approved.
	regs, err := LookupRegistries(manifest, []string{"internal"}, fakeLookup(store))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(regs) != 1 || regs[0].Alias != "internal" {
		t.Errorf("expected only [internal], got %v", regs)
	}
}

// TestLookupRegistries_KeychainUnavailable asserts an unavailable keychain
// backend (headless Linux) maps to a *RegistryCredentialError pointing
// at the OS fix, not a crash.
func TestLookupRegistries_KeychainUnavailable(t *testing.T) {
	manifest := []buildmanifest.RegistryEntry{
		{Alias: "internal", Upstream: "https://maven.internal.example/repo"},
	}
	lookup := func(alias string) (secrets.Secret, error) {
		// Mimic a headless-Linux dbus failure (IsUnavailable=true).
		return secrets.Secret{}, errors.New("org.freedesktop.secrets not provided")
	}
	_, err := LookupRegistries(manifest, []string{"internal"}, lookup)
	if err == nil {
		t.Fatal("expected error for unavailable keychain")
	}
	var rce *RegistryCredentialError
	if !errors.As(err, &rce) {
		t.Fatalf("error = %T, want *RegistryCredentialError", err)
	}
	if !strings.Contains(rce.Render(), "Start the OS keychain backend") {
		t.Errorf("diagnostic must point at OS fix for unavailable backend: %s", rce.Render())
	}
}

// TestKeychainLookup_MissingMapsToErrCredentialMissing asserts the
// production lookup maps keychain.ErrNotFound to ErrCredentialMissing
// (the sentinel LookupRegistries checks). Uses a service name that will
// never exist in any real keychain.
func TestKeychainLookup_MissingMapsToErrCredentialMissing(t *testing.T) {
	_, err := KeychainLookup("nonexistent-alias-for-credproxy-test-06")
	if err == nil {
		t.Skip("keychain returned a credential for a nonexistent alias (unexpected); skipping")
	}
	if !errors.Is(err, ErrCredentialMissing) && !errors.Is(err, keychain.ErrNotFound) {
		// In-sandbox the keychain backend may be unavailable → also acceptable
		// (the LookupRegistries path handles it). Only assert it is NOT a raw
		// non-sentinel error that would bypass the structured denial.
		if !keychain.IsUnavailable(err) {
			t.Fatalf("KeychainLookup error = %v, want ErrCredentialMissing/ErrNotFound/unavailable", err)
		}
	}
}

// TestKeychainLookup_RoundTripAtDocumentedService is the regression test
// for the double-"omac/" prefix bug: KeychainLookup previously called
// keychain.Get (which treats its first arg as a skill name and prepends
// "omac/"), so a credential stored at the doc-documented service
// "omac/build/registry/<alias>" was queried at
// "omac/omac/build/registry/<alias>" and never found. This test stores
// the credential at the EXACT service RegistryKeychainService returns
// (what docs/build-command.md tells the developer to use) and verifies
// KeychainLookup reads it back. It touches the real OS keychain, so it
// skips when the backend is unavailable (in-sandbox, headless CI).
func TestKeychainLookup_RoundTripAtDocumentedService(t *testing.T) {
	alias := "omac-credproxy-roundtrip-test"
	svc := RegistryKeychainService(alias)
	want := "alice:s3cr3t"

	// Store at the documented service (raw, single "omac/" prefix).
	if err := keychain.SetByService(svc, CredentialAccount, secrets.NewSecretString(want)); err != nil {
		// Skip when the keychain is unavailable or sandbox-blocked
		// (in-sandbox macOS returns "exit status 155"; headless Linux
		// returns a dbus error). The round-trip is only meaningful when
		// the backend is actually writable.
		if keychain.IsUnavailable(err) || isSandboxKeychainBlock(err) {
			t.Skipf("keychain backend unavailable: %v", err)
		}
		t.Fatalf("SetByService: %v", err)
	}
	t.Cleanup(func() { _ = keychain.DeleteByService(svc, CredentialAccount) })

	// KeychainLookup must find it. Pre-fix this hit ErrCredentialMissing
	// because the internal keychain.Get query went to omac/omac/... .
	got, err := KeychainLookup(alias)
	if err != nil {
		t.Fatalf("KeychainLookup: %v", err)
	}
	if got.ExposeString() != want {
		t.Errorf("credential = %q, want %q (service convention is %q, account %q)",
			got.ExposeString(), want, svc, CredentialAccount)
	}
}

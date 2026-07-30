package credproxy

import (
	"errors"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
)

// LookupRegistries resolves the keychain credentials for the approved
// private registry aliases and builds the []Registry the credential-
// lift proxy consumes. The manifest declares (alias, upstream) pairs
// non-secretly; this function joins them with the developer's keychain
// credential for each alias (read host-side, unsandboxed, at proxy
// startup — never passed into the executor).
//
// The manifest's full RegistryEntry list is the source of upstream
// identities; approvedAliases (the frozen-for-session capability set
// from Gate) is the subset that may actually be activated. An alias in
// the manifest but NOT in approvedAliases is skipped (it was not
// approved for this session). An alias in approvedAliases but missing
// from the manifest is a contract violation (the gate should have
// caught it) — treated as missing.
//
// A registry whose keychain credential is missing yields a
// *RegistryCredentialError naming the alias (criterion 7): the build
// cannot resolve private dependencies without the lift, and the
// credential cannot be recovered from inside the executor. The keychain
// backend being unavailable (headless Linux without Secret Service) is
// treated the same way — a structured denial, never a crash.
//
// Returns an empty slice (no error) when approvedAliases is empty —
// the common case (no private registries approved). The caller skips
// starting the credential proxy in that case.
func LookupRegistries(manifestRegistries []buildmanifest.RegistryEntry, approvedAliases []string, lookup CredentialLookup) ([]Registry, error) {
	if len(approvedAliases) == 0 {
		return nil, nil
	}
	// Index the manifest's upstreams by alias for the approved subset.
	upstream := map[string]string{}
	for _, r := range manifestRegistries {
		if r.Alias != "" && r.Upstream != "" {
			upstream[r.Alias] = r.Upstream
		}
	}
	approved := map[string]bool{}
	for _, a := range approvedAliases {
		approved[a] = true
	}
	var regs []Registry
	for _, a := range approvedAliases {
		up, ok := upstream[a]
		if !ok {
			// Alias approved but not in the manifest — skip (the gate
			// is the authority; this is defensive).
			continue
		}
		cred, err := lookup(a)
		if err != nil {
			if errors.Is(err, ErrCredentialMissing) || errors.Is(err, keychain.ErrNotFound) {
				return nil, &RegistryCredentialError{Alias: a, Kind: CredentialMissing, Reason: "no keychain entry for the approved registry alias"}
			}
			if keychain.IsUnavailable(err) {
				return nil, &RegistryCredentialError{Alias: a, Kind: CredentialBackendUnavailable, Reason: "keychain backend unavailable on this host"}
			}
			return nil, &RegistryCredentialError{Alias: a, Kind: CredentialReadFailed, Reason: "keychain read failed: " + err.Error()}
		}
		if cred.IsEmpty() {
			return nil, &RegistryCredentialError{Alias: a, Kind: CredentialMissing, Reason: "no keychain entry for the approved registry alias"}
		}
		regs = append(regs, Registry{Alias: a, Upstream: up, Credential: cred})
	}
	return regs, nil
}

// KeychainLookup adapts keychain.Get to the CredentialLookup seam. The
// credential value is stored as a single "<user>:<password>" string
// (HTTP Basic auth credentials) under the registry keychain
// service/account (see RegistryKeychainService / CredentialAccount). A
// missing/unavailable entry maps to ErrCredentialMissing so
// LookupRegistries can produce a structured *RegistryCredentialError.
// The proxy base64-encodes the raw value as the Basic-auth credential
// (base64("user:password")) — no split is needed in-process.
func KeychainLookup(alias string) (secrets.Secret, error) {
	svc := RegistryKeychainService(alias)
	v, err := keychain.Get(svc, CredentialAccount)
	if err != nil {
		if errors.Is(err, keychain.ErrNotFound) {
			return secrets.Secret{}, ErrCredentialMissing
		}
		return secrets.Secret{}, err
	}
	return v, nil
}

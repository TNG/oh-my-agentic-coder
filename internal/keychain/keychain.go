// Package keychain is a thin wrapper over github.com/zalando/go-keyring.
//
// Naming convention (matches oh-my-agentic-coder.md §16.3):
//
//	service = "omac/<skill-name>"                       (legacy / global skill)
//	service = "omac/<workdir-id>/<skill-name>"          (serve-mode, per-workdir)
//	service = "omac/__defaults__/<skill-name>"          (remembered defaults)
//	account = <secret-name>
//
// The unscoped form (omac/<skill>) is what single-workdir `omac start` and
// user-global skills use; it is the backward-compatible default. Serve mode
// isolates workdir-local skills by keying on a persistent workdir-id
// (sha256 of the absolute workdir) so two directories holding a same-named
// skill — or two versions of it — don't share a credential. See
// docs/MULTI_DIR_DESKTOP.md §4.3/§8.2.
//
// The backend (macOS Keychain, Secret Service, Windows Credential Manager)
// is selected by go-keyring based on the host OS. A file-based fallback
// for headless Linux is declared as future work in the design doc and is
// not implemented in v0.
package keychain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"

	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
)

// ErrNotFound is returned when a secret is not present in the keychain.
var ErrNotFound = errors.New("keychain: secret not found")

// DefaultsScope is the reserved workdir-id under which "last-known-good"
// default secret values are mirrored (docs/MULTI_DIR_DESKTOP.md §4.4). It
// is never a real workdir.
const DefaultsScope = "__defaults__"

// WorkdirID returns the persistent identity for a workdir used to scope
// secrets in serve mode: the hex sha256 of the absolute path.
func WorkdirID(absWorkdir string) string {
	sum := sha256.Sum256([]byte(absWorkdir))
	return hex.EncodeToString(sum[:])
}

// Service returns the unscoped service identifier for a skill name
// (omac/<skill>). Used by single-workdir start and by user-global skills.
func Service(skillName string) string {
	return "omac/" + skillName
}

// ScopedService returns the service identifier for a (scope, skill) pair.
// An empty scope yields the unscoped Service form, so callers that don't
// opt into scoping behave exactly as before. A non-empty scope (a
// workdir-id or DefaultsScope) yields "omac/<scope>/<skill>".
func ScopedService(scope, skillName string) string {
	if scope == "" {
		return Service(skillName)
	}
	return "omac/" + scope + "/" + skillName
}

// Set stores a secret for (skill, name) in the unscoped service.
// Overwrites any existing value.
func Set(skillName, name string, value secrets.Secret) error {
	return SetScoped("", skillName, name, value)
}

// Get retrieves a secret for (skill, name) from the unscoped service.
// Returns ErrNotFound if absent.
func Get(skillName, name string) (secrets.Secret, error) {
	return GetScoped("", skillName, name)
}

// Has returns true if an unscoped secret is present for (skill, name).
func Has(skillName, name string) (bool, error) {
	return HasScoped("", skillName, name)
}

// Delete removes an unscoped secret for (skill, name). Missing entries are
// not an error.
func Delete(skillName, name string) error {
	return DeleteScoped("", skillName, name)
}

// SetScoped stores a secret under (scope, skill). scope="" is the unscoped
// (legacy/global) form; a workdir-id isolates per-workdir; DefaultsScope
// mirrors the remembered default.
func SetScoped(scope, skillName, name string, value secrets.Secret) error {
	svc := ScopedService(scope, skillName)
	if err := keyring.Set(svc, name, value.ExposeString()); err != nil {
		return fmt.Errorf("keychain set %s/%s: %w", svc, name, err)
	}
	return nil
}

// GetScoped retrieves a secret under (scope, skill). Returns ErrNotFound if
// absent.
func GetScoped(scope, skillName, name string) (secrets.Secret, error) {
	svc := ScopedService(scope, skillName)
	v, err := keyring.Get(svc, name)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return secrets.Secret{}, ErrNotFound
		}
		return secrets.Secret{}, fmt.Errorf("keychain get %s/%s: %w", svc, name, err)
	}
	return secrets.NewSecretString(v), nil
}

// GetWithFallback retrieves a secret under (scope, skill), falling back to
// the unscoped key (omac/<skill>) when the scoped key is absent. This lets
// readers (start, serve) find secrets whether they were stored scoped
// (per-workdir, written by serve-aware register) or unscoped (legacy /
// global). An empty scope is just the unscoped lookup. Returns ErrNotFound
// only when neither key exists.
func GetWithFallback(scope, skillName, name string) (secrets.Secret, error) {
	if scope != "" {
		v, err := GetScoped(scope, skillName, name)
		if err == nil {
			return v, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return secrets.Secret{}, err
		}
	}
	return GetScoped("", skillName, name)
}

// HasScoped reports whether a secret is present under (scope, skill).
func HasScoped(scope, skillName, name string) (bool, error) {
	svc := ScopedService(scope, skillName)
	_, err := keyring.Get(svc, name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, keyring.ErrNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("keychain probe %s/%s: %w", svc, name, err)
}

// DeleteScoped removes a secret under (scope, skill). Missing entries are
// not an error.
func DeleteScoped(scope, skillName, name string) error {
	svc := ScopedService(scope, skillName)
	err := keyring.Delete(svc, name)
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return fmt.Errorf("keychain delete %s/%s: %w", svc, name, err)
}

// SetWithDefault stores a secret under (scope, skill) AND mirrors it into
// the DefaultsScope so a future registration elsewhere can reuse it as a
// suggested value (docs/MULTI_DIR_DESKTOP.md §4.4). A failure to mirror is
// not fatal to the primary write — defaults are best-effort convenience.
func SetWithDefault(scope, skillName, name string, value secrets.Secret) error {
	if err := SetScoped(scope, skillName, name, value); err != nil {
		return err
	}
	if scope != DefaultsScope {
		_ = SetScoped(DefaultsScope, skillName, name, value)
	}
	return nil
}

// GetDefault returns the remembered default secret for (skill, name), or
// ErrNotFound.
func GetDefault(skillName, name string) (secrets.Secret, error) {
	return GetScoped(DefaultsScope, skillName, name)
}

// SetScopedDefaultMirror writes value only into the remembered-defaults
// scope (omac/__defaults__/<skill>), without touching any per-workdir or
// unscoped key. Used to backfill the default from an already-stored secret
// so `register --defaults` can reuse it later.
func SetScopedDefaultMirror(skillName, name string, value secrets.Secret) error {
	return SetScoped(DefaultsScope, skillName, name, value)
}

// DeleteAll removes every declared unscoped secret for a skill. Secrets not
// listed are left in place (go-keyring has no list-by-service primitive).
func DeleteAll(skillName string, names []string) error {
	return DeleteAllScoped("", skillName, names)
}

// DeleteAllScoped removes every declared secret for (scope, skill).
func DeleteAllScoped(scope, skillName string, names []string) error {
	for _, n := range names {
		if err := DeleteScoped(scope, skillName, n); err != nil {
			return err
		}
	}
	return nil
}

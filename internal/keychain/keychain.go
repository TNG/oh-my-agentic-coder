// Package keychain is a thin wrapper over github.com/zalando/go-keyring.
//
// Naming convention (matches oh-my-agentic-coder.md §16.3):
//
//	service = "omac/<skill-name>"
//	account = <secret-name>
//
// The backend (macOS Keychain, Secret Service, Windows Credential Manager)
// is selected by go-keyring based on the host OS. A file-based fallback
// for headless Linux is declared as future work in the design doc and is
// not implemented in v0.
package keychain

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"

	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
)

// ErrNotFound is returned when a secret is not present in the keychain.
var ErrNotFound = errors.New("keychain: secret not found")

// Service returns the service identifier for a given skill name.
func Service(skillName string) string {
	return "omac/" + skillName
}

// Set stores a secret for (skill, name). Overwrites any existing value.
func Set(skillName, name string, value secrets.Secret) error {
	if err := keyring.Set(Service(skillName), name, value.ExposeString()); err != nil {
		return fmt.Errorf("keychain set %s/%s: %w", Service(skillName), name, err)
	}
	return nil
}

// Get retrieves a secret for (skill, name). Returns ErrNotFound if absent.
func Get(skillName, name string) (secrets.Secret, error) {
	v, err := keyring.Get(Service(skillName), name)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return secrets.Secret{}, ErrNotFound
		}
		return secrets.Secret{}, fmt.Errorf("keychain get %s/%s: %w", Service(skillName), name, err)
	}
	return secrets.NewSecretString(v), nil
}

// Has returns true if a secret is present for (skill, name).
func Has(skillName, name string) (bool, error) {
	_, err := keyring.Get(Service(skillName), name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, keyring.ErrNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("keychain probe %s/%s: %w", Service(skillName), name, err)
}

// Delete removes a secret for (skill, name). Missing entries are not an error.
func Delete(skillName, name string) error {
	err := keyring.Delete(Service(skillName), name)
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return fmt.Errorf("keychain delete %s/%s: %w", Service(skillName), name, err)
}

// DeleteAll removes every declared secret for a skill. Secrets not listed
// are left in place (go-keyring has no list-by-service primitive).
func DeleteAll(skillName string, names []string) error {
	for _, n := range names {
		if err := Delete(skillName, n); err != nil {
			return err
		}
	}
	return nil
}

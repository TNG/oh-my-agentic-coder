package keychain

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"

	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
)

// MCPService returns the keychain service of an MCP server registration.
// It is a separate namespace from skill secrets and is never mirrored into
// DefaultsScope.
func MCPService(scopeKey, server string) string {
	return "omac-mcp/" + scopeKey + "/" + server
}

// SetMCP stores a secret of an MCP server registration, overwriting any existing value.
func SetMCP(scopeKey, server, name string, value secrets.Secret) error {
	svc := MCPService(scopeKey, server)
	if mock := mockBackend(); mock != nil {
		return mock.set(svc, name, value.ExposeString())
	}
	if err := keyring.Set(svc, name, value.ExposeString()); err != nil {
		return fmt.Errorf("keychain set %s/%s: %w", svc, name, err)
	}
	return nil
}

// GetMCP retrieves a secret of an MCP server registration; ErrNotFound when absent.
func GetMCP(scopeKey, server, name string) (secrets.Secret, error) {
	svc := MCPService(scopeKey, server)
	if mock := mockBackend(); mock != nil {
		v, err := mock.get(svc, name)
		if err != nil {
			return secrets.Secret{}, err
		}
		return secrets.NewSecretString(v), nil
	}
	v, err := keyring.Get(svc, name)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return secrets.Secret{}, ErrNotFound
		}
		if IsUnavailable(err) {
			return secrets.Secret{}, fmt.Errorf("%w: %w: %w", ErrNotFound, ErrUnavailable, err)
		}
		return secrets.Secret{}, fmt.Errorf("keychain get %s/%s: %w", svc, name, err)
	}
	return secrets.NewSecretString(v), nil
}

// DeleteMCP removes a secret of an MCP server registration; a missing entry is not an error.
func DeleteMCP(scopeKey, server, name string) error {
	svc := MCPService(scopeKey, server)
	if mock := mockBackend(); mock != nil {
		return mock.delete(svc, name)
	}
	err := keyring.Delete(svc, name)
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return fmt.Errorf("keychain delete %s/%s: %w", svc, name, err)
}

package mcpreg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
)

// ErrRemoteUnsupported is returned for a server entry that is not a local stdio server.
var ErrRemoteUnsupported = errors.New("remote MCP servers are not supported yet")

// serverEntry is the stdio MCP server shape common to harness clients.
type serverEntry struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// remoteKeys name the entry keys that mark a remote server.
var remoteKeys = []string{"url", "httpUrl", "serverUrl", "headers"}

// FromServerEntry builds a definition from one harness MCP server entry of the
// form {type?: "stdio", command, args, env}. Every env value must be a
// non-empty literal; a secret needs ParseServerEntry.
func FromServerEntry(name string, raw []byte) (*Definition, error) {
	def, _, err := ParseServerEntry(name, raw, nil)
	return def, err
}

// ParseServerEntry is FromServerEntry with secretKeys naming the env keys
// that are secrets. Such a key becomes a declared secret of the definition.
// Its entry value is returned in the map when it is a literal, and is absent
// when the entry leaves it empty or refers to a variable, so the caller asks
// for it. Secret values never enter the definition.
func ParseServerEntry(name string, raw []byte, secretKeys []string) (*Definition, map[string]secrets.Secret, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil, nil, fmt.Errorf("server %q: entry must be a JSON object", name)
	}
	for _, k := range remoteKeys {
		if _, ok := keys[k]; ok {
			return nil, nil, fmt.Errorf("server %q: %w", name, ErrRemoteUnsupported)
		}
	}
	var entry serverEntry
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entry); err != nil {
		return nil, nil, fmt.Errorf("server %q: %w", name, err)
	}
	if entry.Type != "" && entry.Type != "stdio" {
		return nil, nil, fmt.Errorf("server %q: %w (type %q)", name, ErrRemoteUnsupported, entry.Type)
	}
	if entry.Command == "" {
		return nil, nil, fmt.Errorf("server %q: command is required", name)
	}

	def := &Definition{Name: name, Server: ServerSpec{Command: append([]string{entry.Command}, entry.Args...)}}
	values := map[string]secrets.Secret{}
	for _, key := range sortedEnvKeys(entry.Env) {
		value := entry.Env[key]
		isRef := strings.Contains(value, "${")
		if slices.Contains(secretKeys, key) {
			def.Secrets = append(def.Secrets, key)
			if value != "" && !isRef {
				values[key] = secrets.NewSecretString(value)
			}
			continue
		}
		if value == "" || isRef {
			return nil, nil, fmt.Errorf("server %q: env %s is empty or a variable reference; declare --secret %s", name, key, key)
		}
		if def.Server.Env == nil {
			def.Server.Env = map[string]string{}
		}
		def.Server.Env[key] = value
	}
	for _, key := range secretKeys {
		if !slices.Contains(def.Secrets, key) {
			def.Secrets = append(def.Secrets, key)
		}
	}
	if err := def.validate(); err != nil {
		return nil, nil, fmt.Errorf("server %q: %w", name, err)
	}
	return def, values, nil
}

func sortedEnvKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

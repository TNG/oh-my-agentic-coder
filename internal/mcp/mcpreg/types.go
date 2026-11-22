// Package mcpreg stores the MCP servers a user has registered with omac.
package mcpreg

import (
	"os"
	"path/filepath"
	"time"
)

const (
	// FacadeNamespace is the facade path namespace under which MCP gateways
	// are mounted; skill mounts cannot use it.
	FacadeNamespace = "__mcp__"
	// GlobalScopeKey is the scope key of registrations that apply to every workdir.
	GlobalScopeKey = "global"
)

// Scope identifies where a registration applies.
type Scope struct {
	Key         string `json:"key"`
	DisplayPath string `json:"display_path"`
}

// Definition is the content of an mcp.yaml definition file.
type Definition struct {
	Name    string     `yaml:"name"              json:"name"`
	Server  ServerSpec `yaml:"server"            json:"server"`
	Secrets []string   `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	Mode    *ModeSpec  `yaml:"mode,omitempty"    json:"mode,omitempty"`
}

// ServerSpec describes the stdio server process.
type ServerSpec struct {
	Command        []string          `yaml:"command"                   json:"command"`
	Env            map[string]string `yaml:"env,omitempty"             json:"env,omitempty"`
	EnvPassthrough []string          `yaml:"env_passthrough,omitempty" json:"env_passthrough,omitempty"`
}

// ModeSpec sets one environment variable that switches the server's mode.
type ModeSpec struct {
	Name  string `yaml:"name"  json:"name"`
	Value string `yaml:"value" json:"value"`
}

// Record is what omac persists about one registration.
type Record struct {
	Name          string    `json:"name"`
	ScopeKey      string    `json:"scope_key"`
	DisplayPath   string    `json:"display_path"`
	Harnesses     []string  `json:"harnesses"`
	BundleHash    string    `json:"bundle_hash"`
	RegisteredAt  time.Time `json:"registered_at"`
	SecretsStored []string  `json:"secrets_stored,omitempty"`
}

// Entry is a loaded registration with its verified definition.
type Entry struct {
	Record
	Def         *Definition `json:"-"`
	SnapshotDir string      `json:"-"`
	DefPath     string      `json:"-"`
}

// BridgePath returns the argv a harness spawns to bridge stdio to the
// gateway of the named server: the running omac binary plus "mcp bridge name".
func BridgePath(name string) []string {
	exe, err := os.Executable()
	if err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
	} else {
		exe = "omac"
	}
	return []string{exe, "mcp", "bridge", name}
}

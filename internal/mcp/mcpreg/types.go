// Package mcpreg stores the MCP servers a user has registered with omac.
package mcpreg

import (
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
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

	// Access limits the classes of tool the agent may call. Empty means
	// read-write-delete.
	Access mcppolicy.Access `yaml:"access,omitempty" json:"access,omitempty"`
	// Tools classifies the server's tools. A tool not listed is refused.
	Tools []mcppolicy.Tool `yaml:"tools,omitempty" json:"tools,omitempty"`
	// Approvals overrides the approval mode per class. Nil means the defaults.
	Approvals *mcppolicy.Approvals `yaml:"approvals,omitempty" json:"approvals,omitempty"`
	// Review records the tool list the user reviewed.
	Review *mcppolicy.Review `yaml:"reviewed,omitempty" json:"reviewed,omitempty"`
}

// Policy builds the effective policy of the definition, with defaults applied.
func (d *Definition) Policy() *mcppolicy.Policy {
	p := &mcppolicy.Policy{
		Server:    d.Name,
		Access:    d.Access,
		Tools:     make(map[string]mcppolicy.Class, len(d.Tools)),
		Approvals: mcppolicy.DefaultApprovals(),
		Review:    d.Review,
	}
	if p.Access == "" {
		p.Access = mcppolicy.DefaultAccess
	}
	for _, t := range d.Tools {
		p.Tools[t.Name] = t.Class
	}
	if a := d.Approvals; a != nil {
		if a.Read != "" {
			p.Approvals.Read = a.Read
		}
		if a.Write != "" {
			p.Approvals.Write = a.Write
		}
		if a.Delete != "" {
			p.Approvals.Delete = a.Delete
		}
	}
	return p
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

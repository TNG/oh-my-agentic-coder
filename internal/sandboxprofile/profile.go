// Package sandboxprofile defines the configuration format for omac's
// built-in sandbox (the nono replacement) and the logic that turns a
// profile file plus CLI flag overrides into a fully resolved grant set.
//
// The JSON shape intentionally mirrors the subset of nono's profile
// schema that omac exercises (see openspec/changes/native-sandbox):
//
//	{
//	  "meta": { "name": "tng-sandbox" },
//	  "workdir": { "access": "readwrite" },
//	  "filesystem": { "allow": [...], "read": [...], "write": [...],
//	                  "deny": [...], "override_deny": [...] },
//	  "network": { "mode": "filtered", "allow_domain": [...],
//	               "deny_domain": [...], "listen_port": [...],
//	               "allow_tcp_connect": [...], "open_port": [...],
//	               "network_prompt": { "enabled": true,
//	                                   "prompt_timeout_secs": 60,
//	                                   "on_unavailable": "deny" },
//	               "enforcement": "kernel" },
//	  "environment": { "allow_vars": [...] }
//	}
//
// Unknown fields are rejected so typos fail loudly instead of silently
// weakening the sandbox.
package sandboxprofile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// Workdir access levels.
const (
	AccessNone      = "none"
	AccessRead      = "read"
	AccessWrite     = "write"
	AccessReadWrite = "readwrite"
)

// Network modes.
const (
	ModeFiltered = "filtered"
	ModeBlocked  = "blocked"
	ModeOpen     = "open"
)

// Network enforcement levels.
const (
	EnforceKernel  = "kernel"
	EnforceEnvOnly = "env-only"
)

// OnUnavailable prompt fallbacks.
const (
	OnUnavailableDeny  = "deny"
	OnUnavailableAllow = "allow"
)

// DefaultPromptTimeoutSecs matches nono's DEFAULT_PROMPT_TIMEOUT.
const DefaultPromptTimeoutSecs = 60

// Profile is the parsed, validated sandbox configuration.
type Profile struct {
	Meta        Meta        `json:"meta,omitempty"`
	Workdir     Workdir     `json:"workdir,omitempty"`
	Filesystem  Filesystem  `json:"filesystem,omitempty"`
	Network     Network     `json:"network,omitempty"`
	Environment Environment `json:"environment,omitempty"`
}

// Meta carries informational fields only.
type Meta struct {
	Name string `json:"name,omitempty"`
}

// Workdir controls the implicit grant on the invocation directory.
type Workdir struct {
	// Access is one of none|read|write|readwrite. Empty means none.
	Access string `json:"access,omitempty"`
}

// Filesystem lists path grants. Entries may be directories or files and
// support ~ and $VAR expansion (see Expand).
type Filesystem struct {
	Allow []string `json:"allow,omitempty"` // read+write
	Read  []string `json:"read,omitempty"`  // read-only
	Write []string `json:"write,omitempty"` // write-only
	// AllowUnixDir grants AF_UNIX connect to every socket under the given
	// directory (a subpath rule) plus read+write access to it, for tools
	// that mint sockets with dynamic names at runtime (e.g. Claude Code's
	// Agent View under /tmp/cc-daemon-<uid>). Unlike allow/read/write it is
	// not existence-filtered: the daemon may create the dir after launch.
	AllowUnixDir []string `json:"allow_unix_dir,omitempty"`
	// Deny carves holes out of the granted trees: matching paths are
	// masked (denied read+write) even when an allow/read/write grant
	// would otherwise expose them. Two entry forms are supported:
	//   - a path (contains a separator, ~ or $VAR), e.g. "~/.env" or
	//     "./config/prod.env": expanded and denied at that exact path;
	//   - a bare basename glob (no separator), e.g. ".env" or "*.key":
	//     denied wherever that filename appears inside a granted tree,
	//     the current working directory included.
	Deny []string `json:"deny,omitempty"`
	// OverrideDeny removes entries from the built-in protected-path
	// deny set (Baseline.ProtectedPaths). It does not grant access by
	// itself; a matching allow/read/write grant is still required.
	OverrideDeny []string `json:"override_deny,omitempty"`
}

// Network configures isolation, filtering and port openings.
type Network struct {
	// Mode is filtered (default), blocked, or open.
	Mode string `json:"mode,omitempty"`
	// AllowDomain / DenyDomain accept exact hostnames or "*.suffix"
	// wildcards (match the suffix itself and any subdomain).
	AllowDomain []string `json:"allow_domain,omitempty"`
	DenyDomain  []string `json:"deny_domain,omitempty"`
	// ListenPort: TCP ports the child may bind/listen on.
	ListenPort []int `json:"listen_port,omitempty"`
	// AllowTCPConnect: direct outbound TCP to ANY host on these ports
	// (kernel cannot constrain the destination host). E.g. 22 for SSH.
	AllowTCPConnect []int `json:"allow_tcp_connect,omitempty"`
	// OpenPort: localhost TCP, both connect and bind (bridge port).
	OpenPort []int `json:"open_port,omitempty"`
	// NetworkPrompt configures the interactive allow/deny dialog.
	NetworkPrompt *NetworkPrompt `json:"network_prompt,omitempty"`
	// Enforcement is kernel (default) or env-only (Linux escape hatch
	// for kernels without Landlock ABI >= 4; advisory only).
	Enforcement string `json:"enforcement,omitempty"`
}

// NetworkPrompt mirrors nono's network_prompt block.
type NetworkPrompt struct {
	// Enabled defaults to true when the network_prompt object is
	// present (nono semantics), hence the pointer.
	Enabled *bool `json:"enabled,omitempty"`
	// PromptTimeoutSecs defaults to 60.
	PromptTimeoutSecs int `json:"prompt_timeout_secs,omitempty"`
	// OnUnavailable is deny (default) or allow.
	OnUnavailable string `json:"on_unavailable,omitempty"`
}

// PromptEnabled reports whether the interactive prompt is on.
func (n *Network) PromptEnabled() bool {
	if n.NetworkPrompt == nil {
		return false
	}
	if n.NetworkPrompt.Enabled == nil {
		return true // present-but-unset means enabled, like nono
	}
	return *n.NetworkPrompt.Enabled
}

// PromptTimeoutSecs returns the configured or default prompt timeout.
func (n *Network) PromptTimeoutSecs() int {
	if n.NetworkPrompt == nil || n.NetworkPrompt.PromptTimeoutSecs <= 0 {
		return DefaultPromptTimeoutSecs
	}
	return n.NetworkPrompt.PromptTimeoutSecs
}

// OnUnavailable returns the configured or default fallback policy.
func (n *Network) OnUnavailable() string {
	if n.NetworkPrompt == nil || n.NetworkPrompt.OnUnavailable == "" {
		return OnUnavailableDeny
	}
	return n.NetworkPrompt.OnUnavailable
}

// EffectiveMode returns the network mode with the default applied.
func (n *Network) EffectiveMode() string {
	if n.Mode == "" {
		return ModeFiltered
	}
	return n.Mode
}

// EffectiveEnforcement returns the enforcement level with the default applied.
func (n *Network) EffectiveEnforcement() string {
	if n.Enforcement == "" {
		return EnforceKernel
	}
	return n.Enforcement
}

// Environment configures env-var filtering for the child.
type Environment struct {
	// AllowVars lists exact names or trailing-* prefixes. Empty/absent
	// means every variable passes (minus the always-on blocklist).
	AllowVars []string `json:"allow_vars,omitempty"`
}

// Parse decodes and validates a profile, rejecting unknown fields.
func Parse(data []byte) (*Profile, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("parse sandbox profile: %w", err)
	}
	// A second JSON value in the stream is a malformed profile.
	if dec.More() {
		return nil, fmt.Errorf("parse sandbox profile: trailing data after JSON object")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate checks enum fields and port ranges.
func (p *Profile) Validate() error {
	switch p.Workdir.Access {
	case "", AccessNone, AccessRead, AccessWrite, AccessReadWrite:
	default:
		return fmt.Errorf("sandbox profile: invalid workdir.access %q (want none|read|write|readwrite)", p.Workdir.Access)
	}
	switch p.Network.Mode {
	case "", ModeFiltered, ModeBlocked, ModeOpen:
	default:
		return fmt.Errorf("sandbox profile: invalid network.mode %q (want filtered|blocked|open)", p.Network.Mode)
	}
	switch p.Network.Enforcement {
	case "", EnforceKernel, EnforceEnvOnly:
	default:
		return fmt.Errorf("sandbox profile: invalid network.enforcement %q (want kernel|env-only)", p.Network.Enforcement)
	}
	if np := p.Network.NetworkPrompt; np != nil {
		switch np.OnUnavailable {
		case "", OnUnavailableDeny, OnUnavailableAllow:
		default:
			return fmt.Errorf("sandbox profile: invalid network.network_prompt.on_unavailable %q (want deny|allow)", np.OnUnavailable)
		}
		if np.PromptTimeoutSecs < 0 {
			return fmt.Errorf("sandbox profile: network.network_prompt.prompt_timeout_secs must be >= 0")
		}
	}
	for _, group := range []struct {
		name  string
		ports []int
		// open_port alone accepts the 0 sentinel.
		allowZero bool
	}{
		{name: "listen_port", ports: p.Network.ListenPort},
		{name: "allow_tcp_connect", ports: p.Network.AllowTCPConnect},
		{name: "open_port", ports: p.Network.OpenPort, allowZero: true},
	} {
		for _, port := range group.ports {
			if group.allowZero && port == 0 {
				continue
			}
			if port < 1 || port > 65535 {
				return fmt.Errorf("sandbox profile: network.%s contains invalid port %d", group.name, port)
			}
		}
	}
	for _, v := range p.Environment.AllowVars {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("sandbox profile: environment.allow_vars contains an empty entry")
		}
	}
	for _, d := range p.Filesystem.Deny {
		if strings.TrimSpace(d) == "" {
			return fmt.Errorf("sandbox profile: filesystem.deny contains an empty entry")
		}
		// A basename-glob entry must be a valid pattern so a typo like
		// "[" fails loudly here rather than silently denying nothing.
		if IsBasenameGlob(d) {
			if _, err := filepath.Match(d, "probe"); err != nil {
				return fmt.Errorf("sandbox profile: filesystem.deny %q is not a valid glob: %w", d, err)
			}
		}
	}
	return nil
}

// IsBasenameGlob reports whether a filesystem.deny entry is a bare
// filename pattern (matched in any granted directory) rather than an
// explicit path. Entries with a path separator, or a leading ~ or $
// (which expand to a path), are treated as explicit paths.
func IsBasenameGlob(entry string) bool {
	if strings.ContainsRune(entry, '/') {
		return false
	}
	if strings.HasPrefix(entry, "~") || strings.HasPrefix(entry, "$") {
		return false
	}
	return true
}

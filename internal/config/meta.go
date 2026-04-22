// Package config defines the on-disk configuration formats used by omac:
// skill meta.yaml (with the sidecar block), the per-workdir sidecar.json
// registry, and the oh-my-agentic-coder.json launcher config.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/tngtech/oh-my-agentic-coder/internal/osinfo"
)

// Meta is the skill metadata as stored in meta.yaml. Only the fields
// omac cares about are declared; unknown keys are ignored.
type Meta struct {
	Name         string   `yaml:"name"`
	Type         string   `yaml:"type"`
	Version      string   `yaml:"version"`
	Description  string   `yaml:"description"`
	Author       string   `yaml:"author"`
	Dependencies []string `yaml:"dependencies"`

	Sidecar *SidecarMeta `yaml:"sidecar,omitempty"`
}

// SidecarMeta is the optional sidecar block in meta.yaml. See
// oh-my-agentic-coder.md §7 for the full schema.
type SidecarMeta struct {
	Command        []string          `yaml:"command"`
	Mount          string            `yaml:"mount,omitempty"`
	EnvPassthrough []string          `yaml:"env_passthrough,omitempty"`
	Secrets        []SecretSpec      `yaml:"secrets,omitempty"`
	Health         *HealthSpec       `yaml:"health,omitempty"`
	InstallScripts map[string]string `yaml:"install_scripts,omitempty"`
	Protocols      []string          `yaml:"protocols,omitempty"`
	Limits         *LimitsSpec       `yaml:"limits,omitempty"`
}

// SecretSpec describes a single credential that omac prompts for at
// register time and injects into the sidecar's env at start time.
type SecretSpec struct {
	Name           string `yaml:"name"`
	Description    string `yaml:"description,omitempty"`
	Required       *bool  `yaml:"required,omitempty"` // default true
	Pattern        string `yaml:"pattern,omitempty"`
	DefaultFromEnv string `yaml:"default_from_env,omitempty"`
	Multiline      bool   `yaml:"multiline,omitempty"`
}

// IsRequired returns true unless the spec explicitly opts out.
func (s SecretSpec) IsRequired() bool { return s.Required == nil || *s.Required }

// HealthSpec controls the liveness probe the supervisor waits on.
type HealthSpec struct {
	Path           string `yaml:"path,omitempty"`
	InitialDelayMS int    `yaml:"initial_delay_ms,omitempty"`
	TimeoutMS      int    `yaml:"timeout_ms,omitempty"`
	IntervalMS     int    `yaml:"interval_ms,omitempty"`
}

// Defaults fills zero values with the documented defaults and returns a copy.
func (h *HealthSpec) Defaults() HealthSpec {
	out := HealthSpec{}
	if h != nil {
		out = *h
	}
	if out.Path == "" {
		out.Path = "/status"
	}
	if out.InitialDelayMS == 0 {
		out.InitialDelayMS = 200
	}
	if out.TimeoutMS == 0 {
		out.TimeoutMS = 5000
	}
	if out.IntervalMS == 0 {
		out.IntervalMS = 500
	}
	return out
}

// LimitsSpec configures per-skill proxy limits.
type LimitsSpec struct {
	MaxBodyBytes    int64 `yaml:"max_body_bytes,omitempty"`
	IdleTimeoutSecs int   `yaml:"idle_timeout_secs,omitempty"`
}

var (
	envNameRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	mountRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// LoadMeta reads meta.yaml from path and validates it.
func LoadMeta(path string) (*Meta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read meta: %w", err)
	}
	var m Meta
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse meta: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks the invariants of a Meta value (including the sidecar block).
func (m *Meta) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("meta.yaml: name is required")
	}
	if m.Sidecar != nil {
		if err := m.Sidecar.Validate(m.Name); err != nil {
			return err
		}
	}
	return nil
}

// Validate enforces the sidecar-block schema.
func (s *SidecarMeta) Validate(skillName string) error {
	if len(s.Command) == 0 {
		return fmt.Errorf("sidecar.command is required")
	}
	if s.Mount != "" && !mountRE.MatchString(s.Mount) {
		return fmt.Errorf("sidecar.mount %q must match %s", s.Mount, mountRE.String())
	}
	for i, sec := range s.Secrets {
		if !envNameRE.MatchString(sec.Name) {
			return fmt.Errorf("sidecar.secrets[%d].name %q is not a valid env var name", i, sec.Name)
		}
		if sec.Pattern != "" {
			if _, err := regexp.Compile(sec.Pattern); err != nil {
				return fmt.Errorf("sidecar.secrets[%d].pattern is not a valid regex: %w", i, err)
			}
		}
	}
	for _, p := range s.EnvPassthrough {
		if !envNameRE.MatchString(p) {
			return fmt.Errorf("sidecar.env_passthrough entry %q is not a valid env var name", p)
		}
	}
	return nil
}

// MountOrDefault returns the routing prefix for this skill.
func (s *SidecarMeta) MountOrDefault(skillName string) string {
	if s.Mount != "" {
		return s.Mount
	}
	return skillName
}

// InstallScriptFor returns the script path for the given OS (possibly empty).
func (s *SidecarMeta) InstallScriptFor(o osinfo.OS) string {
	if s.InstallScripts == nil {
		return ""
	}
	return s.InstallScripts[string(o)]
}

// HashMetaFile returns the sha256 hex digest of the meta.yaml file at path.
// This is used to pin the registered state to specific metadata content.
func HashMetaFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

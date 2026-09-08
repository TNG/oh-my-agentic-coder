package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// LauncherConfig is the oh-my-agentic-coder.yaml file.
//
// Both `yaml:` and `json:` struct tags are kept on every field so the
// type stays compatible if a caller ever needs to dump the config back
// out as JSON (e.g. for diagnostics). YAML is the canonical wire
// format on disk; JSON tags exist for "free" compatibility because
// gopkg.in/yaml.v3 ignores them and encoding/json honors them.
type LauncherConfig struct {
	Sandbox SandboxConfig `yaml:"sandbox" json:"sandbox"`
	Facade  FacadeConfig  `yaml:"facade"  json:"facade"`
	Audit   AuditConfig   `yaml:"audit"   json:"audit"`
	Cache   CacheConfig   `yaml:"cache"   json:"cache"`
}

// CacheScope selects how the persistent tool cache is partitioned.
type CacheScope string

const (
	// CacheScopeGlobal shares one cache across every workdir and config.
	CacheScopeGlobal CacheScope = "global"
	// CacheScopeConfig shares one cache across all workdirs governed by the
	// same launcher config file (falls back to global when none is on disk).
	CacheScopeConfig CacheScope = "config"
	// CacheScopeWorkdir gives each workdir its own isolated cache.
	CacheScopeWorkdir CacheScope = "workdir"
)

// CacheConfig controls the tool cache scope (see internal/toolcache).
//
// Scope defaults to "global": every workdir shares one persistent cache. The
// zero value (unset in YAML) resolves to global via Resolve, so no default
// backfill is needed.
type CacheConfig struct {
	Scope CacheScope `yaml:"scope" json:"scope"`
}

// Resolve returns the effective scope, treating unset as global, and errors
// on an unrecognized value.
func (c CacheConfig) Resolve() (CacheScope, error) {
	if c.Scope == "" {
		return CacheScopeGlobal, nil
	}
	return ValidateCacheScope(string(c.Scope))
}

// ValidateCacheScope normalizes and validates a scope string (from config or
// the --cache-scope flag).
func ValidateCacheScope(s string) (CacheScope, error) {
	switch CacheScope(s) {
	case CacheScopeGlobal, CacheScopeConfig, CacheScopeWorkdir:
		return CacheScope(s), nil
	default:
		return "", fmt.Errorf("invalid cache scope %q (want global, config, or workdir)", s)
	}
}

// AuditConfig controls the security audit trail (see internal/audit).
//
// Enabled defaults to true. Because Go's zero value for a bool is false,
// the field is a *bool so "unset in YAML" (nil) can be distinguished from
// an explicit `enabled: false`; mergeDefaults fills nil with true.
type AuditConfig struct {
	Enabled *bool  `yaml:"enabled" json:"enabled"`
	Path    string `yaml:"path"    json:"path"`   // "" => audit.DefaultPath()
	Syslog  bool   `yaml:"syslog"  json:"syslog"` // mirror to system log (Unix)
	Strict  bool   `yaml:"strict"  json:"strict"` // fail-closed on write failure
}

// AuditEnabled reports whether auditing is on, treating unset as true.
func (a AuditConfig) AuditEnabled() bool { return a.Enabled == nil || *a.Enabled }

// SandboxConfig is the `sandbox` block of the launcher config.
type SandboxConfig struct {
	// DefaultProfile and Profiles are deprecated. omac always launches its
	// built-in sandbox, so neither does anything; they are parsed only so
	// validateSandbox can detect a legacy config and reject or warn on it.
	DefaultProfile string         `yaml:"default_profile" json:"default_profile"`
	Profiles       map[string]any `yaml:"profiles"        json:"profiles"`

	// ProfilePath overrides the built-in default policy profile. It is an
	// absolute path, or relative to the config layer that declared it (see
	// ResolveSandboxProfileRef). Empty uses the default.
	ProfilePath string `yaml:"profile_path" json:"profile_path"`

	// Briefing optionally overrides the embedded sandbox briefing text.
	// Empty/unset uses the compiled-in default (sandboxbrief.Default);
	// resolution happens at launch, not here.
	Briefing string `yaml:"briefing"        json:"briefing"`
}

// FacadeConfig tunes the reverse proxy.
type FacadeConfig struct {
	IdleTimeoutSecs    int      `yaml:"idle_timeout_secs"    json:"idle_timeout_secs"`
	MaxBodyBytes       int64    `yaml:"max_body_bytes"       json:"max_body_bytes"`
	BaseEnvPassthrough []string `yaml:"base_env_passthrough" json:"base_env_passthrough"`
}

// DefaultLauncherConfig returns the config that ships as the compiled-in
// default. It sets no sandbox block: omac always launches its built-in sandbox,
// and the inner command comes from the selected harness at launch.
func DefaultLauncherConfig() LauncherConfig {
	return LauncherConfig{
		Facade: FacadeConfig{
			IdleTimeoutSecs:    300,
			MaxBodyBytes:       10 * 1024 * 1024,
			BaseEnvPassthrough: []string{"PATH", "HOME", "USER", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR"},
		},
		Audit: AuditConfig{
			Enabled: boolPtr(true),
			Path:    "", // audit.DefaultPath() (persistent central location)
			Syslog:  false,
			Strict:  false,
		},
		Cache: CacheConfig{Scope: CacheScopeGlobal},
	}
}

func boolPtr(b bool) *bool { return &b }

// ProjectLauncherConfigPath returns the per-workdir launcher config path.
// LoadLauncher and ResolveSandboxProfileRef both use it so their notion of the
// project config stays identical.
func ProjectLauncherConfigPath(workdir string) string {
	return filepath.Join(workdir, ".opencode", "oh-my-agentic-coder.yaml")
}

// LoadLauncher loads the launcher config from
// <workdir>/.opencode/oh-my-agentic-coder.yaml or, failing that,
// $XDG_CONFIG_HOME/omac/config.yaml (~/.config/omac/config.yaml).
// If neither exists, the compiled-in default is returned.
//
// The config format is YAML (gopkg.in/yaml.v3). YAML is a strict
// superset of JSON, so existing JSON-shaped files continue to parse
// correctly — handy if a user has an inline `omac` config snippet
// they want to paste in. The .yaml extension is the canonical name.
func LoadLauncher(workdir string) (LauncherConfig, string, error) {
	candidates := []string{
		ProjectLauncherConfigPath(workdir),
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "omac", "config.yaml"))
	}
	for _, p := range candidates {
		raw, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return LauncherConfig{}, "", fmt.Errorf("read %s: %w", p, err)
		}
		var lc LauncherConfig
		if err := yaml.Unmarshal(raw, &lc); err != nil {
			return LauncherConfig{}, "", fmt.Errorf("parse %s: %w", p, err)
		}
		// Validate the raw sandbox block (before defaults are merged, so it
		// sees exactly what the user wrote). Fires for every command, so a
		// removed backend can't be masked by e.g. --no-sandbox.
		if err := validateSandbox(lc.Sandbox, p); err != nil {
			return LauncherConfig{}, "", err
		}
		lc = mergeDefaults(lc)
		if _, err := lc.Cache.Resolve(); err != nil {
			return LauncherConfig{}, "", fmt.Errorf("parse %s: %w", p, err)
		}
		return lc, p, nil
	}
	return DefaultLauncherConfig(), "", nil
}

func mergeDefaults(lc LauncherConfig) LauncherConfig {
	def := DefaultLauncherConfig()
	if lc.Facade.IdleTimeoutSecs == 0 {
		lc.Facade.IdleTimeoutSecs = def.Facade.IdleTimeoutSecs
	}
	if lc.Facade.MaxBodyBytes == 0 {
		lc.Facade.MaxBodyBytes = def.Facade.MaxBodyBytes
	}
	if lc.Facade.BaseEnvPassthrough == nil {
		lc.Facade.BaseEnvPassthrough = def.Facade.BaseEnvPassthrough
	}
	// Audit defaults on when the block is unset. An explicit
	// `enabled: false` is preserved (that's why Enabled is a *bool).
	if lc.Audit.Enabled == nil {
		lc.Audit.Enabled = def.Audit.Enabled
	}
	return lc
}

// ResolveSandboxProfileRef resolves sandbox.profile_path to an absolute path,
// or "" when unset (use the built-in default profile).
//
// cfgPath and workdir must be LoadLauncher's returned path and its workdir. A
// relative profile_path is anchored by config layer: the project root for a
// project config, the config directory for a global config. An absolute path
// is used verbatim. A missing file or a directory is an error, not a fall back
// to the default.
func (lc LauncherConfig) ResolveSandboxProfileRef(cfgPath, workdir string) (string, error) {
	raw := strings.TrimSpace(lc.Sandbox.ProfilePath)
	if raw == "" {
		return "", nil
	}
	abs := raw
	if !filepath.IsAbs(abs) {
		base, err := sandboxProfileRelBase(cfgPath, workdir)
		if err != nil {
			return "", err
		}
		abs = filepath.Join(base, abs)
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("sandbox.profile_path %q does not exist (resolved to %s)", raw, abs)
		}
		return "", fmt.Errorf("sandbox.profile_path %q (resolved to %s): %w", raw, abs, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("sandbox.profile_path %q is a directory, not a profile file (resolved to %s)", raw, abs)
	}
	return abs, nil
}

// sandboxProfileRelBase returns the directory a relative profile_path anchors
// on, chosen by config layer (see ResolveSandboxProfileRef). An empty cfgPath
// is a caller bug — a relative path has no anchor — so it errors rather than
// defaulting to cwd.
func sandboxProfileRelBase(cfgPath, workdir string) (string, error) {
	if cfgPath == "" {
		return "", fmt.Errorf("cannot resolve a relative sandbox.profile_path without a config file path")
	}
	if workdir != "" && cfgPath == ProjectLauncherConfigPath(workdir) {
		return workdir, nil
	}
	return filepath.Dir(cfgPath), nil
}

// DeprecationWarnings returns non-fatal notices for legacy sandbox settings
// that still parse but do nothing. Callers print them after a successful load.
func (sb SandboxConfig) DeprecationWarnings() []string {
	var warns []string
	if strings.TrimSpace(sb.DefaultProfile) != "" {
		warns = append(warns, "sandbox.default_profile is deprecated and ignored; "+
			"the built-in sandbox is always used — you can remove this line.")
	}
	return warns
}

// validateSandbox rejects a launcher config that selects a removed or unknown
// sandbox backend, with a migration hint. omac ships only the built-in
// sandbox, so `default_profile` must be "builtin" (or unset).
func validateSandbox(sb SandboxConfig, path string) error {
	switch sb.DefaultProfile {
	case "", "builtin":
		// The built-in sandbox: the only supported backend.
	case "nono", "nono-netprofile":
		return fmt.Errorf("%s: the %q sandbox has been removed; only the built-in sandbox remains.\n"+
			"  Set 'default_profile: builtin' (or delete the line — builtin is the default).\n"+
			"  See docs/configuration.md", path, sb.DefaultProfile)
	case "no-sandbox-debug":
		return fmt.Errorf("%s: the 'no-sandbox-debug' profile has been removed.\n"+
			"  For an unsandboxed shell, run: omac start --no-sandbox --inner bash\n"+
			"  Remove 'default_profile: no-sandbox-debug' from your config.\n"+
			"  See docs/configuration.md", path)
	default:
		return fmt.Errorf("%s: unknown sandbox profile %q; only \"builtin\" is supported.\n"+
			"  Set 'default_profile: builtin' (or delete the line).\n"+
			"  See docs/configuration.md", path, sb.DefaultProfile)
	}
	if len(sb.Profiles) > 0 {
		return fmt.Errorf("%s: custom sandbox launcher profiles are no longer supported.\n"+
			"  Remove the 'sandbox.profiles' block. For a custom sandbox policy set\n"+
			"  'sandbox.profile_path: <file>'; to run a non-native harness add '--inner <binary>'.\n"+
			"  See docs/configuration.md", path)
	}
	return nil
}

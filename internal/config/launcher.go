package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

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

// SandboxConfig declares named sandbox profiles.
type SandboxConfig struct {
	DefaultProfile string                    `yaml:"default_profile" json:"default_profile"`
	Profiles       map[string]SandboxProfile `yaml:"profiles"        json:"profiles"`

	// Briefing optionally overrides the embedded sandbox briefing text.
	// Empty/unset uses the compiled-in default (sandboxbrief.Default);
	// resolution happens at launch, not here.
	Briefing string `yaml:"briefing"        json:"briefing"`
}

// SandboxProfile is one entry under `sandbox.profiles` in the launcher
// config. omac launches only the built-in sandbox, and its command is
// assembled in Go by sandbox.BuildBuiltinArgv rather than from these fields.
// They are kept so existing config files still parse and so `omac doctor`
// can recognize the built-in profile.
type SandboxProfile struct {
	Command  []string `yaml:"command"   json:"command"`
	InnerCmd []string `yaml:"inner_cmd" json:"inner_cmd"`
}

// FacadeConfig tunes the reverse proxy.
type FacadeConfig struct {
	IdleTimeoutSecs    int      `yaml:"idle_timeout_secs"    json:"idle_timeout_secs"`
	MaxBodyBytes       int64    `yaml:"max_body_bytes"       json:"max_body_bytes"`
	BaseEnvPassthrough []string `yaml:"base_env_passthrough" json:"base_env_passthrough"`
}

// DefaultLauncherConfig returns a config that ships as the compiled-in default.
//
// The builtin sandbox profile deliberately ships with an EMPTY inner_cmd: the
// inner command is supplied by the selected harness (the positional
// `omac start <harness>` token; default opencode) via Harness.ResolveInnerCmd.
// This is what lets `omac start claude` actually run Claude Code without editing
// config. A user who pins a profile's inner_cmd in their own
// oh-my-agentic-coder.yaml still wins (that explicit value takes precedence over
// the harness default — see ResolveInnerCmd).
func DefaultLauncherConfig() LauncherConfig {
	return defaultLauncherConfigFor(DefaultHarness())
}

// defaultLauncherConfigFor builds the default launcher config. The harness
// argument is currently only used to keep the signature future-proof and to
// let tests assert harness-independence; the builtin profile intentionally
// leaves inner_cmd empty so the harness fills it at launch. The sandbox
// *command* templates are harness-independent (they only reference
// {{inner_cmd}} / {{inner_args}} placeholders).
func defaultLauncherConfigFor(h Harness) LauncherConfig {
	_ = h // inner_cmd is supplied by the harness at resolve time, not baked here
	return LauncherConfig{
		Sandbox: SandboxConfig{
			DefaultProfile: "builtin",
			Profiles: map[string]SandboxProfile{
				// builtin re-execs the running omac binary as
				// `omac sandbox run` — omac's native OS sandbox
				// (Seatbelt on macOS, bubblewrap+Landlock on Linux).
				// Flag semantics:
				//
				//   --allow-file <socket>   AF_UNIX bridge socket (the
				//                           generated Seatbelt profile
				//                           allows connect explicitly,
				//                           so this works on macOS even
				//                           under the network deny)
				//   --read <socket-dir>     path-component lookup
				//   {{tmpdir_flags}}        rw on the TMPDIR temp dir
				//   --open-port <tcp-port>  loopback facade transport
				//
				// The sandbox profile itself (fs grants, listen_port,
				// allow_tcp_connect, network prompt) is resolved by
				// `omac sandbox run --profile default`: user override at
				// ~/.config/omac/profiles/default.json, else compiled-in
				// defaults. The compiled-in default profile intentionally
				// does NOT broad-grant the host cache roots (~/.cache,
				// ~/Library/Caches) or the whole tool homes (~/go,
				// ~/.cargo, ~/.rustup). Only the toolchain bin leaves
				// (~/.cargo/bin, ~/.rustup, ~/go/bin, ~/.nvm, ~/.bun/bin)
				// are read-only; the selected tool-cache scope leaf
				// (~/.cache/omac/<sha256(scope)>) is granted rw at launch
				// via --allow (see internal/toolcache and
				// internal/cli/start.go's prepareLaunchCache).
				"builtin": {
					Command: []string{
						"{{self}}", "sandbox", "run",
						"--profile", "default",
						"--allow-file", "{{socket}}",
						"--read", "{{socket_dir}}",
						"{{tmpdir_flags}}",
						"--open-port", "{{tcp_port}}",
						"--",
						"{{inner_cmd}}", "{{inner_args}}",
					},
					// Empty: filled by the selected harness at launch.
					InnerCmd: nil,
				},
			},
		},
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
		filepath.Join(workdir, ".opencode", "oh-my-agentic-coder.yaml"),
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
	if lc.Sandbox.DefaultProfile == "" {
		lc.Sandbox.DefaultProfile = def.Sandbox.DefaultProfile
	}
	if lc.Sandbox.Profiles == nil {
		lc.Sandbox.Profiles = def.Sandbox.Profiles
	}
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
			"  See docs/configuration.md.", path, sb.DefaultProfile)
	case "no-sandbox-debug":
		return fmt.Errorf("%s: the 'no-sandbox-debug' profile has been removed.\n"+
			"  For an unsandboxed shell, run: omac start --no-sandbox --inner bash\n"+
			"  Remove 'default_profile: no-sandbox-debug' from your config.\n"+
			"  See docs/configuration.md.", path)
	default:
		return fmt.Errorf("%s: unknown sandbox profile %q; only \"builtin\" is supported.\n"+
			"  Set 'default_profile: builtin' (or delete the line).\n"+
			"  See docs/configuration.md.", path, sb.DefaultProfile)
	}
	if len(sb.Profiles) > 0 {
		return fmt.Errorf("%s: custom sandbox launcher profiles are no longer supported; only the built-in sandbox is available.\n"+
			"  Remove the 'sandbox.profiles' block from your config.\n"+
			"  See docs/configuration.md.", path)
	}
	return nil
}

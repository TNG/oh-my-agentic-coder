package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"gopkg.in/yaml.v3"
)

// LauncherConfig is the config.yaml launcher file (global ~/.config/omac/ or
// project-local <workdir>/.omac/).
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
// Scope defaults to "workdir": each workdir gets its own isolated cache,
// preventing one sandboxed session from poisoning another's cached artifacts.
// Set to "global" or "config" explicitly to share caches across workdirs.
type CacheConfig struct {
	Scope CacheScope `yaml:"scope" json:"scope"`
}

// Resolve returns the effective scope, treating unset as workdir, and errors
// on an unrecognized value.
func (c CacheConfig) Resolve() (CacheScope, error) {
	if c.Scope == "" {
		return CacheScopeWorkdir, nil
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
	// DefaultProfile and Profiles are the 0.9.0 launcher-template settings.
	// omac always launches its built-in sandbox, so neither has any effect;
	// they are parsed only as presence sentinels so validateSandbox can
	// reject any config that still carries them with a migration hint. A nil
	// DefaultProfile means the key was absent; any present value (including
	// "builtin" or "") is rejected.
	DefaultProfile *string        `yaml:"default_profile" json:"default_profile"`
	Profiles       map[string]any `yaml:"profiles"        json:"profiles"`

	// ProfileName selects the sandbox grants profile by name. The name is
	// resolved inside the directory of the launcher config that declared it:
	// the project's <workdir>/.omac/ or the user-global
	// ~/.config/omac/sandbox-profiles/. It never crosses layers. Empty means
	// "default.json of this layer if it exists, else the other layer, else
	// the compiled-in default".
	ProfileName string `yaml:"profile_name" json:"profile_name"`

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
		Cache: CacheConfig{Scope: CacheScopeWorkdir},
	}
}

func boolPtr(b bool) *bool { return &b }

// LocalConfigDir returns the project-local omac config directory
// (<workdir>/.omac). It is created at launch and masked for the agent, so the
// files in it are the sandbox definition and can never be written by a session.
func LocalConfigDir(workdir string) string {
	return filepath.Join(workdir, ".omac")
}

// ProjectLauncherConfigPath returns the per-workdir launcher config path.
func ProjectLauncherConfigPath(workdir string) string {
	return filepath.Join(LocalConfigDir(workdir), "config.yaml")
}

// GlobalLauncherConfigPath returns the user-global launcher config path.
func GlobalLauncherConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "omac", "config.yaml")
}

// legacyProjectConfigPaths are the 0.9.0 locations for project-local config.
// They are no longer read; callers surface a migration warning when one exists.
func legacyProjectConfigPaths(workdir string) []string {
	return []string{
		filepath.Join(workdir, ".opencode", "oh-my-agentic-coder.yaml"),
		filepath.Join(workdir, ".opencode", "sandbox.json"),
	}
}

// LoadLauncher loads the launcher config for workdir.
//
// It reads both the project-local config (<workdir>/.omac/config.yaml) and the
// user-global config (~/.config/omac/config.yaml) when both exist. Operational
// settings (facade timeouts/body limit, cache scope) layer local over global.
// Security-sensitive settings (audit, facade env passthrough, the sandbox
// briefing) come exclusively from the global config or compiled-in defaults.
// sandbox.profile_name is the one sandbox field a project may set, and it
// resolves only within the local .omac/ directory (see ResolveSandboxProfile).
//
// The returned path is the local file when it exists (its operational settings
// are applied), the global file when only that exists, or "" when neither
// exists (compiled-in defaults are used).
func LoadLauncher(workdir string) (LauncherConfig, string, error) {
	global, globalPath, err := loadLauncherFile(GlobalLauncherConfigPath())
	if err != nil {
		return LauncherConfig{}, "", err
	}
	local, localPath, err := loadLauncherFile(ProjectLauncherConfigPath(workdir))
	if err != nil {
		return LauncherConfig{}, "", err
	}

	// Validate the raw sandbox block of both layers before defaults are
	// merged. This turns 0.9.0's launcher-template settings into an
	// actionable error instead of a silent ignore.
	if err := validateSandbox(global.Sandbox, globalPath, globalProfileDir(), false); err != nil {
		return LauncherConfig{}, "", err
	}
	if err := validateSandbox(local.Sandbox, localPath, LocalConfigDir(workdir), true); err != nil {
		return LauncherConfig{}, "", err
	}

	switch {
	case localPath != "" && globalPath != "":
		// Global security fields, local operational settings on top.
		merged := mergeDefaults(global)
		if n := strings.TrimSpace(local.Sandbox.ProfileName); n != "" {
			merged.Sandbox.ProfileName = n
		}
		merged.Facade.IdleTimeoutSecs = pickInt(local.Facade.IdleTimeoutSecs, merged.Facade.IdleTimeoutSecs)
		merged.Facade.MaxBodyBytes = pickInt64(local.Facade.MaxBodyBytes, merged.Facade.MaxBodyBytes)
		// Only override the global cache scope when the project actually sets
		// one; otherwise a project config that exists only for profile_name
		// would silently reset a global scope to the workdir default.
		if local.Cache.Scope != "" {
			merged.Cache = local.Cache
		}
		if _, err := merged.Cache.Resolve(); err != nil {
			return LauncherConfig{}, "", fmt.Errorf("parse %s: %w", localPath, err)
		}
		return merged, localPath, nil
	case localPath != "":
		lc := stripSecurityFields(local)
		lc.Sandbox.ProfileName = strings.TrimSpace(local.Sandbox.ProfileName)
		lc = mergeDefaults(lc)
		if _, err := lc.Cache.Resolve(); err != nil {
			return LauncherConfig{}, "", fmt.Errorf("parse %s: %w", localPath, err)
		}
		return lc, localPath, nil
	case globalPath != "":
		lc := mergeDefaults(global)
		if _, err := lc.Cache.Resolve(); err != nil {
			return LauncherConfig{}, "", fmt.Errorf("parse %s: %w", globalPath, err)
		}
		return lc, globalPath, nil
	default:
		return DefaultLauncherConfig(), "", nil
	}
}

// loadLauncherFile reads and unmarshals one launcher config file.
// Returns an empty config and "" path when the file does not exist.
func loadLauncherFile(path string) (LauncherConfig, string, error) {
	if path == "" {
		return LauncherConfig{}, "", nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return LauncherConfig{}, "", nil
	}
	if err != nil {
		return LauncherConfig{}, "", fmt.Errorf("read %s: %w", path, err)
	}
	var lc LauncherConfig
	if err := yaml.Unmarshal(raw, &lc); err != nil {
		return LauncherConfig{}, "", fmt.Errorf("parse %s: %w", path, err)
	}
	return lc, path, nil
}

// stripSecurityFields zeroes all fields a workdir config must not control:
// sandbox briefing, audit settings, and facade env passthrough. Only
// operational settings (facade timeouts, cache) and sandbox.profile_name
// survive; the caller re-applies profile_name.
func stripSecurityFields(lc LauncherConfig) LauncherConfig {
	lc.Sandbox = SandboxConfig{}
	lc.Audit = AuditConfig{}
	lc.Facade.BaseEnvPassthrough = nil
	return lc
}

// pickInt returns override when non-zero, else fallback.
func pickInt(override, fallback int) int {
	if override != 0 {
		return override
	}
	return fallback
}

// pickInt64 returns override when non-zero, else fallback.
func pickInt64(override, fallback int64) int64 {
	if override != 0 {
		return override
	}
	return fallback
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

// ProfileSelection is the sandbox grants profile a launch should enforce.
type ProfileSelection struct {
	// Path is the profile file to load; "" means the compiled-in default.
	Path string
	// Name is the profile name ("default" for the implicit default).
	Name string
	// Layer is where the selection came from: "workdir", "global", or
	// "builtin".
	Layer string
}

// ResolveSandboxProfile selects the sandbox grants profile for workdir.
//
// The project-local .omac layer wins over the user-global layer, and the
// layers never mix:
//
//  1. .omac/config.yaml sandbox.profile_name -> .omac/<name>.json
//  2. .omac/default.json, if it exists
//  3. global config.yaml sandbox.profile_name -> sandbox-profiles/<name>.json
//  4. global sandbox-profiles/default.json
//  5. compiled-in default
//
// A named profile that does not exist is an error rather than a silent fall
// back, so a typo cannot quietly downgrade the grants.
func ResolveSandboxProfile(workdir string) (ProfileSelection, error) {
	var localDir, localCfgFile string
	if workdir != "" {
		localDir = LocalConfigDir(workdir)
		localCfgFile = filepath.Join(localDir, "config.yaml")
	}
	localCfg, localPath, err := loadLauncherFile(localCfgFile)
	if err != nil {
		return ProfileSelection{}, err
	}
	globalCfg, globalPath, err := loadLauncherFile(GlobalLauncherConfigPath())
	if err != nil {
		return ProfileSelection{}, err
	}
	if err := validateSandbox(localCfg.Sandbox, localPath, localDir, true); err != nil {
		return ProfileSelection{}, err
	}
	if err := validateSandbox(globalCfg.Sandbox, globalPath, globalProfileDir(), false); err != nil {
		return ProfileSelection{}, err
	}

	if name := strings.TrimSpace(localCfg.Sandbox.ProfileName); name != "" && localDir != "" {
		return namedProfileSelection(localDir, name, "workdir")
	}
	if localDir != "" {
		if sel, ok, err := defaultProfileSelection(localDir, "workdir"); err != nil {
			return ProfileSelection{}, err
		} else if ok {
			return sel, nil
		}
	}

	globalDir, err := sandboxprofile.ProfileDir()
	if err != nil {
		return ProfileSelection{}, err
	}
	if name := strings.TrimSpace(globalCfg.Sandbox.ProfileName); name != "" {
		return namedProfileSelection(globalDir, name, "global")
	}
	if sel, ok, err := defaultProfileSelection(globalDir, "global"); err != nil {
		return ProfileSelection{}, err
	} else if ok {
		return sel, nil
	}
	return ProfileSelection{Path: "", Name: "default", Layer: "builtin"}, nil
}

// defaultProfileSelection returns the layer's default.json when a regular file
// (not a symlink or directory) exists there. A symlinked default is rejected
// like a named profile, so the auto-selected path cannot bypass the
// write-protection guarantee.
func defaultProfileSelection(dir, layer string) (ProfileSelection, bool, error) {
	p := filepath.Join(dir, "default.json")
	if _, err := os.Lstat(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ProfileSelection{}, false, nil
		}
		// A permission or I/O error must not silently downgrade the selection
		// to the next layer or the built-in default.
		return ProfileSelection{}, false, fmt.Errorf("stat %s: %w", p, err)
	}
	if err := checkProfileFile(p, "sandbox profile default"); err != nil {
		return ProfileSelection{}, false, err
	}
	return ProfileSelection{Path: p, Name: "default", Layer: layer}, true, nil
}

// ExplicitProfileSelection validates an explicit --profile-path and returns the
// selection. The path must resolve inside the global sandbox-profiles/
// directory or the project's .omac/ directory; anything else is rejected so a
// CLI argument (or a wrapper script) cannot point the launch at an arbitrary
// host file.
func ExplicitProfileSelection(workdir, path string) (ProfileSelection, error) {
	raw := strings.TrimSpace(path)
	if raw == "" {
		return ProfileSelection{}, fmt.Errorf("--profile-path requires a value")
	}
	var abs string
	if filepath.IsAbs(raw) {
		abs = filepath.Clean(raw)
	} else {
		// A relative --profile-path is anchored to --workdir, not the
		// process CWD, so it means the same thing as the docs say: a file
		// inside <workdir>/.omac/.
		if workdir == "" {
			return ProfileSelection{}, fmt.Errorf("a relative --profile-path needs --workdir")
		}
		abs = filepath.Clean(filepath.Join(workdir, raw))
	}
	globalDir, err := sandboxprofile.ProfileDir()
	if err != nil {
		return ProfileSelection{}, err
	}
	localDir := LocalConfigDir(workdir)
	layer := ""
	switch {
	case withinDir(globalDir, abs):
		layer = "global"
	case workdir != "" && withinDir(localDir, abs):
		layer = "workdir"
	default:
		return ProfileSelection{}, fmt.Errorf("--profile-path %q is outside %s and %s; "+
			"a profile must live in the global sandbox-profiles directory or the project's .omac directory",
			path, globalDir, localDir)
	}
	if err := checkProfileFile(abs, "--profile-path "+path); err != nil {
		return ProfileSelection{}, err
	}
	return ProfileSelection{Path: abs, Name: strings.TrimSuffix(filepath.Base(abs), ".json"), Layer: layer}, nil
}

// namedProfileSelection resolves a bare profile name inside dir.
func namedProfileSelection(dir, name, layer string) (ProfileSelection, error) {
	if err := validateProfileName(name); err != nil {
		return ProfileSelection{}, err
	}
	name = strings.TrimSuffix(name, ".json")
	abs := filepath.Join(dir, name+".json")
	if !fileExists(abs) {
		return ProfileSelection{}, fmt.Errorf("sandbox profile %q not found (expected %s)", name, abs)
	}
	if err := checkProfileFile(abs, "sandbox profile "+name); err != nil {
		return ProfileSelection{}, err
	}
	return ProfileSelection{Path: abs, Name: name, Layer: layer}, nil
}

// validateProfileName rejects names that could escape the layer directory.
func validateProfileName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid sandbox profile name %q", name)
	}
	return nil
}

// checkProfileFile rejects a symlinked, missing, or directory profile.
func checkProfileFile(path, label string) error {
	if li, err := os.Lstat(path); err == nil && li.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink (%s); a symlinked profile cannot be write-protected "+
			"inside the sandbox — a session could replace it and steer the next launch", label, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s does not exist (%s)", label, path)
		}
		return fmt.Errorf("%s (%s): %w", label, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory, not a profile file (%s)", label, path)
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// withinDir reports whether path equals dir or lies under it, lexically.
func withinDir(dir, path string) bool {
	dir = filepath.Clean(dir)
	path = filepath.Clean(path)
	if path == dir {
		return true
	}
	return strings.HasPrefix(path+string(os.PathSeparator), dir+string(os.PathSeparator))
}

// globalProfileDir returns the user-global sandbox-profiles directory, or ""
// when the home directory is unavailable (the migration hints degrade to the
// short form).
func globalProfileDir() string {
	dir, err := sandboxprofile.ProfileDir()
	if err != nil {
		return ""
	}
	return dir
}

// LegacyProjectConfigWarnings returns migration notices for 0.9.0 project
// config locations that are no longer read. Callers print them after a load.
func LegacyProjectConfigWarnings(workdir string) []string {
	var warns []string
	for _, p := range legacyProjectConfigPaths(workdir) {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		rel, rerr := filepath.Rel(workdir, p)
		if rerr != nil {
			rel = p
		}
		if strings.HasSuffix(p, ".json") {
			warns = append(warns, fmt.Sprintf("project sandbox profile %s is no longer read; project "+
				"profiles now live in .omac/. Move it to be selected automatically, or name it and "+
				"set sandbox.profile_name:\n    mkdir -p .omac && mv %s .omac/default.json",
				rel, rel))
			continue
		}
		warns = append(warns, fmt.Sprintf("project config %s is no longer read; project omac config "+
			"now lives in .omac/. Move it with:\n    mkdir -p .omac && mv %s .omac/config.yaml",
			rel, rel))
	}
	return warns
}

// maxShownProfileNames caps how many profile names a migration error lists.
// # PONYTAIL: fixed cap; make the cap configurable if profiles grow past it.
const maxShownProfileNames = 5

// listProfileNames returns the selectable profile names in dir (a layer's
// profile directory): regular non-symlink *.json files, excluding pages
// siblings and the implicit default. nil when dir is "" or unreadable. Profiles
// are selected by file name only, so meta.name is irrelevant here.
func listProfileNames(dir string) []string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".pages.json") {
			continue
		}
		if li, err := e.Info(); err != nil || !li.Mode().IsRegular() {
			continue
		}
		if n := strings.TrimSuffix(name, ".json"); n != "" && n != "default" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// joinProfileNames renders names for a message, capping the display.
func joinProfileNames(names []string) string {
	if len(names) <= maxShownProfileNames {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:maxShownProfileNames], ", "), len(names)-maxShownProfileNames)
}

// validateSandbox rejects a launcher config that still carries the 0.9.0
// launcher-template settings. omac always launches its built-in sandbox, so
// `default_profile` and `profiles` have no effect; any presence is a hard
// error. profileDir is the layer's profile directory, enumerated so the hint
// can offer the profiles the user actually has ("" disables the list). local
// selects the location the grants pointer names: the project's .omac/ for a
// project config, the global sandbox-profiles/ for a global one.
func validateSandbox(sb SandboxConfig, path string, profileDir string, local bool) error {
	grantsHint := "set 'sandbox.profile_name: <name>' and put the profile at " +
		"~/.config/omac/sandbox-profiles/<name>.json"
	if local {
		grantsHint = "set 'sandbox.profile_name: <name>' and put the profile at " +
			"<workdir>/.omac/<name>.json"
	}
	if sb.DefaultProfile != nil {
		v := *sb.DefaultProfile
		names := listProfileNames(profileDir)
		if len(names) == 0 {
			return defaultProfileRemovedError(path, v, grantsHint)
		}
		match := slices.Contains(names, v)
		var b strings.Builder
		fmt.Fprintf(&b, "%s: sandbox.default_profile: %q is no longer supported.\n", path, v)
		fmt.Fprintf(&b, "  Profiles defined for this layer: %s\n", joinProfileNames(names))
		if match {
			b.WriteString("  - Keep using your previous one by renaming the field:\n")
			fmt.Fprintf(&b, "        sandbox:\n          profile_name: %q\n", v)
			var others []string
			for _, n := range names {
				if n != v {
					others = append(others, n)
				}
			}
			if len(others) > 0 {
				fmt.Fprintf(&b, "  - Or use one of the other profiles defined for this layer: %s\n", joinProfileNames(others))
			}
		} else {
			fmt.Fprintf(&b, "  - Or use one of the profiles defined for this layer: %s\n", joinProfileNames(names))
		}
		b.WriteString("  - Or remove the line to fall back to this layer's default.json.\n")
		b.WriteString("  See docs/configuration.md")
		if v == "no-sandbox-debug" {
			b.WriteString("\n  For an unsandboxed shell, run: omac start --no-sandbox --inner bash")
		}
		return errors.New(b.String())
	}
	if sb.Profiles != nil {
		return fmt.Errorf("%s: sandbox.profiles (the 0.9.0 launcher argv templates) is no longer "+
			"supported; remove the block.\n"+
			"  omac always runs its built-in sandbox. To choose sandbox grants, %s.\n"+
			"  See docs/configuration.md", path, grantsHint)
	}
	return nil
}

// defaultProfileRemovedError is the short hint for a present default_profile
// when the declaring layer defines no further profiles.
func defaultProfileRemovedError(path, value, grantsHint string) error {
	msg := fmt.Sprintf("%s: sandbox.default_profile: %q is no longer supported; remove the line.\n"+
		"  omac always runs its built-in sandbox. To choose sandbox grants, %s.\n"+
		"  See docs/configuration.md", path, value, grantsHint)
	if value == "no-sandbox-debug" {
		msg += "\n  For an unsandboxed shell, run: omac start --no-sandbox --inner bash"
	}
	return errors.New(msg)
}

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
// (<workdir>/.omac). Inside a session it is masked, so the files in it carry
// the sandbox definition, and the macOS backend cannot block replacing the
// directory itself in the writable workdir, so what a launch may trust is
// what the host-side approval pins (config.ProjectSandboxTrust) recorded —
// this path is the key into that pin store.
func LocalConfigDir(workdir string) string {
	return filepath.Join(workdir, sandboxprofile.ProjectConfigDirName)
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

// launcherLayers is the loaded pair of launcher config files for workdir.
type launcherLayers struct {
	global     LauncherConfig
	globalPath string
	local      LauncherConfig
	localPath  string
}

// readLauncherLayers loads both launcher config layers and validates each
// layer's sandbox block and cache scope before anything is merged. The
// 0.9.0 launcher-template settings are rejected here, whatever the caller
// does with the rest, and per-layer validation makes an invalid cache scope
// name the file that set it.
func readLauncherLayers(workdir string) (launcherLayers, error) {
	var l launcherLayers
	var err error
	if l.global, l.globalPath, err = loadLauncherFile(GlobalLauncherConfigPath()); err != nil {
		return l, err
	}
	if l.local, l.localPath, err = loadLauncherFile(ProjectLauncherConfigPath(workdir)); err != nil {
		return l, err
	}
	if l.globalPath != "" {
		if err := validateSandbox(l.global.Sandbox, l.globalPath, globalProfileDir(), false); err != nil {
			return l, err
		}
		if l.global.Cache.Scope != "" {
			if _, cerr := ValidateCacheScope(string(l.global.Cache.Scope)); cerr != nil {
				return l, fmt.Errorf("parse %s: %w", l.globalPath, cerr)
			}
		}
	}
	if l.localPath != "" {
		if err := validateSandbox(l.local.Sandbox, l.localPath, LocalConfigDir(workdir), true); err != nil {
			return l, err
		}
		if l.local.Cache.Scope != "" {
			if _, cerr := ValidateCacheScope(string(l.local.Cache.Scope)); cerr != nil {
				return l, fmt.Errorf("parse %s: %w", l.localPath, cerr)
			}
		}
	}
	return l, nil
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
	layers, err := readLauncherLayers(workdir)
	if err != nil {
		return LauncherConfig{}, "", err
	}

	switch {
	case layers.localPath != "" && layers.globalPath != "":
		// Global security fields, local operational settings on top.
		merged := mergeDefaults(layers.global)
		merged.Facade.IdleTimeoutSecs = pickInt(layers.local.Facade.IdleTimeoutSecs, merged.Facade.IdleTimeoutSecs)
		merged.Facade.MaxBodyBytes = pickInt64(layers.local.Facade.MaxBodyBytes, merged.Facade.MaxBodyBytes)
		// Only override the global cache scope when the project actually sets
		// one; otherwise a project config that exists only for profile_name
		// would silently reset a global scope to the workdir default.
		if layers.local.Cache.Scope != "" {
			merged.Cache = layers.local.Cache
		}
		return merged, layers.localPath, nil
	case layers.localPath != "":
		return mergeDefaults(stripSecurityFields(layers.local)), layers.localPath, nil
	case layers.globalPath != "":
		return mergeDefaults(layers.global), layers.globalPath, nil
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
// the whole sandbox block, audit settings, and facade env passthrough. Only
// operational settings (facade timeouts, cache) survive.
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
	layers, err := readLauncherLayers(workdir)
	if err != nil {
		return ProfileSelection{}, err
	}
	var localDir string
	if workdir != "" {
		localDir = LocalConfigDir(workdir)
	}

	if name := strings.TrimSpace(layers.local.Sandbox.ProfileName); name != "" && localDir != "" {
		return namedProfileSelection(localDir, name, "workdir")
	}
	if localDir != "" {
		if sel, ok, err := defaultProfileSelection(localDir, "workdir"); err != nil {
			return ProfileSelection{}, err
		} else if ok {
			return sel, nil
		}
	}

	return globalSandboxProfile(layers.global)
}

// globalSandboxProfile resolves the global selection from an already-loaded
// global config: profile_name, else sandbox-profiles/default.json, else builtin.
func globalSandboxProfile(globalCfg LauncherConfig) (ProfileSelection, error) {
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
// like a named profile: an auto-selected path must not escape the layer whose
// content the pin store covers.
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
	if err := sandboxprofile.CheckProfileFile(p, "sandbox profile default"); err != nil {
		return ProfileSelection{}, false, err
	}
	return ProfileSelection{Path: p, Name: "default", Layer: layer}, true, nil
}

// ExplicitProfileSelection validates an explicit --profile-path and returns the
// selection. The path must resolve (symlinks followed) inside the global
// sandbox-profiles/ directory or the project's .omac/ directory; anything else
// is rejected so a CLI argument (or a wrapper script) cannot point the launch
// at an arbitrary host file. The returned Path is the fully resolved location,
// so what the launch reads is the file that was validated, whatever symlinked
// parents are between it and the layer directory.
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
	if err := sandboxprofile.CheckProfileFile(abs, "--profile-path "+path); err != nil {
		return ProfileSelection{}, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return ProfileSelection{}, fmt.Errorf("--profile-path %q: %w", path, err)
	}
	globalDir, err := sandboxprofile.ProfileDir()
	if err != nil {
		return ProfileSelection{}, err
	}
	localDir := LocalConfigDir(workdir)
	layer := ""
	switch {
	case sandboxprofile.DirContainsPath(globalDir, resolved):
		layer = "global"
	case workdir != "" && sandboxprofile.DirContainsPath(localDir, resolved):
		layer = "workdir"
	default:
		return ProfileSelection{}, fmt.Errorf("--profile-path %q resolves outside %s and %s; "+
			"a profile must live in the global sandbox-profiles directory or the project's .omac directory "+
			"to be protected from agentic edits",
			path, globalDir, localDir)
	}
	return ProfileSelection{Path: resolved, Name: strings.TrimSuffix(filepath.Base(resolved), ".json"), Layer: layer}, nil
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
	if err := sandboxprofile.CheckProfileFile(abs, "sandbox profile "+name); err != nil {
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

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
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
		if n := strings.TrimSuffix(name, ".json"); n != "" {
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
			"  For fixed launch environment variables (previously set by a profile's argv template), "+
			"use \"environment.set\" in the sandbox grants profile.\n"+
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

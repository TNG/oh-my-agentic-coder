package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LauncherConfig is the oh-my-agentic-coder.json file.
type LauncherConfig struct {
	Sandbox SandboxConfig `json:"sandbox"`
	Facade  FacadeConfig  `json:"facade"`
}

// SandboxConfig declares named sandbox profiles.
type SandboxConfig struct {
	DefaultProfile string                    `json:"default_profile"`
	Profiles       map[string]SandboxProfile `json:"profiles"`
}

// SandboxProfile describes how to launch the sandbox for a given runtime.
type SandboxProfile struct {
	// Command is a templated argv. Supported placeholders:
	//   {{socket}}, {{socket_dir}}, {{inner_cmd}}, {{inner_args}},
	//   {{skills_csv}}, {{per_skill_env_flags}}, {{workdir}}
	// Tokens that expand to multiple argv entries (inner_args,
	// per_skill_env_flags) must stand alone in their slot.
	Command  []string `json:"command"`
	InnerCmd []string `json:"inner_cmd"`
}

// FacadeConfig tunes the reverse proxy.
type FacadeConfig struct {
	IdleTimeoutSecs    int      `json:"idle_timeout_secs"`
	MaxBodyBytes       int64    `json:"max_body_bytes"`
	BaseEnvPassthrough []string `json:"base_env_passthrough"`
}

// DefaultLauncherConfig returns a config that ships as the compiled-in default.
// It matches the existing tng-opencode invocation at the repo root.
func DefaultLauncherConfig() LauncherConfig {
	return LauncherConfig{
		Sandbox: SandboxConfig{
			DefaultProfile: "nono",
			Profiles: map[string]SandboxProfile{
				"nono": {
					// Reference invocation for nono (https://nono.sh).
					//
					// Why each flag is here:
					//
					//  --allow-file <socket>
					//      Grants the sandbox `open(2)` on the bridge socket
					//      inode. On macOS, Seatbelt classifies `connect(2)`
					//      on a Unix socket as `network-outbound`, NOT a file
					//      operation; this flag therefore covers only half of
					//      what is needed (see --override-deny below).
					//      On Linux (Landlock) AF_UNIX is governed entirely
					//      by filesystem ACLs, so this flag alone is
					//      sufficient.
					//
					//  --read <socket-dir>
					//      Component-wise path resolution during connect(2)
					//      walks the parent dir's stat. $TMPDIR/omac-<hash>/
					//      is not part of the system_read_macos group, so we
					//      grant it explicitly.
					//
					//  --override-deny <socket>
					//      MUST be present whenever the active nono profile
					//      activates proxy mode (i.e. defines
					//      custom_credentials, sets network_profile, or uses
					//      --allow-domain / --credential / --upstream-proxy).
					//      Proxy mode installs `(deny network*)` on macOS,
					//      which blocks `network-outbound` to the Unix
					//      socket — even though --allow-file granted
					//      filesystem access. --override-deny lifts the
					//      deny for this specific path AND requires a
					//      matching --allow-file (which we have above).
					//      Harmless when proxy mode is not active.
					//
					// Env-var injection: nono no longer accepts a literal
					// `--env KEY=VAL` flag. Instead sandbox.Exec sets
					// OMAC_SOCKET / OMAC_SKILLS / OMAC_<SKILL>_BASE in
					// nono's own process environment, and nono propagates
					// the parent env to the inner process by default. If
					// you author a custom nono profile with
					// environment.allow_vars set, add OMAC_* to the list.
					//
					// IMPORTANT: this profile does NOT use --block-net.
					// On macOS that installs `(deny network*)` and there
					// is currently no documented way to punch a Unix-socket
					// hole through it. Use --network-profile (handled by
					// our nono-netprofile variant) instead.
					Command: []string{
						"nono", "run",
						"--allow-cwd",
						"--profile", "tng-sandbox",
						"--allow-file", "{{socket}}",
						"--override-deny", "{{socket}}",
						"--read", "{{socket_dir}}",
						"--",
						"{{inner_cmd}}", "{{inner_args}}",
					},
					InnerCmd: []string{"opencode"},
				},
				// Same as above but adds --network-profile opencode so
				// outbound HTTP goes through nono's credential-injection
				// proxy. --override-deny is required here because
				// network-profile activates proxy mode (see notes above).
				"nono-netprofile": {
					Command: []string{
						"nono", "run",
						"--allow-cwd",
						"--profile", "tng-sandbox",
						"--network-profile", "opencode",
						"--allow-file", "{{socket}}",
						"--override-deny", "{{socket}}",
						"--read", "{{socket_dir}}",
						"--",
						"{{inner_cmd}}", "{{inner_args}}",
					},
					InnerCmd: []string{"opencode"},
				},
				"no-sandbox-debug": {
					Command:  []string{"{{inner_cmd}}", "{{inner_args}}"},
					InnerCmd: []string{"bash"},
				},
			},
		},
		Facade: FacadeConfig{
			IdleTimeoutSecs:    300,
			MaxBodyBytes:       10 * 1024 * 1024,
			BaseEnvPassthrough: []string{"PATH", "HOME", "USER", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR"},
		},
	}
}

// LoadLauncher loads the launcher config from workdir/.opencode/oh-my-agentic-coder.json
// or, failing that, $XDG_CONFIG_HOME/omac/config.json (~/.config/omac/config.json).
// If neither exists, the compiled-in default is returned.
func LoadLauncher(workdir string) (LauncherConfig, string, error) {
	candidates := []string{
		filepath.Join(workdir, ".opencode", "oh-my-agentic-coder.json"),
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "omac", "config.json"))
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
		if err := json.Unmarshal(raw, &lc); err != nil {
			return LauncherConfig{}, "", fmt.Errorf("parse %s: %w", p, err)
		}
		lc = mergeDefaults(lc)
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
	return lc
}

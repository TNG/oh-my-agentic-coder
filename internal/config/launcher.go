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
					// The --allow-file entry grants access to the bridge socket's
					// inode. On macOS, Seatbelt classifies connect(2) to a Unix
					// socket as `network-outbound`; since nono's default network
					// policy is allow, no extra network flag is needed. On Linux
					// (Landlock), Unix sockets are governed by filesystem
					// capabilities alone, so --allow-file is sufficient there too.
					//
					// We also --read the containing directory so component-wise
					// path lookup succeeds without depending on the
					// system_read_macos group covering $TMPDIR/omac-* specifically.
					//
					// IMPORTANT: this profile does NOT use --block-net. Combining
					// --block-net with a Unix-socket facade requires additional
					// configuration (see README "Running under nono"). If you need
					// outbound network filtering, use --network-profile instead;
					// that does not block Unix-socket connect on either platform.
					Command: []string{
						"nono", "run",
						"--allow-cwd",
						"--profile", "tng-sandbox",
						"--allow-file", "{{socket}}",
						"--read", "{{socket_dir}}",
						"--env", "OMAC_SOCKET={{socket}}",
						"--env", "OMAC_SKILLS={{skills_csv}}",
						"{{per_skill_env_flags}}",
						"--",
						"{{inner_cmd}}", "{{inner_args}}",
					},
					InnerCmd: []string{"opencode"},
				},
				// Same as above but adds --network-profile opencode so outbound
				// HTTP goes through nono's credential-injection proxy. The Unix
				// socket is unaffected by network-profile filtering.
				"nono-netprofile": {
					Command: []string{
						"nono", "run",
						"--allow-cwd",
						"--profile", "tng-sandbox",
						"--network-profile", "opencode",
						"--allow-file", "{{socket}}",
						"--read", "{{socket_dir}}",
						"--env", "OMAC_SOCKET={{socket}}",
						"--env", "OMAC_SKILLS={{skills_csv}}",
						"{{per_skill_env_flags}}",
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

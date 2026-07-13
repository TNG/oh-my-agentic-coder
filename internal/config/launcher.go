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

// SandboxProfile describes how to launch the sandbox for a given runtime.
type SandboxProfile struct {
	// Command is a templated argv. Supported placeholders:
	//   {{socket}}, {{socket_dir}}, {{inner_cmd}}, {{inner_args}},
	//   {{skills_csv}}, {{per_skill_env_flags}}, {{workdir}}
	// Tokens that expand to multiple argv entries (inner_args,
	// per_skill_env_flags) must stand alone in their slot.
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
// The sandboxed profiles (nono, nono-netprofile) deliberately ship with an
// EMPTY inner_cmd: the inner command is supplied by the selected harness (the
// positional `omac start <harness>` token; default opencode) via
// Harness.ResolveInnerCmd. This is what lets `omac start claude` actually run
// Claude Code without editing config. A user who pins a profile's inner_cmd in
// their own oh-my-agentic-coder.yaml still wins (that explicit value takes
// precedence over the harness default — see ResolveInnerCmd). The
// no-sandbox-debug profile keeps its explicit `bash` because it is a debug
// shell, not an agent harness.
func DefaultLauncherConfig() LauncherConfig {
	return defaultLauncherConfigFor(DefaultHarness())
}

// defaultLauncherConfigFor builds the default launcher config. The harness
// argument is currently only used to keep the signature future-proof and to
// let tests assert harness-independence; the sandboxed profiles intentionally
// leave inner_cmd empty so the harness fills it at launch. The sandbox
// *command* templates are harness-independent (they only reference
// {{inner_cmd}} / {{inner_args}} placeholders).
func defaultLauncherConfigFor(h Harness) LauncherConfig {
	_ = h // inner_cmd is supplied by the harness at resolve time, not baked here
	return LauncherConfig{
		Sandbox: SandboxConfig{
			DefaultProfile: "builtin",
			Profiles: map[string]SandboxProfile{
				// builtin re-execs the running omac binary as
				// `omac sandbox run` — the native replacement for nono
				// (Seatbelt on macOS, bubblewrap+Landlock on Linux).
				// Flag semantics intentionally mirror the nono profile
				// below so the two stay drop-in interchangeable:
				//
				//   --allow-file <socket>   AF_UNIX bridge socket (the
				//                           generated Seatbelt profile
				//                           allows connect explicitly,
				//                           so unlike nono this works
				//                           on macOS even under the
				//                           network deny)
				//   --read <socket-dir>     path-component lookup
				//   {{tmpdir_flags}}        rw on the TMPDIR temp dir
				//   --open-port <tcp-port>  loopback facade transport
				//
				// The sandbox profile itself (fs grants, listen_port,
				// allow_tcp_connect, network prompt) is resolved by
				// `omac sandbox run --profile default`: user override at
				// ~/.config/omac/profiles/default.json, else compiled-in
				// defaults equivalent to nono's tng-sandbox profile.
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
				// Retained for transition: select with
				// `omac start --sandbox-profile nono` or via config.
				"nono": {
					// Reference invocation for nono (https://nono.sh).
					//
					// Transport: omac binds the facade on BOTH a Unix
					// socket and a 127.0.0.1 TCP port. We tell nono to:
					//
					//   - --allow-file <socket>      grant open(2) on the
					//                                Unix socket inode
					//                                (Linux: this is enough;
					//                                macOS: necessary but
					//                                not sufficient under
					//                                proxy mode).
					//
					//   - --read <socket-dir>        path-component lookup
					//                                during connect(2).
					//
					//   - --open-port <tcp-port>     allow bidirectional
					//                                127.0.0.1:<port> from
					//                                inside the sandbox.
					//                                THIS is the transport
					//                                that works on macOS
					//                                under proxy mode (auto-
					//                                activated by any nono
					//                                profile with
					//                                custom_credentials,
					//                                network_profile,
					//                                --allow-domain,
					//                                --credential, or
					//                                --upstream-proxy).
					//                                Per the nono
					//                                "Networking" docs,
					//                                --open-port emits a
					//                                Seatbelt allow rule
					//                                that takes precedence
					//                                over the proxy-mode
					//                                `(deny network*)`.
					//
					// Inside the sandbox the agent reads OMAC_<SKILL>_BASE
					// (a TCP URL) by default, falling back to
					// OMAC_<SKILL>_SOCKET_BASE for the http+unix:// form.
					//
					// Env-var injection: nono no longer accepts a literal
					// `--env KEY=VAL` flag. Instead sandbox.Exec sets
					// OMAC_* in nono's own process environment, and nono
					// propagates the parent env to the inner process by
					// default. If you author a custom nono profile with
					// environment.allow_vars set, add `OMAC_*` to the
					// list.
					//
					// IMPORTANT: this profile does NOT use --block-net.
					// On macOS that installs `(deny network*)` plus a
					// `--open-port` allowance — but the interaction with
					// --network-profile and Seatbelt rule ordering is
					// untested for our use case. Use --network-profile
					// instead (see nono-netprofile below).
					//
					//   - --read <tmpdir> --write <tmpdir>
					//                                grant the inner command
					//                                read+write on a host temp
					//                                dir that omac also exports
					//                                as TMPDIR. Bun-built
					//                                harnesses (opencode)
					//                                extract their embedded
					//                                runtime into TMPDIR at
					//                                startup; without a
					//                                writable, sandbox-granted
					//                                temp dir that extraction
					//                                fails and the agent never
					//                                starts.
					Command: []string{
						"nono", "run",
						"--allow-cwd",
						"--profile", "tng-sandbox",
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
				// Same as above but adds --network-profile opencode so
				// outbound HTTP goes through nono's credential-injection
				// proxy. --open-port keeps the facade reachable; per the
				// nono docs it works alongside domain filtering.
				"nono-netprofile": {
					Command: []string{
						"nono", "run",
						"--allow-cwd",
						"--profile", "tng-sandbox",
						"--network-profile", "opencode",
						"--allow-file", "{{socket}}",
						"--read", "{{socket_dir}}",
						// See the nono profile above: grant RW on the
						// host temp dir exported as TMPDIR so Bun-built
						// harnesses can extract their runtime.
						"{{tmpdir_flags}}",
						"--open-port", "{{tcp_port}}",
						"--",
						"{{inner_cmd}}", "{{inner_args}}",
					},
					// Empty: filled by the selected harness at launch.
					InnerCmd: nil,
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
		Audit: AuditConfig{
			Enabled: boolPtr(true),
			Path:    "", // audit.DefaultPath() (persistent central location)
			Syslog:  false,
			Strict:  false,
		},
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
	// Audit defaults on when the block is unset. An explicit
	// `enabled: false` is preserved (that's why Enabled is a *bool).
	if lc.Audit.Enabled == nil {
		lc.Audit.Enabled = def.Audit.Enabled
	}
	return lc
}

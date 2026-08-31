//go:build e2e || e2e_fast

// Package e2e provides end-to-end test infrastructure for the omac
// harness×skill matrix. The test itself lives in e2e_test.go behind the
// "e2e" build tag; this file holds pure data and config-writing helpers
// that are testable without that tag.
package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// harnessConfig describes everything the e2e test needs to install,
// configure, and run a single agentic-coder harness.
//
// Each harness owns its full environment adaptation in three fields:
//
//   - ProviderSetup — writes config files into the temp HOME
//   - EnvVars       — returns env vars injected into the omac start subprocess
//   - Sandbox       — declares sandbox deviations (extra domains, paths, flags)
//
// A curious developer should be able to read one *Config() function and
// understand every assumption made for that harness: which env vars are
// required, which files are written, which URLs are allowed, which paths
// are granted, and why each deviation from a local interactive run exists.
type harnessConfig struct {
	Name       string // canonical harness name (matches config.Harness.Name)
	BinaryName string // the CLI binary on $PATH (e.g. "opencode", "claude", "codex", "copilot")

	// InstallCmd is the argv to install the harness globally (run once per
	// test, in a temp $HOME).
	InstallCmd []string

	// ExtraInstallSteps runs after the global install. May be nil.
	ExtraInstallSteps func(t *testing.T, home string)

	// ProviderSetup writes the harness's provider config files (auth.json,
	// config.toml, config.json, opencode.json) into the temp $HOME.
	ProviderSetup func(t *testing.T, home string)

	// EnvVars returns harness-specific env vars for the omac start
	// subprocess. These are injected in addition to the base env (which
	// includes HOME, PATH, NPM_CONFIG_PREFIX, XDG_* — see withHome).
	// SKAINET_TOKEN propagates via os.Environ() inheritance, so it does
	// not need to be re-added here unless the harness expects it under
	// a different name.
	EnvVars func(t *testing.T) []string

	// Sandbox declares this harness's deviations from the base sandbox
	// profile. The base profile (see writeSandboxProfile in e2e_test.go)
	// grants readwrite workdir, filtered network with listen_port 4097
	// (echo-rest), and SSH (port 22). Each harness adds only what it
	// additionally needs. See the SandboxConfig type for fields.
	Sandbox SandboxConfig

	// RunArgs builds the inner-command argv for a non-interactive single-shot
	// agent run with the given prompt.
	RunArgs func(prompt string) []string

	// SkillsBase is the harness's skills directory base (e.g. ".opencode",
	// ".claude", ".codex", ".copilot"). Used to locate installed skills.
	SkillsBase string

	// EnvVarsForAllow returns the env var names (or prefix patterns)
	// the harness needs inside the sandbox. These are added to the
	// profile's environment.allow_vars so FilterEnv passes them through.
	// Used by the security audit test.
	EnvVarsForAllow func() []string

	// ExpectVisibleEnv returns env var names that should appear in the
	// agent's env output (positive assertions). Used by the security
	// audit test to verify the sandbox passes them through.
	ExpectVisibleEnv func() []string
}

// SandboxConfig declares per-harness sandbox deviations beyond the base
// profile. Every field should have a comment explaining WHY the deviation
// is necessary — a curious developer should be able to audit whether it
// is still needed.
type SandboxConfig struct {
	// ExtraAllowDomains are additional domains the sandbox proxy permits
	// beyond the model provider host (always allowed — derived from
	// SKAINET_INTERNAL / ANTHROPIC_BASE_URL at runtime).
	ExtraAllowDomains []string

	// ExtraReadPaths are additional filesystem paths the sandbox permits
	// for read beyond the base read paths (~/.gitconfig,
	// ~/.gitignore_global, ~/.config).
	ExtraReadPaths []string

	// NoSandbox disables the omac sandbox entirely for this harness.
	// Used when the harness's own runtime is incompatible with the
	// sandbox mechanism (e.g. codex's Rust HTTP client on macOS).
	NoSandbox bool

	// StripsEnvVars reports whether the harness strips OMAC_* env vars
	// from tool subprocesses in its headless/exec mode. CodeWhale's
	// child_env module calls cmd.env_clear() and rebuilds from a fixed
	// allowlist that excludes OMAC_* — and its shell_env hook (which
	// re-injects them) only fires in the interactive TUI, not in
	// `codewhale exec` (the headless mode the e2e tests use). When true,
	// sidecar-reachability, cache-isolation, env-var-visibility, and
	// echo-rest assertions are skipped: they would all fail on missing
	// OMAC_* vars that the harness dropped, not on a sandbox defect.
	StripsEnvVars bool
}

// allHarnesses returns the harnesses eligible on this host. codex is
// excluded on darwin — its Rust HTTP client is incompatible with the
// macOS Seatbelt sandbox; `omac start codex` on macOS fails loud (see
// issue #48). Running the e2e with --no-sandbox would disable the entire
// omac sandbox, leaving nothing to assert against.
//
// claude-code is excluded when E2E_SKIP_CLAUDE_CODE=1 — it is the only
// harness billed against a real external Anthropic account (the others
// run against the internal SKAINET gateway), so it's the one harness
// worth an easy opt-out when iterating locally or controlling CI cost.
// Included by default.
func allHarnesses() []harnessConfig {
	all := []harnessConfig{
		opencodeConfig(),
		claudeCodeConfig(),
		codexConfig(),
		copilotConfig(),
		piConfig(),
		codewhaleConfig(),
	}
	if runtime.GOOS == "darwin" {
		// codex and codewhale are excluded on darwin — both are Rust CLIs
		// whose HTTP clients are (codex: confirmed; codewhale: by analogy,
		// unverified — see codewhaleConfig) incompatible with the macOS
		// Seatbelt sandbox. See issue #48.
		out := all[:0]
		for _, h := range all {
			if h.Name != "codex" && h.Name != "codewhale" {
				out = append(out, h)
			}
		}
		all = out
	}
	if skipClaudeCode() {
		out := all[:0]
		for _, h := range all {
			if h.Name != "claude-code" {
				out = append(out, h)
			}
		}
		all = out
	}
	return all
}

// skipClaudeCode reports whether E2E_SKIP_CLAUDE_CODE=1 is set, excluding
// claude-code from allHarnesses(). Set this to iterate locally without an
// ANTHROPIC_BASE_URL / Anthropic-billed token configured, or to skip the
// one harness that costs real money on a given CI run.
func skipClaudeCode() bool {
	return os.Getenv("E2E_SKIP_CLAUDE_CODE") == "1"
}

// harnessByName returns the config for a single harness by canonical name.
// Returns ok=false if the name is unknown.
func harnessByName(name string) (harnessConfig, bool) {
	for _, h := range allHarnesses() {
		if h.Name == name {
			return h, true
		}
	}
	return harnessConfig{}, false
}

// ---------------------------------------------------------------------------
// opencode
// ---------------------------------------------------------------------------

// opencode is installed via bun (not npm) and reads its provider config
// from two files:
//
//   - ~/.local/share/opencode/auth.json  — API key for the model provider
//   - ~/.config/opencode/opencode.json   — model provider definition
//
// Env vars: none beyond os.Environ() inheritance (SKAINET_TOKEN is read
// from the env by opencode's auth.json "key" field, not from a process
// env var at runtime).
//
// Sandbox deviations:
//   - models.dev         — opencode fetches model metadata at startup
//   - registry.npmjs.org — opencode fetches npm provider packages at runtime
//   - CWD (macOS only)   — opencode lstat's the process CWD; sandbox
//     denies it with EPERM unless explicitly granted
//
// Paths: opencode writes to ~/.local/{share,state}/opencode at runtime —
// these are granted via Harness.SandboxDirs at launch. The tool cache
// (XDG_CACHE_HOME / OMAC_CACHE_DIR) is injected by omac and isolated
// per workdir (Tasks 1-10); ~/.cache is NOT broad-granted by the profile.
func opencodeConfig() harnessConfig {
	return harnessConfig{
		Name:       "opencode",
		BinaryName: "opencode",
		InstallCmd: []string{"bun", "install", "-g", pinnedPackage("opencode")},
		ProviderSetup: func(t *testing.T, home string) {
			token := os.Getenv("SKAINET_TOKEN")
			if token == "" {
				t.Fatal("SKAINET_TOKEN not set")
			}
			baseURL := os.Getenv("SKAINET_INTERNAL")
			if baseURL == "" {
				t.Fatal("SKAINET_INTERNAL not set (CI secret for the model provider URL)")
			}
			t.Logf("opencode provider: baseURL=%s tokenLen=%d", baseURL, len(token))
			// Write auth.json with the model API key.
			authDir := filepath.Join(home, ".local", "share", "opencode")
			if err := os.MkdirAll(authDir, 0o755); err != nil {
				t.Fatal(err)
			}
			auth := map[string]map[string]string{
				"model": {
					"type": "api",
					"key":  token,
				},
			}
			authBytes, _ := json.Marshal(auth)
			if err := os.WriteFile(filepath.Join(authDir, "auth.json"), authBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Logf("auth.json written to %s", authDir)
			// Write opencode.json with a model provider — no plugin
			// needed. @ai-sdk/openai-compatible is built into opencode.
			cfgDir := filepath.Join(home, ".config", "opencode")
			if err := os.MkdirAll(cfgDir, 0o755); err != nil {
				t.Fatal(err)
			}
			opencodeJSON := map[string]any{
				"share": "disabled",
				"provider": map[string]any{
					"model": map[string]any{
						"name": "Model",
						"npm":  "@ai-sdk/openai-compatible",
						"options": map[string]any{
							"baseURL": baseURL,
						},
						"models": map[string]any{
							modelID("opencode"): map[string]any{
								// Display name follows the id so an
								// overridden model isn't mislabelled.
								"name": modelID("opencode"),
								"limit": map[string]any{
									"context": contextLimit(),
									"output":  outputLimit(),
								},
							},
						},
					},
				},
			}
			cfgBytes, _ := json.Marshal(opencodeJSON)
			if err := os.WriteFile(filepath.Join(cfgDir, "opencode.json"), cfgBytes, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		EnvVars: func(t *testing.T) []string {
			// opencode reads its API key from auth.json, not from a
			// process env var. SKAINET_TOKEN propagates via os.Environ()
			// inheritance, and auth.json already references it by value.
			return nil
		},
		Sandbox: SandboxConfig{
			ExtraAllowDomains: []string{
				"models.dev",         // opencode fetches model metadata at startup
				"registry.npmjs.org", // opencode fetches npm provider packages at runtime
			},
			ExtraReadPaths: opencodeCWDReadPaths(),
		},
		RunArgs: func(prompt string) []string {
			return []string{"run", "--print-logs", "-m", "model/" + modelID("opencode"), prompt}
		},
		SkillsBase: ".opencode",
		EnvVarsForAllow: func() []string {
			return []string{"SKAINET_TOKEN"}
		},
		ExpectVisibleEnv: func() []string {
			return []string{"SKAINET_TOKEN=", "OMAC_"}
		},
	}
}

// opencodeCWDReadPaths returns the test process CWD on macOS (opencode
// lstat's it; sandbox denies with EPERM unless granted). Returns nil
// on non-darwin.
func opencodeCWDReadPaths() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	if cwd, err := os.Getwd(); err == nil {
		return []string{cwd}
	}
	return nil
}

// ---------------------------------------------------------------------------
// claude-code
// ---------------------------------------------------------------------------

// claude-code reads its provider config entirely from env vars — no
// file-based config needed for BYOK.
//
// Env vars injected:
//
//	ANTHROPIC_AUTH_TOKEN                      — API key (from SKAINET_TOKEN)
//	ANTHROPIC_BASE_URL                        — Anthropic-compatible proxy URL
//	CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC  — disables telemetry/analytics
//
// Sandbox deviations: none. The model provider host (from
// ANTHROPIC_BASE_URL) is allowed by the base profile.
//
// Files written:
//   - ~/.claude/settings.json — disables telemetry (ExtraInstallSteps)
func claudeCodeConfig() harnessConfig {
	return harnessConfig{
		Name:       "claude-code",
		BinaryName: "claude",
		InstallCmd: []string{"npm", "install", "-g", pinnedPackage("claude-code")},
		ExtraInstallSteps: func(t *testing.T, home string) {
			// Write a minimal settings.json disabling telemetry.
			cfgDir := filepath.Join(home, ".claude")
			if err := os.MkdirAll(cfgDir, 0o755); err != nil {
				t.Fatal(err)
			}
			settings := map[string]any{
				"env": map[string]string{
					"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
				},
			}
			b, _ := json.Marshal(settings)
			if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), b, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		ProviderSetup: func(t *testing.T, home string) {
			if os.Getenv("SKAINET_TOKEN") == "" {
				t.Fatal("SKAINET_TOKEN not set")
			}
			if os.Getenv("ANTHROPIC_BASE_URL") == "" {
				t.Fatal("ANTHROPIC_BASE_URL not set (CI secret for the Anthropic proxy)")
			}
			// Fail here, not on an opaque claude-code startup error, when a
			// cross-harness model override lands on the one harness that
			// cannot run it.
			if err := validateModel("claude-code", modelID("claude-code")); err != nil {
				t.Fatal(err)
			}
			// Claude Code provider is configured via env vars set on the
			// omac start subprocess (ANTHROPIC_AUTH_TOKEN +
			// ANTHROPIC_BASE_URL). No file-based config needed.
		},
		EnvVars: func(t *testing.T) []string {
			token := os.Getenv("SKAINET_TOKEN")
			if token == "" {
				t.Fatal("SKAINET_TOKEN not set")
			}
			baseURL := os.Getenv("ANTHROPIC_BASE_URL")
			if baseURL == "" {
				t.Fatal("ANTHROPIC_BASE_URL not set")
			}
			return []string{
				"ANTHROPIC_AUTH_TOKEN=" + token,
				"ANTHROPIC_BASE_URL=" + baseURL,
				"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
			}
		},
		Sandbox: SandboxConfig{}, // no deviations — model host allowed by base profile
		RunArgs: func(prompt string) []string {
			return []string{"-p", prompt, "--model", modelID("claude-code"), "--dangerously-skip-permissions"}
		},
		SkillsBase: ".claude",
		EnvVarsForAllow: func() []string {
			return []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"}
		},
		ExpectVisibleEnv: func() []string {
			return []string{"ANTHROPIC_AUTH_TOKEN=", "ANTHROPIC_BASE_URL=", "OMAC_"}
		},
	}
}

// ---------------------------------------------------------------------------
// codex
// ---------------------------------------------------------------------------

// codex reads its provider config from ~/.codex/config.toml. It uses the
// OpenAI Responses API (wire_api=responses) via SKAINET_INTERNAL.
//
// Env vars: none beyond os.Environ() inheritance. config.toml references
// SKAINET_TOKEN by env_key name; codex reads it from the process env.
//
// Sandbox deviations:
//   - chatgpt.com — codex checks ChatGPT auth at startup (even in BYOK mode)
//   - github.com  — codex checks GitHub at startup (even in BYOK mode)
//   - NoSandbox on macOS — codex's Rust HTTP client is incompatible with
//     sandbox-exec (fails with "stream disconnected" even with network=open).
//     codex already bypasses its own sandbox via --dangerously-bypass-
//     approvals-and-sandbox.
//
// Files written:
//   - ~/.codex/config.toml — model provider definition with wire_api=responses
func codexConfig() harnessConfig {
	return harnessConfig{
		Name:       "codex",
		BinaryName: "codex",
		InstallCmd: []string{"npm", "install", "-g", pinnedPackage("codex")},
		ProviderSetup: func(t *testing.T, home string) {
			token := os.Getenv("SKAINET_TOKEN")
			if token == "" {
				t.Fatal("SKAINET_TOKEN not set")
			}
			baseURL := os.Getenv("SKAINET_INTERNAL")
			if baseURL == "" {
				t.Fatal("SKAINET_INTERNAL not set (CI secret for the responses API URL)")
			}
			codexDir := filepath.Join(home, ".codex")
			if err := os.MkdirAll(codexDir, 0o755); err != nil {
				t.Fatal(err)
			}
			// config.toml: codex requires wire_api=responses (Responses API).
			// The responses API (SKAINET_INTERNAL) supports /v1/responses with the configured model.
			configToml := `model = "` + modelID("codex") + `"
model_provider = "model"

[model_providers.model]
name = "Model"
base_url = "` + baseURL + `"
env_key = "SKAINET_TOKEN"
wire_api = "responses"
http_headers = { "X-User-Agent" = "Codex", "X-Separate-Reasoning" = "1" }
`
			if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(configToml), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		EnvVars: func(t *testing.T) []string {
			// codex reads SKAINET_TOKEN from the process env (referenced
			// by env_key in config.toml). It propagates via os.Environ()
			// inheritance — no additional env vars needed.
			return nil
		},
		Sandbox: SandboxConfig{
			ExtraAllowDomains: []string{
				"chatgpt.com", // codex checks ChatGPT auth at startup (even in BYOK mode)
				"github.com",  // codex checks GitHub at startup (even in BYOK mode)
			},
			// codex's Rust HTTP client is incompatible with macOS Seatbelt
			// (stream disconnected before completion). The builtin profile
			// uses sandbox-exec, which hits that incompatibility. Linux
			// (bwrap) works.
			NoSandbox: runtime.GOOS == "darwin",
		},
		RunArgs: func(prompt string) []string {
			return []string{"exec", "--dangerously-bypass-approvals-and-sandbox", "-m", modelID("codex"), prompt}
		},
		SkillsBase: ".codex",
		EnvVarsForAllow: func() []string {
			return []string{"SKAINET_TOKEN"}
		},
		ExpectVisibleEnv: func() []string {
			return []string{"SKAINET_TOKEN=", "OMAC_"}
		},
	}
}

// ---------------------------------------------------------------------------
// copilot
// ---------------------------------------------------------------------------

// copilot uses BYOK (Bring Your Own Key) via COPILOT_PROVIDER_* env vars,
// bypassing GitHub OAuth/PAT entirely. No GitHub token is needed for
// this test — a GitHub token is only required for /delegate, the GitHub
// MCP server, or GitHub Code Search, none of which this test exercises.
//
// Env vars injected:
//
//	COPILOT_PROVIDER_TYPE=openai       — use OpenAI-compatible provider
//	COPILOT_PROVIDER_BASE_URL=<url>   — model provider base URL (from SKAINET_INTERNAL)
//	COPILOT_PROVIDER_API_KEY=<token>  — API key (from SKAINET_TOKEN)
//	COPILOT_MODEL=<model>             — model ID
//	COPILOT_PROVIDER_WIRE_API=responses — use Responses API wire format
//
// Sandbox deviations: none. The model provider host (from
// SKAINET_INTERNAL) is allowed by the base profile.
//
// Files written:
//   - ~/.copilot/config.json — pre-trusts the workdir so the first-run
//     "trust this folder?" prompt doesn't block the non-interactive run
func copilotConfig() harnessConfig {
	return harnessConfig{
		Name:       "copilot",
		BinaryName: "copilot",
		InstallCmd: []string{"npm", "install", "-g", pinnedPackage("copilot")},
		ProviderSetup: func(t *testing.T, home string) {
			// Provider config (COPILOT_PROVIDER_*) is injected as process
			// env vars in EnvVars — copilot CLI reads them from the
			// environment, not from a sourced file. ProviderSetup only
			// pre-trusts the workdir so the first-run "trust this folder?"
			// prompt doesn't block the non-interactive run.
			copilotDir := filepath.Join(home, ".copilot")
			if err := os.MkdirAll(copilotDir, 0o755); err != nil {
				t.Fatal(err)
			}
			config := map[string]any{
				"trustedFolders": []string{home},
			}
			b, _ := json.Marshal(config)
			if err := os.WriteFile(filepath.Join(copilotDir, "config.json"), b, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		EnvVars: func(t *testing.T) []string {
			token := os.Getenv("SKAINET_TOKEN")
			if token == "" {
				t.Fatal("SKAINET_TOKEN not set")
			}
			baseURL := os.Getenv("SKAINET_INTERNAL")
			if baseURL == "" {
				t.Fatal("SKAINET_INTERNAL not set (CI secret for the responses API URL)")
			}
			return []string{
				"COPILOT_PROVIDER_TYPE=openai",
				"COPILOT_PROVIDER_BASE_URL=" + baseURL,
				"COPILOT_PROVIDER_API_KEY=" + token,
				"COPILOT_MODEL=" + modelID("copilot"),
				"COPILOT_PROVIDER_WIRE_API=responses",
			}
		},
		Sandbox: SandboxConfig{}, // no deviations — model host allowed by base profile
		RunArgs: func(prompt string) []string {
			return []string{"-p", prompt, "--model", modelID("copilot"), "--allow-all-tools"}
		},
		SkillsBase: ".copilot",
		EnvVarsForAllow: func() []string {
			return []string{
				"COPILOT_PROVIDER_TYPE",
				"COPILOT_PROVIDER_BASE_URL",
				"COPILOT_PROVIDER_API_KEY",
				"COPILOT_MODEL",
				"COPILOT_PROVIDER_WIRE_API",
			}
		},
		ExpectVisibleEnv: func() []string {
			// copilot strips COPILOT_PROVIDER_* vars after reading them.
			// COPILOT_MODEL and COPILOT_CLI survive.
			return []string{"COPILOT_MODEL=", "COPILOT_CLI", "OMAC_"}
		},
	}
}

// ---------------------------------------------------------------------------
// pi
// ---------------------------------------------------------------------------

// pi reads custom OpenAI-compatible provider definitions from
// ~/.pi/agent/models.json (a "providers" map of name -> {baseUrl, api,
// apiKey, models}) — this is pi's actual documented custom-provider
// mechanism (https://pi.dev/docs/latest/custom-provider), confirmed live
// against the real SKAINET gateway. An earlier version of this config
// wrote ~/.pi/agent/auth.json + settings.json instead; that schema isn't
// one pi's provider loader recognizes, and RunArgs omitted the required
// --provider flag (pi defaults to "google" without it) — together those
// caused every real pi invocation to hang indefinitely rather than fail
// fast, both confirmed by reproducing it against the real gateway.
//
// Env vars: none beyond os.Environ() inheritance. apiKey is written as the
// literal string "$SKAINET_TOKEN" in models.json; pi resolves $ENV_VAR
// references from the process env at call time, so the raw token is never
// written to disk (same env_key trick codex's config.toml uses).
//
// Sandbox deviations: none. Verified live: `omac start pi` under the real
// bwrap sandbox against SKAINET needed no ExtraAllowDomains/ExtraReadPaths
// beyond the base profile (pi is pure Node/TypeScript, no Rust-HTTP-client
// issue like codex has on macOS — that specific exclusion is unverified on
// darwin here, only Linux was exercised).
//
// Files written:
//   - ~/.pi/agent/models.json — custom provider definition
func piConfig() harnessConfig {
	return harnessConfig{
		Name:       "pi",
		BinaryName: "pi",
		InstallCmd: []string{"npm", "install", "-g", pinnedPackage("pi")},
		ProviderSetup: func(t *testing.T, home string) {
			token := os.Getenv("SKAINET_TOKEN")
			if token == "" {
				t.Fatal("SKAINET_TOKEN not set")
			}
			baseURL := os.Getenv("SKAINET_INTERNAL")
			if baseURL == "" {
				t.Fatal("SKAINET_INTERNAL not set (CI secret for the model provider URL)")
			}
			t.Logf("pi provider: baseURL=%s tokenLen=%d", baseURL, len(token))
			agentDir := filepath.Join(home, ".pi", "agent")
			if err := os.MkdirAll(agentDir, 0o755); err != nil {
				t.Fatal(err)
			}
			// models.json: apiKey is the literal string "$SKAINET_TOKEN" —
			// pi resolves $ENV_VAR references from the process env at
			// runtime, so the token itself is never written to disk.
			modelsJSON := map[string]any{
				"providers": map[string]any{
					"model": map[string]any{
						"baseUrl": baseURL,
						"api":     "openai-completions",
						"apiKey":  "$SKAINET_TOKEN",
						"headers": map[string]string{
							"X-User-Agent":         "pi",
							"X-Separate-Reasoning": "1",
						},
						"models": []map[string]any{
							{"id": modelID("pi")},
						},
					},
				},
			}
			b, _ := json.Marshal(modelsJSON)
			if err := os.WriteFile(filepath.Join(agentDir, "models.json"), b, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("models.json written to %s", agentDir)
		},
		EnvVars: func(t *testing.T) []string {
			// pi reads SKAINET_TOKEN from the process env (referenced via
			// "$SKAINET_TOKEN" in models.json). It propagates via
			// os.Environ() inheritance — no additional env vars needed.
			return nil
		},
		Sandbox: SandboxConfig{}, // no deviations — confirmed via a real `omac start pi` run (Linux)
		RunArgs: func(prompt string) []string {
			// pi has no built-in tool-approval popups, so no
			// --dangerously-skip-permissions-equivalent flag is needed.
			// --provider is required: pi defaults to "google" without it,
			// which would silently ignore the "model" custom provider above.
			return []string{"-p", prompt, "--provider", "model", "--model", modelID("pi")}
		},
		SkillsBase: ".pi",
		EnvVarsForAllow: func() []string {
			return []string{"SKAINET_TOKEN"}
		},
		ExpectVisibleEnv: func() []string {
			return []string{"SKAINET_TOKEN=", "OMAC_"}
		},
	}
}

// ---------------------------------------------------------------------------
// codewhale
// ---------------------------------------------------------------------------

// codewhale is a Rust CLI (npm package "codewhale") that is multi-provider.
// For this test it runs against the internal SKAINET gateway via CodeWhale's
// generic OpenAI-compatible route: config.example.toml documents
// `provider = "openai"` + a `[providers.openai]` table for "generic
// OpenAI-compatible gateways" (config.example.toml:354). We point that
// provider's base_url at SKAINET_INTERNAL (which already includes /v1) and
// rely on the default chat-completions path: codewhale appends
// /chat/completions to a versioned base_url, yielding <base>/chat/completions
// — the same shape opencode's @ai-sdk/openai-compatible client uses.
// path_suffix is deliberately NOT set: that field strips the version segment
// (e.g. /v1) from base_url before appending, which would drop /v1 and result in 404.
//
// Env vars: OPENAI_API_KEY carries the bearer token. CodeWhale's openai
// provider reads it from the process env (crates/config/src/provider.rs:546),
// so — like codex's env_key and pi's $ENV_VAR indirection — the raw token is
// never written to disk. SKAINET_TOKEN also propagates via os.Environ().
//
// Headless execution: `codewhale exec` alone is a one-shot model response;
// `--auto` enables tool-backed agent mode with auto-approvals (crates/cli/
// src/lib.rs:252,259) — required so the agent can call the echo-rest skill,
// write files, and git-commit. config.toml pins approval_policy = "never"
// and sandbox_mode = "external-sandbox" so CodeWhale neither prompts nor
// applies its OWN inner OS sandbox inside omac's bwrap/Seatbelt sandbox.
// `[update] check_for_updates = false` suppresses the startup version check
// so no fixed release host is contacted. The token binds via [providers.openai]
// api_key_env = "OPENAI_API_KEY": CodeWhale refuses an ambient key on a custom
// base_url unless it is bound explicitly.
//
// shell_env hook (EXPERIMENTAL, TUI-only): CodeWhale's child_env calls
// cmd.env_clear() and rebuilds from a fixed allowlist that excludes
// OMAC_* — so bash tool subprocesses can't see OMAC_ECHO_BASE,
// OMAC_CACHE_DIR, etc. The [hooks] shell_env entry re-injects OMAC_*
// and tool cache mappings from CodeWhale's own process env before each
// exec_shell. OPENAI_API_KEY is deliberately NOT re-injected.
//
// Hooks only fire in the interactive TUI, NOT in `codewhale exec`
// (headless mode). The e2e tests use `codewhale exec --auto`, so the
// hook is a no-op during tests and OMAC_* is stripped from bash
// subprocesses. StripsEnvVars=true skips sidecar, cache, env-visibility,
// and echo-rest assertions. Real TUI users get the hook — see
// docs/HARNESSES.md.
//
// Sandbox deviations: none expected — the model host (SKAINET_INTERNAL) is
// allowed by the base profile and the update check is disabled. This has NOT
// been verified against a live gateway run (unlike opencode/pi, which were);
// treat ExtraAllowDomains as provisional if a real run surfaces a startup host.
//
// macOS: excluded from allHarnesses() by analogy with codex — CodeWhale is
// Rust with its own HTTP client, the same class that makes codex's client
// disconnect mid-stream under macOS Seatbelt (issue #48). This is a
// precaution, NOT a verified CodeWhale failure; re-test on macOS and drop the
// exclusion (here, in expectedHarnessNames, and in e2e.yml) if it works.
//
// SkillsBase is ".agents", not ".codewhale": CodeWhale loads workspace-local
// skills from `.agents/skills` or `./skills` (CONFIGURATION.md:1490), and
// does NOT read a workspace `.codewhale/skills`. The e2e installs the skill
// into <workdir>/<SkillsBase>/skills, so it must target a dir CodeWhale reads.
//
// Files written:
//   - ~/.codewhale/config.toml — provider, model, approvals, sandbox, update
func codewhaleConfig() harnessConfig {
	return harnessConfig{
		Name:       "codewhale",
		BinaryName: "codewhale",
		InstallCmd: []string{"npm", "install", "-g", pinnedPackage("codewhale")},
		ProviderSetup: func(t *testing.T, home string) {
			token := os.Getenv("SKAINET_TOKEN")
			if token == "" {
				t.Fatal("SKAINET_TOKEN not set")
			}
			baseURL := os.Getenv("SKAINET_INTERNAL")
			if baseURL == "" {
				t.Fatal("SKAINET_INTERNAL not set (CI secret for the model provider URL)")
			}
			t.Logf("codewhale provider: baseURL=%s tokenLen=%d", baseURL, len(token))
			cwDir := filepath.Join(home, ".codewhale")
			if err := os.MkdirAll(cwDir, 0o755); err != nil {
				t.Fatal(err)
			}
			// The API key is NOT written here — api_key_env binds it to the
			// OPENAI_API_KEY process env var (see EnvVars), keeping the token
			// off disk. This explicit binding is REQUIRED: CodeWhale refuses to
			// send an ambient OPENAI_API_KEY to a custom (non-default) base_url
			// ("Custom endpoint credentials … must be bound explicitly"), so
			// api_key_env is what lets the auth path reach the gateway.
			//
			// sandbox_mode = "external-sandbox": CodeWhale runs inside omac's
			// bwrap sandbox, so it must not apply its own inner OS isolation;
			// "external-sandbox" is the semantically exact value for that (an
			// external sandbox is already in force). approval_policy = "never"
			// auto-approves tool actions for the non-interactive run.
			configToml := `provider = "openai"
default_text_model = "` + modelID("codewhale") + `"
approval_policy = "never"
sandbox_mode = "external-sandbox"

[providers.openai]
base_url = "` + baseURL + `"
api_key_env = "OPENAI_API_KEY"

[update]
check_for_updates = false

# shell_env hook (EXPERIMENTAL, TUI-only): re-injects OMAC_* and tool cache
# mappings that CodeWhale's child_env strips from bash subprocesses. Only
# fires in the interactive TUI, NOT in codewhale exec (headless mode).
# See docs/HARNESSES.md.
[hooks]
enabled = true

[[hooks.hooks]]
event = "shell_env"
command = "env | grep -E '^(OMAC_|GOCACHE=|GOMODCACHE=|NPM_CONFIG_CACHE=|NPM_CONFIG_PREFIX=|PIP_CACHE_DIR=|XDG_CACHE_HOME=|XDG_CONFIG_HOME=|XDG_DATA_HOME=|XDG_STATE_HOME=|PWD=|BUN_INSTALL_CACHE_DIR=)' || true"
`
			if err := os.WriteFile(filepath.Join(cwDir, "config.toml"), []byte(configToml), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("config.toml written to %s", cwDir)
		},
		EnvVars: func(t *testing.T) []string {
			token := os.Getenv("SKAINET_TOKEN")
			if token == "" {
				t.Fatal("SKAINET_TOKEN not set")
			}
			// CodeWhale's openai provider reads OPENAI_API_KEY from the env.
			return []string{"OPENAI_API_KEY=" + token}
		},
		Sandbox: SandboxConfig{
			// codewhale exec strips OMAC_* from bash subprocesses (child_env
			// env_clear + fixed allowlist). The shell_env hook would re-inject
			// them but only fires in the TUI, not in exec mode — see the doc
			// comment above. Skip sidecar/cache/echo assertions.
			StripsEnvVars: true,
		},
		RunArgs: func(prompt string) []string {
			// `exec` leads so the contract deriver captures it as a subcommand
			// (flagsAndSub only treats a LEADING positional as the subcommand;
			// a preceding `--model <val>` would hide `exec` and its --model
			// value would be misread). The model comes from config.toml's
			// default_text_model instead. `--auto` = tool-backed agent mode
			// with auto-approvals (required for the echo-rest tool call).
			return []string{"exec", "--auto", prompt}
		},
		SkillsBase: ".agents",
		EnvVarsForAllow: func() []string {
			return []string{"OPENAI_API_KEY"}
		},
		ExpectVisibleEnv: func() []string {
			// OMAC_* is NOT visible in bash tool output under exec mode:
			// child_env strips it and the shell_env hook doesn't fire.
			return nil
		},
	}
}

// withHome returns environ with HOME set to home, PATH augmented
// with the harness global bin dirs under home, and NPM_CONFIG_PREFIX
// set so `npm install -g` installs into the temp HOME (not the
// system node prefix). Without NPM_CONFIG_PREFIX, npm's global
// packages land in the host's node prefix, and platform-specific
// optional deps (e.g. @openai/codex-linux-x64) may not resolve.
//
// npm's and bun's *download* caches are pointed at a location outside
// the per-test temp HOME (see sharedInstallCacheRoot) so a package
// already fetched by one subtest — or by a previous CI run, when
// E2E_CACHE_DIR is restored from a workflow cache — doesn't need a
// fresh registry round-trip. Registry flakiness under CI network
// throttling is a leading cause of e2e infra failures.
func withHome(environ []string, home string) []string {
	extraBins := []string{
		filepath.Join(home, ".bun", "bin"),
		filepath.Join(home, "bin"),
		filepath.Join(home, ".local", "bin"),
	}
	npmPrefix := filepath.Join(home)
	cacheRoot := sharedInstallCacheRoot()
	npmCacheDir := filepath.Join(cacheRoot, "npm")
	bunCacheDir := filepath.Join(cacheRoot, "bun-install-cache")
	out := make([]string, 0, len(environ)+8)
	seenHome, seenNpmPrefix, seenXDG, seenXDGData, seenXDGState := false, false, false, false, false
	seenNpmCache, seenBunCache := false, false
	for _, kv := range environ {
		switch {
		case strings.HasPrefix(kv, "HOME="):
			out = append(out, "HOME="+home)
			seenHome = true
		case strings.HasPrefix(kv, "PATH="):
			existing := strings.TrimPrefix(kv, "PATH=")
			out = append(out, "PATH="+strings.Join(extraBins, ":")+":"+existing)
		case strings.HasPrefix(kv, "NPM_CONFIG_PREFIX="):
			out = append(out, "NPM_CONFIG_PREFIX="+npmPrefix)
			seenNpmPrefix = true
		case strings.HasPrefix(kv, "XDG_CONFIG_HOME="):
			out = append(out, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
			seenXDG = true
		case strings.HasPrefix(kv, "XDG_DATA_HOME="):
			out = append(out, "XDG_DATA_HOME="+filepath.Join(home, ".local", "share"))
			seenXDGData = true
		case strings.HasPrefix(kv, "XDG_STATE_HOME="):
			out = append(out, "XDG_STATE_HOME="+filepath.Join(home, ".local", "state"))
			seenXDGState = true
		case strings.HasPrefix(kv, "NPM_CONFIG_CACHE="):
			out = append(out, "NPM_CONFIG_CACHE="+npmCacheDir)
			seenNpmCache = true
		case strings.HasPrefix(kv, "BUN_INSTALL_CACHE_DIR="):
			out = append(out, "BUN_INSTALL_CACHE_DIR="+bunCacheDir)
			seenBunCache = true
		default:
			out = append(out, kv)
		}
	}
	if !seenHome {
		out = append(out, "HOME="+home)
	}
	if !seenNpmPrefix {
		out = append(out, "NPM_CONFIG_PREFIX="+npmPrefix)
	}
	if !seenXDG {
		out = append(out, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	}
	if !seenXDGData {
		out = append(out, "XDG_DATA_HOME="+filepath.Join(home, ".local", "share"))
	}
	if !seenXDGState {
		out = append(out, "XDG_STATE_HOME="+filepath.Join(home, ".local", "state"))
	}
	if !seenNpmCache {
		out = append(out, "NPM_CONFIG_CACHE="+npmCacheDir)
	}
	if !seenBunCache {
		out = append(out, "BUN_INSTALL_CACHE_DIR="+bunCacheDir)
	}
	return out
}

// sharedInstallCacheRoot returns a directory outside any per-test temp
// HOME for npm/bun package download caches, so it survives across the
// two subtests (TestE2EEchoRest, TestE2ESecurityAudit) that each install
// the harness fresh within one test binary run. CI sets E2E_CACHE_DIR to
// a path restored from a workflow cache (see .github/workflows/e2e.yml)
// so the cache also survives across CI runs, not just within one.
func sharedInstallCacheRoot() string {
	if dir := os.Getenv("E2E_CACHE_DIR"); dir != "" {
		return dir
	}
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "omac-e2e-install")
	}
	return filepath.Join(os.TempDir(), "omac-e2e-install-cache")
}

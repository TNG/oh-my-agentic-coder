// Package e2e provides end-to-end test infrastructure for the omac
// harness×skill matrix. The test itself lives in e2e_test.go behind the
// "e2e" build tag; this file holds pure data and config-writing helpers
// that are testable without that tag.
package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// harnessConfig describes everything the e2e test needs to install,
// configure, and run a single agentic-coder harness.
type harnessConfig struct {
	Name       string // canonical harness name (matches config.Harness.Name)
	BinaryName string // the CLI binary on $PATH (e.g. "opencode", "claude", "codex", "copilot")

	// InstallCmd is the argv to install the harness globally (run once per
	// test, in a temp $HOME).
	InstallCmd []string

	// ExtraInstallSteps runs after the global install. May be nil.
	ExtraInstallSteps func(t *testing.T, home string)

	// ProviderSetup writes the harness's provider config files (auth.json,
	// config.toml, provider.env, opencode.json) into the temp $HOME.
	ProviderSetup func(t *testing.T, home string)

	// RunArgs builds the inner-command argv for a non-interactive single-shot
	// agent run with the given prompt.
	RunArgs func(prompt string) []string

	// SkillsBase is the harness's skills directory base (e.g. ".opencode",
	// ".claude", ".codex", ".copilot"). Used to locate installed skills.
	SkillsBase string
}

// allHarnesses returns the full 4-harness registry.
func allHarnesses() []harnessConfig {
	return []harnessConfig{
		opencodeConfig(),
		claudeCodeConfig(),
		codexConfig(),
		copilotConfig(),
	}
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
							modelIDs["opencode"]: map[string]any{
								"name": "GLM 5.2",
								"limit": map[string]any{
									"context": 131072,
									"output":  32000,
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
		RunArgs: func(prompt string) []string {
			return []string{"run", "--print-logs", "-m", "model/" + modelIDs["opencode"], prompt}
		},
		SkillsBase: ".opencode",
	}
}

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
			// Claude Code provider is configured via env vars set on the
			// omac start subprocess (ANTHROPIC_AUTH_TOKEN +
			// ANTHROPIC_BASE_URL). No file-based config needed.
		},
		RunArgs: func(prompt string) []string {
			return []string{"-p", prompt, "--model", modelIDs["claude-code"], "--dangerously-skip-permissions"}
		},
		SkillsBase: ".claude",
	}
}

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
			configToml := `model = "` + modelIDs["codex"] + `"
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
		RunArgs: func(prompt string) []string {
			return []string{"exec", "--dangerously-bypass-approvals-and-sandbox", "-m", modelIDs["codex"], prompt}
		},
		SkillsBase: ".codex",
	}
}

func copilotConfig() harnessConfig {
	return harnessConfig{
		Name:       "copilot",
		BinaryName: "copilot",
		InstallCmd: []string{"npm", "install", "-g", pinnedPackage("copilot")},
		ProviderSetup: func(t *testing.T, home string) {
			// Provider config (COPILOT_PROVIDER_*) is injected as process
			// env vars in buildAgentEnv — copilot CLI reads them from the
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
		RunArgs: func(prompt string) []string {
			return []string{"-p", prompt, "--model", modelIDs["copilot"], "--allow-all-tools"}
		},
		SkillsBase: ".copilot",
	}
}

// withHome returns environ with HOME set to home, PATH augmented
// with the harness global bin dirs under home, and NPM_CONFIG_PREFIX
// set so `npm install -g` installs into the temp HOME (not the
// system node prefix). Without NPM_CONFIG_PREFIX, npm's global
// packages land in the host's node prefix, and platform-specific
// optional deps (e.g. @openai/codex-linux-x64) may not resolve.
func withHome(environ []string, home string) []string {
	extraBins := []string{
		filepath.Join(home, ".bun", "bin"),
		filepath.Join(home, "bin"),
		filepath.Join(home, ".local", "bin"),
	}
	npmPrefix := filepath.Join(home)
	out := make([]string, 0, len(environ)+4)
	seenHome, seenNpmPrefix, seenXDG, seenXDGData, seenXDGState := false, false, false, false, false
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
	return out
}

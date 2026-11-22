package mcpinject

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

const (
	// OpenCodeConfigEnv carries the inline OpenCode config.
	OpenCodeConfigEnv = "OPENCODE_CONFIG_CONTENT"
	claudeConfigFile  = "omac-mcp.json"
)

const strictMCPConfigFlag = "--strict-mcp-config"

// Build returns the injection that adds the servers next to whatever the
// harness already has. userArgs are the arguments the user passes to the
// harness. An inactive policy injects nothing, except that Claude Code is
// always started in strict MCP mode when unregistered servers are refused.
func Build(kind config.MCPInjectionKind, servers []Server, p Policy, tmpDir string, userArgs []string) (Injection, error) {
	var inj Injection
	if kind == config.MCPInjectClaudeFlag && p.Unregistered == ModeRefuse && !slices.Contains(userArgs, strictMCPConfigFlag) {
		// Strict mode also keeps Claude Code from auto-loading MCP servers from plugins and the
		// user's Claude config. Whether it also skips the claude.ai connectors is not documented
		// and has to be checked with a real session. Only refuse mode passes it: warn and allow
		// mean the user wants Claude's own entries.
		inj.InnerArgs = []string{strictMCPConfigFlag}
	}
	if !p.Active {
		return inj, nil
	}
	for _, s := range servers {
		if len(s.Argv) == 0 {
			return Injection{}, fmt.Errorf("MCP server %q has no command", s.Name)
		}
	}
	sorted := append([]Server(nil), servers...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	if len(sorted) == 0 {
		return inj, nil
	}

	switch kind {
	case config.MCPInjectOpenCodeEnv:
		return buildOpenCode(sorted)
	case config.MCPInjectClaudeFlag:
		configArg, err := buildClaude(sorted, tmpDir)
		if err != nil {
			return Injection{}, err
		}
		inj.InnerArgs = append(inj.InnerArgs, configArg)
		return inj, nil
	}
	return Injection{}, fmt.Errorf("MCP injection kind %d is not supported", kind)
}

func buildOpenCode(servers []Server) (Injection, error) {
	entries := map[string]any{}
	for _, s := range servers {
		entries[s.Name] = map[string]any{
			"type":        "local",
			"command":     s.Argv,
			"environment": nonNilEnv(s.Env),
			"enabled":     true,
		}
	}
	content, err := json.Marshal(map[string]any{"mcp": entries})
	if err != nil {
		return Injection{}, err
	}
	return Injection{
		Env:      map[string]string{OpenCodeConfigEnv: string(content)},
		AllowEnv: []string{OpenCodeConfigEnv},
	}, nil
}

func buildClaude(servers []Server, tmpDir string) (string, error) {
	entries := map[string]any{}
	for _, s := range servers {
		entries[s.Name] = map[string]any{
			"command": s.Argv[0],
			"args":    s.Argv[1:],
			"env":     nonNilEnv(s.Env),
		}
	}
	content, err := json.Marshal(map[string]any{"mcpServers": entries})
	if err != nil {
		return "", err
	}
	file := filepath.Join(tmpDir, claudeConfigFile)
	if err := os.WriteFile(file, content, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", file, err)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		return "", fmt.Errorf("chmod %s: %w", file, err)
	}
	return "--mcp-config=" + file, nil
}

func nonNilEnv(env map[string]string) map[string]string {
	if env == nil {
		return map[string]string{}
	}
	return env
}

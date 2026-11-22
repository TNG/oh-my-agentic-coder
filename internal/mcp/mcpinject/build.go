package mcpinject

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

const (
	// OpenCodeConfigEnv carries the inline OpenCode config.
	OpenCodeConfigEnv = "OPENCODE_CONFIG_CONTENT"
	claudeConfigFile  = "omac-mcp.json"
)

// Build returns the injection that adds the servers next to whatever the
// harness already has. An inactive policy injects nothing.
func Build(kind config.MCPInjectionKind, servers []Server, p Policy, tmpDir string) (Injection, error) {
	if !p.Active {
		return Injection{}, nil
	}
	for _, s := range servers {
		if len(s.Argv) == 0 {
			return Injection{}, fmt.Errorf("MCP server %q has no command", s.Name)
		}
	}
	sorted := append([]Server(nil), servers...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	if len(sorted) == 0 {
		return Injection{}, nil
	}

	switch kind {
	case config.MCPInjectOpenCodeEnv:
		return buildOpenCode(sorted)
	case config.MCPInjectClaudeFlag:
		return buildClaude(sorted, tmpDir)
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

func buildClaude(servers []Server, tmpDir string) (Injection, error) {
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
		return Injection{}, err
	}
	file := filepath.Join(tmpDir, claudeConfigFile)
	if err := os.WriteFile(file, content, 0o600); err != nil {
		return Injection{}, fmt.Errorf("write %s: %w", file, err)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		return Injection{}, fmt.Errorf("chmod %s: %w", file, err)
	}
	return Injection{InnerArgs: []string{"--mcp-config=" + file}}, nil
}

func nonNilEnv(env map[string]string) map[string]string {
	if env == nil {
		return map[string]string{}
	}
	return env
}

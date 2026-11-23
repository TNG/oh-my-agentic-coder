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
	// CodeWhaleConfigEnv names the MCP config file CodeWhale reads instead of
	// ~/.codewhale/mcp.json.
	CodeWhaleConfigEnv  = "CODEWHALE_MCP_CONFIG"
	codeWhaleConfigFile = "omac-mcp-codewhale.json"
)

const (
	strictMCPConfigFlag = "--strict-mcp-config"
	noChromeFlag        = "--no-chrome"
	chromeFlag          = "--chrome"
)

// Build returns the injection that adds the servers next to whatever the
// harness already has. userArgs are the arguments the user passes to the
// harness. An inactive policy injects nothing, except that Claude Code is
// always started in strict MCP mode when unregistered servers are refused or ignored.
func Build(kind config.MCPInjectionKind, servers []Server, p Policy, tmpDir string, userArgs []string) (Injection, error) {
	var inj Injection
	if kind == config.MCPInjectClaudeFlag && (p.Unregistered == ModeRefuse || p.Unregistered == ModeIgnore) && !slices.Contains(userArgs, strictMCPConfigFlag) {
		// Strict mode also keeps Claude Code from auto-loading MCP servers from plugins and the
		// user's Claude config. Whether it also skips the claude.ai connectors is not documented
		// and has to be checked with a real session. Only refuse and ignore modes pass it: warn and allow
		// mean the user wants Claude's own entries.
		inj.InnerArgs = []string{strictMCPConfigFlag}
	}
	if kind == config.MCPInjectClaudeFlag && (p.Unregistered == ModeRefuse || p.Unregistered == ModeIgnore) && !slices.Contains(userArgs, noChromeFlag) && !slices.Contains(userArgs, chromeFlag) {
		// The Claude in Chrome integration is a built-in MCP server that
		// --strict-mcp-config does not cover. It would give the agent the
		// user's browser sessions, so MCP comes only from the omac registry:
		// the user opts in with --chrome, which is not overridden.
		inj.InnerArgs = append(inj.InnerArgs, noChromeFlag)
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
	case config.MCPInjectCodeWhaleEnv:
		return buildCodeWhale(sorted, tmpDir)
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
	file, err := writeServersFile(servers, tmpDir, claudeConfigFile, "mcpServers", func(s Server) map[string]any {
		return map[string]any{
			"command": s.Argv[0],
			"args":    s.Argv[1:],
			"env":     nonNilEnv(s.Env),
		}
	})
	if err != nil {
		return "", err
	}
	return "--mcp-config=" + file, nil
}

// buildCodeWhale points CodeWhale at a file that holds only the registered
// servers. CODEWHALE_MCP_CONFIG replaces the path of the global mcp.json, so the
// user's own global file is not read for this run. That the override replaces
// the file and does not add to it, that it wins over mcp_config_path in
// config.toml, and that CodeWhale's project file (.codewhale/mcp.json, which
// the user approves separately) and plugin bundles stay active, all come from
// the public documentation and are unverified. The injection therefore does not
// let the "ignore" mode of mcp.unregistered apply to CodeWhale.
func buildCodeWhale(servers []Server, tmpDir string) (Injection, error) {
	file, err := writeServersFile(servers, tmpDir, codeWhaleConfigFile, "mcpServers", func(s Server) map[string]any {
		return map[string]any{
			"command": s.Argv[0],
			"args":    s.Argv[1:],
			"env":     nonNilEnv(s.Env),
		}
	})
	if err != nil {
		return Injection{}, err
	}
	return Injection{
		Env:      map[string]string{CodeWhaleConfigEnv: file},
		AllowEnv: []string{CodeWhaleConfigEnv},
	}, nil
}

// writeServersFile writes {key: {name: entry}} to tmpDir/name with mode 0600
// and returns the path.
func writeServersFile(servers []Server, tmpDir, name, key string, entry func(Server) map[string]any) (string, error) {
	entries := map[string]any{}
	for _, s := range servers {
		entries[s.Name] = entry(s)
	}
	content, err := json.Marshal(map[string]any{key: entries})
	if err != nil {
		return "", err
	}
	file := filepath.Join(tmpDir, name)
	if err := os.WriteFile(file, content, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", file, err)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		return "", fmt.Errorf("chmod %s: %w", file, err)
	}
	return file, nil
}

func nonNilEnv(env map[string]string) map[string]string {
	if env == nil {
		return map[string]string{}
	}
	return env
}

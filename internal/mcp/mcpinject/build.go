package mcpinject

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

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
	copilotConfigFile   = "omac-mcp-copilot.json"
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
	case config.MCPInjectCodexFlag:
		return buildCodex(sorted)
	case config.MCPInjectCopilotFlag:
		args, err := buildCopilot(sorted, tmpDir, userArgs)
		if err != nil {
			return Injection{}, err
		}
		inj.InnerArgs = append(inj.InnerArgs, args...)
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

// buildCopilot passes the servers in a session-only config file with
// --additional-mcp-config @file, so omac never touches ~/.copilot/mcp-config.json.
// The flag and its "@file" form come from the public documentation and are
// unverified against a running Copilot CLI, as are the entry fields: "type"
// local, "command", "args", "env" and "tools" ["*"]. Whether Copilot accepts
// the flag twice is unknown, so a flag the user passes too is an error.
func buildCopilot(servers []Server, tmpDir string, userArgs []string) ([]string, error) {
	for _, a := range userArgs {
		if a == additionalMCPConfigFlag || strings.HasPrefix(a, additionalMCPConfigFlag+"=") {
			return nil, fmt.Errorf("cannot add registered MCP servers: %s is also passed after the omac flags; remove it or register the servers it defines", additionalMCPConfigFlag)
		}
	}
	file, err := writeServersFile(servers, tmpDir, copilotConfigFile, "mcpServers", func(s Server) map[string]any {
		return map[string]any{
			"type":    "local",
			"command": s.Argv[0],
			"args":    s.Argv[1:],
			"env":     nonNilEnv(s.Env),
			"tools":   []string{"*"},
		}
	})
	if err != nil {
		return nil, err
	}
	return []string{additionalMCPConfigFlag, "@" + file}, nil
}

var codexBareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// buildCodex passes each server as a -c mcp_servers.<name>={...} override, so
// omac never edits config.toml. Codex does not hand its own environment to MCP
// servers, so the bridge variables are named in env_vars and set in the
// harness environment; no value appears on the command line. The -c dotted-key
// override, an inline table as its value and the env_vars list come from the
// public documentation and are unverified against a running Codex. A name that
// is not a bare TOML key is refused, because quoted keys in -c are unverified.
func buildCodex(servers []Server) (Injection, error) {
	env := map[string]string{}
	var args []string
	for _, s := range servers {
		if !codexBareKey.MatchString(s.Name) {
			return Injection{}, fmt.Errorf("MCP server %q: Codex accepts only letters, digits, - and _ in a server name", s.Name)
		}
		names := make([]string, 0, len(s.Env))
		for k, v := range s.Env {
			if old, ok := env[k]; ok && old != v {
				return Injection{}, fmt.Errorf("MCP servers pass different values for %s, which Codex cannot take from one environment", k)
			}
			env[k] = v
			names = append(names, k)
		}
		sort.Strings(names)
		table := "{command = " + tomlString(s.Argv[0]) + ", args = " + tomlStringList(s.Argv[1:])
		if len(names) > 0 {
			table += ", env_vars = " + tomlStringList(names)
		}
		table += "}"
		args = append(args, "-c", "mcp_servers."+s.Name+"="+table)
	}
	allow := make([]string, 0, len(env))
	for k := range env {
		allow = append(allow, k)
	}
	sort.Strings(allow)
	return Injection{Env: env, AllowEnv: allow, InnerArgs: args}, nil
}

func tomlStringList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = tomlString(item)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// tomlString writes s as a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
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

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
	piExtensionFile     = "omac-mcp-pi.js"
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
	case config.MCPInjectPiExtension:
		args, env, allow, err := buildPi(sorted, tmpDir)
		if err != nil {
			return Injection{}, err
		}
		inj.InnerArgs = append(inj.InnerArgs, args...)
		inj.Env, inj.AllowEnv = env, allow
		return inj, nil
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
// user's own global file is not read for this run. From CodeWhale's source: the
// variable overrides mcp_config_path in config.toml and replaces the global
// file, while the project file (.codewhale/mcp.json, which the user approves
// separately) and plugin bundles stay active, so the "ignore" mode of
// mcp.unregistered does not apply to CodeWhale. The key mcpServers is the alias
// of servers. This was read in the source, not run.
func buildCodeWhale(servers []Server, tmpDir string) (Injection, error) {
	if err := refuseExpandedValues("CodeWhale", servers, "${"); err != nil {
		return Injection{}, err
	}
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
// The flag, its "@file" form and the entry fields ("type" local, "command",
// "args", "env", "tools" ["*"]) are in GitHub's documentation, and the changelog
// says the flag may be repeated; this was read, not run. Whether repeated flags
// merge by server name is not stated, so a flag the user passes too is an error.
func buildCopilot(servers []Server, tmpDir string, userArgs []string) ([]string, error) {
	for _, a := range userArgs {
		if a == additionalMCPConfigFlag || strings.HasPrefix(a, additionalMCPConfigFlag+"=") {
			return nil, fmt.Errorf("cannot add registered MCP servers: %s is also passed after the omac flags; remove it or register the servers it defines", additionalMCPConfigFlag)
		}
	}
	if err := refuseExpandedValues("Copilot CLI", servers, "$"); err != nil {
		return nil, err
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

// refuseExpandedValues refuses a server whose env value holds marker, the text
// the harness expands in env values (CodeWhale: ${NAME}; Copilot CLI: $VAR,
// ${VAR} and ${VAR:-default}). Neither documents an escape, so the value would
// reach the server changed.
func refuseExpandedValues(harness string, servers []Server, marker string) error {
	for _, s := range servers {
		for k, v := range s.Env {
			if strings.Contains(v, marker) {
				return fmt.Errorf("MCP server %q: %s expands %q in env values and has no escape, so the value of %s cannot be passed unchanged", s.Name, harness, marker, k)
			}
		}
	}
	return nil
}

var codexBareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// buildCodex passes each server as a -c mcp_servers.<name>={...} override, so
// omac never edits config.toml. Codex does not hand its own environment to MCP
// servers, so the bridge variables are named in env_vars and set in the
// harness environment; no value appears on the command line. From Codex's
// source: the value of -c is parsed as TOML (an inline table is valid), the
// override is deep-merged over config.toml, and env_vars lists the names Codex
// copies from its own environment to the server. This was read, not run. A name
// that is not a bare TOML key is refused, because the key is split at every dot.
func buildCodex(servers []Server) (Injection, error) {
	env := map[string]string{}
	var args []string
	for _, s := range servers {
		if !codexBareKey.MatchString(s.Name) {
			return Injection{}, fmt.Errorf("MCP server %q: Codex accepts only letters, digits, - and _ in a server name", s.Name)
		}
		names, err := mergeServerEnv(env, s, "Codex")
		if err != nil {
			return Injection{}, err
		}
		table := "{command = " + tomlString(s.Argv[0]) + ", args = " + tomlStringList(s.Argv[1:])
		if len(names) > 0 {
			table += ", env_vars = " + tomlStringList(names)
		}
		table += "}"
		args = append(args, "-c", "mcp_servers."+s.Name+"="+table)
	}
	return Injection{Env: env, AllowEnv: sortedEnvNames(env), InnerArgs: args}, nil
}

// mergeServerEnv adds the server's env to the one shared environment of the
// harness process and returns the sorted names. A name that two servers set to
// different values cannot be served from one environment.
func mergeServerEnv(env map[string]string, s Server, harness string) ([]string, error) {
	names := make([]string, 0, len(s.Env))
	for k, v := range s.Env {
		if old, ok := env[k]; ok && old != v {
			return nil, fmt.Errorf("MCP servers pass different values for %s, which %s cannot take from one environment", k, harness)
		}
		env[k] = v
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}

func sortedEnvNames(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var piServerName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// buildPi writes a small extension that registers the servers with
// pi.registerMcpServer and loads it with -e, so omac never edits mcp.json or
// pi's settings. pi documents -e (repeatable, loaded even with --no-extensions)
// and registerMcpServer(name, config) for this, and the file is code that runs
// inside pi, so it is written 0600 and only from omac's own data. The server
// entries carry no env: pi resolves $VAR and !command in env values, and the
// server process inherits pi's environment, so the bridge variables are set in
// the harness environment as for Codex. A server of the same name in pi's
// mcp.json takes precedence over the registration; the default mcp.unregistered
// refuse mode already stops a start that finds such an entry. Names that differ
// only in - versus _ share a namespace in pi and are refused.
func buildPi(servers []Server, tmpDir string) (args []string, env map[string]string, allow []string, err error) {
	env = map[string]string{}
	pairs := make([][2]any, 0, len(servers))
	seen := map[string]string{}
	for _, s := range servers {
		if !piServerName.MatchString(s.Name) {
			return nil, nil, nil, fmt.Errorf("MCP server %q: pi accepts only letters, digits, - and _ in a server name", s.Name)
		}
		ns := strings.ReplaceAll(s.Name, "-", "_")
		if other, ok := seen[ns]; ok {
			return nil, nil, nil, fmt.Errorf("MCP servers %q and %q differ only in - versus _, which pi treats as one name", other, s.Name)
		}
		seen[ns] = s.Name
		if _, err := mergeServerEnv(env, s, "pi"); err != nil {
			return nil, nil, nil, err
		}
		pairs = append(pairs, [2]any{s.Name, map[string]any{"command": s.Argv[0], "args": s.Argv[1:]}})
	}
	// An array of pairs, not an object literal: a server named __proto__ would
	// set the prototype of an object literal instead of an entry.
	data, err := json.Marshal(pairs)
	if err != nil {
		return nil, nil, nil, err
	}
	code := "export default function (pi) {\n  for (const [name, config] of " + string(data) + ") pi.registerMcpServer(name, config);\n}\n"
	file := filepath.Join(tmpDir, piExtensionFile)
	if err := os.WriteFile(file, []byte(code), 0o600); err != nil {
		return nil, nil, nil, fmt.Errorf("write %s: %w", file, err)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		return nil, nil, nil, fmt.Errorf("chmod %s: %w", file, err)
	}
	return []string{"-e", file}, env, sortedEnvNames(env), nil
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

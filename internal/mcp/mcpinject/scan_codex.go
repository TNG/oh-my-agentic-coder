package mcpinject

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

const codexConfigFlagLabel = "-c/--config (command line)"

func codexSources(host Host, managedDirs []string) []source {
	user := codexFile(filepath.Join(configHome(host, "codex"), "config.toml"))
	user.userLevel = true
	out := []source{user}
	for _, dir := range projectDirs(host.Workdir) {
		out = append(out, codexFile(filepath.Join(dir, ".codex", "config.toml")))
	}
	for _, dir := range managedDirs {
		src := codexFile(filepath.Join(dir, "config.toml"))
		src.managed, src.userLevel = true, true
		out = append(out, src)
	}
	if overrides := codexOverrides(host.InnerArgs); overrides != "" {
		src := inlineSource(codexConfigFlagLabel, []byte(overrides), parseCodexServers)
		src.userLevel = true
		out = append(out, src)
	}
	return out
}

func codexFile(file string) source {
	return parsedFileSource(file, parseCodexServers)
}

// codexOverrides turns the MCP settings of -c and --config arguments into one
// TOML document. Codex reads a value that is not valid TOML as a string.
func codexOverrides(args []string) string {
	var values []string
	for _, flag := range []string{"-c", "--config"} {
		values = append(values, singleFlagValues(args, flag)...)
	}
	var lines []string
	for _, value := range values {
		key, setting, ok := strings.Cut(value, "=")
		key = strings.TrimSpace(key)
		if !ok || (!strings.HasPrefix(key, "mcp_servers") && !strings.HasPrefix(key, "plugins")) {
			continue
		}
		line := key + " = " + setting
		if _, err := parseTOML([]byte(line)); err != nil {
			line = key + " = " + fmt.Sprintf("%q", setting)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// parseCodexServers reads [mcp_servers.<name>] tables and the mcp_servers
// tables of plugins.
func parseCodexServers(raw []byte) ([]Existing, error) {
	doc, err := parseTOML(raw)
	if err != nil {
		return nil, err
	}
	out := codexServers(doc["mcp_servers"], false)
	plugins, _ := doc["plugins"].(tomlTable)
	names := make([]string, 0, len(plugins))
	for name := range plugins {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if plugin, ok := plugins[name].(tomlTable); ok {
			out = append(out, codexServers(plugin["mcp_servers"], true)...)
		}
	}
	return out, nil
}

func codexServers(value any, pluginProvided bool) []Existing {
	servers, _ := value.(tomlTable)
	out := make([]Existing, 0, len(servers))
	for name, entry := range servers {
		table, _ := entry.(tomlTable)
		e := decodeCodexEntry(table)
		e.Name, e.PluginProvided = name, pluginProvided
		out = append(out, e)
	}
	return out
}

func decodeCodexEntry(t tomlTable) Existing {
	e := Existing{EnvKeys: tableKeys(t["env"]), HeaderKeys: append(tableKeys(t["http_headers"]), tableKeys(t["env_http_headers"])...)}
	e.Command, _ = t["command"].(string)
	args, _ := t["args"].([]any)
	for _, arg := range args {
		if s, ok := arg.(string); ok {
			e.Args = append(e.Args, s)
		}
	}
	vars, _ := t["env_vars"].([]any)
	for _, v := range vars {
		switch item := v.(type) {
		case string:
			e.EnvKeys = append(e.EnvKeys, item)
		case tomlTable:
			if name, ok := item["name"].(string); ok {
				e.EnvKeys = append(e.EnvKeys, name)
			}
		}
	}
	if name, ok := t["bearer_token_env_var"].(string); ok && name != "" {
		e.EnvKeys = append(e.EnvKeys, name)
	}
	switch {
	case t["url"] != nil:
		e.Transport = "http"
	case e.Command != "":
		e.Transport = "stdio"
	}
	return e
}

func tableKeys(value any) []string {
	table, _ := value.(tomlTable)
	keys := make([]string, 0, len(table))
	for k := range table {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

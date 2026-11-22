package mcpinject

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	mcpConfigFlag    = "--mcp-config"
	agentsFlag       = "--agents"
	addDirFlag       = "--add-dir"
	pluginDirFlag    = "--plugin-dir"
	inlineJSONPrefix = "{"
)

func claudeSources(host Host, managedDirs []string) []source {
	configDir := host.env("CLAUDE_CONFIG_DIR")
	homes := []string{filepath.Join(host.Home, ".claude")}
	userFiles := []string{filepath.Join(host.Home, ".claude.json")}
	if configDir != "" {
		configDir = absolute(host.Workdir, configDir)
		homes = append(homes, configDir)
		userFiles = append(userFiles, filepath.Join(configDir, ".claude.json"))
	}
	userLevel := func(src source) source { src.userLevel = true; return src }
	claudeFile := func(file, key string, projectKeys []string) source {
		return fileSource(file, key, projectKeys, decodeClaudeEntry)
	}

	var out []source
	projectKeys := projectKeyCandidates(host.Workdir)
	for _, file := range userFiles {
		out = append(out,
			userLevel(claudeFile(file, "mcpServers", nil)),
			claudeFile(file, "mcpServers", projectKeys))
	}
	for _, dir := range ancestors(host.Workdir) {
		out = append(out, claudeFile(filepath.Join(dir, ".mcp.json"), "mcpServers", nil))
	}
	for _, home := range homes {
		out = append(out, userLevel(claudeFile(filepath.Join(home, "settings.json"), "mcpServers", nil)))
	}
	out = append(out,
		claudeFile(filepath.Join(host.Workdir, ".claude", "settings.json"), "mcpServers", nil),
		claudeFile(filepath.Join(host.Workdir, ".claude", "settings.local.json"), "mcpServers", nil))

	for _, dir := range managedDirs {
		for _, src := range []source{
			claudeFile(filepath.Join(dir, "managed-mcp.json"), "mcpServers", nil),
			claudeFile(filepath.Join(dir, "managed-settings.json"), "managedMcpServers", nil),
		} {
			src.managed, src.userLevel = true, true
			out = append(out, src)
		}
	}

	for _, value := range flagValues(host.InnerArgs, mcpConfigFlag) {
		out = append(out, jsonOrFileSource(host.Workdir, value, parseClaudeServers, mcpConfigFlag+" (inline JSON)"))
	}

	var agentDirs []string
	for _, dir := range ancestors(host.Workdir) {
		agentDirs = append(agentDirs, filepath.Join(dir, ".claude", "agents"))
	}
	for _, dir := range flagValues(host.InnerArgs, addDirFlag) {
		agentDirs = append(agentDirs, filepath.Join(absolute(host.Workdir, dir), ".claude", "agents"))
	}
	for _, home := range homes {
		out = append(out, agentSources(filepath.Join(home, "agents"), true)...)
	}
	for _, dir := range agentDirs {
		out = append(out, agentSources(dir, false)...)
	}
	for _, value := range flagValues(host.InnerArgs, agentsFlag) {
		out = append(out, jsonOrFileSource(host.Workdir, value, parseCLIAgents, agentsFlag+" (inline JSON)"))
	}
	for _, home := range homes {
		out = append(out, claudePluginSources(home)...)
	}
	for _, dir := range flagValues(host.InnerArgs, pluginDirFlag) {
		out = append(out, pluginDirSources(absolute(host.Workdir, dir))...)
	}
	return out
}

func projectKeyCandidates(workdir string) []string {
	keys := []string{workdir}
	if resolved, err := filepath.EvalSymlinks(workdir); err == nil && resolved != workdir {
		keys = append(keys, resolved)
	}
	return keys
}

// flagValues returns the values the user passes to a Claude Code flag that
// takes several, as in "--mcp-config a.json b.json" or "--mcp-config=a.json".
func flagValues(args []string, flag string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], flag+"="):
			out = append(out, strings.TrimPrefix(args[i], flag+"="))
		case args[i] == flag:
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				out = append(out, args[i])
			}
		}
	}
	return out
}

func jsonOrFileSource(workdir, value string, parse func([]byte) ([]Existing, error), inlineLabel string) source {
	if strings.HasPrefix(strings.TrimSpace(value), inlineJSONPrefix) {
		return inlineSource(inlineLabel, []byte(value), parse)
	}
	file := absolute(workdir, value)
	return source{file: file, load: readSource(file, parse)}
}

func parseClaudeServers(raw []byte) ([]Existing, error) {
	return serversUnder(raw, "mcpServers", nil, decodeClaudeEntry)
}

// parseCLIAgents reads the value of --agents: a map from agent name to a
// definition that may carry mcpServers.
func parseCLIAgents(raw []byte) ([]Existing, error) {
	doc, err := parseDoc(raw)
	if err != nil {
		return nil, err
	}
	var out []Existing
	for _, def := range doc {
		var agent struct{ McpServers []json.RawMessage }
		_ = json.Unmarshal(def, &agent)
		out = append(out, agentServers(agent.McpServers)...)
	}
	return out, nil
}

// agentServers keeps the inline definitions of an agent's mcpServers list.
// A string entry names a server defined elsewhere.
func agentServers(list []json.RawMessage) []Existing {
	var out []Existing
	for _, item := range list {
		var inline map[string]json.RawMessage
		if json.Unmarshal(item, &inline) == nil {
			out = append(out, entriesOf(inline, decodeClaudeEntry)...)
		}
	}
	return out
}

// agentSources lists one source per Markdown file under dir. Each reads the
// mcpServers of the file's YAML frontmatter.
func agentSources(dir string, userLevel bool) []source {
	var out []source
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".md") {
			out = append(out, source{file: path, userLevel: userLevel, load: readSource(path, parseAgentFile)})
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		out = append(out, source{file: dir, load: func() ([]Existing, error) { return nil, err }})
	}
	return out
}

func parseAgentFile(raw []byte) ([]Existing, error) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return nil, nil
	}
	front, _, ok := strings.Cut(rest, "\n---")
	if !ok || !strings.Contains(front, "mcpServers") {
		return nil, nil
	}
	var meta struct {
		McpServers []any `yaml:"mcpServers"`
	}
	if err := yaml.Unmarshal([]byte(front), &meta); err != nil {
		return nil, fmt.Errorf("%w: %v", errParse, err)
	}
	var list []json.RawMessage
	for _, item := range meta.McpServers {
		encoded, err := json.Marshal(jsonCompatible(item))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errParse, err)
		}
		list = append(list, encoded)
	}
	return agentServers(list), nil
}

// jsonCompatible turns the map[any]any values yaml can produce into
// string-keyed maps encoding/json accepts.
func jsonCompatible(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = jsonCompatible(e)
		}
	case []any:
		for i, e := range x {
			x[i] = jsonCompatible(e)
		}
	}
	return v
}

// pluginDirSources covers a plugin directory passed with --plugin-dir: the
// .mcp.json at its root and the mcpServers of .claude-plugin/plugin.json
// given inline or as a .json file path inside the plugin.
func pluginDirSources(dir string) []source {
	manifest := filepath.Join(dir, ".claude-plugin", "plugin.json")
	out := []source{
		parsedFileSource(filepath.Join(dir, ".mcp.json"), parsePluginServersFile),
		parsedFileSource(manifest, func(raw []byte) ([]Existing, error) { return parsePluginManifest(dir, raw) }),
	}
	for i := range out {
		out[i].pluginProvided = true
	}
	return out
}

func parsePluginManifest(dir string, raw []byte) ([]Existing, error) {
	doc, err := parseDoc(raw)
	if err != nil {
		return nil, err
	}
	var forms []json.RawMessage
	if json.Unmarshal(doc["mcpServers"], &forms) != nil {
		forms = []json.RawMessage{doc["mcpServers"]}
	}
	var out []Existing
	for _, form := range forms {
		var path string
		if json.Unmarshal(form, &path) == nil {
			file := filepath.Join(dir, filepath.FromSlash(path))
			content, err := os.ReadFile(file)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, err
			}
			servers, err := parsePluginServersFile(content)
			if err != nil {
				return nil, err
			}
			out = append(out, servers...)
			continue
		}
		var inline map[string]json.RawMessage
		if json.Unmarshal(form, &inline) == nil {
			out = append(out, entriesOf(inline, decodeClaudeEntry)...)
		}
	}
	return out, nil
}

// parsePluginServersFile reads a .json file a plugin manifest names, which
// holds the server map either at its top level or under "mcpServers".
func parsePluginServersFile(raw []byte) ([]Existing, error) {
	doc, err := parseDoc(raw)
	if err != nil {
		return nil, err
	}
	servers := map[string]json.RawMessage{}
	if nested, ok := doc["mcpServers"]; ok {
		_ = json.Unmarshal(nested, &servers)
	} else {
		for name, entry := range doc {
			if isJSONObject(entry) {
				servers[name] = entry
			}
		}
	}
	return entriesOf(servers, decodeClaudeEntry), nil
}

package mcpinject

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

const (
	// FindingParseError marks a config file omac could not read or parse; the
	// file may define servers omac cannot see.
	FindingParseError = "parse-error"
)

const (
	harnessOpenCode   = "opencode"
	harnessClaudeCode = "claude-code"
)

// Scan lists the MCP servers the harness already knows on this host. Files
// that exist but cannot be read or parsed are reported as FindingParseError
// findings.
func Scan(kind config.MCPInjectionKind, workdir, home string) ([]Existing, []Finding, error) {
	var (
		harness string
		sources []source
	)
	switch kind {
	case config.MCPInjectOpenCodeEnv:
		harness, sources = harnessOpenCode, openCodeSources(workdir, home)
	case config.MCPInjectClaudeFlag:
		harness, sources = harnessClaudeCode, claudeSources(workdir, home)
	default:
		return nil, nil, fmt.Errorf("MCP injection kind %d has no config scan", kind)
	}

	var existing []Existing
	var findings []Finding
	reportedParse := map[string]bool{}
	for _, src := range sources {
		servers, err := src.load()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			if reportedParse[src.file] {
				continue
			}
			reportedParse[src.file] = true
			findings = append(findings, Finding{File: src.file, Kind: FindingParseError, Detail: err.Error()})
			continue
		}
		sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
		for _, e := range servers {
			e.Harness, e.File, e.UserLevel = harness, src.file, src.userLevel
			existing = append(existing, e)
		}
	}
	return existing, findings, nil
}

type source struct {
	file      string
	userLevel bool
	load      func() ([]Existing, error)
}

func openCodeSources(workdir, home string) []source {
	root := os.Getenv("XDG_CONFIG_HOME")
	if root == "" {
		root = filepath.Join(home, ".config")
	}
	var out []source
	for _, dir := range []string{filepath.Join(root, "opencode"), workdir} {
		for _, name := range []string{"opencode.json", "opencode.jsonc"} {
			src := fileSource(filepath.Join(dir, name), "mcp", nil, decodeOpenCodeEntry)
			src.userLevel = dir != workdir
			out = append(out, src)
		}
	}
	return out
}

func claudeSources(workdir, home string) []source {
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	userFile := filepath.Join(home, ".claude.json")
	settingsDir := filepath.Join(home, ".claude")
	if configDir != "" {
		userFile = filepath.Join(configDir, ".claude.json")
		settingsDir = configDir
	}
	projectKeys := projectKeyCandidates(workdir)
	user := func(file string) source {
		src := fileSource(file, "mcpServers", nil, decodeClaudeEntry)
		src.userLevel = true
		return src
	}
	project := func(file string) source { return fileSource(file, "mcpServers", nil, decodeClaudeEntry) }
	return []source{
		user(userFile),
		fileSource(userFile, "mcpServers", projectKeys, decodeClaudeEntry),
		project(filepath.Join(workdir, ".mcp.json")),
		user(filepath.Join(settingsDir, "settings.json")),
		project(filepath.Join(workdir, ".claude", "settings.json")),
		project(filepath.Join(workdir, ".claude", "settings.local.json")),
	}
}

func projectKeyCandidates(workdir string) []string {
	keys := []string{workdir}
	if resolved, err := filepath.EvalSymlinks(workdir); err == nil && resolved != workdir {
		keys = append(keys, resolved)
	}
	return keys
}

// fileSource reads serversKey from the file's top level, or, when
// projectKeys is set, from the first matching entry of its "projects" map.
// Entry values are decoded by decode, which keeps names of env and header
// keys and drops their values.
func fileSource(file, serversKey string, projectKeys []string, decode func(json.RawMessage) Existing) source {
	return source{file: file, load: func() ([]Existing, error) {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var doc map[string]json.RawMessage
		if err := parseJSONC(raw, &doc); err != nil {
			return nil, fmt.Errorf("parse: %w", err)
		}
		scope := doc
		if projectKeys != nil {
			scope = nil
			var projects map[string]map[string]json.RawMessage
			_ = json.Unmarshal(doc["projects"], &projects)
			for _, key := range projectKeys {
				if entry, ok := projects[key]; ok {
					scope = entry
					break
				}
			}
		}
		var servers map[string]json.RawMessage
		_ = json.Unmarshal(scope[serversKey], &servers)
		out := make([]Existing, 0, len(servers))
		for name, entry := range servers {
			e := decode(entry)
			e.Name = name
			out = append(out, e)
		}
		return out, nil
	}}
}

// claudeEntry is the Claude Code shape of an MCP server. Env and header
// values are decoded raw so they are dropped without being read.
type claudeEntry struct {
	Type    string                     `json:"type"`
	Command string                     `json:"command"`
	Args    []string                   `json:"args"`
	Env     map[string]json.RawMessage `json:"env"`
	URL     string                     `json:"url"`
	Headers map[string]json.RawMessage `json:"headers"`
}

func decodeClaudeEntry(raw json.RawMessage) Existing {
	var c claudeEntry
	if err := json.Unmarshal(raw, &c); err != nil {
		return Existing{}
	}
	transport := c.Type
	if transport == "" {
		switch {
		case c.URL != "":
			transport = "http"
		case c.Command != "":
			transport = "stdio"
		}
	}
	return Existing{
		Transport:  transport,
		Command:    c.Command,
		Args:       c.Args,
		EnvKeys:    sortedKeys(c.Env),
		HeaderKeys: sortedKeys(c.Headers),
	}
}

// openCodeEntry is the OpenCode shape of an MCP server.
type openCodeEntry struct {
	Type        string                     `json:"type"`
	Command     []string                   `json:"command"`
	Environment map[string]json.RawMessage `json:"environment"`
	URL         string                     `json:"url"`
	Headers     map[string]json.RawMessage `json:"headers"`
}

func decodeOpenCodeEntry(raw json.RawMessage) Existing {
	var o openCodeEntry
	if err := json.Unmarshal(raw, &o); err != nil {
		return Existing{}
	}
	e := Existing{EnvKeys: sortedKeys(o.Environment), HeaderKeys: sortedKeys(o.Headers)}
	switch {
	case o.Type == "remote" || (o.Type == "" && o.URL != ""):
		e.Transport = "remote"
	case len(o.Command) > 0:
		e.Transport = "stdio"
		e.Command, e.Args = o.Command[0], o.Command[1:]
	}
	return e
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

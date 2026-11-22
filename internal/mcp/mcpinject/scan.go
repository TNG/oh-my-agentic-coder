package mcpinject

import (
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
		names, err := src.load()
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
		sort.Strings(names)
		for _, name := range names {
			existing = append(existing, Existing{Harness: harness, File: src.file, Name: name})
		}
	}
	return existing, findings, nil
}

type source struct {
	file string
	load func() ([]string, error)
}

func openCodeSources(workdir, home string) []source {
	root := os.Getenv("XDG_CONFIG_HOME")
	if root == "" {
		root = filepath.Join(home, ".config")
	}
	var out []source
	for _, dir := range []string{filepath.Join(root, "opencode"), workdir} {
		for _, name := range []string{"opencode.json", "opencode.jsonc"} {
			out = append(out, fileSource(filepath.Join(dir, name), "mcp", nil))
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
	return []source{
		fileSource(userFile, "mcpServers", nil),
		fileSource(userFile, "mcpServers", projectKeys),
		fileSource(filepath.Join(workdir, ".mcp.json"), "mcpServers", nil),
		fileSource(filepath.Join(settingsDir, "settings.json"), "mcpServers", nil),
		fileSource(filepath.Join(workdir, ".claude", "settings.json"), "mcpServers", nil),
		fileSource(filepath.Join(workdir, ".claude", "settings.local.json"), "mcpServers", nil),
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
func fileSource(file, serversKey string, projectKeys []string) source {
	return source{file: file, load: func() ([]string, error) {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var doc map[string]any
		if err := parseJSONC(raw, &doc); err != nil {
			return nil, fmt.Errorf("parse: %w", err)
		}
		scope := doc
		if projectKeys != nil {
			scope = nil
			projects, _ := doc["projects"].(map[string]any)
			for _, key := range projectKeys {
				if entry, ok := projects[key].(map[string]any); ok {
					scope = entry
					break
				}
			}
		}
		servers, _ := scope[serversKey].(map[string]any)
		names := make([]string, 0, len(servers))
		for name := range servers {
			names = append(names, name)
		}
		return names, nil
	}}
}

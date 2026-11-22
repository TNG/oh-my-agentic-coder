package mcpinject

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

const (
	// FindingLiteralCredential marks a literal secret in an MCP definition.
	FindingLiteralCredential = "literal-credential"
	// FindingParseError marks a config file omac could not read or parse; the
	// file may define servers or credentials omac cannot see.
	FindingParseError = "parse-error"
)

const (
	harnessOpenCode   = "opencode"
	harnessClaudeCode = "claude-code"
)

var (
	envReference     = regexp.MustCompile(`\{env:[A-Za-z_][A-Za-z0-9_]*\}|\$\{[^}]+\}|\$[A-Za-z_][A-Za-z0-9_]*`)
	secretParamNames = []string{"token", "key", "secret", "password", "auth"}
)

type serverMap map[string]map[string]any

// Scan reads the MCP servers the harness already knows on this host and
// reports literal credentials in them. Files that exist but cannot be read
// or parsed are reported as FindingParseError findings.
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
			findings = append(findings, Finding{File: src.file, Kind: FindingParseError, Field: err.Error()})
			continue
		}
		names := make([]string, 0, len(servers))
		for name := range servers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			existing = append(existing, Existing{Harness: harness, File: src.file, Name: name})
			findings = append(findings, credentialFindings(src.file, name, servers[name])...)
		}
	}
	return existing, findings, nil
}

type source struct {
	file string
	load func() (serverMap, error)
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
	return source{file: file, load: func() (serverMap, error) {
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
		return serversFrom(scope[serversKey]), nil
	}}
}

func serversFrom(v any) serverMap {
	raw, _ := v.(map[string]any)
	out := serverMap{}
	for name, def := range raw {
		m, _ := def.(map[string]any)
		out[name] = m
	}
	return out
}

func credentialFindings(file, server string, def map[string]any) []Finding {
	var out []Finding
	add := func(field string) {
		out = append(out, Finding{File: file, Server: server, Field: field, Kind: FindingLiteralCredential})
	}
	for _, block := range []string{"headers", "env", "environment"} {
		values, _ := def[block].(map[string]any)
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if s, ok := values[k].(string); ok && isLiteral(s) {
				add(block + "." + k)
			}
		}
	}
	if raw, ok := def["url"].(string); ok {
		for _, field := range urlCredentialFields(raw) {
			add(field)
		}
	}
	return out
}

func isLiteral(s string) bool {
	return s != "" && !envReference.MatchString(s)
}

func urlCredentialFields(raw string) []string {
	var out []string
	if userinfo := urlUserinfo(raw); isLiteral(userinfo) {
		out = append(out, "url.userinfo")
	}
	_, query, found := strings.Cut(raw, "?")
	if !found {
		return out
	}
	query, _, _ = strings.Cut(query, "#")
	params, _ := url.ParseQuery(query)
	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !secretLikeName(name) {
			continue
		}
		for _, value := range params[name] {
			if isLiteral(value) {
				out = append(out, "url.query."+name)
				break
			}
		}
	}
	return out
}

func urlUserinfo(raw string) string {
	_, rest, found := strings.Cut(raw, "://")
	if !found {
		return ""
	}
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		rest = rest[:end]
	}
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return ""
	}
	return rest[:at]
}

func secretLikeName(name string) bool {
	lower := strings.ToLower(name)
	for _, part := range secretParamNames {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}

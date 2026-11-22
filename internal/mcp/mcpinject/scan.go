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
	harnessOpenCode   = "opencode"
	harnessClaudeCode = "claude-code"
)

// Host is what the scan needs to know about the machine and the launch.
type Host struct {
	Workdir string
	Home    string
	// Getenv looks up the host environment; nil means an empty environment.
	Getenv func(string) string
	// InnerArgs are the arguments the user passes to the harness after the omac flags.
	InnerArgs []string
}

func (h Host) env(name string) string {
	if h.Getenv == nil {
		return ""
	}
	return h.Getenv(name)
}

// managedDirs are the administrator-managed config directories per harness.
type managedDirs struct {
	openCode []string
	claude   []string
}

var systemManagedDirs = managedDirs{
	openCode: []string{"/Library/Application Support/opencode", "/etc/opencode"},
	claude:   []string{"/Library/Application Support/ClaudeCode", "/etc/claude-code"},
}

// Scan lists the MCP servers the harness already knows on this host. Files
// that exist but cannot be read or parsed are reported as findings.
func Scan(kind config.MCPInjectionKind, host Host) ([]Existing, []Finding, error) {
	return scan(kind, host, systemManagedDirs)
}

func scan(kind config.MCPInjectionKind, host Host, managed managedDirs) ([]Existing, []Finding, error) {
	var (
		harness string
		sources []source
	)
	switch kind {
	case config.MCPInjectOpenCodeEnv:
		harness, sources = harnessOpenCode, openCodeSources(host, managed.openCode)
	case config.MCPInjectClaudeFlag:
		harness, sources = harnessClaudeCode, claudeSources(host, managed.claude)
	default:
		return nil, nil, fmt.Errorf("MCP injection kind %d has no config scan", kind)
	}

	var existing []Existing
	var findings []Finding
	seen, reported := map[string]bool{}, map[string]bool{}
	for _, src := range sources {
		if seen[src.file+"\x00"+src.id] {
			continue
		}
		seen[src.file+"\x00"+src.id] = true
		servers, err := src.load()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			if reported[src.file] {
				continue
			}
			reported[src.file] = true
			kind := FindingUnreadable
			if errors.Is(err, errParse) {
				kind = FindingParseError
			}
			findings = append(findings, Finding{File: src.file, Kind: kind, Detail: err.Error(), Managed: src.managed})
			continue
		}
		sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
		for _, e := range servers {
			e.Harness, e.File, e.UserLevel, e.Managed = harness, src.file, src.userLevel, src.managed
			existing = append(existing, e)
		}
	}
	return existing, findings, nil
}

// source is one place that can define MCP servers. id tells apart several
// sources that share a file, such as the user and the local scope of ~/.claude.json.
type source struct {
	file      string
	id        string
	userLevel bool
	managed   bool
	load      func() ([]Existing, error)
}

var errParse = errors.New("parse")

func readSource(file string, parse func([]byte) ([]Existing, error)) func() ([]Existing, error) {
	return func() ([]Existing, error) {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		return parse(raw)
	}
}

func parseDoc(raw []byte) (map[string]json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if err := parseJSONC(raw, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", errParse, err)
	}
	return doc, nil
}

func entriesOf(servers map[string]json.RawMessage, decode func(json.RawMessage) Existing) []Existing {
	out := make([]Existing, 0, len(servers))
	for name, entry := range servers {
		e := decode(entry)
		e.Name = name
		out = append(out, e)
	}
	return out
}

// serversUnder decodes the map of server entries stored under key of the
// document's top level, or of the first matching entry of its "projects" map
// when projectKeys is set. Entry values are decoded by decode, which keeps
// names of env and header keys and drops their values.
func serversUnder(raw []byte, key string, projectKeys []string, decode func(json.RawMessage) Existing) ([]Existing, error) {
	doc, err := parseDoc(raw)
	if err != nil {
		return nil, err
	}
	scope := doc
	if projectKeys != nil {
		scope = nil
		var projects map[string]map[string]json.RawMessage
		_ = json.Unmarshal(doc["projects"], &projects)
		for _, k := range projectKeys {
			if entry, ok := projects[k]; ok {
				scope = entry
				break
			}
		}
	}
	var servers map[string]json.RawMessage
	_ = json.Unmarshal(scope[key], &servers)
	return entriesOf(servers, decode), nil
}

func fileSource(file, key string, projectKeys []string, decode func(json.RawMessage) Existing) source {
	return source{file: file, id: key + fmt.Sprint(projectKeys), load: readSource(file, func(raw []byte) ([]Existing, error) {
		return serversUnder(raw, key, projectKeys, decode)
	})}
}

func inlineSource(label string, raw []byte, parse func([]byte) ([]Existing, error)) source {
	return source{file: label, load: func() ([]Existing, error) { return parse(raw) }}
}

// ancestors lists dir and each of its parents up to the filesystem root.
func ancestors(dir string) []string {
	var out []string
	for {
		out = append(out, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			return out
		}
		dir = parent
	}
}

func absolute(workdir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(workdir, path)
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

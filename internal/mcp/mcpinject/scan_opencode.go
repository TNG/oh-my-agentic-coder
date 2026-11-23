package mcpinject

import (
	"encoding/json"
	"path/filepath"
)

const openCodeConfigContentLabel = "OPENCODE_CONFIG_CONTENT (environment variable)"

var openCodeConfigNames = []string{"opencode.json", "opencode.jsonc"}

func openCodeSources(host Host, managedDirs []string) []source {
	root := host.env("XDG_CONFIG_HOME")
	if root == "" {
		root = filepath.Join(host.Home, ".config")
	}
	var out []source
	addDir := func(dir string, userLevel, managed bool) {
		for _, name := range openCodeConfigNames {
			src := openCodeFile(filepath.Join(dir, name))
			src.userLevel, src.managed = userLevel, managed
			out = append(out, src)
		}
	}
	addDir(filepath.Join(root, "opencode"), true, false)
	if dir := host.env("OPENCODE_CONFIG_DIR"); dir != "" {
		addDir(absolute(host.Workdir, dir), true, false)
	}
	if file := host.env("OPENCODE_CONFIG"); file != "" {
		src := openCodeFile(absolute(host.Workdir, file))
		src.userLevel = true
		out = append(out, src)
	}
	for _, dir := range ancestors(host.Workdir) {
		addDir(dir, false, false)
		addDir(filepath.Join(dir, ".opencode"), false, false)
	}
	if content := host.env("OPENCODE_CONFIG_CONTENT"); content != "" {
		src := inlineSource(openCodeConfigContentLabel, []byte(content), parseOpenCodeDoc)
		src.userLevel = true
		out = append(out, src)
	}
	for _, dir := range managedDirs {
		addDir(dir, true, true)
	}
	return out
}

func openCodeFile(file string) source {
	return source{file: file, load: readSource(file, parseOpenCodeDoc)}
}

// parseOpenCodeDoc reads the servers of an OpenCode config. Version 1 keeps
// them directly under "mcp"; version 2 under "mcp.servers". "timeout" is an
// option, and an entry that only sets "enabled" defines no server.
func parseOpenCodeDoc(raw []byte) ([]Existing, error) {
	doc, err := parseDoc(raw)
	if err != nil {
		return nil, err
	}
	var mcp map[string]json.RawMessage
	_ = json.Unmarshal(doc["mcp"], &mcp)
	servers := map[string]json.RawMessage{}
	var stubs []Existing
	for name, entry := range mcp {
		switch {
		case name == "servers" && !isOpenCodeServer(entry):
			var nested map[string]json.RawMessage
			_ = json.Unmarshal(entry, &nested)
			for k, v := range nested {
				servers[k] = v
			}
		case name == "timeout" && !isOpenCodeServer(entry):
		case isEnabledOnly(entry):
			stubs = append(stubs, Existing{Name: name, enabledOnly: true, disabled: !openCodeEnabled(entry)})
		default:
			servers[name] = entry
		}
	}
	return append(entriesOf(servers, decodeOpenCodeEntry), stubs...), nil
}

func openCodeEnabled(raw json.RawMessage) bool {
	var o struct{ Enabled *bool }
	return json.Unmarshal(raw, &o) != nil || o.Enabled == nil || *o.Enabled
}

// dropDisabledOpenCode removes the entries OpenCode does not start. Configs merge by
// server name, so a server is skipped only when every place that mentions the name
// turns it off. Any definition or override that may enable it keeps it reported.
// The stubs that only set "enabled" never appear in the result.
func dropDisabledOpenCode(all []Existing) []Existing {
	enabled := map[string]bool{}
	for _, e := range all {
		if !e.disabled {
			enabled[e.Name] = true
		}
	}
	var out []Existing
	for _, e := range all {
		if e.enabledOnly || (e.disabled && !enabled[e.Name]) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func isOpenCodeServer(raw json.RawMessage) bool {
	var o struct{ Type string }
	_ = json.Unmarshal(raw, &o)
	return o.Type == "local" || o.Type == "remote"
}

func isEnabledOnly(raw json.RawMessage) bool {
	var o map[string]json.RawMessage
	if json.Unmarshal(raw, &o) != nil {
		return false
	}
	_, typed := o["type"]
	var enabled bool
	return !typed && json.Unmarshal(o["enabled"], &enabled) == nil
}

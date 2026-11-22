package mcpinject

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// claudePluginSources covers the plugins Claude Code loads from its config
// directory: the bundles under plugins/synced and the install paths listed in
// plugins/installed_plugins.json. plugins/marketplaces holds catalogs of
// plugins that are available but not loaded, so it is not scanned. The layout
// is observed on a machine, not documented.
//
// disabled holds the plugin keys ("name@marketplace") that settings turn off.
// Their servers do not load, so they are not scanned. A plugin whose key cannot
// be determined is scanned.
func claudePluginSources(configDir string, disabled map[string]bool) []source {
	pluginsDir := filepath.Join(configDir, "plugins")
	syncedDir := filepath.Join(pluginsDir, "synced")
	var out []source
	err := walkFiles(syncedDir, func(path string) {
		bundle := filepath.Dir(path)
		if disabled[syncedPluginKey(syncedDir, path)] {
			return
		}
		switch {
		case filepath.Base(path) == ".mcp.json":
			src := parsedFileSource(path, parsePluginServersFile)
			src.pluginProvided = true
			out = append(out, src)
		case filepath.Base(path) == "plugin.json" && filepath.Base(bundle) == ".claude-plugin":
			src := parsedFileSource(path, func(raw []byte) ([]Existing, error) {
				return parsePluginManifest(filepath.Dir(bundle), raw)
			})
			src.pluginProvided = true
			out = append(out, src)
		}
	})
	if err != nil {
		out = append(out, failingSource(syncedDir, err))
	}
	return append(out, installedPluginSources(configDir, filepath.Join(pluginsDir, "installed_plugins.json"), disabled)...)
}

// installedPluginSources reads installed_plugins.json and scans each install
// path it lists. The file's shape is unverified, so every "installPath" string
// anywhere in the document counts, and a "plugins" entry from which no path
// can be read is a parse error.
func installedPluginSources(configDir, file string, disabled map[string]bool) []source {
	raw, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return nil
	}
	var paths []string
	if err == nil {
		paths, err = installPaths(raw)
	}
	if err != nil {
		return []source{failingSource(file, err)}
	}
	skip := disabledInstallPaths(raw, disabled)
	var out []source
	for _, path := range paths {
		if skip[path] {
			continue
		}
		out = append(out, pluginDirSources(absolute(configDir, path))...)
	}
	return out
}

func installPaths(raw []byte) ([]string, error) {
	doc, err := parseDoc(raw)
	if err != nil {
		return nil, err
	}
	var paths []string
	var collect func(value any)
	collect = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, item := range v {
				if path, ok := item.(string); ok && key == "installPath" && path != "" {
					paths = append(paths, path)
					continue
				}
				collect(item)
			}
		case []any:
			for _, item := range v {
				collect(item)
			}
		}
	}
	for _, entry := range doc {
		var decoded any
		if err := json.Unmarshal(entry, &decoded); err != nil {
			return nil, fmt.Errorf("%w: %v", errParse, err)
		}
		collect(decoded)
	}
	var plugins any
	_ = json.Unmarshal(doc["plugins"], &plugins)
	if len(paths) == 0 && !isEmptyJSONValue(plugins) {
		return nil, fmt.Errorf(`%w: "plugins" lists no installPath`, errParse)
	}
	sort.Strings(paths)
	return paths, nil
}

func isEmptyJSONValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	}
	return false
}

// syncedPluginKey returns "<name>@synced" for the plugin bundle that holds
// file under the synced directory. The directory names are opaque ids, so the
// name comes from the plugin's own manifest. It returns "" when no bundle with
// a readable name encloses the file.
func syncedPluginKey(syncedDir, file string) string {
	for dir := filepath.Dir(file); dir != syncedDir && strings.HasPrefix(dir, syncedDir); dir = filepath.Dir(dir) {
		raw, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
		if err != nil {
			continue
		}
		var manifest struct {
			Name string `json:"name"`
		}
		if parseJSONC(raw, &manifest) != nil || manifest.Name == "" {
			return ""
		}
		return manifest.Name + "@synced"
	}
	return ""
}

// disabledInstallPaths returns the install paths of the plugins in
// installed_plugins.json whose "name@marketplace" key is disabled.
func disabledInstallPaths(raw []byte, disabled map[string]bool) map[string]bool {
	out := map[string]bool{}
	if len(disabled) == 0 {
		return out
	}
	doc, err := parseDoc(raw)
	if err != nil {
		return out
	}
	var plugins map[string]json.RawMessage
	if json.Unmarshal(doc["plugins"], &plugins) != nil {
		return out
	}
	for key, entry := range plugins {
		if !disabled[key] {
			continue
		}
		paths, err := installPaths([]byte(`{"plugins":` + string(entry) + `}`))
		if err != nil {
			continue
		}
		for _, p := range paths {
			out[p] = true
		}
	}
	return out
}

// disabledClaudePlugins reads enabledPlugins from the settings files, which
// override in order. A settings file that cannot be read or parsed changes nothing,
// so the plugin stays scanned.
func disabledClaudePlugins(files []string) map[string]bool {
	state := map[string]bool{}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		doc, err := parseDoc(raw)
		if err != nil {
			continue
		}
		var enabled map[string]json.RawMessage
		if json.Unmarshal(doc["enabledPlugins"], &enabled) != nil {
			continue
		}
		for key, v := range enabled {
			var on bool
			if json.Unmarshal(v, &on) == nil {
				state[key] = on
			}
		}
	}
	out := map[string]bool{}
	for key, on := range state {
		if !on {
			out[key] = true
		}
	}
	return out
}

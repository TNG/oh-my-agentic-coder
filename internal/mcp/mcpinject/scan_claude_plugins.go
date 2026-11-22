package mcpinject

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// claudePluginSources covers the plugins Claude Code loads from its config
// directory: the bundles under plugins/synced and the install paths listed in
// plugins/installed_plugins.json. plugins/marketplaces holds catalogs of
// plugins that are available but not loaded, so it is not scanned. The layout
// is observed on a machine, not documented.
func claudePluginSources(configDir string) []source {
	pluginsDir := filepath.Join(configDir, "plugins")
	var out []source
	err := walkFiles(filepath.Join(pluginsDir, "synced"), func(path string) {
		bundle := filepath.Dir(path)
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
		out = append(out, failingSource(filepath.Join(pluginsDir, "synced"), err))
	}
	return append(out, installedPluginSources(configDir, filepath.Join(pluginsDir, "installed_plugins.json"))...)
}

// installedPluginSources reads installed_plugins.json and scans each install
// path it lists. The file's shape is unverified, so every "installPath" string
// anywhere in the document counts, and a "plugins" entry from which no path
// can be read is a parse error.
func installedPluginSources(configDir, file string) []source {
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
	var out []source
	for _, path := range paths {
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

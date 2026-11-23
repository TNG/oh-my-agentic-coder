package mcpinject

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func codeWhaleSources(host Host) []source {
	userLevel := func(src source) source { src.userLevel = true; return src }
	var out []source
	for _, name := range []string{"CODEWHALE_MCP_CONFIG", "DEEPSEEK_MCP_CONFIG"} {
		if file := host.env(name); file != "" {
			out = append(out, userLevel(parsedFileSource(absolute(host.Workdir, file), parseCodeWhaleServers)))
		}
	}

	defaultHome := filepath.Join(host.Home, ".codewhale")
	homes := []string{defaultHome}
	if relocated := configHome(host, "codewhale"); relocated != defaultHome {
		homes = append(homes, relocated)
	}
	for _, home := range homes {
		out = append(out, userLevel(parsedFileSource(filepath.Join(home, "mcp.json"), parseCodeWhaleServers)))
		out = append(out, configPathSources(host, home)...)
	}
	legacy := filepath.Join(host.Home, ".deepseek", "mcp.json")
	if _, err := os.Stat(filepath.Join(defaultHome, "mcp.json")); errors.Is(err, os.ErrNotExist) {
		out = append(out, userLevel(parsedFileSource(legacy, parseCodeWhaleServers)))
	}
	out = append(out, parsedFileSource(filepath.Join(host.Workdir, ".codewhale", "mcp.json"), parseCodeWhaleServers))
	return out
}

// configPathSources follows mcp_config_path in home/config.toml. A config.toml
// that cannot be read or parsed is reported, because it may redirect the file.
func configPathSources(host Host, home string) []source {
	file := filepath.Join(home, "config.toml")
	raw, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil {
		var doc tomlTable
		if doc, err = parseTOML(raw); err == nil {
			path, _ := doc["mcp_config_path"].(string)
			if path == "" {
				return nil
			}
			if rest, ok := strings.CutPrefix(path, "~/"); ok {
				path = filepath.Join(host.Home, rest)
			}
			src := parsedFileSource(absolute(home, path), parseCodeWhaleServers)
			src.userLevel = true
			return []source{src}
		}
	}
	return []source{{file: file, userLevel: true, load: func() ([]Existing, error) { return nil, err }}}
}

// parseCodeWhaleServers reads "servers" or "mcpServers"; CodeWhale rejects a
// file that has both.
func parseCodeWhaleServers(raw []byte) ([]Existing, error) {
	doc, err := parseDoc(raw)
	if err != nil {
		return nil, err
	}
	_, servers := doc["servers"]
	_, mcpServers := doc["mcpServers"]
	if servers && mcpServers {
		return nil, fmt.Errorf(`%w: both "servers" and "mcpServers" are set`, errParse)
	}
	return parseServersUnderKeys(raw, "servers", "mcpServers")
}

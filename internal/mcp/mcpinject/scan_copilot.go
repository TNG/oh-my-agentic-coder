package mcpinject

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

const additionalMCPConfigFlag = "--additional-mcp-config"

func copilotSources(host Host) []source {
	home := configHome(host, "copilot")
	user := parsedFileSource(filepath.Join(home, "mcp-config.json"), parseCopilotServers)
	user.userLevel = true
	out := []source{user}
	for _, dir := range projectDirs(host.Workdir) {
		out = append(out,
			parsedFileSource(filepath.Join(dir, ".mcp.json"), parseCopilotServers),
			parsedFileSource(filepath.Join(dir, ".github", "mcp.json"), parseCopilotServers))
	}
	for _, value := range singleFlagValues(host.InnerArgs, additionalMCPConfigFlag) {
		if file, ok := strings.CutPrefix(value, "@"); ok {
			out = append(out, parsedFileSource(absolute(host.Workdir, file), parseCopilotServers))
			continue
		}
		out = append(out, inlineSource(additionalMCPConfigFlag+" (inline JSON)", []byte(value), parseCopilotServers))
	}
	out = append(out, pluginFileSources(filepath.Join(home, "installed-plugins"), "mcp.json", parseCopilotServers)...)
	return out
}

// parseCopilotServers reads the servers under "mcpServers"; a project file
// may instead use the bare form, where each top-level key is a server name.
func parseCopilotServers(raw []byte) ([]Existing, error) {
	doc, err := parseDoc(raw)
	if err != nil {
		return nil, err
	}
	if _, ok := doc["mcpServers"]; ok {
		return parseServersUnderKeys(raw, "mcpServers")
	}
	bare := map[string]json.RawMessage{}
	for name, entry := range doc {
		if isJSONObject(entry) {
			bare[name] = entry
		}
	}
	return entriesOf(bare, decodeGenericEntry), nil
}

func isJSONObject(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

package mcpinject

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// settingsFiles lists the Claude Code settings files in override order: the user
// settings of each config directory, then the project and local settings.
func settingsFiles(homes []string, workdir string) []string {
	var files []string
	for _, home := range homes {
		files = append(files, filepath.Join(home, "settings.json"))
	}
	return append(files,
		filepath.Join(workdir, ".claude", "settings.json"),
		filepath.Join(workdir, ".claude", "settings.local.json"))
}

// disabledMcpjsonNames returns the servers any settings file lists in
// disabledMcpjsonServers. Claude Code rejects those in every permission mode.
// Servers that are merely unapproved are not skipped: print mode loads them
// without asking.
func disabledMcpjsonNames(files []string) map[string]bool {
	out := map[string]bool{}
	for _, file := range files {
		doc := readDocQuietly(file)
		addStrings(out, doc["disabledMcpjsonServers"])
	}
	return out
}

// disabledServerNames returns the servers the user turned off for this project
// with the /mcp toggle, which Claude Code records in the project entry of
// ~/.claude.json as disabledMcpServers.
func disabledServerNames(userFiles, projectKeys []string) map[string]bool {
	out := map[string]bool{}
	for _, file := range userFiles {
		var projects map[string]json.RawMessage
		if json.Unmarshal(readDocQuietly(file)["projects"], &projects) != nil {
			continue
		}
		for _, key := range projectKeys {
			var project map[string]json.RawMessage
			if json.Unmarshal(projects[key], &project) == nil {
				addStrings(out, project["disabledMcpServers"])
			}
		}
	}
	return out
}

// readDocQuietly returns the top-level keys of a JSON file, or nil when it
// cannot be read or parsed. Callers treat that as "nothing is disabled", so
// the servers stay reported.
func readDocQuietly(file string) map[string]json.RawMessage {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	doc, err := parseDoc(raw)
	if err != nil {
		return nil
	}
	return doc
}

func addStrings(into map[string]bool, raw json.RawMessage) {
	var list []string
	if json.Unmarshal(raw, &list) != nil {
		return
	}
	for _, s := range list {
		into[s] = true
	}
}

// withoutNames drops the named servers from what src loads.
func withoutNames(src source, names map[string]bool) source {
	if len(names) == 0 {
		return src
	}
	load := src.load
	src.load = func() ([]Existing, error) {
		servers, err := load()
		if err != nil {
			return nil, err
		}
		kept := servers[:0:0]
		for _, e := range servers {
			if !names[e.Name] {
				kept = append(kept, e)
			}
		}
		return kept, nil
	}
	return src
}

package mcpinject

import "path/filepath"

func piSources(host Host) []source {
	user := parsedFileSource(filepath.Join(configHome(host, "pi"), "mcp.json"), parsePiServers)
	user.userLevel = true
	return []source{user, parsedFileSource(filepath.Join(host.Workdir, ".pi", "mcp.json"), parsePiServers)}
}

// parsePiServers accepts both map keys because the key pi uses is unverified.
func parsePiServers(raw []byte) ([]Existing, error) {
	return parseServersUnderKeys(raw, "mcpServers", "servers")
}

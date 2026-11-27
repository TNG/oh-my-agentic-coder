package mcpinject

import (
	"fmt"
	"regexp"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// Turning entries off by name is only as complete as the scan: an MCP config
// location the scan does not know is neither refused nor switched off, and the
// harness loads it. The locations and the switches come from the harness
// documentation and have to be researched again at regular intervals, at least
// when a supported harness ships a release that touches MCP, MCP config or
// plugins. Last checked 2026-11-23 (documentation and source only, nothing run).
//
// Codex:
//   - https://developers.openai.com/codex/mcp
//   - https://developers.openai.com/codex/config-advanced
//   - https://github.com/openai/codex/blob/main/codex-rs/config/src/mcp_types.rs
//
// Copilot CLI:
//   - https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/add-mcp-servers
//   - https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-command-reference
//   - https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-config-dir-reference
var disableableName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// SplitDisableable separates the entries omac can turn off for one run, by
// name, from those it cannot. Entries it cannot turn off still need the refuse
// treatment. An entry is left to refuse when turning it off by name is not safe
// or not known to work:
//   - it has the name of a registered server, because the switch would turn off the
//     injected server as well;
//   - a plugin or an administrator provides it, or the user passes it on the command line;
//   - for Codex, it is not in the user's own config: the override only merges into an
//     entry Codex has loaded, and an override for an entry that is not loaded (a project
//     file in a folder Codex does not trust) is an incomplete server that Codex rejects;
//   - its name has characters that would need quoting in the switch.
func SplitDisableable(kind config.MCPInjectionKind, ex []Existing) (disable, rest []Existing) {
	for _, e := range ex {
		if canDisable(kind, e) {
			disable = append(disable, e)
		} else {
			rest = append(rest, e)
		}
	}
	return disable, rest
}

func canDisable(kind config.MCPInjectionKind, e Existing) bool {
	if e.AlsoRegistered || e.PluginProvided || e.Managed || !disableableName.MatchString(e.Name) {
		return false
	}
	switch kind {
	case config.MCPInjectCodexFlag:
		return e.UserLevel && e.File != codexConfigFlagLabel
	case config.MCPInjectCopilotFlag:
		return true
	}
	return false
}

// DisableArgs returns the harness arguments that turn off the named servers for
// one run, without editing any of the harness's files.
func DisableArgs(kind config.MCPInjectionKind, names []string) []string {
	var args []string
	for _, n := range names {
		switch kind {
		case config.MCPInjectCodexFlag:
			args = append(args, "-c", "mcp_servers."+n+".enabled=false")
		case config.MCPInjectCopilotFlag:
			args = append(args, "--disable-mcp-server", n)
		}
	}
	return args
}

// DisabledNotices tells the user which entries are turned off and that the
// entries, and any token in them, stay in the files.
func DisabledNotices(harness string, ex []Existing) []string {
	var out []string
	for _, e := range ex {
		out = append(out, fmt.Sprintf("MCP server %q in %s is turned off for this run (%s); the entry and any token in it stay in that file, where the agent can read them: rotate the token, or register the server with \"omac mcp register\"", e.Name, shown(e.File), harness))
	}
	return out
}

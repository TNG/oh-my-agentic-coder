package mcpinject

import (
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

func TestScanOpenCodeDisabledAcrossFiles(t *testing.T) {
	const local = `{"mcp":{"srv":{"type":"local","command":["x"],"enabled":false}}}`
	runScanCases(t, config.MCPScanOpenCode, nil, []scanCase{
		{
			name:  "disabled in its only definition",
			files: map[string]string{"home/.config/opencode/opencode.json": local},
		},
		{
			name: "a stub that disables it in another file",
			files: map[string]string{
				"home/.config/opencode/opencode.json": `{"mcp":{"srv":{"type":"local","command":["x"]}}}`,
				"opencode.json":                       `{"mcp":{"srv":{"enabled":false}}}`,
			},
			// Merge order is not modelled: another definition may stay enabled, so it is reported.
			want: []found{{"srv", "home/.config/opencode/opencode.json"}},
		},
		{
			name: "a stub that enables it in another file",
			files: map[string]string{
				"home/.config/opencode/opencode.json": local,
				"opencode.json":                       `{"mcp":{"srv":{"enabled":true}}}`,
			},
			want: []found{{"srv", "home/.config/opencode/opencode.json"}},
		},
		{
			name: "disabled everywhere it is mentioned",
			files: map[string]string{
				"home/.config/opencode/opencode.json": local,
				"opencode.json":                       `{"mcp":{"srv":{"enabled":false}}}`,
			},
		},
	})
}

func TestScanClaudeDisabledServers(t *testing.T) {
	two := `{"mcpServers":{"off":{"command":"x"},"on":{"command":"x"}}}`
	runScanCases(t, config.MCPScanClaude, nil, []scanCase{
		{
			name: "disabledMcpjsonServers in settings rejects a .mcp.json server",
			files: map[string]string{
				".mcp.json":                   two,
				".claude/settings.local.json": `{"disabledMcpjsonServers":["off"]}`,
			},
			want: []found{{"on", ".mcp.json"}},
		},
		{
			name: "disabledMcpjsonServers in user settings counts too",
			files: map[string]string{
				".mcp.json":                  two,
				"home/.claude/settings.json": `{"disabledMcpjsonServers":["off"]}`,
			},
			want: []found{{"on", ".mcp.json"}},
		},
		{
			name: "disabledMcpjsonServers does not touch other sources",
			files: map[string]string{
				"home/.claude/settings.json": `{"disabledMcpjsonServers":["off"],"mcpServers":{"off":{"command":"x"}}}`,
			},
			want: []found{{"off", "home/.claude/settings.json"}},
		},
		{
			name: "unapproved and enabled lists do not hide a server",
			files: map[string]string{
				".mcp.json":                   two,
				".claude/settings.local.json": `{"enabledMcpjsonServers":["on"]}`,
			},
			want: []found{{"off", ".mcp.json"}, {"on", ".mcp.json"}},
		},
		{
			name: "disabledMcpServers of this project hides user and settings servers",
			files: map[string]string{
				"home/.claude.json":          `{"mcpServers":{"off":{"command":"x"},"on":{"command":"x"}},"projects":{"{root}":{"disabledMcpServers":["off"]},"/elsewhere":{"disabledMcpServers":["on"]}}}`,
				"home/.claude/settings.json": `{"mcpServers":{"off":{"command":"x"}}}`,
			},
			want: []found{{"on", "home/.claude.json"}},
		},
		{
			name: "disabledMcpServers does not hide a .mcp.json server",
			files: map[string]string{
				".mcp.json":         two,
				"home/.claude.json": `{"projects":{"{root}":{"disabledMcpServers":["off"]}}}`,
			},
			want: []found{{"off", ".mcp.json"}, {"on", ".mcp.json"}},
		},
		{
			name: "a per-server enabled:false is not trusted",
			files: map[string]string{
				".mcp.json": `{"mcpServers":{"flagged":{"command":"x","enabled":false,"disabled":true}}}`,
			},
			want: []found{{"flagged", ".mcp.json"}},
		},
		{
			name: "an unparseable user config hides nothing",
			files: map[string]string{
				".mcp.json":         two,
				"home/.claude.json": `{oops`,
			},
			want:      []found{{"off", ".mcp.json"}, {"on", ".mcp.json"}},
			wantFinds: []string{"home/.claude.json"},
		},
	})
}

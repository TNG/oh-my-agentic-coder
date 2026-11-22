package mcpinject

import (
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

func TestScanClaudeDisabledPlugins(t *testing.T) {
	entry := func(name string) string { return `{"mcpServers":{"` + name + `":{"command":"npx"}}}` }
	manifest := func(name string) string { return `{"name":"` + name + `"}` }
	const synced = "home/.claude/plugins/synced/u1/"
	runScanCases(t, config.MCPScanClaude, nil, []scanCase{
		{
			name: "a disabled synced plugin is skipped, found by its manifest name",
			files: map[string]string{
				"home/.claude/settings.json":               `{"enabledPlugins":{"alpha@synced":false,"beta@synced":true}}`,
				synced + "id-a/.mcp.json":                  entry("from-alpha"),
				synced + "id-a/.claude-plugin/plugin.json": manifest("alpha"),
				synced + "id-b/.mcp.json":                  entry("from-beta"),
				synced + "id-b/.claude-plugin/plugin.json": manifest("beta"),
				synced + "id-c/.mcp.json":                  entry("from-gamma"),
				synced + "id-c/.claude-plugin/plugin.json": manifest("gamma"),
				synced + "id-d/.mcp.json":                  entry("no-manifest"),
				synced + "id-e/nested/dir/.mcp.json":       entry("nested-alpha"),
				synced + "id-e/.claude-plugin/plugin.json": manifest("alpha"),
				synced + "id-f/.claude-plugin/plugin.json": `{"name":"alpha","mcpServers":{"inline":{"command":"x"}}}`,
				synced + "id-g/.claude-plugin/plugin.json": `{oops`,
				synced + "id-g/.mcp.json":                  entry("bad-manifest"),
			},
			want: []found{
				{"bad-manifest", synced + "id-g/.mcp.json"},
				{"from-beta", synced + "id-b/.mcp.json"},
				{"from-gamma", synced + "id-c/.mcp.json"},
				{"no-manifest", synced + "id-d/.mcp.json"},
			},
			wantPlug:  []string{"bad-manifest", "from-beta", "from-gamma", "no-manifest"},
			wantFinds: []string{synced + "id-g/.claude-plugin/plugin.json"},
		},
		{
			name: "the project setting overrides the user setting",
			files: map[string]string{
				"home/.claude/settings.json":               `{"enabledPlugins":{"alpha@synced":false}}`,
				".claude/settings.json":                    `{"enabledPlugins":{"alpha@synced":true}}`,
				synced + "id-a/.mcp.json":                  entry("from-alpha"),
				synced + "id-a/.claude-plugin/plugin.json": manifest("alpha"),
			},
			want:     []found{{"from-alpha", synced + "id-a/.mcp.json"}},
			wantPlug: []string{"from-alpha"},
		},
		{
			name: "the local setting disables a plugin the user enabled",
			files: map[string]string{
				"home/.claude/settings.json":               `{"enabledPlugins":{"alpha@synced":true}}`,
				".claude/settings.local.json":              `{"enabledPlugins":{"alpha@synced":false}}`,
				synced + "id-a/.mcp.json":                  entry("from-alpha"),
				synced + "id-a/.claude-plugin/plugin.json": manifest("alpha"),
			},
		},
		{
			name: "an unreadable settings file keeps the plugin scanned",
			files: map[string]string{
				"home/.claude/settings.json":               `{oops`,
				synced + "id-a/.mcp.json":                  entry("from-alpha"),
				synced + "id-a/.claude-plugin/plugin.json": manifest("alpha"),
			},
			want:      []found{{"from-alpha", synced + "id-a/.mcp.json"}},
			wantPlug:  []string{"from-alpha"},
			wantFinds: []string{"home/.claude/settings.json"},
		},
		{
			name: "a plugin disabled for another marketplace is still scanned",
			files: map[string]string{
				"home/.claude/settings.json":               `{"enabledPlugins":{"alpha@other":false}}`,
				synced + "id-a/.mcp.json":                  entry("from-alpha"),
				synced + "id-a/.claude-plugin/plugin.json": manifest("alpha"),
			},
			want:     []found{{"from-alpha", synced + "id-a/.mcp.json"}},
			wantPlug: []string{"from-alpha"},
		},
		{
			name: "a disabled installed plugin is skipped",
			files: map[string]string{
				"home/.claude/settings.json":                  `{"enabledPlugins":{"p@m":false}}`,
				"home/.claude/plugins/installed_plugins.json": `{"version":2,"plugins":{"p@m":[{"installPath":"{root}/cache/p"}],"q@m":[{"installPath":"{root}/cache/q"}]}}`,
				"cache/p/.mcp.json":                           entry("fromp"),
				"cache/q/.mcp.json":                           entry("fromq"),
			},
			want:     []found{{"fromq", "cache/q/.mcp.json"}},
			wantPlug: []string{"fromq"},
		},
	})
}

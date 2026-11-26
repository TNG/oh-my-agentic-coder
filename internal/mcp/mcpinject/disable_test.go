package mcpinject

import (
	"reflect"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

func TestSplitDisableable(t *testing.T) {
	ok := Existing{Name: "ok", File: "/home/u/.codex/config.toml", UserLevel: true}
	cases := map[string]struct {
		kind config.MCPInjectionKind
		e    Existing
		want bool
	}{
		"codex user entry":              {config.MCPInjectCodexFlag, ok, true},
		"codex project entry":           {config.MCPInjectCodexFlag, Existing{Name: "p", File: "/w/.codex/config.toml"}, false},
		"codex command-line entry":      {config.MCPInjectCodexFlag, Existing{Name: "c", File: codexConfigFlagLabel, UserLevel: true}, false},
		"codex plugin entry":            {config.MCPInjectCodexFlag, Existing{Name: "x", UserLevel: true, PluginProvided: true}, false},
		"codex managed entry":           {config.MCPInjectCodexFlag, Existing{Name: "m", UserLevel: true, Managed: true}, false},
		"codex also registered":         {config.MCPInjectCodexFlag, Existing{Name: "r", UserLevel: true, AlsoRegistered: true}, false},
		"codex name needs quoting":      {config.MCPInjectCodexFlag, Existing{Name: "a.b", UserLevel: true}, false},
		"copilot project entry":         {config.MCPInjectCopilotFlag, Existing{Name: "p", File: "/w/.mcp.json"}, true},
		"copilot also registered":       {config.MCPInjectCopilotFlag, Existing{Name: "r", AlsoRegistered: true}, false},
		"copilot plugin entry":          {config.MCPInjectCopilotFlag, Existing{Name: "x", PluginProvided: true}, false},
		"harness without a name switch": {config.MCPInjectPiExtension, Existing{Name: "p", UserLevel: true}, false},
	}
	for name, c := range cases {
		disable, rest := SplitDisableable(c.kind, []Existing{c.e})
		if got := len(disable) == 1 && len(rest) == 0; got != c.want {
			t.Errorf("%s: disable = %v, want %v", name, got, c.want)
		}
	}
}

func TestDisableArgs(t *testing.T) {
	if got, want := DisableArgs(config.MCPInjectCodexFlag, []string{"a", "b"}), []string{"-c", "mcp_servers.a.enabled=false", "-c", "mcp_servers.b.enabled=false"}; !reflect.DeepEqual(got, want) {
		t.Errorf("codex = %q", got)
	}
	if got, want := DisableArgs(config.MCPInjectCopilotFlag, []string{"a"}), []string{"--disable-mcp-server", "a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("copilot = %q", got)
	}
	if got := DisableArgs(config.MCPInjectPiExtension, []string{"a"}); got != nil {
		t.Errorf("pi = %q", got)
	}
}

package mcpinject

import (
	"fmt"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// ResolvePolicy combines the launcher config, the per-run flag and the number
// of registered servers. The flag overrides the unregistered mode for one run.
// canIgnore says whether the harness can suppress its own MCP config. Without
// an explicit mode the default is ignore when it can and refuse when it
// cannot. An explicit ignore for a harness that cannot falls back to refuse and
// sets IgnoreUnavailable.
func ResolvePolicy(c config.MCPConfig, flag string, registered int, canIgnore bool) (Policy, error) {
	p := Policy{Unregistered: ModeRefuse}
	if canIgnore {
		p.Unregistered = ModeIgnore
	}

	unregistered := c.Unregistered
	if flag != "" {
		unregistered = flag
	}
	if unregistered != "" {
		mode := Mode(unregistered)
		if !mode.Valid() {
			return Policy{}, fmt.Errorf("invalid MCP unregistered mode %q: want allow, warn, ignore or refuse", unregistered)
		}
		p.Unregistered = mode
		if mode == ModeIgnore && !canIgnore {
			p.Unregistered = ModeRefuse
			p.IgnoreUnavailable = true
		}
	}
	p.Active = registered > 0 || unregistered != ""
	return p, nil
}

// IgnoreUnavailableMessage tells the user that ignore falls back to refuse.
func IgnoreUnavailableMessage(harness string) string {
	return fmt.Sprintf("mcp.unregistered ignore is not available for %s, which cannot suppress its own MCP config; using refuse", harness)
}

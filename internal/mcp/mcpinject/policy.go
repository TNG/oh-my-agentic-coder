package mcpinject

import (
	"fmt"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// ResolvePolicy combines the launcher config, the per-run flag and the number
// of registered servers. The flag overrides the unregistered mode for one run.
func ResolvePolicy(c config.MCPConfig, flag string, registered int) (Policy, error) {
	p := Policy{Unregistered: ModeRefuse}

	unregistered := c.Unregistered
	if flag != "" {
		unregistered = flag
	}
	if unregistered != "" {
		mode := Mode(unregistered)
		if !mode.Valid() {
			return Policy{}, fmt.Errorf("invalid MCP unregistered mode %q: want allow, warn or refuse", unregistered)
		}
		p.Unregistered = mode
	}
	p.Active = registered > 0 || unregistered != ""
	return p, nil
}

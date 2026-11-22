package mcpinject

import (
	"fmt"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// ResolvePolicy combines the launcher config, the per-run flag and the number
// of registered servers. The flag overrides the unregistered mode for one run.
func ResolvePolicy(c config.MCPConfig, flag string, registered int) (Policy, error) {
	p := Policy{Unregistered: ModeRefuse, LiteralCredentials: CredRefuse}

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
	}
	if c.LiteralCredentials != "" {
		mode := CredMode(c.LiteralCredentials)
		if !mode.Valid() {
			return Policy{}, fmt.Errorf("invalid MCP literal_credentials mode %q: want allow, warn or refuse", c.LiteralCredentials)
		}
		p.LiteralCredentials = mode
	}

	p.Active = registered > 0 || unregistered != "" || c.LiteralCredentials != ""
	return p, nil
}

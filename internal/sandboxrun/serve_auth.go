package sandboxrun

import (
	"fmt"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// filterServeAuthEnv checks the actual runtime filter, not only the launcher's
// earlier policy snapshot. The marker contains a name, never the password.
func filterServeAuthEnv(environ []string, policy sandboxprofile.Environment, injected map[string]string) ([]string, error) {
	filtered := sandboxprofile.FilterEnv(environ, sandboxprofile.EffectiveAllowVars(policy.AllowVars), policy.DenyVars, injected)
	var target, expected string
	for _, kv := range environ {
		key, value, _ := strings.Cut(kv, "=")
		if key == sandboxprofile.ServeAuthTargetEnv {
			target = value
		}
	}
	if target == "" {
		return filtered, nil
	}
	for _, kv := range environ {
		key, value, _ := strings.Cut(kv, "=")
		if key == target {
			expected = value
		}
	}
	if expected != "" {
		for _, kv := range filtered {
			if kv == target+"="+expected {
				return filtered, nil
			}
		}
	}
	return nil, fmt.Errorf("OMAC_SERVE_PASSWORD cannot reach the server: runtime environment filtering (including deny_vars) blocked or replaced its auth target; review the sandbox environment policy")
}

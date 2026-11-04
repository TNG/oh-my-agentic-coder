package sandboxrun

import (
	"fmt"
	"slices"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func filterServeAuthEnv(environ []string, policy sandboxprofile.Environment, injected map[string]string) ([]string, error) {
	filtered := sandboxprofile.FilterEnv(environ, sandboxprofile.EffectiveAllowVars(policy.AllowVars), policy.DenyVars, injected)
	values := map[string]string{}
	for _, kv := range environ {
		key, value, _ := strings.Cut(kv, "=")
		values[key] = value
	}
	if target := values[sandboxprofile.ServeAuthTargetEnv]; target != "" {
		if values[target] == "" || !slices.Contains(filtered, target+"="+values[target]) {
			return nil, fmt.Errorf("OMAC_SERVE_PASSWORD cannot reach the server: the final sandbox environment policy blocks its auth variable")
		}
	}
	return filtered, nil
}

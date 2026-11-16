package sandboxrun

import (
	"fmt"
	"slices"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// filterServeAuthEnv builds the child environment and, when `omac serve`
// passed a dedicated server password (marked by ServeAuthTargetEnv), fails
// instead of silently dropping it. Its errors are meant to be printed after
// "omac serve:", since they describe the serve password. profile is the
// policy profile reference, or "" when unknown.
func filterServeAuthEnv(environ []string, policy sandboxprofile.Environment, injected map[string]string, profile string) ([]string, error) {
	filtered := sandboxprofile.FilterEnv(environ, sandboxprofile.EffectiveAllowVars(policy.AllowVars), policy.DenyVars, injected)
	values := map[string]string{}
	for _, kv := range environ {
		key, value, _ := strings.Cut(kv, "=")
		values[key] = value
	}
	if target := values[sandboxprofile.ServeAuthTargetEnv]; target != "" {
		if sandboxprofile.EnvVarMatches(target, policy.DenyVars) {
			return nil, sandboxprofile.ServeAuthDeniedError(target, profile)
		}
		if values[target] == "" || !slices.Contains(filtered, target+"="+values[target]) {
			return nil, fmt.Errorf("%s cannot reach the server: %s is missing from the sandbox environment", sandboxprofile.ServePasswordEnv, target)
		}
	}
	return filtered, nil
}

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
	target := lookupEnv(environ, sandboxprofile.ServeAuthTargetEnv)
	if target == "" {
		return filtered, nil
	}
	if sandboxprofile.EnvDenied(target, policy.DenyVars) {
		return nil, sandboxprofile.ServeAuthDeniedError(target, profile)
	}
	// Not denied, but allow_vars (or a missing --allow-env) may still drop it.
	if value := lookupEnv(environ, target); value == "" || !slices.Contains(filtered, target+"="+value) {
		return nil, fmt.Errorf("%s cannot reach the server: the sandbox filtered out %s. "+
			"Check environment.allow_vars and environment.deny_vars in %s, or unset %s.",
			sandboxprofile.ServePasswordEnv, target, sandboxprofile.ServeAuthProfileName(profile), sandboxprofile.ServePasswordEnv)
	}
	return filtered, nil
}

// lookupEnv returns the value of key in a "KEY=VALUE" list, or "" when
// absent. Like the process environment, a later entry wins.
func lookupEnv(environ []string, key string) string {
	value := ""
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			value = v
		}
	}
	return value
}

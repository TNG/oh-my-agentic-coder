package cli

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
)

func injectOpenCodeProjectDiscovery(argv []string, extra map[string]string, h config.Harness, inner []string, plan sandboxPlan) ([]string, error) {
	if runtime.GOOS != "darwin" || !plan.Native || h.Name != "opencode" || !openCodeV2(inner, false) {
		return argv, nil
	}
	const name = "OPENCODE_CONFIG_PROJECT_DISABLE"
	if plan.Policy != nil && sandboxprofile.EnvVarMatches(name, plan.Policy.Environment.DenyVars) {
		return argv, fmt.Errorf("profile %q: deny_vars denies %s, required to disable OpenCode v2 project discovery and avoid denied ancestor lookups in the macOS sandbox; remove the matching deny_vars rule to launch", plan.PolicyRef, name)
	}
	unwrapped := sandboxrun.UnwrapEnv(inner)
	for _, arg := range inner[:len(inner)-len(unwrapped)] {
		if value, ok := strings.CutPrefix(arg, name+"="); ok && value != "1" {
			return argv, fmt.Errorf("inner env wrapper assigns %s=%q, but the OpenCode v2 macOS sandbox requires %s=1; remove the conflicting assignment", name, value, name)
		}
	}
	if len(unwrapped) < len(inner) && len(unwrapped) > 0 && strings.HasPrefix(unwrapped[0], "-") {
		return argv, fmt.Errorf("inner env options can remove %s=1, required by the OpenCode v2 macOS sandbox; use an env wrapper with assignments only", name)
	}
	extra[name] = "1"
	return injectSandboxEnvAllow(argv, []string{name}, plan), nil
}

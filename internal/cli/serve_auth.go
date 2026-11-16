package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// Only the inner server receives the dedicated password, not host sidecars.
func prepareServeAuthEnv(password string, h config.Harness, plan sandboxPlan, noSandbox bool) (map[string]string, error) {
	if password == "" {
		return nil, nil
	}
	if h.ServerLaunch == nil || h.ServerLaunch.AuthEnvVar == "" {
		return nil, fmt.Errorf("%s only works with a harness whose server takes a password, and %s has none. Unset %s.",
			sandboxprofile.ServePasswordEnv, h.Name, sandboxprofile.ServePasswordEnv)
	}
	target := h.ServerLaunch.AuthEnvVar
	extra := map[string]string{target: password}
	if !noSandbox {
		if !plan.Native {
			profile := "the selected sandbox profile"
			if plan.Name != "" {
				profile = fmt.Sprintf("profile %q", plan.Name)
			}
			return nil, fmt.Errorf("%s only works with the native sandbox backend, and %s uses another one. "+
				"Unset %s, or set %s yourself where that backend runs the server.",
				sandboxprofile.ServePasswordEnv, profile, sandboxprofile.ServePasswordEnv, target)
		}
		if plan.Policy == nil {
			return nil, fmt.Errorf("%s needs the sandbox policy %q to check its environment.deny_vars, but it could not be loaded: %v. "+
				"Fix that profile or unset %s.", sandboxprofile.ServePasswordEnv, plan.PolicyRef, plan.PolicyErr, sandboxprofile.ServePasswordEnv)
		}
		if len(sandboxprofile.FilterEnv(nil, nil, plan.Policy.Environment.DenyVars, extra)) != 1 {
			return nil, sandboxprofile.ServeAuthDeniedError(target, plan.PolicyRef)
		}
		// The supervisor rechecks the final policy, then removes this marker.
		extra[sandboxprofile.ServeAuthTargetEnv] = target
	}
	return extra, nil
}

// injectServeAuthAllow lets the auth variable that carries OMAC_SERVE_PASSWORD
// through the native sandbox's env filter. Without a password it adds
// nothing, so a shell value of that variable stays filtered out.
// (runServe rejects a password together with --no-inner before this point.)
func injectServeAuthAllow(argv []string, h config.Harness, plan sandboxPlan, password string) []string {
	if password == "" || h.ServerLaunch == nil {
		return argv
	}
	return injectSandboxEnvAllow(argv, []string{h.ServerLaunch.AuthEnvVar}, plan)
}

// The warning must consider the sandbox filter, not just the host shell.
func serveAuthGetenv(h config.Harness, plan sandboxPlan, extra map[string]string, noSandbox bool) func(string) string {
	var allow, deny []string
	if !noSandbox {
		if !plan.Native || plan.Policy == nil {
			return func(string) string { return "" }
		}
		// Same inputs as the sandbox's filter: the policy's allowlist plus the
		// --allow-env names forwardHarnessEnv adds.
		allow = slices.Concat(sandboxprofile.EffectiveAllowVars(plan.Policy.Environment.AllowVars), harnessEnvAllowVars(h, plan))
		deny = plan.Policy.Environment.DenyVars
	}
	values := map[string]string{}
	for _, kv := range sandboxprofile.FilterEnv(os.Environ(), allow, deny, extra) {
		key, value, _ := strings.Cut(kv, "=")
		values[key] = value
	}
	return func(key string) string { return values[key] }
}

package cli

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// Only the inner server receives the dedicated password, not host sidecars.
func prepareServeAuthEnv(password string, h config.Harness, plan sandboxPlan, noSandbox bool) (map[string]string, error) {
	if password == "" {
		return nil, nil
	}
	if h.ServerLaunch == nil || h.ServerLaunch.AuthEnvVar == "" {
		return nil, fmt.Errorf("%s only works with a harness whose server takes a password, and %s has none. Unset %s",
			sandboxprofile.ServePasswordEnv, h.Name, sandboxprofile.ServePasswordEnv)
	}
	target := h.ServerLaunch.AuthEnvVar
	extra := map[string]string{target: password}
	if !noSandbox {
		if !plan.Native {
			// plan.Name is the sandbox runtime (sandbox.default_profile or
			// --sandbox), not a sandbox profile file, so it is not called a
			// profile here; see sandboxprofile.ServeAuthProfileName.
			runtimeName := "the selected sandbox runtime"
			if plan.Name != "" {
				runtimeName = fmt.Sprintf("sandbox runtime %q", plan.Name)
			}
			return nil, fmt.Errorf("%s only works with the native sandbox backend, and %s uses another one. "+
				"Unset %s, or set %s yourself where that backend runs the server",
				sandboxprofile.ServePasswordEnv, runtimeName, sandboxprofile.ServePasswordEnv, target)
		}
		if plan.Policy == nil {
			return nil, fmt.Errorf("%s needs %s to check its environment.deny_vars, but it could not be loaded: %v. "+
				"Fix that profile or unset %s", sandboxprofile.ServePasswordEnv, sandboxprofile.ServeAuthProfileName(plan.PolicyRef),
				plan.PolicyErr, sandboxprofile.ServePasswordEnv)
		}
		if sandboxprofile.EnvDenied(target, plan.Policy.Environment.DenyVars) {
			return nil, sandboxprofile.ServeAuthDeniedError(target, plan.PolicyRef)
		}
		// The supervisor rechecks the final policy, then removes this marker.
		extra[sandboxprofile.ServeAuthTargetEnv] = target
	}
	return extra, nil
}

// addServeAuthEnv adds the variables from prepareServeAuthEnv (the server
// password and the supervisor's marker) to the inner server's environment.
func addServeAuthEnv(extra, authExtra map[string]string) {
	maps.Copy(extra, authExtra)
}

// serveAuditConfig resolves the audit trail config for `omac serve`. It
// seeds the redactor with the server password, so the log shows a
// placeholder instead if the password ever appears on a logged argv.
func serveAuditConfig(lc config.LauncherConfig, fl auditFlags, version, password string, fatal func(error)) (audit.Config, string) {
	return resolveAuditConfig(lc.Audit, fl, audit.ModeServe, version, []string{password}, fatal)
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
// For another sandbox backend omac cannot see that backend's filter, so it
// reads the host environment, which is what such a backend starts from.
func serveAuthGetenv(h config.Harness, plan sandboxPlan, extra map[string]string, noSandbox bool) func(string) string {
	var allow, deny []string
	if !noSandbox && plan.Native {
		if plan.Policy == nil {
			// The policy could not be loaded, so nothing is known to pass.
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

// serveAuthAdvice completes the hint's last sentence ("To keep the same
// password, …" or "to require a password, …"). It recommends OMAC_SERVE_PASSWORD only where prepareServeAuthEnv
// would accept it, and otherwise names what to do instead, so a user who
// follows the hint never runs into an error.
func serveAuthAdvice(h config.Harness, plan sandboxPlan, noSandbox bool) string {
	setPassword := "set " + sandboxprofile.ServePasswordEnv
	if noSandbox || h.ServerLaunch == nil || h.ServerLaunch.AuthEnvVar == "" {
		return setPassword
	}
	target := h.ServerLaunch.AuthEnvVar
	switch {
	case !plan.Native:
		return fmt.Sprintf("set %s where your sandbox backend runs the server", target)
	case plan.Policy == nil:
		return fmt.Sprintf("fix %s, which could not be loaded, then %s",
			sandboxprofile.ServeAuthProfileName(plan.PolicyRef), setPassword)
	case sandboxprofile.EnvDenied(target, plan.Policy.Environment.DenyVars):
		return fmt.Sprintf("remove the entry matching %s from environment.deny_vars in %s, then %s",
			target, sandboxprofile.ServeAuthProfileName(plan.PolicyRef), setPassword)
	}
	return setPassword
}

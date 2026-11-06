package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// prepareServeAuthEnv maps a host-only dedicated password to the selected
// server's target. Never set the target in omac's environment: sidecars must
// not inherit this exception. The native supervisor verifies it again at exec.
func prepareServeAuthEnv(password string, h config.Harness, plan sandboxPlan, noSandbox bool) (map[string]string, error) {
	if password == "" {
		return nil, nil
	}
	if h.ServerLaunch == nil || h.ServerLaunch.AuthEnvVar == "" {
		return nil, fmt.Errorf("OMAC_SERVE_PASSWORD requires a harness with ServerLaunch.AuthEnvVar; unset it or select a server-capable harness")
	}
	target := h.ServerLaunch.AuthEnvVar
	extra := map[string]string{target: password}
	if noSandbox {
		return extra, nil
	}
	if !plan.Native {
		return nil, fmt.Errorf("OMAC_SERVE_PASSWORD: cannot verify password forwarding through this opaque sandbox backend; select the native backend or unset the dedicated password and configure the backend explicitly")
	}
	if plan.Policy == nil {
		return nil, fmt.Errorf("OMAC_SERVE_PASSWORD: cannot verify the sandbox environment policy; fix the selected policy before launching")
	}
	filtered := sandboxprofile.FilterEnv(nil, nil, plan.Policy.Environment.DenyVars, extra)
	if len(filtered) != 1 {
		return nil, fmt.Errorf("OMAC_SERVE_PASSWORD is blocked by sandbox environment filtering; review environment.deny_vars for the server auth target")
	}
	extra[sandboxprofile.ServeAuthTargetEnv] = target
	return extra, nil
}

// serveAuthGetenv reports only authentication that the known launch policy
// forwards, not an ambient password the sandbox will silently strip. An opaque
// backend cannot provide this assurance.
func serveAuthGetenv(h config.Harness, plan sandboxPlan, extra map[string]string, noSandbox bool) func(string) string {
	var allow, deny []string
	if !noSandbox {
		if !plan.Native || plan.Policy == nil {
			return func(string) string { return "" }
		}
		allow = sandboxprofile.EffectiveAllowVars(plan.Policy.Environment.AllowVars)
		if len(plan.Policy.Environment.AllowVars) > 0 {
			allow = append(allow, h.ForwardedEnvVars()...)
		} else if h.HomeEnv != "" {
			allow = append(allow, h.HomeEnv)
		}
		deny = plan.Policy.Environment.DenyVars
	}
	values := map[string]string{}
	for _, kv := range sandboxprofile.FilterEnv(os.Environ(), allow, deny, extra) {
		key, value, _ := strings.Cut(kv, "=")
		values[key] = value
	}
	return func(key string) string { return values[key] }
}

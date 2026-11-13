package cli

import (
	"fmt"
	"os"
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
		return nil, fmt.Errorf("OMAC_SERVE_PASSWORD requires a harness with server authentication")
	}
	target := h.ServerLaunch.AuthEnvVar
	extra := map[string]string{target: password}
	if !noSandbox {
		if !plan.Native || plan.Policy == nil {
			return nil, fmt.Errorf("OMAC_SERVE_PASSWORD requires the native sandbox; configure other backends explicitly")
		}
		if len(sandboxprofile.FilterEnv(nil, nil, plan.Policy.Environment.DenyVars, extra)) != 1 {
			return nil, fmt.Errorf("OMAC_SERVE_PASSWORD is blocked by the sandbox environment policy")
		}
		// The supervisor rechecks the final policy, then removes this marker.
		extra[sandboxprofile.ServeAuthTargetEnv] = target
	}
	return extra, nil
}

// The warning must consider the sandbox filter, not just the host shell.
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

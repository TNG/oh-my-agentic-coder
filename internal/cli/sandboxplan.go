package cli

import (
	"fmt"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// sandboxPlan is the launch's single resolved answer to "which sandbox is
// this run using?" — it carries BOTH of omac's confusingly similar
// "sandbox profile" namespaces side by side, resolved once:
//
//	launcher profile  a templated argv, keyed by the names in
//	                  config.SandboxConfig.Profiles ("builtin", "nono",
//	                  "no-sandbox-debug"), selected by --sandbox /
//	                  sandbox.default_profile;
//	policy profile    the grant JSON at ~/.config/omac/sandbox-profiles/
//	                  <ref>.json ("default"), spelled INSIDE the launcher
//	                  profile's command template.
//
// Both are plain strings, so before #173 nothing stopped a launcher name
// from being handed to the policy resolver — which is exactly what the
// facade wiring did, silently disabling GET /sandbox/denied on every
// default launch. Consumers now take the plan, so the mix-up cannot be
// expressed.
type sandboxPlan struct {
	// Name is the launcher profile key (e.g. "builtin").
	Name string
	// Launcher is the launcher profile itself; the zero value when Name
	// is not configured (see Known).
	Launcher config.SandboxProfile
	// Known reports whether Name existed in the launcher config.
	Known bool
	// Native reports whether the launcher execs omac's own supervisor
	// (`{{self}} sandbox run …`) — the only backend whose policy omac can
	// inspect and whose launch-injected flags (--allow-env, …) it defines.
	Native bool
	// PolicyRef is the policy reference the launcher template passes to
	// `omac sandbox run --profile`; "" when !Native.
	PolicyRef string
	// Policy is the resolved policy profile; nil when !Native or when
	// PolicyErr is set.
	Policy *sandboxprofile.Profile
	// PolicyPath is the file Policy was loaded from; "" means the
	// compiled-in defaults were used and no file was consulted.
	PolicyPath string
	// PolicyErr records a failed policy resolution. Never fatal: the
	// launch proceeds (the `omac sandbox run` child resolves the policy
	// itself), but facade features derived from the policy are disabled.
	PolicyErr error
}

// resolveSandboxPlan resolves the launcher profile selected by flagProfile
// (empty means sandbox.default_profile) and, when that launcher is omac's
// native sandbox, its policy profile — read-only, so inspecting a profile
// never scaffolds files. Resolution is cheap (one file read plus path
// expansion; the full grant resolution happens inside the `omac sandbox
// run` child), so it is done once per launch and shared.
//
// An unknown launcher name is returned as an error alongside a plan with
// Name set and Known false: callers decide whether that is fatal (it is
// not under --no-sandbox / --no-inner, where no sandbox is launched).
func resolveSandboxPlan(lc config.LauncherConfig, flagProfile string) (sandboxPlan, error) {
	name := flagProfile
	if name == "" {
		name = lc.Sandbox.DefaultProfile
	}
	plan := sandboxPlan{Name: name}
	prof, ok := lc.Sandbox.Profiles[name]
	if !ok {
		return plan, fmt.Errorf("unknown sandbox profile %q", name)
	}
	plan.Launcher = prof
	plan.Known = true
	ref, native := prof.PolicyRef()
	plan.Native = native
	plan.PolicyRef = ref
	if !native {
		return plan, nil
	}
	policy, path, err := sandboxprofile.Resolve(ref)
	if err != nil {
		plan.PolicyErr = err
		return plan, nil
	}
	plan.Policy = policy
	plan.PolicyPath = path
	return plan, nil
}

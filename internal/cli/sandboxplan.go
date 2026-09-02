package cli

import (
	"fmt"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// sandboxPlan is the launch's resolved sandbox state: the launcher profile
// name plus the resolved policy profile (the grant JSON under
// ~/.config/omac/sandbox-profiles/<ref>.json).
type sandboxPlan struct {
	// Name is the launcher profile key (e.g. "builtin").
	Name string
	// Launcher is the launcher profile itself; the zero value when Name
	// is not configured (see Known).
	Launcher config.SandboxProfile
	// Known reports whether Name existed in the launcher config.
	Known bool
	// PolicyRef is the policy profile the run enforces: "default", or the
	// resolved sandbox.profile_path when one is set.
	PolicyRef string
	// Policy is the resolved policy profile; nil when PolicyErr is set.
	Policy *sandboxprofile.Profile
	// PolicyPath is the file Policy was loaded from; "" means the
	// compiled-in defaults were used and no file was consulted.
	PolicyPath string
	// PolicyErr records a failed policy resolution. Never fatal: the
	// launch proceeds (the `omac sandbox run` child resolves the policy
	// itself), but facade features derived from the policy are disabled.
	PolicyErr error
}

// resolveSandboxPlan resolves the configured launcher profile
// (sandbox.default_profile) and the policy profile the run enforces —
// read-only, so inspecting a profile never scaffolds files.
//
// profileRef is the resolved sandbox.profile_path (absolute) or "" for the
// built-in "default" profile — see LauncherConfig.ResolveSandboxProfileRef.
//
// An unknown launcher name is returned as an error alongside a plan with
// Name set and Known false: callers decide whether that is fatal (it is
// not under --no-sandbox / --no-inner, where no sandbox is launched).
func resolveSandboxPlan(lc config.LauncherConfig, profileRef string) (sandboxPlan, error) {
	name := lc.Sandbox.DefaultProfile
	plan := sandboxPlan{Name: name}
	prof, ok := lc.Sandbox.Profiles[name]
	if !ok {
		return plan, fmt.Errorf("unknown sandbox profile %q", name)
	}
	plan.Launcher = prof
	plan.Known = true
	ref := profileRef
	if ref == "" {
		ref = "default"
	}
	plan.PolicyRef = ref
	policy, path, err := sandboxprofile.Resolve(ref)
	if err != nil {
		plan.PolicyErr = err
		return plan, nil
	}
	plan.Policy = policy
	plan.PolicyPath = path
	return plan, nil
}

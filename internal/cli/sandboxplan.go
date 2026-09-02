package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/profileaudit"
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

// warnPermissiveProfile prints advisory findings for a custom sandbox profile
// that weakens the sandbox (secret-path grants, open network, empty allow_vars,
// ...). It is warn-and-continue: findings never block the launch, they only
// make a permissive profile visible — a committed profile_path may be authored
// by someone other than the person launching. The default profile is not linted
// here (doctor covers it), so ref == "" or a nil policy is a no-op.
func warnPermissiveProfile(w io.Writer, ref string, policy *sandboxprofile.Profile) {
	if ref == "" || policy == nil {
		return
	}
	findings := profileaudit.Check(policy)
	if len(findings) == 0 {
		return
	}
	fmt.Fprintf(w, "[warn] sandbox profile %s has %d advisory finding(s):\n", ref, len(findings))
	for _, f := range findings {
		fmt.Fprintf(w, "  [%s] %s: %s (%s)\n", f.Severity, f.Field, f.Message, f.Value)
	}
}

// excludeProfilePagesFile keeps a custom profile's learned-decisions sibling
// (<profile>.pages.json) out of git when the profile lives inside the workdir.
// The sandbox child writes that file lazily on the first permanent network
// decision; excluding it up front stops a per-user file from being committed.
// No-op for the default profile, a profile outside the workdir, or a non-git
// workdir.
func excludeProfilePagesFile(workdir, profileRef string) {
	if profileRef == "" {
		return
	}
	pages := sandboxprofile.PagesPath(profileRef)
	rel, err := filepath.Rel(workdir, pages)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return // the pages file is outside the workdir
	}
	gitExcludePath(workdir, rel)
}

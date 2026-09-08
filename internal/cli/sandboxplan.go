package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/profileaudit"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// sandboxPlan is the launch's resolved sandbox policy: the grant JSON the run
// enforces, resolved from sandbox.profile_path or the built-in "default".
type sandboxPlan struct {
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

// resolveSandboxPlan resolves the policy profile the run enforces — read-only,
// so inspecting a profile never scaffolds files.
//
// profileRef is the resolved sandbox.profile_path (absolute) or "" for the
// built-in "default" profile — see LauncherConfig.ResolveSandboxProfileRef.
func resolveSandboxPlan(profileRef string) sandboxPlan {
	ref := profileRef
	if ref == "" {
		ref = "default"
	}
	plan := sandboxPlan{PolicyRef: ref}
	policy, path, err := sandboxprofile.Resolve(ref)
	if err != nil {
		plan.PolicyErr = err
		return plan
	}
	plan.Policy = policy
	plan.PolicyPath = path
	return plan
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
// omac creates the file at launch (see sandboxrun.writeProtectProfilePaths),
// so excluding it up front stops a per-user file from being committed.
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

// inspectProfileRef returns the profile a read-only inspection should examine,
// matching a launch: the explicit --profile value, else sandbox.profile_path,
// else "" (the built-in "default"). Best-effort: on a config or profile_path
// error it returns "" plus the error, so callers can warn before falling back
// to the default — a silently swapped profile would hide the very problem
// the inspection is meant to reveal.
func inspectProfileRef(workdir, flagRef string) (string, error) {
	if flagRef != "" {
		return flagRef, nil
	}
	lc, cfgPath, err := config.LoadLauncher(workdir)
	if err != nil {
		return "", err
	}
	return profileRefFromConfig(lc, cfgPath, workdir, "")
}

// profileRefFromConfig resolves the configured sandbox.profile_path from an
// already-loaded launcher config, so a caller that loaded the config once
// (doctor) does not load it again. flagRef is a non-empty explicit --profile
// reference, passed through verbatim.
func profileRefFromConfig(lc config.LauncherConfig, cfgPath, workdir, flagRef string) (string, error) {
	if flagRef != "" {
		return flagRef, nil
	}
	return lc.ResolveSandboxProfileRef(cfgPath, workdir)
}

// profileDisplayName returns the label inspection output should use for a
// profile ref: the ref itself, or "default" for the built-in profile ("").
func profileDisplayName(ref string) string {
	if ref == "" {
		return "default"
	}
	return ref
}

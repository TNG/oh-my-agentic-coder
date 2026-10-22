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
// enforces, resolved from the selected profile or the built-in "default".
type sandboxPlan struct {
	// PolicyRef is the policy profile the run enforces: "default", or the
	// selected profile's absolute path when one is set.
	PolicyRef string
	// Policy is the resolved policy profile; nil when PolicyErr is set.
	Policy *sandboxprofile.Profile
	// PolicyPath is the file Policy was loaded from; "" means the
	// compiled-in defaults were used and no file was consulted.
	PolicyPath string
	// Layer is where the profile was selected: "workdir", "global", or
	// "builtin".
	Layer string
	// Workdir is the project root the plan resolved against, so callers can
	// derive the non-overridable .omac protection without threading it
	// separately.
	Workdir string
	// PolicyErr records a failed policy resolution. Never fatal: the
	// launch proceeds (the `omac sandbox run` child resolves the policy
	// itself), but facade features derived from the policy are disabled.
	PolicyErr error
}

// resolveSandboxPlan resolves the policy profile the run enforces — read-only,
// so inspecting a profile never scaffolds files.
//
// sel is the profile selected by the launcher config or --profile-path; its
// Path is absolute and already validated, or "" for the built-in default.
// workdir is the project root, the one place outside the trusted profile
// directory a project-committed profile may live.
func resolveSandboxPlan(workdir string, sel config.ProfileSelection) sandboxPlan {
	ref := sel.Path
	if ref == "" {
		ref = "default"
	}
	plan := sandboxPlan{PolicyRef: ref, Layer: sel.Layer, Workdir: workdir}
	policy, path, err := sandboxprofile.Resolve(ref, sandboxprofile.WithProjectDir(config.LocalConfigDir(workdir)))
	if err != nil {
		plan.PolicyErr = err
		return plan
	}
	plan.Policy = policy
	plan.PolicyPath = path
	return plan
}

// activeProfileSelection applies --profile-path when given, else the launcher
// config's layer-local selection.
func activeProfileSelection(workdir, cliPath string) (config.ProfileSelection, error) {
	if strings.TrimSpace(cliPath) != "" {
		return config.ExplicitProfileSelection(workdir, cliPath)
	}
	return config.ResolveSandboxProfile(workdir)
}

// warnPermissiveProfile prints advisory findings for a custom sandbox profile
// that weakens the sandbox (secret-path grants, open network, empty allow_vars,
// ...). It is warn-and-continue: findings never block the launch, they only
// make a permissive profile visible — a committed project profile may be
// authored by someone other than the person launching. The default is not linted
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

// profileRefFromConfig returns the profile a read-only inspection should
// examine, matching a launch: the explicit --profile value, else the launcher
// config's layer-local selection, else "" (the built-in "default"). On a config
// error it returns "" plus the error, so callers can warn before falling back
// to the default — a silently swapped profile would hide the very problem the
// inspection is meant to reveal.
func profileRefFromConfig(workdir, flagRef string) (string, error) {
	if strings.TrimSpace(flagRef) != "" {
		return flagRef, nil
	}
	sel, err := config.ResolveSandboxProfile(workdir)
	if err != nil {
		return "", err
	}
	return sel.Path, nil
}

// profileDisplayName returns the label inspection output should use for a
// profile ref: the ref itself, or "default" for the built-in profile ("").
func profileDisplayName(ref string) string {
	if ref == "" {
		return "default"
	}
	return ref
}

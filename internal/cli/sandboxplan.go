package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/profileaudit"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
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
//
// A project-local selection is only used while it matches the content pinned in
// the host-only store (see config.ProjectSandboxTrust). The workdir is
// agent-writable, and the macOS backend cannot block replacing <workdir>/.omac
// between sessions, so a mismatch means the local layer may have been planted:
// the launch aborts (ExitConfigInvalid) until the changed content is
// re-approved with --accept-project-config or restored.
func activeProfileSelection(workdir, cliPath string, acceptProject bool, warn io.Writer) (config.ProfileSelection, error) {
	if strings.TrimSpace(cliPath) != "" {
		sel, err := config.ExplicitProfileSelection(workdir, cliPath)
		if err != nil {
			return sel, err
		}
		if sel.Layer != "workdir" {
			// A global-layer path lives in the non-overridable host config
			// dir, which no session can read or write: the command line itself
			// is the approval, and every launch re-reads the file.
			return sel, nil
		}
		return approvedProjectSandbox(workdir, sel, acceptProject, warn)
	}
	sel, err := config.ResolveSandboxProfile(workdir)
	if err != nil || workdir == "" {
		return sel, err
	}
	return approvedProjectSandbox(workdir, sel, acceptProject, warn)
}

// approvedProjectSandbox enforces the trust pin behind one explicit path. The
// pin hash covers <workdir>/.omac/config.yaml and, when a workdir-layer
// profile is selected, the profile file — so a config that selects nothing is
// still covered against a later profile_name injection (the replacement path).
func approvedProjectSandbox(workdir string, sel config.ProfileSelection, acceptProject bool, warn io.Writer) (config.ProfileSelection, error) {
	trusted, firstUse, reason := config.ProjectSandboxTrust(workdir, sel)
	switch {
	case firstUse, acceptProject && !trusted:
		if perr := config.PinProjectSandbox(workdir, sel); perr != nil && warn != nil {
			fmt.Fprintf(warn, "omac: cannot record the approved project sandbox configuration (%v); "+
				"a later tampering will not be detected\n", perr)
		}
	case !trusted:
		return sel, fmt.Errorf("%s — refusing to start until re-approved\n"+
			"       review the change (git diff -- .omac) and re-approve with --accept-project-config, or restore it (git checkout -- .omac)",
			reason)
	}
	return sel, nil
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

// ensureOmacLocalDir creates <workdir>/.omac before the launch. It returns the
// error untouched when .omac is a symlink (callers refuse the launch); any
// other creation failure only warns — a workdir omac cannot create .omac in
// cannot have it created by the agent either, so nothing plantable is missing.
func ensureOmacLocalDir(w io.Writer, workdir string) error {
	_, err := sandboxrun.EnsureLocalConfigDir(workdir)
	if err == nil || errors.Is(err, sandboxrun.ErrLocalConfigDirSymlink) {
		return err
	}
	fmt.Fprintf(w, "[warn] %v\n", err)
	fmt.Fprintln(w, "[warn] proceeding without project-local sandbox configuration; the agent")
	fmt.Fprintln(w, "[warn] runs with your own permissions, so it cannot create this directory either.")
	return nil
}

// profileRefFromConfig returns the profile a read-only inspection should
// examine, matching a launch: the explicit --profile value, else the launcher
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

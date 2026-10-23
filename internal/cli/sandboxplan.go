package cli

import (
	"errors"
	"fmt"
	"io"
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
// A project-local selection is only used while the files it loads match the
// approved digests in the host-only pin store (see config.ProjectSandboxTrust).
// The workdir is agent-writable, and the macOS backend cannot block replacing
// <workdir>/.omac between sessions, so a mismatch or still-unapproved content
// aborts the launch (ExitConfigInvalid) until a human reviews the files and
// re-runs with --accept-project-config.
func activeProfileSelection(workdir, cliPath string, acceptProject bool, warn io.Writer) (config.ProfileSelection, error) {
	if strings.TrimSpace(cliPath) != "" {
		sel, err := config.ExplicitProfileSelection(workdir, cliPath)
		if err != nil {
			return sel, err
		}
		if sel.Layer != "workdir" {
			// A global-layer path lives in the non-overridable host config
			// dir; the command line itself is the approval, and every launch
			// re-reads the file.
			return sel, nil
		}
		return approvedProjectSandbox(workdir, sel, acceptProject, false)
	}
	sel, err := config.ResolveSandboxProfile(workdir)
	if err != nil || workdir == "" {
		return sel, err
	}
	return approvedProjectSandbox(workdir, sel, acceptProject, true)
}

// approvedProjectSandbox enforces the trust pin behind one selection.
// configDriven is false for an explicit --profile-path: the typed path is its
// own approval, so a first use pins without a flag, while config-driven first
// use (content that could have been planted in the repo) requires one.
func approvedProjectSandbox(workdir string, sel config.ProfileSelection, acceptProject bool, configDriven bool) (config.ProfileSelection, error) {
	trusted, firstUse, reason, terr := config.ProjectSandboxTrust(workdir, sel, configDriven)
	if terr != nil {
		// A read failure is not a content change: re-approving would record
		// nothing, so refusals must say what is actually wrong.
		return sel, fmt.Errorf("cannot read the project sandbox configuration: %s", terr)
	}
	switch {
	case trusted:
		return sel, nil
	case firstUse && configDriven && !acceptProject:
		return sel, fmt.Errorf("%s — refusing to start until the content is approved\n"+
			"       the project directory is agent-writable, so omac only runs sandbox configuration you have reviewed; "+
			"if it is yours, re-run with --accept-project-config",
			reason)
	case !firstUse && !acceptProject:
		return sel, fmt.Errorf("%s — refusing to start until re-approved\n"+
			"       the project directory is agent-writable and its sandbox configuration steers future launches, "+
			"so omac runs only the content you have reviewed; if the change is yours, re-run with --accept-project-config",
			reason)
	default:
		// First use behind an explicit path (the command line is the
		// approval), or re-approval via --accept-project-config: pin the
		// loaded files. Only the files this launch loads get entries, so an
		// approval never covers content it did not see. A pin that cannot be
		// recorded must abort: the user would believe they approved, but
		// tomorrow's tampering check would be missing.
		if perr := config.PinProjectSandbox(workdir, sel, configDriven); perr != nil {
			return sel, fmt.Errorf("cannot record the approved project sandbox configuration (%s) — "+
				"without the approval record omac cannot detect later tampering; "+
				"check that ~/.config/omac is writable and re-run with --accept-project-config", perr)
		}
		return sel, nil
	}
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

// projectTrustMode maps the launching parent's selection source onto the
// --project-trust value the sandbox child verifies its loaded content with:
// "config" when the launch followed .omac/config.yaml, "explicit" when it
// followed --profile-path.
func projectTrustMode(cliPath string) string {
	if strings.TrimSpace(cliPath) != "" {
		return "explicit"
	}
	return "config"
}

// ensureOmacLocalDir creates <workdir>/.omac before the launch. It returns the
// error untouched when .omac is a symlink (callers refuse the launch); any
// other creation failure only warns — whatever keeps omac from creating
// .omac keeps the agent (with the same user's permissions — its own) from
// creating it too, so nothing plantable is missing.
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

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
)

func injectOpenCodeProjectDiscovery(argv []string, extra map[string]string, h config.Harness, inner []string, plan sandboxPlan) ([]string, error) {
	if runtime.GOOS != "darwin" || !plan.Native || h.Name != "opencode" || !openCodeV2(inner, false) {
		return argv, nil
	}
	const name = "OPENCODE_CONFIG_PROJECT_DISABLE"
	if plan.Policy != nil && sandboxprofile.EnvVarMatches(name, plan.Policy.Environment.DenyVars) {
		return argv, fmt.Errorf("profile %q: deny_vars denies %s, required to disable OpenCode v2 project discovery and avoid denied ancestor lookups in the macOS sandbox; remove the matching deny_vars rule to launch", plan.PolicyRef, name)
	}
	unwrapped := sandboxrun.UnwrapEnv(inner)
	for _, arg := range inner[:len(inner)-len(unwrapped)] {
		if value, ok := strings.CutPrefix(arg, name+"="); ok && value != "1" {
			return argv, fmt.Errorf("inner env wrapper assigns %s=%q, but the OpenCode v2 macOS sandbox requires %s=1; remove the conflicting assignment", name, value, name)
		}
	}
	if len(unwrapped) < len(inner) && len(unwrapped) > 0 && strings.HasPrefix(unwrapped[0], "-") {
		return argv, fmt.Errorf("inner env options can remove %s=1, required by the OpenCode v2 macOS sandbox; use an env wrapper with assignments only", name)
	}
	extra[name] = "1"
	return injectSandboxEnvAllow(argv, []string{name}, plan), nil
}

func warnOpenCodeAgentsAccess(env *Env, argv []string, plan sandboxPlan, extra map[string]string) {
	if extra["OPENCODE_CONFIG_PROJECT_DISABLE"] != "1" || !plan.Native || plan.Policy == nil || len(argv) < 3 || argv[1] != "sandbox" || argv[2] != "run" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	agents := filepath.Join(home, ".agents")
	info, err := os.Stat(agents)
	if err != nil || !info.IsDir() {
		return
	}
	resolved, err := filepath.EvalSymlinks(agents)
	if err != nil {
		return
	}
	flags, err := sandboxprofile.ParseFlags(argv[3:])
	if err != nil {
		return
	}
	profile, _ := sandboxprofile.Merge(plan.Policy, flags)
	grants, err := sandboxrun.ResolveGrants(profile, env.Workdir, nil)
	if err != nil {
		return
	}
	covers := func(root, path string) bool {
		if canonical, err := filepath.EvalSymlinks(root); err == nil {
			root = canonical
		}
		return path == root || strings.HasPrefix(path, strings.TrimRight(root, string(filepath.Separator))+string(filepath.Separator))
	}
	denied := false
	for _, path := range grants.ProtectedPaths {
		if covers(path, resolved) {
			denied = true
			break
		}
	}
	if !denied {
		for _, paths := range [][]string{grants.ReadPaths, grants.AllowPaths} {
			for _, path := range paths {
				if covers(path, resolved) {
					return
				}
			}
		}
		if workdir, err := filepath.EvalSymlinks(env.Workdir); err == nil && workdir != resolved && covers(resolved, workdir) && profile.Workdir.Access != "" && profile.Workdir.Access != sandboxprofile.AccessNone {
			return
		}
	}
	profileRef := plan.PolicyRef
	if profileRef == "" {
		profileRef = "default"
	}
	profilePath := plan.PolicyPath
	if profilePath == "" {
		profilePath, _ = sandboxprofile.ProfilePath(profileRef)
	}
	fmt.Fprintf(env.Stderr, "omac: warning: sandbox profile %q cannot list %s; OpenCode v2 may fail with EPERM during project initialization, even with OPENCODE_CONFIG_PROJECT_DISABLE=1.\n", profileRef, agents)
	fmt.Fprintf(env.Stderr, "  Refresh the profile from its installer, or add %q to filesystem.read in %s and restart omac. This grants recursive read access to file and skill contents, not write access.\n", "~/.agents", profilePath)
	if denied {
		fmt.Fprintln(env.Stderr, "  A matching filesystem deny also blocks this directory; adding a read grant alone will not remove that restriction.")
	}
}

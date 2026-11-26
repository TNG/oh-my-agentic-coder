package cli

import (
	"fmt"
	"os"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// The folder sits directly under /tmp because a Unix socket path must stay
// below 104 bytes on macOS.
const (
	ptySocketParent  = "/tmp"
	ptySocketPattern = "omac-pty-*"
)

// ptySocketDir is a per-launch folder the sandbox may connect into, so a
// harness's helper daemon can place its sockets where the sandbox reaches them.
type ptySocketDir struct{ Dir, EnvVar string }

func needsPTYSocketDir(h config.Harness, plan sandboxPlan, noSandbox bool) bool {
	return !noSandbox && plan.Native && h.PTYSocketDirEnv != ""
}

// ptySocketDirDenied reports a profile whose deny_vars would strip the variable omac sets.
func ptySocketDirDenied(h config.Harness, plan sandboxPlan, noSandbox bool) error {
	if !needsPTYSocketDir(h, plan, noSandbox) || plan.Policy == nil {
		return nil
	}
	if !sandboxprofile.EnvVarMatches(h.PTYSocketDirEnv, plan.Policy.Environment.DenyVars) {
		return nil
	}
	profile := plan.PolicyRef
	if profile == "" {
		profile = plan.Name
	}
	return fmt.Errorf("profile %q: deny_vars denies %s, which omac sets so that the harness can reach its shell helper inside the sandbox; remove it from the profile's deny_vars", profile, h.PTYSocketDirEnv)
}

// newPTYSocketDir creates the folder; it returns nil, nil when the launch needs none.
func newPTYSocketDir(h config.Harness, plan sandboxPlan, noSandbox bool) (*ptySocketDir, error) {
	if !needsPTYSocketDir(h, plan, noSandbox) {
		return nil, nil
	}
	dir, err := os.MkdirTemp(ptySocketParent, ptySocketPattern)
	if err != nil {
		return nil, err
	}
	return &ptySocketDir{Dir: dir, EnvVar: h.PTYSocketDirEnv}, nil
}

// install grants the folder and passes the variable through the profile's env filter.
func (p *ptySocketDir) install(argv []string, plan sandboxPlan) []string {
	if p == nil {
		return argv
	}
	argv = injectSandboxFlag(argv, "--allow-unix-dir", p.Dir)
	return injectSandboxEnvAllow(argv, []string{p.EnvVar}, plan)
}

func (p *ptySocketDir) apply(extra map[string]string) {
	if p == nil {
		return
	}
	extra[p.EnvVar] = p.Dir
}

func (p *ptySocketDir) Close() {
	if p == nil {
		return
	}
	_ = os.RemoveAll(p.Dir)
}

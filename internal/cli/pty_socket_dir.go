package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

const ptySocketPattern = "omac-pty-*"

// ptySocketParent is /tmp because a Unix socket path must stay below 104 bytes
// on macOS. Tests redirect it; production code never assigns it.
var ptySocketParent = "/tmp"

// ptySocketDir is a per-launch folder the sandbox may connect into, so a
// harness's helper daemon can place its sockets where the sandbox reaches them.
type ptySocketDir struct{ Dir, EnvVar string }

func needsPTYSocketDir(h config.Harness, plan sandboxPlan, noSandbox bool) bool {
	return !noSandbox && plan.Native && h.PTYSocketDirEnv != ""
}

// openPTYSocketDir checks the profile and creates the folder for a launch.
// On failure it prints the reason after prefix and returns the exit code;
// on success the code is ExitOK and the folder is nil when the launch needs none.
func openPTYSocketDir(stderr io.Writer, prefix string, h config.Harness, plan sandboxPlan, noSandbox bool) (*ptySocketDir, int) {
	if err := ptySocketDirDenied(h, plan, noSandbox); err != nil {
		fmt.Fprintln(stderr, prefix+": pty socket folder:", err)
		return nil, ExitConfigInvalid
	}
	dir, err := newPTYSocketDir(h, plan, noSandbox)
	if err != nil {
		fmt.Fprintln(stderr, prefix+": pty socket folder:", err)
		return nil, ExitIOError
	}
	return dir, ExitOK
}

// ptySocketDirDenied reports a profile whose deny_vars would strip the variable omac sets.
func ptySocketDirDenied(h config.Harness, plan sandboxPlan, noSandbox bool) error {
	if !needsPTYSocketDir(h, plan, noSandbox) || plan.Policy == nil {
		return nil
	}
	for _, entry := range plan.Policy.Environment.DenyVars {
		if sandboxprofile.EnvVarMatches(h.PTYSocketDirEnv, []string{entry}) {
			return fmt.Errorf("profile %q: deny_vars entry %q denies %s, which omac sets so that the harness can reach its shell helper inside the sandbox; remove or narrow that entry", plan.profileLabel(), entry, h.PTYSocketDirEnv)
		}
	}
	return nil
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
func (p *ptySocketDir) install(argv []string, plan sandboxPlan, stderr io.Writer, verbose bool) []string {
	if p == nil {
		return argv
	}
	if verbose {
		fmt.Fprintf(stderr, "[verbose] pty socket folder: %s\n", p.Dir)
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

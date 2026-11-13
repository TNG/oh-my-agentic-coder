package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/opencodestate"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func reportDesktopSkipped(out io.Writer, skipped []opencodestate.SkippedWorktree, worktrees []string) {
	for _, s := range skipped {
		if s.Reason == opencodestate.SkipInvalidSource {
			fmt.Fprintf(out, "omac serve: ignoring invalid OpenCode state source %q\n", s.Path)
			continue
		}
		// A folder can be rejected from one source and granted from another.
		// Source failures are different: always report them, without contents.
		if !pathCoveredBy(s.Path, worktrees) {
			fmt.Fprintf(out, "omac serve: skipping OpenCode worktree %s (%s)\n", s.Path, s.Reason)
		}
	}
}

func validateDesktopSandbox(desktop, noInner, noSandbox, learn bool, plan sandboxPlan) error {
	if !desktop || noInner {
		return nil
	}
	if noSandbox || learn || !plan.Known || !plan.Native || plan.PolicyErr != nil {
		return fmt.Errorf("--for-opencode-desktop requires the native sandbox with filesystem isolation; remove --no-sandbox/--learn and select a valid native sandbox profile")
	}
	for _, a := range plan.Launcher.Command {
		if a == "--" {
			break
		}
		if a == "--learn" || strings.HasPrefix(a, "--learn=") {
			return fmt.Errorf("--for-opencode-desktop cannot protect trusted stores with --learn in the launcher template; remove --learn")
		}
	}
	return nil
}

// desktopProtectionArgv is applied at every Desktop launch, not scaffold time.
// The second flag makes the native backend refuse layouts where its ordinary
// best-effort file masks cannot protect these trusted authorization inputs.
func desktopProtectionArgv(argv, worktrees []string) ([]string, error) {
	if len(argv) < 3 || argv[1] != "sandbox" || argv[2] != "run" {
		return nil, fmt.Errorf("--for-opencode-desktop requires the native sandbox launcher")
	}
	f, err := sandboxprofile.ParseFlags(argv[3:])
	if err != nil {
		return nil, fmt.Errorf("--for-opencode-desktop: inspect native sandbox flags: %w", err)
	}
	if f.Learn {
		return nil, fmt.Errorf("--for-opencode-desktop cannot protect trusted stores with --learn; remove --learn")
	}
	for _, p := range opencodestate.DesktopStorePaths() {
		argv = injectSandboxFlag(argv, "--deny-literal", p)
		argv = injectSandboxFlag(argv, "--require-protected-path", p)
	}
	for _, wt := range worktrees {
		argv = injectSandboxFlag(argv, "--allow-literal", wt)
	}
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		if !filepath.IsAbs(state) {
			return nil, fmt.Errorf("--for-opencode-desktop: XDG_STATE_HOME must be absolute")
		}
		argv = injectSandboxFlag(argv, "--allow-literal", filepath.Join(state, "opencode"))
	}
	return argv, nil
}

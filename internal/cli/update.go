package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	iofs "io/fs"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/term"

	"github.com/tngtech/oh-my-agentic-coder/internal/updater"
)

func runUpdate(args []string, env *Env) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	var yes bool
	fs.BoolVar(&yes, "yes", false, "Skip the confirmation prompt (for scripting/non-interactive use).")
	fs.BoolVar(&yes, "y", false, "Shorthand for --yes.")
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac update [--yes|-y]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return ExitMisuse
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return ExitMisuse
	}
	return runUpdateWithDeps(env, yes, updater.RealDeps(env.Stdin, env.Stdout, env.Stderr))
}

func runUpdateWithDeps(env *Env, yes bool, deps updater.Deps) int {
	ctx := context.Background()
	plan, err := updater.Check(ctx, updater.Options{CurrentVersion: env.Version}, deps)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac update:", err)
		switch {
		case errors.Is(err, updater.ErrChecksumMismatch):
			return ExitChecksumMismatch
		case errors.Is(err, updater.ErrNoMatchingAsset):
			return ExitPrerequisiteMissing
		default:
			return ExitIOError
		}
	}

	if plan.LocalPath != "" {
		defer os.Remove(plan.LocalPath)
	}

	if plan.Method == updater.MethodUpToDate {
		if plan.CurrentVersion != plan.LatestVersion {
			fmt.Fprintf(env.Stdout, "[ok] omac %s is newer than the latest release %s; nothing to do\n", plan.CurrentVersion, plan.LatestVersion)
		} else {
			fmt.Fprintf(env.Stdout, "[ok] omac %s is already up to date\n", plan.CurrentVersion)
		}
		return ExitOK
	}

	printUpdatePlan(env, plan)

	if !yes {
		proceed, askErr := confirmUpdate(env)
		if askErr != nil {
			fmt.Fprintln(env.Stdout, "[noop]", askErr)
			return ExitOK
		}
		if !proceed {
			fmt.Fprintln(env.Stdout, "[noop] update cancelled")
			return ExitOK
		}
	}

	if err := updater.Apply(ctx, plan, deps); err != nil {
		fmt.Fprintln(env.Stderr, "omac update:", err)
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return ExitGeneric
		}
		if errors.Is(err, iofs.ErrPermission) {
			fmt.Fprintln(env.Stderr, "omac update: permission denied replacing the binary — re-run with sudo, or reinstall via your package manager")
		}
		return ExitIOError
	}
	fmt.Fprintf(env.Stdout, "[ok] updated omac %s -> %s\n", plan.CurrentVersion, plan.LatestVersion)
	reportShadowedBinary(env, ctx, plan, deps)
	return ExitOK
}

// reportShadowedBinary runs the post-install self-check and, if an older omac
// earlier on PATH is shadowing the freshly-installed one, warns the user and
// suggests removing the stale binary. A probe failure is silent: the update
// already succeeded, and a missing/unreadable omac on PATH is not worth a
// scary message.
func reportShadowedBinary(env *Env, ctx context.Context, plan updater.Plan, deps updater.Deps) {
	if deps.PathLookup == nil || deps.VersionProbe == nil {
		return
	}
	res, err := updater.SelfCheck(ctx, plan, deps)
	if err != nil || !res.Shadowed {
		return
	}
	fmt.Fprintf(env.Stdout, "[warn] `omac` on your PATH still resolves to an older binary:\n")
	fmt.Fprintf(env.Stdout, "         %s (%s)\n", res.ResolvedPath, res.ResolvedVersion)
	fmt.Fprintf(env.Stdout, "       it shadows the %s you just installed. To use the new version, remove the stale binary:\n", plan.LatestVersion)
	fmt.Fprintf(env.Stdout, "         rm %s && hash -r\n", res.ResolvedPath)
}

func printUpdatePlan(env *Env, plan updater.Plan) {
	fmt.Fprintf(env.Stdout, "omac %s -> %s\n", plan.CurrentVersion, plan.LatestVersion)
	switch plan.Method {
	case updater.MethodBrew:
		fmt.Fprintln(env.Stdout, "method: brew upgrade oh-my-agentic-coder")
	case updater.MethodLinuxPackage:
		fmt.Fprintf(env.Stdout, "method: %s (%s), checksum verified: %v\n", plan.PackageManager, plan.Asset.Name, plan.ChecksumVerified)
	case updater.MethodTarballSelfReplace:
		fmt.Fprintf(env.Stdout, "method: replace running binary from %s, checksum verified: %v\n", plan.Asset.Name, plan.ChecksumVerified)
	}
}

func confirmUpdate(env *Env) (bool, error) {
	if !term.IsTerminal(int(env.Stdin.Fd())) {
		return false, errors.New("stdin is not a terminal; declining automatic update (re-run with --yes/-y to update non-interactively)")
	}
	fmt.Fprint(env.Stdout, "Proceed with update? [y/N] ")
	line, _ := bufio.NewReader(env.Stdin).ReadString('\n')
	return parseYesNo(line), nil
}

func parseYesNo(line string) bool {
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

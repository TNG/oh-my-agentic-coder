package cli

import "fmt"

// runContinue implements `omac continue [harness] [flags] [-- inner args...]`:
// re-launch the most recent session for the current workdir by appending the
// harness's "continue" inner flag to the shared start pipeline.
func runContinue(args []string, env *Env) int {
	opts, code := buildContinueOpts(args, env)
	if code != ExitOK {
		return code
	}
	return runLaunch(env, opts)
}

// buildContinueOpts parses the continue command line (the same surface as
// start) and prepends the resolved harness's continue flag to the inner args.
// It returns the assembled opts and ExitOK, or a non-OK exit code (with a
// message already written to stderr) on error.
func buildContinueOpts(args []string, env *Env) (launchOpts, int) {
	opts, ok := parseLaunchArgs("continue", args, env)
	if !ok {
		return launchOpts{}, ExitMisuse
	}
	sess := opts.harness.Session
	if sess == nil || len(sess.ContinueArgs) == 0 {
		fmt.Fprintf(env.Stderr,
			"omac continue: harness %q does not support continuing sessions\n", opts.harness.Name)
		return launchOpts{}, ExitMisuse
	}
	// Put the continue flag first, then any user-supplied inner args, so a
	// trailing user positional (if any) is not mistaken for the flag's value.
	inner := append([]string(nil), sess.ContinueArgs...)
	opts.innerArgs = append(inner, opts.innerArgs...)
	return opts, ExitOK
}

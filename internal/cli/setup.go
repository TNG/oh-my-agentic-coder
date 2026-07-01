package cli

import (
	"flag"
	"fmt"
	"os/exec"

	"github.com/tngtech/oh-my-agentic-coder/internal/builtinskills"
	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/plugin"
)

// runSetup provisions omac's built-in skill bundles into the native skills
// directory of each installed harness. Built-ins are guidance-only skills
// (SKILL.md, no omac.yaml): omac neither registers nor activates them, so this
// is pure file placement into the dir each harness's own loader reads.
func runSetup(args []string, env *Env) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	force := fs.Bool("force", false, "Overwrite a same-named skills directory even if it is not omac-owned.")
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac setup [harness] [--force]")
		fmt.Fprintln(env.Stderr, "")
		fmt.Fprintln(env.Stderr, "Provision omac's built-in skills into each installed harness's skills dir.")
		fmt.Fprintln(env.Stderr, "Optional [harness] (opencode|claude) narrows to one; default: all installed.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return ExitMisuse
	}

	var targets []config.Harness
	switch fs.NArg() {
	case 0:
		targets = installedHarnesses()
		if len(targets) == 0 {
			// No harness on PATH (e.g. setup run before installing one). Land
			// the bundle in every known harness's dir so it's there once a
			// harness arrives, rather than silently doing nothing.
			fmt.Fprintln(env.Stderr, "[warn] no harness detected on PATH; provisioning to all known harness skills dirs")
			targets = config.AllHarnesses()
		}
	case 1:
		h, ok := config.LookupHarness(fs.Arg(0))
		if !ok {
			fmt.Fprintln(env.Stderr, "omac setup:", config.UnknownHarnessError(fs.Arg(0)))
			return ExitMisuse
		}
		targets = []config.Harness{h}
	default:
		fs.Usage()
		return ExitMisuse
	}

	sOut := newStyler(env.Stdout)
	okTag := sOut.paint("[ok]", ansiBold, ansiGreen)
	skipTag := sOut.paint("[skip]", ansiBold, ansiYellow)
	exit := ExitOK
	foreign := false

	for _, h := range targets {
		dir := h.GlobalSkillsDir()
		if dir == "" {
			fmt.Fprintf(env.Stderr, "[warn] cannot resolve a global skills dir for %s; skipping\n", h.Name)
			exit = ExitIOError
			continue
		}
		for _, name := range builtinskills.Names() {
			res, err := builtinskills.Materialize(name, dir, *force)
			if err != nil {
				fmt.Fprintf(env.Stderr, "omac setup: %s (%s): %v\n", name, h.Name, err)
				exit = ExitIOError
				continue
			}
			switch res.Status {
			case builtinskills.StatusForeign:
				foreign = true
				fmt.Fprintf(env.Stdout, "%s %s (%s): a non-omac directory already exists at %s; left untouched\n",
					skipTag, sOut.bold(name), h.Name, res.Dir)
			case builtinskills.StatusUnchanged:
				fmt.Fprintf(env.Stdout, "%s %s (%s) %s\n", okTag, sOut.bold(name), h.Name, sOut.gray("already up to date"))
			case builtinskills.StatusCreated:
				fmt.Fprintf(env.Stdout, "%s %s (%s) %s\n", okTag, sOut.bold(name), h.Name, sOut.gray("installed → "+res.Dir))
			case builtinskills.StatusUpdated:
				fmt.Fprintf(env.Stdout, "%s %s (%s) %s\n", okTag, sOut.bold(name), h.Name, sOut.gray("refreshed → "+res.Dir))
			}
		}
	}

	if foreign {
		fmt.Fprintf(env.Stderr, "\n[hint] re-run with %s to overwrite the non-omac directories above.\n", sOut.bold("--force"))
	}
	return exit
}

// ensureBuiltinSkills idempotently provisions omac's built-in skills into the
// active harness's native skills dir on launch, so they are available with no
// separate setup step. It is quiet when nothing changes and never fails the
// launch — a provisioning error (or a foreign same-named dir) is a warning, not
// a hard stop. The explicit `omac setup` command remains for all-harness or
// forced (re)provisioning.
func ensureBuiltinSkills(env *Env, harness config.Harness) {
	dir := harness.GlobalSkillsDir()
	if dir == "" {
		return
	}
	for _, name := range builtinskills.Names() {
		res, err := builtinskills.Materialize(name, dir, false)
		if err != nil {
			fmt.Fprintf(env.Stderr, "[warn] could not provision built-in skill %s for %s: %v\n", name, harness.Name, err)
			continue
		}
		switch res.Status {
		case builtinskills.StatusCreated:
			fmt.Fprintf(env.Stderr, "[ok] provisioned built-in skill %s for %s\n", name, harness.Name)
		case builtinskills.StatusUpdated:
			fmt.Fprintf(env.Stderr, "[ok] refreshed built-in skill %s for %s\n", name, harness.Name)
		case builtinskills.StatusForeign:
			fmt.Fprintf(env.Stderr, "[hint] a non-omac %q directory exists for %s; not overwriting (run `omac setup --force` to replace)\n", name, harness.Name)
			// StatusUnchanged: stay silent — the common case on every launch.
		}
	}
}

// ensureOpenCodePlugin idempotently provisions omac's OpenCode bridge plugin
// into the harness's global plugins dir (~/.config/opencode/plugins) on
// launch, so the sandbox-briefing relay works even when the user never ran
// `omac plugin install`. Mirrors the global provisioning ensureBuiltinSkills
// does for skills: OpenCode-only, quiet when unchanged, and a failure (or a
// foreign same-named file, which InstallMultiDirIn refuses to clobber) is a
// warning, never a launch blocker.
func ensureOpenCodePlugin(env *Env, harness config.Harness) {
	if !harness.NeedsPluginBootstrap {
		return
	}
	dir := harness.GlobalBridgeDir()
	if dir == "" {
		return
	}
	// force=false, so an existing differing file yields an error (not an
	// overwrite) and Unchanged covers the silent common path.
	res, err := plugin.InstallMultiDirIn(dir, false)
	if err != nil {
		fmt.Fprintf(env.Stderr, "[warn] could not provision the omac OpenCode plugin (%v); the sandbox briefing won't appear in OpenCode. Install it with: omac plugin install opencode-desktop --global\n", err)
		return
	}
	if !res.Unchanged {
		fmt.Fprintln(env.Stderr, "[ok] provisioned the omac OpenCode plugin")
	}
}

// installedHarnesses returns the registered harnesses whose inner command
// resolves on PATH.
func installedHarnesses() []config.Harness {
	var out []config.Harness
	for _, h := range config.AllHarnesses() {
		if len(h.InnerCmd) == 0 {
			continue
		}
		if _, err := exec.LookPath(h.InnerCmd[0]); err == nil {
			out = append(out, h)
		}
	}
	return out
}

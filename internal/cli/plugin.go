package cli

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/plugin"
	"github.com/TNG/oh-my-agentic-coder/internal/prefs"
)

// pluginTarget names an installable client-side bridge plugin and maps it
// to the harness whose bridge directory it belongs in.
type pluginTarget struct {
	// name is the canonical selector accepted by `omac plugin install <name>`.
	name string
	// aliases are additional accepted spellings.
	aliases []string
	// harness is the harness name whose BridgeDir hosts this plugin.
	harness string
	// summary is a short one-line description for help text.
	summary string
}

// pluginTargets is the set of plugins `omac plugin install` understands.
// Today there is exactly one: the OpenCode Desktop multi-directory plugin.
func pluginTargets() []pluginTarget {
	return []pluginTarget{
		{
			name:    "opencode-desktop",
			aliases: []string{"multidir", "opencode"},
			harness: "opencode",
			summary: "OpenCode Desktop multi-directory plugin (the omac serve bridge).",
		},
	}
}

func lookupPluginTarget(name string) (pluginTarget, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	for _, t := range pluginTargets() {
		if t.name == want {
			return t, true
		}
		for _, a := range t.aliases {
			if a == want {
				return t, true
			}
		}
	}
	return pluginTarget{}, false
}

// runPlugin dispatches `omac plugin <action> ...`.
func runPlugin(args []string, env *Env) int {
	if len(args) == 0 {
		printPluginUsage(env.Stderr)
		return ExitMisuse
	}
	action, rest := args[0], args[1:]
	switch action {
	case "install":
		return runPluginInstall(rest, env)
	case "-h", "--help", "help":
		printPluginUsage(env.Stdout)
		return ExitOK
	default:
		fmt.Fprintf(env.Stderr, "omac plugin: unknown action %q\n", action)
		printPluginUsage(env.Stderr)
		return ExitMisuse
	}
}

// runPluginInstall implements `omac plugin install <target>`.
func runPluginInstall(args []string, env *Env) int {
	fs := flag.NewFlagSet("plugin install", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	force := fs.Bool("force", false, "Overwrite an existing plugin file even if it differs from the bundled version.")
	global := fs.Bool("global", false, "Install into the harness's user-global plugin directory (e.g. ~/.config/opencode/plugins) instead of this workdir.")
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintln(out, "Usage: omac plugin install <target> [--global] [--force]")
		fmt.Fprintln(out, "\nTargets:")
		for _, t := range pluginTargets() {
			fmt.Fprintf(out, "  %-18s %s\n", t.name, t.summary)
		}
		fmt.Fprintln(out)
		fs.PrintDefaults()
	}
	if code, ok := parseFlags(fs, args, env); !ok {
		return code
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(env.Stderr, "omac plugin install: missing target (e.g. opencode-desktop)")
		fs.Usage()
		return ExitMisuse
	}
	if len(rest) > 1 {
		fmt.Fprintf(env.Stderr, "omac plugin install: unexpected extra arguments: %s\n", strings.Join(rest[1:], " "))
		return ExitMisuse
	}

	target, ok := lookupPluginTarget(rest[0])
	if !ok {
		names := make([]string, 0)
		for _, t := range pluginTargets() {
			names = append(names, t.name)
		}
		fmt.Fprintf(env.Stderr, "omac plugin install: unknown target %q (supported: %s)\n",
			rest[0], strings.Join(names, ", "))
		return ExitMisuse
	}

	harness, hok := config.LookupHarness(target.harness)
	if !hok || harness.BridgeDir == "" {
		fmt.Fprintf(env.Stderr, "omac plugin install: harness %q has no bridge directory\n", target.harness)
		return ExitGeneric
	}

	var (
		res   plugin.InstallResult
		err   error
		scope string
	)
	if *global {
		gdir := harness.GlobalBridgeDir()
		if gdir == "" {
			fmt.Fprintf(env.Stderr, "omac plugin install: cannot resolve a global plugin directory for harness %q (set $HOME or $XDG_CONFIG_HOME)\n", target.harness)
			return ExitIOError
		}
		scope = "global"
		res, err = plugin.InstallMultiDirIn(gdir, *force)
	} else {
		scope = "workdir"
		res, err = plugin.InstallMultiDir(env.Workdir, harness.BridgeDir, *force)
	}
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac plugin install:", err)
		return ExitIOError
	}

	sOut := newStyler(env.Stdout)
	okTag := sOut.paint("[ok]", ansiBold, ansiGreen)
	scopeTag := sOut.gray("(" + scope + ")")
	switch {
	case res.Unchanged:
		fmt.Fprintf(env.Stdout, "%s %s %s already installed (up to date) at %s\n",
			okTag, sOut.bold(target.name), scopeTag, res.Path)
	case res.Overwrote:
		fmt.Fprintf(env.Stdout, "%s %s %s reinstalled (overwrote existing) at %s\n",
			okTag, sOut.bold(target.name), scopeTag, res.Path)
	default:
		fmt.Fprintf(env.Stdout, "%s installed %s %s at %s\n",
			okTag, sOut.bold(target.name), scopeTag, res.Path)
	}
	return ExitOK
}

// warnPluginMissing checks whether the harness's client-side multidir
// bridge plugin is installed (in env.Workdir OR in the harness's
// user-global plugin directory) and, if it is not, warns the user. It
// returns true when serve should proceed and false when the user chose to
// abort.
//
// Behavior:
//   - Harnesses without a bridge directory (none today, but defensive),
//     and the case where the plugin is already present either workdir-local
//     or globally, return true silently.
//   - If the user has previously chosen "do not warn again" (persisted in
//     the global prefs), it returns true silently.
//   - When stdin is an interactive terminal, it presents three choices:
//     continue, abort, or never warn again (which it persists).
//   - When stdin is not a TTY (OpenCode Desktop, CI, piped input), it
//     cannot prompt; it warns once to stderr and proceeds.
func warnPluginMissing(env *Env, harness config.Harness) bool {
	if harness.BridgeDir == "" {
		return true
	}
	installed, err := plugin.IsMultiDirInstalled(env.Workdir, harness.BridgeDir)
	if err != nil {
		// Unknown state (e.g. permission error): don't block startup, just
		// note it in case it matters.
		fmt.Fprintf(env.Stderr, "omac serve: [warn] could not check for the %s plugin: %v\n",
			plugin.MultiDirFileName, err)
		return true
	}
	if installed {
		return true
	}
	// A global install satisfies OpenCode just as well (it auto-loads from
	// ~/.config/opencode/plugins too), so don't warn if it's there.
	if gdir := harness.GlobalBridgeDir(); gdir != "" {
		if ok, gerr := plugin.IsMultiDirInstalledIn(gdir); gerr == nil && ok {
			return true
		}
	}

	// Respect a persisted "do not warn again".
	if p, err := prefs.Load(); err == nil && p.SuppressPluginWarning {
		return true
	}

	st := newStyler(env.Stderr)
	pluginPath := plugin.MultiDirPath(env.Workdir, harness.BridgeDir)
	warnTag := st.paint("[warn]", ansiBold, ansiYellow)
	fmt.Fprintln(env.Stderr)
	fmt.Fprintf(env.Stderr, "%s the OpenCode Desktop multi-directory plugin is not installed in this workdir.\n", warnTag)
	fmt.Fprintf(env.Stderr, "       expected: %s\n", st.bold(pluginPath))
	fmt.Fprintln(env.Stderr, "       Without it, OpenCode Desktop will not surface this directory's skills.")
	fmt.Fprintf(env.Stderr, "       Install it with: %s\n", st.paint("omac plugin install opencode-desktop", ansiBold, ansiGreen))
	fmt.Fprintf(env.Stderr, "       (or install it for every workdir with: %s)\n", st.paint("omac plugin install opencode-desktop --global", ansiBold, ansiGreen))

	// Non-interactive: we can't prompt, so warn once and continue.
	if env.Stdin == nil || !term.IsTerminal(int(env.Stdin.Fd())) {
		fmt.Fprintln(env.Stderr, "       (stdin is not a terminal; continuing without the plugin)")
		return true
	}

	// Interactive three-way prompt.
	fmt.Fprintln(env.Stderr)
	fmt.Fprintf(env.Stderr, "  %s Continue without the plugin\n", st.bold("[c]"))
	fmt.Fprintf(env.Stderr, "  %s Abort the start of omac\n", st.bold("[a]"))
	fmt.Fprintf(env.Stderr, "  %s Do not warn me again\n", st.bold("[n]"))
	reader := bufio.NewReader(env.Stdin)
	for {
		fmt.Fprintf(env.Stderr, "%s choose [c/a/n] (default c): ", st.cyan("?"))
		line, err := reader.ReadString('\n')
		choice := strings.ToLower(strings.TrimSpace(line))
		switch choice {
		case "", "c", "continue":
			return true
		case "a", "abort":
			return false
		case "n", "never", "no":
			p, lerr := prefs.Load()
			if lerr != nil {
				p = &prefs.Store{}
			}
			p.SuppressPluginWarning = true
			if serr := prefs.Save(p); serr != nil {
				fmt.Fprintf(env.Stderr, "omac serve: [warn] could not persist preference: %v (continuing)\n", serr)
			} else {
				fmt.Fprintln(env.Stderr, "omac serve: ok — you won't be warned about this again.")
			}
			return true
		default:
			fmt.Fprintf(env.Stderr, "  please answer c, a, or n.\n")
		}
		// On EOF with no usable input, default to continue.
		if err != nil {
			return true
		}
	}
}

func printPluginUsage(w *os.File) {
	fmt.Fprintln(w, `omac plugin — manage client-side harness bridge plugins

Usage:
  omac [--workdir <dir>] plugin install <target> [--global] [--force]

Actions:
  install   Install a bundled bridge plugin (this workdir by default, or
            the user-global plugin directory with --global).

Targets:
  opencode-desktop   OpenCode Desktop multi-directory plugin (the omac serve bridge).

Flags:
  --global   Install into the harness's user-global plugin directory
             (e.g. ~/.config/opencode/plugins) instead of this workdir.
  --force    Overwrite an existing, differing plugin file.

Examples:
  omac plugin install opencode-desktop
  omac plugin install opencode-desktop --global
  omac --workdir ~/proj plugin install opencode-desktop --force`)
}

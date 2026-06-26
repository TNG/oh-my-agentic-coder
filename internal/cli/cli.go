// Package cli dispatches the omac command-line interface.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
)

// Exit codes mirror those documented in oh-my-agentic-coder.md §10.6.
const (
	ExitOK                     = 0
	ExitGeneric                = 1
	ExitMisuse                 = 2
	ExitConfigInvalid          = 3
	ExitPrerequisiteMissing    = 4
	ExitIOError                = 5
	ExitSidecarHealthcheckFail = 6
	ExitSandboxAbnormal        = 7
	ExitKeychainError          = 8
	ExitSecretRefused          = 9
)

// Command is a runnable omac subcommand.
type Command struct {
	Name  string
	Short string
	Run   func(args []string, env *Env) int
}

// Env captures process-wide state passed to every subcommand.
type Env struct {
	Version string
	Workdir string
	Stdout  *os.File
	Stderr  *os.File
	Stdin   *os.File
}

// Run is the process entry point. It returns the OS exit code.
func Run(args []string, version string) int {
	env := &Env{
		Version: version,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Stdin:   os.Stdin,
	}
	// Resolve workdir. --workdir <dir> may appear anywhere before the
	// subcommand. We parse a small top-level flag set greedily.
	subArgs, wd, err := splitTopLevelFlags(args)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac:", err)
		printUsage(env.Stderr)
		return ExitMisuse
	}
	if wd == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(env.Stderr, "omac: cannot resolve cwd:", err)
			return ExitIOError
		}
		wd = cwd
	}
	abs, err := filepath.Abs(wd)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac: cannot absolutize workdir:", err)
		return ExitIOError
	}
	env.Workdir = abs

	if len(subArgs) == 0 {
		printUsage(env.Stderr)
		return ExitMisuse
	}
	name, rest := subArgs[0], subArgs[1:]
	cmd, ok := commands()[name]
	if !ok {
		fmt.Fprintf(env.Stderr, "omac: unknown subcommand %q\n", name)
		printUsage(env.Stderr)
		return ExitMisuse
	}
	return cmd.Run(rest, env)
}

// splitTopLevelFlags pulls --workdir <dir> (and --workdir=<dir>) out of args
// before the first positional. Returns the remaining args and the workdir (or "").
func splitTopLevelFlags(args []string) ([]string, string, error) {
	var (
		out []string
		wd  string
		i   int
	)
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--workdir":
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("--workdir requires a value")
			}
			wd = args[i+1]
			i += 2
		case len(a) > len("--workdir=") && a[:len("--workdir=")] == "--workdir=":
			wd = a[len("--workdir="):]
			i++
		case a == "--help" || a == "-h":
			printUsage(os.Stderr)
			os.Exit(ExitOK)
		default:
			out = append(out, args[i:]...)
			return out, wd, nil
		}
	}
	return out, wd, nil
}

func commands() map[string]Command {
	return map[string]Command{
		"register":   {Name: "register", Short: "Register a skill's sidecar in this workdir.", Run: runRegister},
		"deregister": {Name: "deregister", Short: "Deregister a skill's sidecar.", Run: runDeregister},
		"list":       {Name: "list", Short: "List registered skills.", Run: runList},
		"secrets":    {Name: "secrets", Short: "Manage skill secrets in the OS keychain.", Run: runSecrets},
		"config":     {Name: "config", Short: "Show resolved config + secret fingerprints for a skill.", Run: runConfig},
		"start":      {Name: "start", Short: "Start sidecars + facade + sandbox. Optional [harness]: opencode|claude.", Run: runStart},
		"continue":   {Name: "continue", Short: "Continue the last session for this workdir. Optional [harness].", Run: runContinue},
		"resume":     {Name: "resume", Short: "Pick a recent session for this workdir and launch it. Optional [harness].", Run: runResume},
		"serve":      {Name: "serve", Short: "Long-lived multi-directory server. Optional [harness]: opencode|claude.", Run: runServe},
		"setup":      {Name: "setup", Short: "Provision omac's built-in skills into installed harnesses' skills dirs.", Run: runSetup},
		"plugin":     {Name: "plugin", Short: "Install client-side harness bridge plugins (e.g. opencode-desktop).", Run: runPlugin},
		"sandbox":    {Name: "sandbox", Short: "Built-in kernel sandbox (run|stage2).", Run: runSandbox},
		"doctor":     {Name: "doctor", Short: "Run sanity checks.", Run: runDoctor},
		"version":    {Name: "version", Short: "Print version.", Run: runVersion},
	}
}

func runVersion(_ []string, env *Env) int {
	fmt.Fprintln(env.Stdout, "omac", env.Version)
	return ExitOK
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, `omac — oh-my-agentic-coder

Usage:
  omac [--workdir <dir>] <subcommand> [flags] [args]

Subcommands:
  register     Register a skill's sidecar in this workdir.
  deregister   Deregister a skill's sidecar.
  list         List registered skills.
  secrets      Manage skill secrets in the OS keychain.
  config       Show resolved config + secret fingerprints for a skill.
  setup        Provision omac's built-in skills into installed harnesses.
  start        Start sidecars + facade + sandbox.       [harness]: opencode|claude
  continue     Continue the last session for this workdir.   [harness]: opencode|claude
  resume       Pick a recent session for this workdir, launch it. [harness]: opencode|claude
  serve        Long-lived multi-directory server.        [harness]: opencode|claude
  plugin       Install client-side bridge plugins (e.g. opencode-desktop).
  sandbox      Built-in kernel sandbox: omac sandbox run [flags] -- <cmd>.
  doctor       Run sanity checks.
  version      Print version.

Harness selection (start/serve):
  omac start            # default harness (opencode)
  omac start opencode   # OpenCode
  omac start claude     # Claude Code

Run 'omac <subcommand> --help' for subcommand-specific flags.`)
}

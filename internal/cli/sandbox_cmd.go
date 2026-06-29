package cli

import (
	"fmt"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxrun"
)

// runSandbox dispatches `omac sandbox <verb>`.
//
//	omac sandbox run [flags] -- <cmd> [args...]   supervisor entry point
//	omac sandbox stage2 ...                        hidden; exec'd by bwrap (Linux)
func runSandbox(args []string, env *Env) int {
	if len(args) == 0 {
		printSandboxUsage(env)
		return ExitMisuse
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "run":
		return runSandboxRun(rest, env)
	case "stage2":
		// Linux-only: applies Landlock net rules inside bwrap, then
		// execs the inner command. Returns only on error.
		if err := sandboxrun.RunStage2(rest); err != nil {
			fmt.Fprintln(env.Stderr, "omac sandbox stage2:", err)
			return ExitSandboxAbnormal
		}
		return ExitOK
	case "--help", "-h", "help":
		printSandboxUsage(env)
		return ExitOK
	default:
		fmt.Fprintf(env.Stderr, "omac sandbox: unknown verb %q\n", verb)
		printSandboxUsage(env)
		return ExitMisuse
	}
}

func runSandboxRun(args []string, env *Env) int {
	flags, err := sandboxprofile.ParseFlags(args)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac sandbox run:", err)
		fmt.Fprintln(env.Stderr, "usage: omac sandbox run [--profile <ref>] [--allow <path>] [--read <path>] [--write <path>] [--deny <path|glob>] [--allow-file <path>] [--allow-unix-dir <dir>] [--open-port <port>] [--listen-port <port>] [--allow-tcp-connect <port>] [--allow-domain <d>] [--deny-domain <d>] [--block-net] [--workdir-access <level>] -- <cmd> [args...]")
		return ExitMisuse
	}
	return sandboxrun.Run(sandboxrun.Options{
		Flags:   flags,
		Workdir: env.Workdir,
		Stderr:  env.Stderr,
	})
}

func printSandboxUsage(env *Env) {
	fmt.Fprintln(env.Stderr, `omac sandbox — built-in kernel sandbox (Seatbelt on macOS, bubblewrap+Landlock on Linux)

Usage:
  omac sandbox run [flags] -- <cmd> [args...]

Flags (list flags are repeatable; they merge additively onto the profile):
  --profile <ref>            profile name, path, or builtin (default: "default")
  --allow <path>             grant read+write on a directory or file
  --read <path>              grant read-only
  --write <path>             grant write-only
  --deny <path|glob>         mask a path within granted trees; a bare name
                             like ".env" or "*.key" matches in every granted
                             directory (the cwd included)
  --allow-file <path>        grant read+write on a single file (e.g. a unix socket)
  --allow-unix-dir <dir>     allow AF_UNIX connect to any socket under <dir> (dynamic sockets)
  --open-port <port>         localhost TCP, connect+bind (e.g. the omac bridge port)
  --listen-port <port>       allow binding/listening on a TCP port
  --allow-tcp-connect <port> direct outbound TCP to any host on this port (e.g. 22)
  --allow-domain <domain>    add to the proxy allowlist (exact or *.suffix)
  --deny-domain <domain>     add to the proxy blocklist (exact or *.suffix)
  --block-net                block all network access (overrides profile)
  --workdir-access <level>   none|read|write|readwrite (replaces profile value)`)
}

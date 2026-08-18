package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

// splitHarnessToken inspects the first token of a subcommand's args and
// resolves the inner-harness selector.
//
// The first positional slot (before any flag and before "--") is the harness
// selector slot:
//
//   - empty args, a leading flag, or a leading "--": no selector given →
//     default harness, args unchanged.
//   - a known harness name/alias: consume it → that harness, remaining args.
//   - any other bareword: treated as an attempted-but-unknown harness and
//     rejected (err non-nil), so typos like `omac start claud` fail loudly
//     instead of being silently passed through as an inner argument. Inner
//     arguments that happen to be barewords must be placed after "--".
//
// This implements the positional-harness UX: `omac start claude --verbose`,
// `omac start opencode`, `omac start` (defaults to opencode), and
// `omac start -- some-inner-arg`.
func splitHarnessToken(args []string) (config.Harness, []string, error) {
	if len(args) == 0 {
		return config.DefaultHarness(), args, nil
	}
	first := args[0]
	if first == "" || first == "--" || isFlag(first) {
		return config.DefaultHarness(), args, nil
	}
	if h, ok := config.LookupHarness(first); ok {
		return h, args[1:], nil
	}
	return config.Harness{}, nil, config.UnknownHarnessError(first)
}

// reorderFlagsFirst sorts args so all flag-like tokens ("-x", "--xx", "--xx=v")
// come before any positional. A bare "--" is a hard stop that forwards the rest
// verbatim, and "-" (a single dash) is treated as a positional (convention for
// stdin).
//
// This is a small QoL tweak so users can write either
//
//	omac register demo-echo --no-secrets
//
// or
//
//	omac register --no-secrets demo-echo
//
// without the stdlib flag package rejecting the first form.
//
// Value-taking flags are kept adjacent to their value: any "--foo bar" pair
// where the second token is not itself a flag moves as a unit. Boolean flags
// are looked up on fs and never absorb the following token — the stdlib parser
// does not consume a value for them, so gluing the next token on would make it
// stop parsing at that positional and silently drop every later flag (see
// TestReorderFlagsFirst_BoolFlagDoesNotSwallowPositional).
//
// Users with a positional literally starting with "-" should pass "--" first.
func reorderFlagsFirst(fs *flag.FlagSet, args []string) []string {
	var flags, positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			// Everything after -- is positional verbatim.
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if isFlag(a) {
			flags = append(flags, a)
			// If this looks like "--foo" with no "=" and a following value token
			// that is not itself a flag, take that value with it.
			if !strings.Contains(a, "=") && !isBoolFlag(fs, a) && i+1 < len(args) && !isFlag(args[i+1]) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		positionals = append(positionals, a)
	}
	return append(flags, positionals...)
}

// isBoolFlag reports whether token names a boolean flag registered on fs.
// Unknown flags answer false: fs.Parse rejects them anyway, so keeping the
// value-taking behavior leaves typo diagnostics unchanged.
func isBoolFlag(fs *flag.FlagSet, token string) bool {
	if fs == nil {
		return false
	}
	name := strings.TrimLeft(token, "-")
	if eq := strings.IndexByte(name, '='); eq >= 0 {
		name = name[:eq]
	}
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func isFlag(a string) bool {
	return len(a) >= 2 && a[0] == '-' && a != "-"
}

// wantsHelp reports whether an explicit -h/--help appears before a "--" stop.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

// parseFlags applies the shared help contract: an explicit -h/--help prints
// usage on stdout and stops with ExitOK; a real parse error leaves flag's own
// message on stderr and stops with ExitMisuse. When proceed is true, parsing
// succeeded and the caller should continue.
func parseFlags(fs *flag.FlagSet, args []string, env *Env) (code int, proceed bool) {
	reordered := reorderFlagsFirst(fs, args)
	if wantsHelp(reordered) {
		fs.SetOutput(env.Stdout)
		fs.Usage()
		return ExitOK, false
	}
	fs.SetOutput(env.Stderr)
	if err := fs.Parse(reordered); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// Defensive: ContinueOnError + PrintDefaults already ran on
			// stderr via the default Usage; treat as misuse only if
			// wantsHelp somehow missed it (should not happen).
			return ExitMisuse, false
		}
		return ExitMisuse, false
	}
	return ExitOK, true
}

// writeLaunchUsage prints the start-family / serve usage: omac flags, then
// `--` plus whatever the inner harness should see.
func writeLaunchUsage(cmd string, fs *flag.FlagSet) {
	w := fs.Output()
	fmt.Fprintf(w, "Usage: omac %s [harness] [flags] [-- <harness args>]\n", cmd)
	fmt.Fprintf(w, "  Args after -- go to the harness, e.g. omac %s --verbose -- run --model x\n", cmd)
	fmt.Fprintf(w, "\nharness: one of %s (default: %s)\n\n",
		strings.Join(config.HarnessNames(), ", "), config.DefaultHarness().Name)
	fs.PrintDefaults()
}

// parseWithHarnessArgsHint parses fs after reorderFlagsFirst. On failure it
// prints the stdlib error, a `--` reminder, then usage — in that order, so
// the reminder is not buried under PrintDefaults. An explicit -h/--help
// prints usage on stdout and returns ExitOK, matching parseFlags. Parse's
// own error/usage write is discarded because FlagSet always emits those
// before returning.
func parseWithHarnessArgsHint(fs *flag.FlagSet, cmd string, args []string, env *Env) (code int, proceed bool) {
	reordered := reorderFlagsFirst(fs, args)
	if wantsHelp(reordered) {
		fs.SetOutput(env.Stdout)
		fs.Usage()
		return ExitOK, false
	}
	fs.SetOutput(io.Discard)
	err := fs.Parse(reordered)
	fs.SetOutput(env.Stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(env.Stderr, err)
			writeInnerArgsParseHint(cmd, err, env.Stderr)
		}
		fs.Usage()
		return ExitMisuse, false
	}
	return ExitOK, true
}

// writeInnerArgsParseHint reminds the user that harness flags go after --.
// Boolean-aware reorderFlagsFirst no longer hides later harness flags behind
// a glued positional, so mixed forms like `omac start --verbose run --model x`
// now fail as unknown omac flags. Help requests already print Usage.
func writeInnerArgsParseHint(cmd string, err error, w io.Writer) {
	if err == nil || errors.Is(err, flag.ErrHelp) || w == nil {
		return
	}
	fmt.Fprintf(w, "omac %s: pass harness flags after -- (e.g. omac %s --verbose -- run --model x)\n", cmd, cmd)
}

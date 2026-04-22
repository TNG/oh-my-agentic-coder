package cli

import "strings"

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
// It does NOT know which flags take values; any "--foo bar" pair where the
// second token is not itself a flag is kept adjacent. Users with a positional
// literally starting with "-" should pass "--" first.
func reorderFlagsFirst(args []string) []string {
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
			if !strings.Contains(a, "=") && i+1 < len(args) && !isFlag(args[i+1]) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		positionals = append(positionals, a)
	}
	return append(flags, positionals...)
}

func isFlag(a string) bool {
	return len(a) >= 2 && a[0] == '-' && a != "-"
}

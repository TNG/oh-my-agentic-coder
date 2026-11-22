package cli

import "fmt"

// runMCPPin is a stub until the command is implemented.
func runMCPPin(args []string, env *Env) int {
	fmt.Fprintln(env.Stderr, "omac mcp pin: not implemented")
	return ExitGeneric
}

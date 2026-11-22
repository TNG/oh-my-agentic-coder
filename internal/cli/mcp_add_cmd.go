package cli

import "fmt"

// runMCPAdd is a stub until the command is implemented.
func runMCPAdd(args []string, env *Env) int {
	fmt.Fprintln(env.Stderr, "omac mcp add: not implemented")
	return ExitGeneric
}

// runMCPAddJSON is a stub until the command is implemented.
func runMCPAddJSON(args []string, env *Env) int {
	fmt.Fprintln(env.Stderr, "omac mcp add-json: not implemented")
	return ExitGeneric
}

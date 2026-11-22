package cli

import "fmt"

// runMCPTools is a stub until the command is implemented.
func runMCPTools(args []string, env *Env) int {
	fmt.Fprintln(env.Stderr, "omac mcp tools: not implemented")
	return ExitGeneric
}

package cli

import "fmt"

func runMCPServe(_ []string, env *Env) int {
	fmt.Fprintln(env.Stderr, "omac mcp serve: not implemented")
	return ExitGeneric
}

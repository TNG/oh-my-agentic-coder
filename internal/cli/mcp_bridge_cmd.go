package cli

import "fmt"

func runMCPBridge(_ []string, env *Env) int {
	fmt.Fprintln(env.Stderr, "omac mcp bridge: not implemented")
	return ExitGeneric
}

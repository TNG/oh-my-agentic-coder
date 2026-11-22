package cli

import "fmt"

// runMCPDiagnose is a stub until the command is implemented.
func runMCPDiagnose(args []string, env *Env) int {
	fmt.Fprintln(env.Stderr, "omac mcp diagnose: not implemented")
	return ExitGeneric
}

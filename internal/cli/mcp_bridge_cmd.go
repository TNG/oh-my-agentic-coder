package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/bridge"
)

func runMCPBridge(args []string, env *Env) int {
	if len(args) != 1 {
		fmt.Fprintln(env.Stderr, "usage: omac mcp bridge <name>")
		return ExitMisuse
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := bridge.Run(ctx, args[0], os.Stdin, os.Stdout, os.Getenv); err != nil {
		fmt.Fprintf(env.Stderr, "omac mcp bridge: %v\n", err)
		return ExitGeneric
	}
	return ExitOK
}

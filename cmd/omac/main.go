// Command omac is the oh-my-agentic-coder CLI.
//
// It bridges host-side sidecar HTTP services into a sandboxed agent-coding
// environment through a single Unix-domain-socket facade, storing per-skill
// secrets in the OS keychain.
//
// See oh-my-agentic-coder.md in this repository for the full design.
package main

import (
	"fmt"
	"os"

	"github.com/tngtech/oh-my-agentic-coder/internal/cli"
)

// Version is the binary version string. Overridden at link time via -ldflags.
var Version = "0.1.0-dev"

func main() {
	exitCode := cli.Run(os.Args[1:], Version)
	if exitCode != 0 {
		// cli.Run has already printed a user-facing message.
		os.Exit(exitCode)
	}
}

// Keep fmt referenced for linters if cli.Run is later inlined.
var _ = fmt.Sprintf

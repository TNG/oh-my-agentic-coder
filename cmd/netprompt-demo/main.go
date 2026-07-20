// Command netprompt-demo pops up the real omac network-access dialog so its
// appearance can be inspected on the current platform (zenity/kdialog on
// Linux, osascript on macOS).
//
// It drives the same netprompt.Prompter used in production, so any change to
// the dialog code — window size, option labels, prompt text — is reflected
// here automatically; there is no separate demo layout to keep in sync.
//
// Usage:
//
//	go run ./cmd/netprompt-demo [-host H] [-port P] [-intent TEXT]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/tngtech/oh-my-agentic-coder/internal/netprompt"
)

func main() {
	host := flag.String("host", "api.github.com", "destination host shown in the dialog")
	port := flag.Int("port", 443, "destination port shown in the dialog")
	intent := flag.String("intent", "clone the target repository", "agent-declared intent line (empty shows \"not declared\")")
	flag.Parse()

	// Force the real GUI backend even if a stub decision source is configured
	// in the environment (e.g. from an e2e shell).
	os.Unsetenv("OMAC_PROMPT_STUB")

	var lookupIntent func(string) (string, bool)
	if *intent != "" {
		lookupIntent = func(string) (string, bool) { return *intent, true }
	}

	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
	p, ok := netprompt.NewPrompter(120, logf, lookupIntent, nil, nil, nil)
	if !ok {
		fmt.Fprintln(os.Stderr, "no dialog backend available (install zenity or kdialog on Linux, osascript ships with macOS)")
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "showing network-access dialog for %s:%d ...\n", *host, *port)
	res := p.Prompt(context.Background(), *host, *port)
	fmt.Printf("decision: allow=%v persist=%v scope=%q suffix=%q needs_intent=%v\n",
		res.Allow, res.Persist, res.Scope, res.Suffix, res.NeedsIntent)
}

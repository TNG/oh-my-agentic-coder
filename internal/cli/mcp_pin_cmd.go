package cli

import (
	"bufio"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
)

func runMCPPin(args []string, env *Env) int {
	fs := flag.NewFlagSet("mcp pin", flag.ContinueOnError)
	scopeFlag := fs.String("scope", scopeProject, "Where the server is registered: project (this workdir) or global.")
	yes := fs.Bool("yes", false, "Pin without asking.")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: omac mcp pin <name> [--scope project|global] [--yes]")
		fs.PrintDefaults()
	}
	if code, ok := parseFlags(fs, args, env); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return ExitMisuse
	}
	name := fs.Arg(0)
	if refuseInsideSandbox(env, "pin") {
		return ExitMisuse
	}
	scope, err := resolveMCPScope(*scopeFlag, env)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp pin:", err)
		return ExitMisuse
	}
	entry, live, ok := probeMCPServer(env, "pin", scope, name)
	if !ok {
		return ExitGeneric
	}
	drift := mcppolicy.Compare(entry.Def.Pin, live)
	printDrift(env, entry.Def.Pin, drift)

	current := currentClasses(entry.Def)
	var missing []string
	for _, t := range sortedLive(live) {
		if _, ok := current[t.Name]; !ok {
			missing = append(missing, stripControlChars(t.Name))
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(env.Stderr, "omac mcp pin: refused: no class for %s; classify it with omac mcp tools %s --classify\n", strings.Join(missing, ", "), name)
		return ExitMisuse
	}
	if entry.Def.Pin != nil && drift.Empty() {
		fmt.Fprintln(env.Stdout, "nothing to pin: the live tools match the pin")
		return ExitOK
	}
	if !*yes && !confirmPin(env, entry.Name) {
		fmt.Fprintln(env.Stderr, "omac mcp pin: not pinned")
		return ExitGeneric
	}
	hashes := liveHashes(live)
	if _, err := mcpreg.Update(scope, name, func(d *mcpreg.Definition) error {
		d.Pin = &mcppolicy.Pin{ToolsHash: mcppolicy.ListHash(hashes), Tools: hashes, PinnedAt: time.Now().UTC()}
		return nil
	}); err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp pin:", err)
		return ExitIOError
	}
	fmt.Fprintf(env.Stdout, "pinned %d tools of %s\n", len(live), stripControlChars(entry.Name))
	notifyMCPReload(env, scope)
	return ExitOK
}

func printDrift(env *Env, pin *mcppolicy.Pin, d mcppolicy.Drift) {
	if pin == nil {
		fmt.Fprintf(env.Stdout, "not pinned yet; live tools: %s\n", joinNames(d.Added))
		return
	}
	if d.Empty() {
		fmt.Fprintln(env.Stdout, "no drift: the live tools match the pin")
		return
	}
	fmt.Fprintln(env.Stdout, "drift since the pin of", pin.PinnedAt.Format("2006-01-02"))
	for _, g := range []struct {
		label string
		names []string
	}{{"added", d.Added}, {"removed", d.Removed}, {"changed", d.Changed}} {
		if len(g.names) > 0 {
			fmt.Fprintf(env.Stdout, "  %-8s%s\n", g.label, joinNames(g.names))
		}
	}
}

func joinNames(names []string) string {
	clean := make([]string, len(names))
	for i, n := range names {
		clean[i] = stripControlChars(n)
	}
	return strings.Join(clean, ", ")
}

func confirmPin(env *Env, name string) bool {
	fmt.Fprintf(env.Stdout, "Pin the live tools of %s as reviewed? [y/N] ", stripControlChars(name))
	line, _ := bufio.NewReader(env.Stdin).ReadString('\n')
	return parseYesNo(line)
}

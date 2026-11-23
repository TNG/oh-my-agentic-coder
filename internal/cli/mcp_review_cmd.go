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

func runMCPReview(args []string, env *Env) int {
	fs := flag.NewFlagSet("mcp review", flag.ContinueOnError)
	scopeFlag := fs.String("scope", scopeProject, "Where the server is registered: project (this workdir) or global.")
	yes := fs.Bool("yes", false, "Accept the current tools without asking.")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: omac mcp review <name> [--scope project|global] [--yes]")
		fmt.Fprintln(fs.Output(), "Shows which tools of the server were added, removed or changed since the last review,")
		fmt.Fprintln(fs.Output(), "and after you confirm records the current tools as reviewed. Until then the agent can call no tool.")
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
	if refuseInsideSandbox(env, "review") {
		return ExitMisuse
	}
	scope, err := resolveMCPScope(*scopeFlag, env)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp review:", err)
		return ExitMisuse
	}
	entry, live, ok := probeMCPServer(env, "review", scope, name)
	if !ok {
		return ExitGeneric
	}
	changes := mcppolicy.Compare(entry.Def.Review, live)
	printChanges(env, entry.Def.Review, changes)
	printChangeDetails(env, entry.Def.Review, live, changes)

	current := currentClasses(entry.Def)
	var missing []string
	for _, t := range sortedLive(live) {
		if _, ok := current[t.Name]; !ok {
			missing = append(missing, stripControlChars(t.Name))
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(env.Stderr, "omac mcp review: refused: no class for %s; classify it with omac mcp tools %s --classify\n", strings.Join(missing, ", "), name)
		return ExitMisuse
	}
	if changes.Empty() {
		if entry.Def.Review == nil {
			fmt.Fprintf(env.Stdout, "nothing to review: the tools of %s have not been reviewed yet; run: omac mcp tools %s --classify\n", stripControlChars(entry.Name), name)
		} else {
			fmt.Fprintln(env.Stdout, "nothing to review: the tools are unchanged since the last review")
		}
		return ExitOK
	}
	if !*yes && !confirmReview(env, entry.Name) {
		fmt.Fprintln(env.Stderr, "omac mcp review: not accepted")
		return ExitGeneric
	}
	hashes := liveHashes(live)
	if _, err := mcpreg.Update(scope, name, func(d *mcpreg.Definition) error {
		d.Review = &mcppolicy.Review{ToolsHash: mcppolicy.ListHash(hashes), Tools: hashes, Text: liveText(live), At: time.Now().UTC()}
		return nil
	}); err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp review:", err)
		return ExitIOError
	}
	fmt.Fprintf(env.Stdout, "recorded %d tools of %s as reviewed\n", len(live), stripControlChars(entry.Name))
	notifyMCPReload(env, scope)
	return ExitOK
}

func printChanges(env *Env, review *mcppolicy.Review, d mcppolicy.Changes) {
	if d.Empty() {
		return
	}
	if review == nil {
		fmt.Fprintf(env.Stdout, "not reviewed yet; current tools: %s\n", joinNames(d.Added))
		return
	}
	fmt.Fprintln(env.Stdout, "changes since the last review on", review.At.Format("2006-01-02"))
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

func confirmReview(env *Env, name string) bool {
	fmt.Fprintf(env.Stdout, "Review the live tools of %s as reviewed? [y/N] ", stripControlChars(name))
	line, _ := bufio.NewReader(env.Stdin).ReadString('\n')
	return parseYesNo(line)
}

// printChangeDetails shows the text of every added tool, and for every changed
// tool the reviewed text next to the current one, so the user reads what
// changed before accepting. All of it is server-supplied and untrusted.
func printChangeDetails(env *Env, review *mcppolicy.Review, live []mcppolicy.ToolInfo, d mcppolicy.Changes) {
	if d.Empty() {
		return
	}
	w := func(format string, a ...any) { fmt.Fprintf(env.Stdout, format, a...) }
	byName := map[string]mcppolicy.ToolInfo{}
	for _, t := range live {
		byName[t.Name] = t
	}
	fmt.Fprintln(env.Stdout, "\nServer-supplied text, untrusted (it may try to instruct the agent; descriptions are cut at 500 characters):")
	for _, n := range d.Added {
		w("  added: %s\n", stripControlChars(n))
		printToolText(w, "      ", toolText(byName[n]))
	}
	for _, n := range d.Changed {
		cur := toolText(byName[n])
		w("  changed: %s\n", stripControlChars(n))
		old, ok := mcppolicy.ToolText{}, false
		if review != nil {
			old, ok = review.Text[n]
		}
		switch {
		case !ok:
			w("      old text was not recorded (reviewed before descriptions were stored); the fingerprint differs\n")
		case old == cur:
			w("      description and parameters are the same; the change is in the title, the output schema or a part of the input schema not summarized here\n")
		default:
			w("      old:\n")
			printToolText(w, "          ", old)
			w("      new:\n")
			printToolText(w, "          ", cur)
			continue
		}
		w("      current:\n")
		printToolText(w, "          ", cur)
	}
	for _, n := range d.Removed {
		w("  removed: %s\n", stripControlChars(n))
		if review != nil {
			if old, ok := review.Text[n]; ok {
				printToolText(w, "      ", old)
			}
		}
	}
}

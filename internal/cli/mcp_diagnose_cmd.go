package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpinject"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpprobe"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
)

const mcpProbeTimeout = 30 * time.Second

// mcpProbeListTools starts a server and lists its tools; tests replace it.
var mcpProbeListTools = mcpprobe.ListTools

type diagnosedServer struct {
	label    string
	problems []string
}

func runMCPDiagnose(args []string, env *Env) int {
	fs := flag.NewFlagSet("mcp diagnose", flag.ContinueOnError)
	scopeFlag := fs.String("scope", "", "Check only project or global registrations (default: both).")
	all := fs.Bool("all", false, "Check registrations of every scope.")
	probe := fs.Bool("probe", false, "Also start each server and compare its live tools to the last review (host terminal only).")
	if code, ok := parseFlags(fs, args, env); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(env.Stderr, "Usage: omac mcp diagnose [--scope project|global] [--all] [--probe]")
		return ExitMisuse
	}
	if *probe && refuseInsideSandbox(env, "diagnose --probe") {
		return ExitMisuse
	}
	scopes, err := diagnoseScopes(*scopeFlag, *all, env)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp diagnose:", err)
		return ExitMisuse
	}

	var servers []diagnosedServer
	registered := map[string]bool{}
	for _, scope := range scopes {
		for _, r := range mcpreg.Inspect(scope) {
			label := fmt.Sprintf("%s (%s)", r.Name, mcpScopeLabel(scope))
			if r.Err != nil {
				servers = append(servers, diagnosedServer{label, []string{"fails to load: " + stripControlChars(r.Err.Error())}})
				continue
			}
			registered[r.Name] = true
			problems := diagnoseEntry(scope, r.Entry, env.Workdir)
			if *probe {
				problems = append(problems, probeEntry(scope, r.Entry)...)
			}
			servers = append(servers, diagnosedServer{label, problems})
		}
	}

	problemCount := 0
	for _, s := range servers {
		if len(s.problems) == 0 {
			fmt.Fprintf(env.Stdout, "%s: ok\n", s.label)
			continue
		}
		fmt.Fprintln(env.Stdout, s.label)
		for _, p := range s.problems {
			fmt.Fprintf(env.Stdout, "  - %s\n", p)
		}
		problemCount += len(s.problems)
	}
	if len(servers) == 0 {
		fmt.Fprintln(env.Stdout, "no MCP servers registered")
	}
	unregistered, code := diagnoseUnregistered(env, registered)
	problemCount += unregistered
	if code != ExitOK {
		return code
	}

	if problemCount == 0 {
		fmt.Fprintln(env.Stdout, "no problems found")
		return ExitOK
	}
	fmt.Fprintf(env.Stdout, "%d problem(s) found\n", problemCount)
	return ExitGeneric
}

func diagnoseScopes(scopeFlag string, all bool, env *Env) ([]mcpreg.Scope, error) {
	switch {
	case all:
		return mcpreg.AllScopes()
	case scopeFlag != "":
		s, err := resolveMCPScope(scopeFlag, env)
		return []mcpreg.Scope{s}, err
	}
	return []mcpreg.Scope{mcpreg.GlobalScope(), mcpreg.ScopeFor(env.Workdir)}, nil
}

func diagnoseEntry(scope mcpreg.Scope, e *mcpreg.Entry, workdir string) []string {
	var problems []string
	var missing []string
	for _, name := range e.Def.Secrets {
		v, err := mcpreg.GetSecret(scope, e.Name, name)
		if err != nil {
			missing = append(missing, name)
			continue
		}
		v.Zero()
	}
	if len(missing) > 0 {
		problems = append(problems, "secrets missing from the keychain: "+strings.Join(missing, ", "))
	}
	if err := checkMCPCommand(e.Def, workdir); err != nil {
		problems = append(problems, err.Error())
	}
	if unclassified := unclassifiedTools(e.Def); len(unclassified) > 0 {
		problems = append(problems, "tools not classified (refused until classified): "+strings.Join(unclassified, ", "))
	}
	if e.Def.Review == nil {
		problems = append(problems, fmt.Sprintf("tools not reviewed yet; run: omac mcp tools %s --classify", e.Name))
	}
	return problems
}

func checkMCPCommand(def *mcpreg.Definition, workdir string) error {
	prog := def.Server.Command[0]
	if err := mcpreg.CheckCommandOutsideWorkdir(def, workdir); err != nil {
		return err
	}
	if _, err := exec.LookPath(prog); err != nil {
		return fmt.Errorf("command %q not found: %v", stripControlChars(prog), err)
	}
	return nil
}

// unclassifiedTools lists the reviewed tools that have no class.
func unclassifiedTools(def *mcpreg.Definition) []string {
	if def.Review == nil {
		return nil
	}
	classes := def.Policy().Tools
	var out []string
	for name := range def.Review.Tools {
		if _, ok := classes[name]; !ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func probeEntry(scope mcpreg.Scope, e *mcpreg.Entry) []string {
	sec := map[string]secrets.Secret{}
	defer func() {
		for _, v := range sec {
			v.Zero()
		}
	}()
	for _, name := range e.Def.Secrets {
		if v, err := mcpreg.GetSecret(scope, e.Name, name); err == nil {
			sec[name] = v
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpProbeTimeout+5*time.Second)
	defer cancel()
	live, stderrTail, err := mcpProbeListTools(ctx, e, sec, os.Environ(), mcpProbeTimeout)
	if err != nil {
		msg := "probe failed: " + stripControlChars(err.Error())
		if tail := strings.TrimSpace(stripControlChars(stderrTail)); tail != "" {
			msg += "\n    server stderr: " + strings.ReplaceAll(tail, "\n", "\n    ")
		}
		return []string{msg}
	}
	if e.Def.Review == nil {
		return nil
	}
	d := mcppolicy.Compare(e.Def.Review, live)
	var problems []string
	for _, g := range []struct {
		what  string
		names []string
	}{{"added", d.Added}, {"removed", d.Removed}, {"changed", d.Changed}} {
		if len(g.names) > 0 {
			problems = append(problems, fmt.Sprintf("tools changed since the last review (%s: %s); run: omac mcp review %s", g.what, stripControlChars(strings.Join(g.names, ", ")), e.Name))
		}
	}
	return problems
}

// diagnoseUnregistered prints the harness MCP entries of the workdir that omac
// does not manage and returns how many problems that is.
func diagnoseUnregistered(env *Env, registered map[string]bool) (int, int) {
	home, _ := os.UserHomeDir()
	problems := 0
	header := false
	section := func() {
		if !header {
			fmt.Fprintln(env.Stdout, "harness MCP entries not managed by omac")
			header = true
		}
	}
	for _, hn := range config.HarnessNames() {
		h, _ := config.LookupHarness(hn)
		if h.MCPInjection == config.MCPInjectNone {
			continue
		}
		existing, findings, err := mcpinject.Scan(h.MCPInjection, env.Workdir, home)
		if err != nil {
			fmt.Fprintf(env.Stderr, "omac mcp diagnose: MCP config scan: %v\n", err)
			return problems, ExitIOError
		}
		for _, ex := range existing {
			ex.AlsoRegistered = registered[ex.Name]
			section()
			also := ""
			if ex.AlsoRegistered {
				also = " (a server of this name is registered, but this entry still runs inside the sandbox)"
			}
			fmt.Fprintf(env.Stdout, "  - %s: %q in %s is not managed by omac%s\n", ex.Harness,
				stripControlChars(ex.Name), stripControlChars(ex.File), also)
			if hint := mcpinject.AddCommand(ex); hint != "" {
				fmt.Fprintf(env.Stdout, "    register it: %s\n", stripControlChars(hint))
			}
			problems++
		}
		for _, f := range findings {
			section()
			fmt.Fprintf(env.Stdout, "  - %s could not be read (%s): %s\n", stripControlChars(f.File), f.Kind, stripControlChars(f.Detail))
			problems++
		}
	}
	return problems, ExitOK
}

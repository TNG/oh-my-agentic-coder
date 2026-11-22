package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
)

const (
	scopeProject = "project"
	scopeGlobal  = "global"
)

func runMCP(args []string, env *Env) int {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "Usage: omac mcp <register|add|add-json|list|show|tools|review|diagnose|deregister|serve|bridge> ...")
		return ExitMisuse
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "register":
		return runMCPRegister(rest, env)
	case "add":
		return runMCPAdd(rest, env)
	case "add-json":
		return runMCPAddJSON(rest, env)
	case "tools":
		return runMCPTools(rest, env)
	case "review":
		return runMCPReview(rest, env)
	case "diagnose":
		return runMCPDiagnose(rest, env)
	case "list":
		return runMCPList(rest, env)
	case "show":
		return runMCPShow(rest, env)
	case "deregister":
		return runMCPDeregister(rest, env)
	case "serve":
		return runMCPServe(rest, env)
	case "bridge":
		return runMCPBridge(rest, env)
	}
	fmt.Fprintf(env.Stderr, "omac mcp: unknown subcommand %q\n", sub)
	return ExitMisuse
}

// insideSandbox reports whether this process runs under omac's sandbox,
// where the facade token and socket are injected into the environment.
func insideSandbox() bool {
	return os.Getenv("OMAC_SOCKET") != "" || os.Getenv("OMAC_FACADE_TOKEN") != ""
}

func refuseInsideSandbox(env *Env, cmd string) bool {
	if !insideSandbox() {
		return false
	}
	fmt.Fprintf(env.Stderr, "omac mcp %s: refused inside the omac sandbox; run it from a host terminal\n", cmd)
	return true
}

func resolveMCPScope(name string, env *Env) (mcpreg.Scope, error) {
	switch name {
	case scopeProject:
		return mcpreg.ScopeFor(env.Workdir), nil
	case scopeGlobal:
		return mcpreg.GlobalScope(), nil
	}
	return mcpreg.Scope{}, fmt.Errorf("--scope must be %s or %s, got %q", scopeProject, scopeGlobal, name)
}

func parseHarnessList(csv string) ([]string, error) {
	var out []string
	for _, tok := range strings.Split(csv, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		h, ok := config.LookupHarness(tok)
		if !ok {
			return nil, config.UnknownHarnessError(tok)
		}
		out = append(out, h.Name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--harness is required (%s)", strings.Join(config.HarnessNames(), ", "))
	}
	return out, nil
}

func runMCPRegister(args []string, env *Env) int {
	fs := flag.NewFlagSet("mcp register", flag.ContinueOnError)
	scopeFlag := fs.String("scope", scopeProject, "Where the server applies: project (this workdir) or global.")
	harnessFlag := fs.String("harness", "", "Comma-separated harnesses the server is injected into (required).")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: omac mcp register <def.yaml> --harness opencode,claude-code [--scope project|global]")
		fs.PrintDefaults()
	}
	if code, ok := parseFlags(fs, args, env); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return ExitMisuse
	}
	if refuseInsideSandbox(env, "register") {
		return ExitMisuse
	}
	scope, err := resolveMCPScope(*scopeFlag, env)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp register:", err)
		return ExitMisuse
	}
	harnesses, err := parseHarnessList(*harnessFlag)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp register:", err)
		return ExitMisuse
	}
	defPath := fs.Arg(0)
	raw, err := os.ReadFile(defPath)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp register:", err)
		return ExitIOError
	}
	def, err := mcpreg.ParseDefinition(raw)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp register: refused:", err)
		return ExitMisuse
	}
	if err := mcpreg.CheckCommandOutsideWorkdir(def, env.Workdir); err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp register: refused:", err)
		return ExitMisuse
	}
	entry, err := mcpreg.Register(scope, defPath, harnesses)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp register:", err)
		return ExitIOError
	}
	if err := storeMCPSecrets(env, scope, entry, nil); err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp register:", err)
		return ExitIOError
	}
	fmt.Fprintf(env.Stdout, "registered %s (%s; harnesses: %s)\n",
		entry.Name, mcpScopeLabel(scope), strings.Join(entry.Harnesses, ", "))
	return ExitOK
}

// storeMCPSecrets saves the value of every secret the entry declares in the
// keychain. A value in preset is used as given and zeroed; any other is read
// from OMAC_SECRET_<NAME> or prompted for.
func storeMCPSecrets(env *Env, scope mcpreg.Scope, entry *mcpreg.Entry, preset map[string]secrets.Secret) error {
	for _, name := range entry.Def.Secrets {
		value, ok := preset[name]
		if !ok {
			var err error
			if value, err = readMCPSecret(name); err != nil {
				return fmt.Errorf("secret %s: %w", name, err)
			}
		}
		err := mcpreg.SetSecret(scope, entry.Name, name, value)
		value.Zero()
		if err != nil {
			return fmt.Errorf("secret %s: %w", name, err)
		}
	}
	return nil
}

func readMCPSecret(name string) (secrets.Secret, error) {
	if v, ok := os.LookupEnv("OMAC_SECRET_" + name); ok {
		if v == "" {
			return secrets.Secret{}, errors.New("OMAC_SECRET_" + name + " is empty")
		}
		return secrets.NewSecretString(v), nil
	}
	value, err := readSecretInput(fmt.Sprintf("  ? %s (input hidden): ", name))
	if err != nil {
		return secrets.Secret{}, err
	}
	if value.IsEmpty() {
		return secrets.Secret{}, errors.New("no value supplied")
	}
	return value, nil
}

func mcpScopeLabel(s mcpreg.Scope) string {
	if s.Key == mcpreg.GlobalScopeKey {
		return scopeGlobal
	}
	return "project " + stripControlChars(s.DisplayPath)
}

func runMCPList(args []string, env *Env) int {
	fs := flag.NewFlagSet("mcp list", flag.ContinueOnError)
	all := fs.Bool("all", false, "List registrations of every scope, marking scopes whose directory no longer exists.")
	if code, ok := parseFlags(fs, args, env); !ok {
		return code
	}
	scopes := []mcpreg.Scope{mcpreg.GlobalScope(), mcpreg.ScopeFor(env.Workdir)}
	if *all {
		var err error
		if scopes, err = mcpreg.AllScopes(); err != nil {
			fmt.Fprintln(env.Stderr, "omac mcp list:", err)
			return ExitIOError
		}
	}
	code := ExitOK
	printed := 0
	for _, scope := range scopes {
		entries, err := mcpreg.List(scope)
		if err != nil {
			fmt.Fprintf(env.Stderr, "omac mcp list: %s: %v\n", mcpScopeLabel(scope), err)
			code = ExitIOError
			continue
		}
		for _, e := range entries {
			marker := ""
			if *all && scope.Key != mcpreg.GlobalScopeKey {
				if _, err := os.Stat(scope.DisplayPath); err != nil {
					marker = "  [stale: directory missing]"
				}
			}
			total, classified, review := mcpToolCounts(e.Def)
			fmt.Fprintf(env.Stdout, "%-24s %-40s %-24s %3d tools %3d classified  %s%s\n", e.Name, mcpScopeLabel(scope),
				strings.Join(e.Harnesses, ","), total, classified, review, marker)
			printed++
		}
	}
	if printed == 0 && code == ExitOK {
		fmt.Fprintln(env.Stdout, "no MCP servers registered")
	}
	return code
}

// mcpToolCounts returns the number of tools omac knows (the reviewed ones, or
// the classified ones when nothing is reviewed), how many of them have a class,
// and "reviewed" or "not reviewed".
func mcpToolCounts(def *mcpreg.Definition) (total, classified int, review string) {
	classes := def.Policy().Tools
	if def.Review == nil {
		return len(classes), len(classes), "not reviewed"
	}
	for name := range def.Review.Tools {
		if _, ok := classes[name]; ok {
			classified++
		}
	}
	return len(def.Review.Tools), classified, "reviewed"
}

func findMCPEntry(name, scopeFlag string, env *Env) (mcpreg.Scope, *mcpreg.Entry, error) {
	candidates := []string{scopeProject, scopeGlobal}
	if scopeFlag != "" {
		candidates = []string{scopeFlag}
	}
	for _, c := range candidates {
		scope, err := resolveMCPScope(c, env)
		if err != nil {
			return mcpreg.Scope{}, nil, err
		}
		e, err := mcpreg.Load(scope, name)
		if err == nil {
			return scope, e, nil
		}
		if !errors.Is(err, mcpreg.ErrNotRegistered) {
			return mcpreg.Scope{}, nil, err
		}
	}
	return mcpreg.Scope{}, nil, fmt.Errorf("%w: %s", mcpreg.ErrNotRegistered, name)
}

func runMCPShow(args []string, env *Env) int {
	fs := flag.NewFlagSet("mcp show", flag.ContinueOnError)
	scopeFlag := fs.String("scope", "", "Look only in project or global (default: project, then global).")
	if code, ok := parseFlags(fs, args, env); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(env.Stderr, "Usage: omac mcp show <name> [--scope project|global]")
		return ExitMisuse
	}
	scope, e, err := findMCPEntry(fs.Arg(0), *scopeFlag, env)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp show:", err)
		return ExitGeneric
	}
	w := env.Stdout
	fmt.Fprintf(w, "name:        %s\n", e.Name)
	fmt.Fprintf(w, "scope:       %s\n", mcpScopeLabel(scope))
	fmt.Fprintf(w, "harnesses:   %s\n", strings.Join(e.Harnesses, ", "))
	fmt.Fprintf(w, "registered:  %s\n", e.RegisteredAt.Format("2006-01-02 15:04:05Z"))
	fmt.Fprintf(w, "bundle hash: %s\n", e.BundleHash)
	fmt.Fprintf(w, "command:     %s\n", stripControlChars(strings.Join(e.Def.Server.Command, " ")))
	if keys := sortedKeys(e.Def.Server.Env); len(keys) > 0 {
		fmt.Fprintf(w, "env:         %s\n", stripControlChars(strings.Join(keys, ", ")))
	}
	if len(e.Def.Server.EnvPassthrough) > 0 {
		fmt.Fprintf(w, "passthrough: %s\n", stripControlChars(strings.Join(e.Def.Server.EnvPassthrough, ", ")))
	}
	if e.Def.Mode != nil {
		fmt.Fprintf(w, "mode:        %s=%s\n", stripControlChars(e.Def.Mode.Name), stripControlChars(e.Def.Mode.Value))
	}
	for _, name := range e.Def.Secrets {
		state := "missing"
		if v, err := mcpreg.GetSecret(scope, e.Name, name); err == nil {
			state = "stored"
			v.Zero()
		}
		fmt.Fprintf(w, "secret:      %s (%s)\n", name, state)
	}
	printMCPPolicy(w, e.Name, e.Def)
	return ExitOK
}

func printMCPPolicy(w io.Writer, name string, def *mcpreg.Definition) {
	p := def.Policy()
	fmt.Fprintf(w, "access:      %s\n", p.Access)
	fmt.Fprintf(w, "approvals:   read=%s write=%s delete=%s\n", p.Approvals.Read, p.Approvals.Write, p.Approvals.Delete)
	if p.Review == nil {
		fmt.Fprintf(w, "reviewed:    no (run: omac mcp tools %s --classify)\n", name)
	} else {
		hash := p.Review.ToolsHash
		if len(hash) > 12 {
			hash = hash[:12]
		}
		fmt.Fprintf(w, "reviewed:    %s  %d tools  fingerprint %s\n", p.Review.At.Format("2006-01-02"), len(p.Review.Tools), hash)
	}
	names := map[string]bool{}
	for n := range p.Tools {
		names[n] = true
	}
	if p.Review != nil {
		for n := range p.Review.Tools {
			names[n] = true
		}
	}
	if len(names) == 0 {
		fmt.Fprintln(w, "tools:       none")
		return
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	fmt.Fprintln(w, "tools:")
	for _, n := range sorted {
		class := string(p.Tools[n])
		if class == "" {
			class = "unclassified"
		}
		fmt.Fprintf(w, "  %-32s %s\n", stripControlChars(n), class)
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func runMCPDeregister(args []string, env *Env) int {
	fs := flag.NewFlagSet("mcp deregister", flag.ContinueOnError)
	scopeFlag := fs.String("scope", scopeProject, "Scope of the registration: project or global.")
	if code, ok := parseFlags(fs, args, env); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(env.Stderr, "Usage: omac mcp deregister <name> [--scope project|global]")
		return ExitMisuse
	}
	if refuseInsideSandbox(env, "deregister") {
		return ExitMisuse
	}
	scope, err := resolveMCPScope(*scopeFlag, env)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp deregister:", err)
		return ExitMisuse
	}
	if err := mcpreg.Deregister(scope, fs.Arg(0)); err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp deregister:", err)
		return ExitGeneric
	}
	fmt.Fprintf(env.Stdout, "deregistered %s (%s)\n", fs.Arg(0), mcpScopeLabel(scope))
	return ExitOK
}

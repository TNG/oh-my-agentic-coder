package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
	"github.com/TNG/oh-my-agentic-coder/internal/sidecarauth"
)

const (
	addUsage     = "Usage: omac mcp add [--scope project|global] [--harness a,b] [-e KEY=value]... [--secret NAME]... [--replace] <name> -- <command> [args...]\n       omac mcp add [--scope project|global] [--harness a,b] --url <https-url> [--bearer-secret NAME] [-H 'Name: value']... [--allow-private-network] [--replace] <name>"
	addJSONUsage = "Usage: omac mcp add-json [--scope project|global] [--harness a,b] [--secret NAME]... [--replace] <name> '<json>'\n       omac mcp add-json [flags] --file <file> [names...]"
)

// splitAtDoubleDash splits args at the first "--". found is false when there is none.
func splitAtDoubleDash(args []string) (before, after []string, found bool) {
	i := slices.Index(args, "--")
	if i < 0 {
		return args, nil, false
	}
	return args[:i], args[i+1:], true
}

type mcpAddFlags struct {
	scope, harness string
	secrets        multiFlag
	replace        bool
}

func (f *mcpAddFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.scope, "scope", scopeProject, "Where the server applies: project (this workdir) or global.")
	fs.StringVar(&f.harness, "harness", "", "Comma-separated harnesses the server is injected into (required for a new server).")
	fs.Var(&f.secrets, "secret", "Name of an environment variable that holds a secret; its value is prompted for or read from OMAC_SECRET_<NAME> and kept in the keychain. Repeatable.")
	fs.BoolVar(&f.replace, "replace", false, "Replace the command and environment of a registered server, keeping its tools, approvals, access and review.")
}

func runMCPAdd(args []string, env *Env) int {
	left, command, found := splitAtDoubleDash(args)
	fs := flag.NewFlagSet("mcp add", flag.ContinueOnError)
	var f mcpAddFlags
	var envFlags, headerFlags multiFlag
	url := fs.String("url", "", "Register a Streamable HTTP server by URL (https, or http for localhost) instead of a command.")
	bearer := fs.String("bearer-secret", "", "With --url: name of the secret that holds the bearer token. omac asks for its value or reads OMAC_SECRET_<NAME>, and keeps it in the keychain only.")
	allowPrivate := fs.Bool("allow-private-network", false, "With --url: allow the host to resolve to a private address (company-internal servers).")
	fs.Var(&headerFlags, "H", "With --url: a literal, non-credential request header, 'Name: value'. Repeatable.")
	f.register(fs)
	fs.Var(&envFlags, "e", "Environment variable for the server, KEY=value. The value is stored in the definition, so use --secret for a credential. Repeatable.")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), addUsage)
		fs.PrintDefaults()
	}
	if code, ok := parseFlags(fs, left, env); !ok {
		return code
	}
	remote := *url != ""
	if (!remote && (!found || len(command) == 0)) || (remote && (found || len(envFlags) > 0)) || fs.NArg() != 1 {
		fs.Usage()
		return ExitMisuse
	}
	if refuseInsideSandbox(env, "add") {
		return ExitMisuse
	}
	var def *mcpreg.Definition
	var err error
	if remote {
		def, err = definitionFromURLArgs(fs.Arg(0), *url, *bearer, headerFlags, *allowPrivate, f.secrets)
	} else {
		def, err = definitionFromAddArgs(fs.Arg(0), command, envFlags, f.secrets)
	}
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp add: refused:", err)
		return ExitMisuse
	}
	return installMCPDefinitions(env, "add", f, []mcpAddition{{def: def}})
}

func definitionFromURLArgs(name, url, bearer string, headerFlags []string, allowPrivate bool, secretNames []string) (*mcpreg.Definition, error) {
	if len(secretNames) > 0 {
		return nil, errors.New("--secret applies to a command; for a URL server use --bearer-secret")
	}
	def := &mcpreg.Definition{Name: name, Server: mcpreg.ServerSpec{URL: url, BearerSecret: bearer, AllowPrivateNetwork: allowPrivate}}
	if bearer != "" {
		def.Secrets = []string{bearer}
	}
	for _, h := range headerFlags {
		key, value, ok := strings.Cut(h, ":")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" || value == "" {
			return nil, fmt.Errorf("-H %q must be 'Name: value'", h)
		}
		if strings.Contains(value, "${") {
			return nil, fmt.Errorf("-H %s has a variable reference; a credential must not be written into the definition, use --bearer-secret", key)
		}
		if _, dup := def.Server.Headers[key]; dup {
			return nil, fmt.Errorf("-H %s is given twice", key)
		}
		if def.Server.Headers == nil {
			def.Server.Headers = map[string]string{}
		}
		def.Server.Headers[key] = value
	}
	return def, nil
}

func definitionFromAddArgs(name string, command, envFlags, secretNames []string) (*mcpreg.Definition, error) {
	def := &mcpreg.Definition{Name: name, Server: mcpreg.ServerSpec{Command: command}, Secrets: secretNames}
	for _, kv := range envFlags {
		key, value, ok := strings.Cut(kv, "=")
		switch {
		case !ok:
			return nil, fmt.Errorf("-e %q must be KEY=value", kv)
		case slices.Contains(secretNames, key):
			return nil, fmt.Errorf("%s is given with -e and --secret; use one", key)
		case value == "" || strings.Contains(value, "${"):
			return nil, fmt.Errorf("-e %s has an empty value or a variable reference; declare --secret %s", key, key)
		}
		if _, dup := def.Server.Env[key]; dup {
			return nil, fmt.Errorf("-e %s is given twice", key)
		}
		if def.Server.Env == nil {
			def.Server.Env = map[string]string{}
		}
		def.Server.Env[key] = value
	}
	return def, nil
}

func runMCPAddJSON(args []string, env *Env) int {
	fs := flag.NewFlagSet("mcp add-json", flag.ContinueOnError)
	var f mcpAddFlags
	f.register(fs)
	file := fs.String("file", "", "Read the servers from this JSON file holding an mcpServers object; positional arguments select names.")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), addJSONUsage)
		fs.PrintDefaults()
	}
	if code, ok := parseFlags(fs, args, env); !ok {
		return code
	}
	if *file == "" && fs.NArg() != 2 {
		fs.Usage()
		return ExitMisuse
	}
	if refuseInsideSandbox(env, "add-json") {
		return ExitMisuse
	}

	var adds []mcpAddition
	var err error
	if *file == "" {
		adds, err = additionsFromJSON(fs.Arg(0), []byte(fs.Arg(1)), f.secrets)
	} else {
		var raw []byte
		if raw, err = os.ReadFile(*file); err != nil {
			fmt.Fprintln(env.Stderr, "omac mcp add-json:", err)
			return ExitIOError
		}
		adds, err = additionsFromFile(raw, fs.Args(), f.secrets)
	}
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp add-json: refused:", err)
		return ExitMisuse
	}
	return installMCPDefinitions(env, "add-json", f, adds)
}

// mcpAddition is a definition to register and the secret values its entry carried.
type mcpAddition struct {
	def    *mcpreg.Definition
	preset map[string]secrets.Secret
}

func additionsFromJSON(name string, raw []byte, secretNames []string) ([]mcpAddition, error) {
	entry := json.RawMessage(raw)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("the JSON is not an object: %w", err)
	}
	if wrapped, ok := top["mcpServers"]; ok {
		if len(top) != 1 {
			return nil, errors.New("unknown keys next to mcpServers")
		}
		var servers map[string]json.RawMessage
		if err := json.Unmarshal(wrapped, &servers); err != nil {
			return nil, fmt.Errorf("mcpServers is not an object: %w", err)
		}
		if entry, ok = servers[name]; !ok {
			return nil, fmt.Errorf("mcpServers has no server %q", name)
		}
	}
	def, preset, err := mcpreg.ParseServerEntry(name, entry, secretNames)
	if err != nil {
		return nil, err
	}
	return []mcpAddition{{def: def, preset: preset}}, nil
}

func additionsFromFile(raw []byte, names, secretNames []string) ([]mcpAddition, error) {
	var doc struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("the file is not valid JSON: %w", err)
	}
	if len(doc.Servers) == 0 {
		return nil, errors.New("the file has no mcpServers object with servers")
	}
	if len(names) == 0 {
		for name := range doc.Servers {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	used := map[string]bool{}
	var adds []mcpAddition
	for _, name := range names {
		entry, ok := doc.Servers[name]
		if !ok {
			return nil, fmt.Errorf("the file has no server %q", name)
		}
		var withEnv struct {
			Env map[string]json.RawMessage `json:"env"`
		}
		_ = json.Unmarshal(entry, &withEnv)
		var own []string
		for _, s := range secretNames {
			if _, has := withEnv.Env[s]; has {
				own = append(own, s)
				used[s] = true
			}
		}
		def, preset, err := mcpreg.ParseServerEntry(name, entry, own)
		if err != nil {
			return nil, err
		}
		adds = append(adds, mcpAddition{def: def, preset: preset})
	}
	for _, s := range secretNames {
		if !used[s] {
			return nil, fmt.Errorf("--secret %s matches no env key of the selected servers", s)
		}
	}
	return adds, nil
}

// installMCPDefinitions registers every addition, or replaces it when it is
// registered and --replace is given. It checks all of them before it writes any.
func installMCPDefinitions(env *Env, cmd string, f mcpAddFlags, adds []mcpAddition) (code int) {
	defer func() {
		for _, a := range adds {
			for _, v := range a.preset {
				v.Zero()
			}
		}
	}()
	fail := func(code int, err error) int {
		fmt.Fprintf(env.Stderr, "omac mcp %s: %v\n", cmd, err)
		return code
	}
	scope, err := resolveMCPScope(f.scope, env)
	if err != nil {
		return fail(ExitMisuse, err)
	}
	existing := make([]*mcpreg.Entry, len(adds))
	for i, a := range adds {
		name := a.def.Name
		if sidecarauth.Reserved(name) {
			return fail(ExitMisuse, fmt.Errorf("refused: name %q is reserved", name))
		}
		if err := mcpreg.CheckCommandOutsideWorkdir(a.def, env.Workdir); err != nil {
			return fail(ExitMisuse, fmt.Errorf("refused: %w", err))
		}
		switch e, err := mcpreg.Load(scope, name); {
		case err == nil && !f.replace:
			return fail(ExitMisuse, fmt.Errorf("refused: %q is already registered; use --replace to change its command and environment", name))
		case err == nil:
			existing[i] = e
		case !errors.Is(err, mcpreg.ErrNotRegistered):
			return fail(ExitIOError, err)
		}
	}

	harnesses := make([][]string, len(adds))
	for i := range adds {
		if existing[i] != nil && f.harness == "" {
			harnesses[i] = existing[i].Harnesses
			continue
		}
		if harnesses[i], err = parseHarnessList(f.harness); err != nil {
			return fail(ExitMisuse, err)
		}
		if existing[i] != nil && !sameSet(harnesses[i], existing[i].Harnesses) {
			return fail(ExitMisuse, fmt.Errorf("refused: %q is registered for %s; deregister it to change its harnesses",
				adds[i].def.Name, strings.Join(existing[i].Harnesses, ",")))
		}
	}

	for i, a := range adds {
		var entry *mcpreg.Entry
		verb := "added"
		if existing[i] != nil {
			verb = "replaced"
			entry, err = mcpreg.Update(scope, a.def.Name, func(d *mcpreg.Definition) error {
				d.Server, d.Secrets = a.def.Server, a.def.Secrets
				return nil
			})
		} else {
			entry, err = mcpreg.RegisterDefinition(scope, a.def, harnesses[i])
		}
		if err != nil {
			return fail(ExitMisuse, fmt.Errorf("refused: %w", err))
		}
		if err := storeMCPSecrets(env, scope, entry, a.preset); err != nil {
			return fail(ExitIOError, err)
		}
		fmt.Fprintf(env.Stdout, "%s %s (%s; harnesses: %s)\n", verb, entry.Name, mcpScopeLabel(scope), strings.Join(entry.Harnesses, ", "))
		if existing[i] == nil {
			fmt.Fprintf(env.Stdout, "  no tool is callable yet; review them with: omac mcp tools %s --classify\n", entry.Name)
		}
	}
	return ExitOK
}

func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

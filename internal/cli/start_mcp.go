package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/keychain"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpinject"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
	"github.com/TNG/oh-my-agentic-coder/internal/supervisor"
)

// mcpPlan is the MCP part of one launch. A nil *mcpPlan means omac leaves
// MCP alone: nothing is registered and no mode was set.
type mcpPlan struct {
	policy  mcpinject.Policy
	entries []*mcpreg.Entry
	harness string

	sup       *supervisor.Supervisor
	servers   []*mcpServer
	injection mcpinject.Injection

	exe     string
	rtDir   string
	workdir string
	prefix  string
	stderr  *os.File
	auditor audit.Auditor

	// secrets holds the keychain secrets of each entry, loaded by
	// loadAllSecrets before the auditor exists so its redactor knows them.
	// An entry whose secrets could not be loaded has no key.
	secrets map[string]map[string]secrets.Secret

	stop     chan struct{}
	stopOnce sync.Once
	watchers sync.WaitGroup

	noticeMu sync.Mutex
	noticed  map[string]bool
}

const mcpGatewayShutdownTimeout = 5 * time.Second

func mcpSpawnAuthorizer(spec supervisor.SidecarSpec) error {
	if mcpreg.IsSnapshotPath(spec.SkillDir) {
		return nil
	}
	return fmt.Errorf("MCP server %q does not run from a registry snapshot", spec.Name)
}

// prepareMCP decides what MCP handling the launch needs and applies the
// unregistered-entry policy. A non-zero exit code means the launch must stop;
// nothing has been spawned at that point.
func prepareMCP(env *Env, harness config.Harness, lc config.LauncherConfig, opts launchOpts) (*mcpPlan, int) {
	prefix := "omac " + opts.label
	if !harness.CanReceiveMCP() {
		return nil, prepareUnsupportedHarnessMCP(env, harness, lc, opts, prefix)
	}
	entries, shadowed, err := mcpreg.Effective(env.Workdir, harness.Name)
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+": MCP registry:", err)
		return nil, ExitIOError
	}
	for _, name := range shadowed {
		fmt.Fprintf(env.Stderr, "%s: MCP server %q is registered for this project and globally; the project registration is used\n", prefix, name)
	}
	applicable, err := mcpreg.Applicable(env.Workdir)
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+": MCP registry:", err)
		return nil, ExitIOError
	}
	policy, err := mcpinject.ResolvePolicy(lc.MCP, opts.mcpUnregistered, len(applicable), harness.CanIgnoreMCP())
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+":", err)
		return nil, ExitConfigInvalid
	}
	if policy.IgnoreUnavailable {
		fmt.Fprintf(env.Stderr, "%s: %s\n", prefix, mcpinject.IgnoreUnavailableMessage(harness.Name))
	}
	if !policy.Active {
		strict := policy.Unregistered == mcpinject.ModeRefuse || policy.Unregistered == mcpinject.ModeIgnore
		if harness.MCPInjection != config.MCPInjectClaudeFlag || !strict {
			return nil, ExitOK
		}
		noticeStrictMCP(env, harness, prefix)
		return &mcpPlan{policy: policy, harness: harness.Name, stop: make(chan struct{}), noticed: map[string]bool{}}, ExitOK
	}

	home, _ := os.UserHomeDir()
	existing, findings, err := mcpinject.Scan(harness.MCPScan, mcpinject.Host{Workdir: env.Workdir, Home: home, Getenv: os.Getenv, InnerArgs: opts.innerArgs})
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+": MCP config scan:", err)
		return nil, ExitIOError
	}
	var loaded []mcpinject.Existing
	var loadedFindings []mcpinject.Finding
	if policy.Unregistered == mcpinject.ModeIgnore {
		// Strict mode does not stop what the user passes on the command line, such as --mcp-config.
		// Those entries load, so they are reported as loaded and not as ignored.
		bare, bareFindings, err := mcpinject.Scan(harness.MCPScan, mcpinject.Host{Workdir: env.Workdir, Home: home, Getenv: os.Getenv})
		if err != nil {
			fmt.Fprintln(env.Stderr, prefix+": MCP config scan:", err)
			return nil, ExitIOError
		}
		existing, loaded = splitExisting(existing, bare)
		findings, loadedFindings = splitFindings(findings, bareFindings)
	}
	registered := map[string]bool{}
	for _, e := range entries {
		registered[e.Name] = true
	}
	for i := range existing {
		existing[i].AlsoRegistered = registered[existing[i].Name]
	}
	warnings, refusal := mcpinject.Evaluate(policy, existing, findings)
	label := "warning"
	if policy.Unregistered == mcpinject.ModeIgnore {
		label = "notice"
	}
	for _, w := range warnings {
		fmt.Fprintf(env.Stderr, "%s: %s: %s\n", prefix, label, w)
	}
	if len(loaded)+len(loadedFindings) > 0 {
		loadedWarnings, _ := mcpinject.Evaluate(mcpinject.Policy{Unregistered: mcpinject.ModeWarn, Active: true}, loaded, loadedFindings)
		for _, w := range loadedWarnings {
			fmt.Fprintf(env.Stderr, "%s: warning: %s\n", prefix, w)
		}
	}
	if refusal != nil {
		fmt.Fprintf(env.Stderr, "%s: %v\n", prefix, refusal)
		return nil, ExitConfigInvalid
	}
	return &mcpPlan{policy: policy, entries: entries, harness: harness.Name, stop: make(chan struct{}), noticed: map[string]bool{}}, ExitOK
}

// noticeStrictMCP tells the user which MCP servers from the harness's own
// configuration strict mode keeps from loading. The user's own --mcp-config
// arguments are not scanned because they still load. A failed scan prints nothing.
func noticeStrictMCP(env *Env, harness config.Harness, prefix string) {
	home, _ := os.UserHomeDir()
	existing, findings, err := mcpinject.Scan(harness.MCPScan, mcpinject.Host{Workdir: env.Workdir, Home: home, Getenv: os.Getenv})
	if err != nil {
		return
	}
	names := mcpinject.UnloadedNames(existing, findings)
	if len(names) == 0 {
		return
	}
	const maxNamed = 10
	list := strings.Join(names, ", ")
	if len(names) > maxNamed {
		list = fmt.Sprintf("%s and %d more", strings.Join(names[:maxNamed], ", "), len(names)-maxNamed)
	}
	advice := `Register them with "omac mcp add" or run with --mcp-unregistered=allow to keep them.`
	if onlyRemote(existing, findings) {
		advice = `Remote MCP servers are not supported yet; run with --mcp-unregistered=allow to keep them.`
	}
	fmt.Fprintf(env.Stderr, "%s: Claude Code runs in strict MCP mode, so these MCP servers from its own configuration are ignored and will not load inside omac: %s. %s\n", prefix, list, advice)
}

// onlyRemote reports whether every entry found is a remote server, which cannot be registered yet.
func onlyRemote(ex []mcpinject.Existing, f []mcpinject.Finding) bool {
	if len(f) > 0 {
		return false
	}
	for _, e := range ex {
		if e.Transport == "" || e.Transport == "stdio" {
			return false
		}
	}
	return len(ex) > 0
}

// splitExisting separates the entries a scan with the user's arguments found into
// those the bare scan also found (the harness config) and those only the
// arguments add.
func splitExisting(all, bare []mcpinject.Existing) (config, fromArgs []mcpinject.Existing) {
	known := map[string]bool{}
	for _, e := range bare {
		known[e.File+"\x00"+e.Name] = true
	}
	for _, e := range all {
		if known[e.File+"\x00"+e.Name] {
			config = append(config, e)
		} else {
			fromArgs = append(fromArgs, e)
		}
	}
	return config, fromArgs
}

func splitFindings(all, bare []mcpinject.Finding) (config, fromArgs []mcpinject.Finding) {
	known := map[string]bool{}
	for _, f := range bare {
		known[f.File+"\x00"+f.Kind] = true
	}
	for _, f := range all {
		if known[f.File+"\x00"+f.Kind] {
			config = append(config, f)
		} else {
			fromArgs = append(fromArgs, f)
		}
	}
	return config, fromArgs
}

// prepareUnsupportedHarnessMCP applies the unregistered-entry policy to a
// harness omac cannot provide MCP servers to. Registrations of any harness
// count, because none of them will be available. Nothing is injected.
func prepareUnsupportedHarnessMCP(env *Env, harness config.Harness, lc config.LauncherConfig, opts launchOpts, prefix string) int {
	entries, err := mcpreg.Applicable(env.Workdir)
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+": MCP registry:", err)
		return ExitIOError
	}
	policy, err := mcpinject.ResolvePolicy(lc.MCP, opts.mcpUnregistered, len(entries), false)
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+":", err)
		return ExitConfigInvalid
	}
	if policy.IgnoreUnavailable {
		fmt.Fprintf(env.Stderr, "%s: %s\n", prefix, mcpinject.IgnoreUnavailableMessage(harness.Name))
	}
	if !policy.Active {
		return ExitOK
	}
	var existing []mcpinject.Existing
	var findings []mcpinject.Finding
	if harness.MCPScan != config.MCPScanNone {
		home, _ := os.UserHomeDir()
		existing, findings, err = mcpinject.Scan(harness.MCPScan, mcpinject.Host{Workdir: env.Workdir, Home: home, Getenv: os.Getenv, InnerArgs: opts.innerArgs})
		if err != nil {
			fmt.Fprintln(env.Stderr, prefix+": MCP config scan:", err)
			return ExitIOError
		}
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	warnings, refusal := mcpinject.EvaluateUnsupported(policy, harness.Name, names, existing, findings)
	for _, w := range warnings {
		fmt.Fprintf(env.Stderr, "%s: warning: %s\n", prefix, w)
	}
	if refusal != nil {
		fmt.Fprintf(env.Stderr, "%s: %v\n", prefix, refusal)
		return ExitConfigInvalid
	}
	return ExitOK
}

// launchMCPGateways starts one gateway per registered server on the host and
// returns the facade routes that reach them. A server whose secret is missing
// is skipped with a warning.
func (p *mcpPlan) launchMCPGateways(ctx context.Context, env *Env, prefix string, lc config.LauncherConfig, auditor audit.Auditor, rtDir string) ([]facade.Route, error) {
	if p == nil {
		return nil, nil
	}
	exe, err := executablePath()
	if err != nil {
		return nil, fmt.Errorf("MCP gateway: %w", err)
	}
	p.sup = supervisor.New(lc.Facade.BaseEnvPassthrough, auditor, mcpSpawnAuthorizer)
	p.exe, p.rtDir, p.workdir, p.prefix, p.stderr, p.auditor = exe, rtDir, env.Workdir, prefix, env.Stderr, auditor

	if p.secrets == nil {
		p.loadAllSecrets(env, prefix)
	}
	var routes []facade.Route
	for _, entry := range p.entries {
		serverSecrets, ok := p.secrets[entry.Name]
		if !ok {
			continue
		}
		s := &mcpServer{p: p, name: entry.Name, entry: entry, spec: p.spec(entry, serverSecrets), revive: make(chan struct{}, 1)}
		if err := s.spawn(ctx); err != nil {
			return nil, fmt.Errorf("MCP server %q: %w", entry.Name, err)
		}
		p.servers = append(p.servers, s)
		routes = append(routes, s.route())
	}
	return routes, nil
}

// spec describes how to run the gateway of one registered server.
func (p *mcpPlan) spec(entry *mcpreg.Entry, serverSecrets map[string]secrets.Secret) supervisor.SidecarSpec {
	return supervisor.SidecarSpec{
		Name:           "mcp/" + entry.Name,
		SkillName:      entry.Name,
		SkillDir:       entry.SnapshotDir,
		Command:        []string{p.exe, "mcp", "serve", "--def", entry.DefPath},
		EnvPassthrough: entry.Def.Server.EnvPassthrough,
		Secrets:        serverSecrets,
		Health:         config.HealthSpec{Path: "/status"},
		LogPath:        filepath.Join(p.rtDir, "logs", "mcp-"+entry.Name+".log"),
		Workdir:        p.workdir,
		RequireSecret:  true,
	}
}

// loadAllSecrets reads the secrets of every entry once, warning about and
// skipping an entry whose secret is missing.
func (p *mcpPlan) loadAllSecrets(env *Env, prefix string) {
	if p == nil {
		return
	}
	p.secrets = map[string]map[string]secrets.Secret{}
	for _, entry := range p.entries {
		if sec, ok := p.loadSecrets(env, prefix, entry); ok {
			p.secrets[entry.Name] = sec
		}
	}
}

// secretValues returns the loaded secret values for the audit redactor.
func (p *mcpPlan) secretValues() []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, sec := range p.secrets {
		for _, v := range sec {
			out = append(out, v.ExposeString())
		}
	}
	return out
}

func (p *mcpPlan) loadSecrets(env *Env, prefix string, entry *mcpreg.Entry) (map[string]secrets.Secret, bool) {
	scope := mcpreg.Scope{Key: entry.ScopeKey, DisplayPath: entry.DisplayPath}
	out := map[string]secrets.Secret{}
	for _, name := range entry.Def.Secrets {
		value, err := mcpreg.GetSecret(scope, entry.Name, name)
		if err != nil {
			reason := err.Error()
			if errors.Is(err, keychain.ErrNotFound) {
				reason = "not found in the keychain"
			}
			fmt.Fprintf(env.Stderr, "%s: warning: MCP server %q skipped: secret %s: %s; run `omac mcp deregister %s` and register it again\n",
				prefix, entry.Name, name, reason, entry.Name)
			return nil, false
		}
		out[name] = value
	}
	return out, true
}

// shutdown stops the gateways. Safe on a nil plan.
func (p *mcpPlan) shutdown() {
	if p == nil || p.sup == nil {
		return
	}
	p.stopOnce.Do(func() { close(p.stop) })
	p.watchers.Wait()
	p.sup.ShutdownAll(mcpGatewayShutdownTimeout)
}

// buildInjection computes what the harness needs to start the bridges. It
// runs after the facade is up because the entries carry its address and token.
func (p *mcpPlan) buildInjection(harness config.Harness, userArgs []string, socketPath string, tcpPort int, facadeToken, sandboxTmp string) error {
	if p == nil {
		return nil
	}
	servers := make([]mcpinject.Server, 0, len(p.servers))
	for _, srv := range p.servers {
		servers = append(servers, mcpinject.Server{
			Name: srv.name,
			Argv: mcpreg.BridgePath(srv.name),
			Env: map[string]string{
				"OMAC_BASE":         fmt.Sprintf("http://127.0.0.1:%d/", tcpPort),
				"OMAC_SOCKET":       socketPath,
				"OMAC_FACADE_TOKEN": facadeToken,
			},
		})
	}
	injection, err := mcpinject.Build(harness.MCPInjection, servers, p.policy, sandboxTmp, userArgs)
	if err != nil {
		return fmt.Errorf("MCP injection: %w", err)
	}
	p.injection = injection
	return nil
}

// innerArgs returns the harness arguments the injection adds.
func (p *mcpPlan) innerArgs() []string {
	if p == nil {
		return nil
	}
	return p.injection.InnerArgs
}

// addEnv copies the injected variables into the sandbox runtime's environment.
func (p *mcpPlan) addEnv(extra map[string]string) {
	if p == nil {
		return
	}
	for k, v := range p.injection.Env {
		extra[k] = v
	}
}

// allowEnv lets the injected variables through the sandbox's environment
// filter. Variables omac does not inject stay filtered.
func (p *mcpPlan) allowEnv(argv []string, plan sandboxPlan) []string {
	if p == nil {
		return argv
	}
	return injectSandboxEnvAllow(argv, p.injection.AllowEnv, plan)
}

// checkSandboxEnv fails when the sandbox profile's deny_vars strips a variable
// the injection relies on. Backends omac cannot inspect only get a warning.
func (p *mcpPlan) checkSandboxEnv(env *Env, prefix string, plan sandboxPlan, noSandbox bool) error {
	if p == nil || noSandbox || len(p.injection.AllowEnv) == 0 {
		return nil
	}
	if !plan.Native || plan.Policy == nil {
		fmt.Fprintf(env.Stderr, "%s: warning: MCP injection needs %s in the sandbox, which omac cannot verify for sandbox backend %q\n",
			prefix, strings.Join(p.injection.AllowEnv, ", "), plan.Name)
		return nil
	}
	for _, name := range p.injection.AllowEnv {
		if sandboxprofile.EnvVarMatches(name, plan.Policy.Environment.DenyVars) {
			profile := plan.PolicyRef
			if profile == "" {
				profile = plan.Name
			}
			return fmt.Errorf("profile %q: deny_vars denies %s, which MCP injection needs; the registered MCP servers would not reach the harness, remove %s from the profile's deny_vars", profile, name, name)
		}
	}
	return nil
}

func executablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// mcpServeNotice tells the user that omac serve runs no MCP gateways.
func mcpServeNotice(env *Env, harness config.Harness) {
	entries, _, err := mcpreg.Effective(env.Workdir, harness.Name)
	if err != nil || len(entries) == 0 {
		return
	}
	fmt.Fprintf(env.Stderr, "omac serve: %d registered MCP server(s) are not started or injected by omac serve; use omac start for them\n", len(entries))
}

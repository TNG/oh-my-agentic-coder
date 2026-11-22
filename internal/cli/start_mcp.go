package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	sup       *supervisor.Supervisor
	started   []*mcpreg.Entry
	injection mcpinject.Injection
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
	if harness.MCPInjection == config.MCPInjectNone {
		return nil, ExitOK
	}
	prefix := "omac " + opts.label
	entries, shadowed, err := mcpreg.Effective(env.Workdir, harness.Name)
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+": MCP registry:", err)
		return nil, ExitIOError
	}
	for _, name := range shadowed {
		fmt.Fprintf(env.Stderr, "%s: MCP server %q is registered for this project and globally; the project registration is used\n", prefix, name)
	}
	policy, err := mcpinject.ResolvePolicy(lc.MCP, opts.mcpUnregistered, len(entries))
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+":", err)
		return nil, ExitConfigInvalid
	}
	if !policy.Active {
		return nil, ExitOK
	}

	home, _ := os.UserHomeDir()
	existing, findings, err := mcpinject.Scan(harness.MCPInjection, env.Workdir, home)
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+": MCP config scan:", err)
		return nil, ExitIOError
	}
	registered := map[string]bool{}
	for _, e := range entries {
		registered[e.Name] = true
	}
	for i := range existing {
		existing[i].AlsoRegistered = registered[existing[i].Name]
	}
	warnings, refusal := mcpinject.Evaluate(policy, existing, findings)
	for _, w := range warnings {
		fmt.Fprintf(env.Stderr, "%s: warning: %s\n", prefix, w)
	}
	if refusal != nil {
		fmt.Fprintf(env.Stderr, "%s: %v\n", prefix, refusal)
		return nil, ExitConfigInvalid
	}
	return &mcpPlan{policy: policy, entries: entries}, ExitOK
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

	var routes []facade.Route
	for _, entry := range p.entries {
		serverSecrets, ok := p.loadSecrets(env, prefix, entry)
		if !ok {
			continue
		}
		running, err := p.sup.AddSidecar(ctx, supervisor.SidecarSpec{
			Name:           "mcp/" + entry.Name,
			SkillName:      entry.Name,
			SkillDir:       entry.SnapshotDir,
			Command:        []string{exe, "mcp", "serve", "--def", entry.DefPath},
			EnvPassthrough: entry.Def.Server.EnvPassthrough,
			Secrets:        serverSecrets,
			Health:         config.HealthSpec{Path: "/status"},
			LogPath:        filepath.Join(rtDir, "logs", "mcp-"+entry.Name+".log"),
			Workdir:        env.Workdir,
			RequireSecret:  true,
		})
		if err != nil {
			return nil, fmt.Errorf("MCP server %q: %w", entry.Name, err)
		}
		p.started = append(p.started, entry)
		routes = append(routes, facade.Route{
			Namespace: mcpreg.FacadeNamespace,
			Mount:     entry.Name,
			Upstream:  running.Upstream,
			Skill:     entry.Name,
			State:     facade.RouteReady,
		})
	}
	return routes, nil
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
	p.sup.ShutdownAll(mcpGatewayShutdownTimeout)
}

// buildInjection computes what the harness needs to start the bridges. It
// runs after the facade is up because the entries carry its address and token.
func (p *mcpPlan) buildInjection(harness config.Harness, socketPath string, tcpPort int, facadeToken, sandboxTmp string) error {
	if p == nil {
		return nil
	}
	servers := make([]mcpinject.Server, 0, len(p.started))
	for _, entry := range p.started {
		servers = append(servers, mcpinject.Server{
			Name: entry.Name,
			Argv: mcpreg.BridgePath(entry.Name),
			Env: map[string]string{
				"OMAC_BASE":         fmt.Sprintf("http://127.0.0.1:%d/", tcpPort),
				"OMAC_SOCKET":       socketPath,
				"OMAC_FACADE_TOKEN": facadeToken,
			},
		})
	}
	injection, err := mcpinject.Build(harness.MCPInjection, servers, p.policy, sandboxTmp)
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

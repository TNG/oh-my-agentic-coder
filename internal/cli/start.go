package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/facade"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandbox"
	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillconfig"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillsource"
	"github.com/tngtech/oh-my-agentic-coder/internal/supervisor"
)

func runStart(args []string, env *Env) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	var (
		profile            = fs.String("sandbox", "", "Name of a sandbox profile from the launcher config.")
		innerCmdOverride   = fs.String("inner", "", "Override inner_cmd's executable.")
		noSandbox          = fs.Bool("no-sandbox", false, "Run inner command directly, without a sandbox (debug only).")
		keepRunning        = fs.Bool("keep-running", false, "Do not stop sidecars when the inner command exits.")
		acceptSkillChanges = fs.Bool("accept-skill-changes", false, "Tolerate bundle_hash drift in registered skills (proceed even if the on-disk skill differs from what was registered).")
		prune              = fs.Bool("prune", false, "Remove installed skills not listed in .opencode/skills.yaml (requires skills.yaml to be present).")
		verbose            = fs.Bool("verbose", false, "Verbose lifecycle logging.")
	)
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac start [harness] [flags] [-- inner args...]")
		fmt.Fprintf(env.Stderr, "\nharness: one of %s (default: %s)\n\n",
			strings.Join(config.HarnessNames(), ", "), config.DefaultHarness().Name)
		fs.PrintDefaults()
	}
	// Preserve everything after "--" verbatim as inner args.
	var ourArgs, innerArgs []string
	split := false
	for _, a := range args {
		if !split && a == "--" {
			split = true
			continue
		}
		if split {
			innerArgs = append(innerArgs, a)
		} else {
			ourArgs = append(ourArgs, a)
		}
	}
	// Consume the optional leading positional harness token (e.g.
	// `omac start claude`) before flag parsing. The remainder is parsed as
	// flags (+ any non-flag positionals, which become inner args).
	harness, ourArgs, err := splitHarnessToken(ourArgs)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac start:", err)
		return ExitMisuse
	}
	if err := fs.Parse(reorderFlagsFirst(ourArgs)); err != nil {
		return ExitMisuse
	}
	innerArgs = append(fs.Args(), innerArgs...)

	// 1. Load launcher config.
	lc, cfgPath, err := config.LoadLauncher(env.Workdir)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac start: launcher config:", err)
		return ExitConfigInvalid
	}
	if *verbose && cfgPath != "" {
		fmt.Fprintf(env.Stderr, "[verbose] loaded launcher config: %s\n", cfgPath)
	}
	profName := *profile
	if profName == "" {
		profName = lc.Sandbox.DefaultProfile
	}
	prof, ok := lc.Sandbox.Profiles[profName]
	if !ok && !*noSandbox {
		fmt.Fprintf(env.Stderr, "omac start: unknown sandbox profile %q\n", profName)
		return ExitConfigInvalid
	}

	// 2. Reconcile registry against on-disk reality.
	//
	// Four kinds of drift are checked, in this order, before we spawn
	// anything. The order matters: pruning deleted skills first
	// shrinks the working set; then we make sure every on-disk skill
	// is registered; then we hash-check each registration; finally we
	// verify required config fields are resolvable. Any class of drift
	// short-circuits the start unless the user opts in (only bundle
	// drift is opt-in-able, with --accept-skill-changes).
	//
	// An empty registry is NOT in itself an error: omac still works as
	// a thin sandbox launcher even before any skills are registered.
	//
	// Registrations live in two layers: the workdir registry
	// (.opencode/sidecar.json) and the user-global registry
	// (~/.config/omac/sidecar.json). User-global skills register once,
	// globally; workdir-local skills register per-workdir. We load both
	// and merge them with the workdir layer winning on name collision
	// (same precedence as skillsource). Auto-deregister still operates
	// on the workdir layer only — see autoDeregisterMissing.
	workdirReg, err := registry.Load(env.Workdir)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac start: registry:", err)
		return ExitIOError
	}
	globalReg, err := registry.LoadGlobal()
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac start: global registry:", err)
		return ExitIOError
	}

	// 2a. Auto-deregister skills whose source directory has vanished.
	//     This is the only drift we silently fix; the user asked for
	//     a log line and a hint about purging the leftover state, but
	//     no exit-non-zero. Secrets and skill-config entries are
	//     intentionally KEPT so an accidental `rm -rf` on the skills
	//     dir doesn't lose values; the hint tells the user how to
	//     purge them later.
	pruned, err := autoDeregisterMissing(env, workdirReg, false)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac start: auto-deregister:", err)
		return ExitIOError
	}
	globalPruned, err := autoDeregisterMissing(env, globalReg, true)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac start: auto-deregister (global):", err)
		return ExitIOError
	}
	for _, p := range append(pruned, globalPruned...) {
		fmt.Fprintf(env.Stderr,
			"[info] %s: skill directory missing on disk; auto-deregistered. "+
				"Stored secrets and config remain. To purge: omac deregister --purge-secrets --purge-fields %s\n",
			p, p)
	}

	// Harness scoping: drop registry entries whose skill dir belongs to
	// another harness (e.g. a global skill under ~/.config/opencode/skills
	// while running `omac start claude`). The active harness cannot load
	// them, so omac must not mount or require them. Entries under the active
	// harness's own dir or the shared .agents dir, or under no recognizable
	// skills base, are kept.
	workdirReg = filterRegistryByHarness(workdirReg, env.Workdir, harness)
	globalReg = filterRegistryByHarness(globalReg, env.Workdir, harness)

	// Merge the two layers into the working registry used by the rest
	// of start. Workdir entries win over global entries with the same
	// name.
	reg := mergeRegistries(globalReg, workdirReg)

	// 2b. Warn (do not fail) if any installed skill is not registered.
	//     Installed-but-unregistered skills are visible in `omac list`
	//     (STATUS=installed) and can be activated later with `omac register`.
	//     Only registered skills are loaded by this start run.
	discovered, unregistered, err := findUnregisteredSkills(env.Workdir, harness, reg)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac start: scan skills:", err)
		return ExitIOError
	}
	if len(unregistered) > 0 {
		fmt.Fprintln(env.Stderr, "omac start: installed skills not yet registered (skipping, run omac register to activate):")
		for _, name := range unregistered {
			fmt.Fprintf(env.Stderr, "  %s\n", name)
		}
	}

	// Warn once per project about globally-uninstalled skills (tombstone check).
	warnUnacknowledgedRemovals(env)

	if len(reg.Registered) == 0 {
		fmt.Fprintln(env.Stderr,
			"omac start: no skills registered in this workdir; "+
				"starting sandbox without sidecars (run `omac register` to add some)")
	}

	// 2c-d / 3. Per-skill validation + secret/config resolution.
	//
	// We do this in a single pass that accumulates every per-skill
	// problem rather than returning on the first one. The user's
	// complaint when this returned early was that fixing skill A
	// revealed skill B revealed skill C, etc. — N invocations to fix
	// N problems. With accumulation, the user sees every
	// re-registration command they need at once.
	//
	// Problems are bucketed by class so the consolidated error block
	// has a section header per class with an actionable hint. A skill
	// that hits multiple classes appears in every relevant section
	// (we don't short-circuit to "first failing class").
	//
	// Secret values are eagerly fetched from the keychain even when
	// we may not end up using them; they're zeroed by the deferred
	// cleanup below regardless of which path we take.
	type withSecrets struct {
		entry   registry.Entry
		meta    *config.Meta
		abs     string
		secrets map[string]secrets.Secret
		config  map[string]string
	}
	var allSecrets []secrets.Secret
	defer func() {
		for i := range allSecrets {
			allSecrets[i].Zero()
		}
	}()

	workdirCfg, err := skillconfig.Load(env.Workdir)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac start: skill-config:", err)
		return ExitIOError
	}
	globalCfg, err := skillconfig.LoadGlobal()
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac start: global skill-config:", err)
		return ExitIOError
	}
	// Merge config the same way as the registry: workdir values
	// override global values per (skill, field).
	configStore := mergeConfig(globalCfg, workdirCfg)

	// Per-class problem accumulators. Each maps a hint template to
	// the affected skill names so we can render "do X for these N
	// skills" rather than N copies of the same hint. Order is
	// preserved by also tracking which classes saw any input.
	type bundleDriftProblem struct{ skill string }
	type missingSecretProblem struct{ skill, secret string }
	type missingFieldProblem struct {
		skill  string
		fields []string
	}
	type metaProblem struct{ skill, msg string }

	var bundleDrifts []bundleDriftProblem
	var missingSecrets []missingSecretProblem
	var missingFields []missingFieldProblem
	var metaProblems []metaProblem

	armed := make([]withSecrets, 0, len(reg.Registered))
	for _, e := range reg.Registered {
		absDir := e.SkillDir
		if !filepath.IsAbs(absDir) {
			absDir = filepath.Join(env.Workdir, absDir)
		}
		metaPath := filepath.Join(absDir, config.MetaFileName)
		m, err := config.LoadMeta(metaPath)
		if err != nil {
			metaProblems = append(metaProblems, metaProblem{skill: e.Name, msg: err.Error()})
			continue
		}
		if m.Sidecar == nil {
			metaProblems = append(metaProblems, metaProblem{skill: e.Name, msg: "meta no longer has a sidecar block"})
			continue
		}

		// Bundle hash. Excluded from scanning when the user has
		// explicitly opted in to drift via --accept-skill-changes.
		if !*acceptSkillChanges {
			bundle, err := config.BundleHash(absDir)
			if err != nil {
				// I/O errors during hashing are class-level (we can't
				// produce useful per-skill diagnostics if the directory
				// is unreadable). Abort immediately.
				fmt.Fprintln(env.Stderr, "omac start: bundle hash:", err)
				return ExitIOError
			}
			if bundle != e.BundleHash {
				bundleDrifts = append(bundleDrifts, bundleDriftProblem{skill: e.Name})
				// Don't `continue`: continue collecting problems for
				// THIS skill (missing secret + missing field) so the
				// user sees everything needed in one shot.
			}
		}

		// Secrets. Read with the workdir-scoped key first, falling back to
		// the unscoped key — so secrets stored by a serve-aware register
		// (scoped per workdir) and legacy/global secrets (unscoped) both
		// resolve. See docs/MULTI_DIR_DESKTOP.md §4.3.
		secScope := keychain.WorkdirID(env.Workdir)
		secMap := map[string]secrets.Secret{}
		for _, spec := range m.Sidecar.Secrets {
			val, err := keychain.GetWithFallback(secScope, e.Name, spec.Name)
			if err != nil {
				if errors.Is(err, keychain.ErrNotFound) {
					if spec.IsRequired() {
						missingSecrets = append(missingSecrets,
							missingSecretProblem{skill: e.Name, secret: spec.Name})
					}
					continue
				}
				// Keychain I/O failure is class-level (likely auth
				// rejection on macOS); the user fixes it once and
				// retries. No point continuing.
				fmt.Fprintln(env.Stderr, "omac start: keychain:", err)
				return ExitKeychainError
			}
			secMap[spec.Name] = val
			allSecrets = append(allSecrets, val)
		}

		// Config fields. Same precedence as `omac config show`:
		// stored > spec.Default > $spec.DefaultFromEnv > missing.
		cfgMap := map[string]string{}
		var missingForSkill []string
		for _, spec := range m.Sidecar.Config {
			v, ok := configStore.Get(e.Name, spec.Name)
			if ok {
				cfgMap[spec.Name] = v
				continue
			}
			if spec.Default != "" {
				cfgMap[spec.Name] = spec.Default
				continue
			}
			if spec.DefaultFromEnv != "" {
				if envVal, ok := os.LookupEnv(spec.DefaultFromEnv); ok && envVal != "" {
					cfgMap[spec.Name] = envVal
					continue
				}
			}
			if spec.IsRequired() {
				missingForSkill = append(missingForSkill, spec.Name)
			}
		}
		if len(missingForSkill) > 0 {
			missingFields = append(missingFields,
				missingFieldProblem{skill: e.Name, fields: missingForSkill})
		}

		armed = append(armed, withSecrets{
			entry: e, meta: m, abs: absDir,
			secrets: secMap, config: cfgMap,
		})
	}

	// If anything went wrong above, render one consolidated report
	// (grouped by problem class) and abort.
	if len(bundleDrifts) > 0 || len(missingSecrets) > 0 || len(missingFields) > 0 || len(metaProblems) > 0 {
		total := len(bundleDrifts) + len(missingSecrets) + len(missingFields) + len(metaProblems)
		fmt.Fprintf(env.Stderr, "omac start: refusing to start, found %d problem(s):\n", total)

		if len(metaProblems) > 0 {
			fmt.Fprintln(env.Stderr, "\n  "+config.MetaFileName+" broken:")
			for _, p := range metaProblems {
				fmt.Fprintf(env.Stderr, "    %s — %s\n", p.skill, p.msg)
			}
		}
		if len(bundleDrifts) > 0 {
			fmt.Fprintln(env.Stderr, "\n  bundle changed since register (pass --accept-skill-changes to proceed, or re-register):")
			for _, p := range bundleDrifts {
				fmt.Fprintf(env.Stderr, "    %s — omac register --force %s\n", p.skill, p.skill)
			}
		}
		if len(missingSecrets) > 0 {
			fmt.Fprintln(env.Stderr, "\n  required secret missing:")
			for _, p := range missingSecrets {
				fmt.Fprintf(env.Stderr, "    %s/%s — omac secrets set %s %s\n",
					p.skill, p.secret, p.skill, p.secret)
			}
		}
		if len(missingFields) > 0 {
			fmt.Fprintln(env.Stderr, "\n  required config field missing:")
			for _, p := range missingFields {
				fmt.Fprintf(env.Stderr, "    %s — fields: %s — omac register --reprompt-fields %s\n",
					p.skill, strings.Join(p.fields, ", "), p.skill)
			}
		}
		fmt.Fprintln(env.Stderr)

		// Pick the most actionable exit code: secrets/fields refused
		// outweighs config-invalid (the latter is a build/dev problem,
		// the former usually a one-command fix). Bundle drift is
		// strictly config-invalid because the user hasn't explicitly
		// accepted the change yet. Meta problems are config-invalid.
		if len(missingSecrets) > 0 || len(missingFields) > 0 {
			return ExitSecretRefused
		}
		return ExitConfigInvalid
	}

	// 4. Create runtime directory.
	rtDir, err := createRuntimeDir(env.Workdir)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac start: runtime dir:", err)
		return ExitIOError
	}
	if *verbose {
		fmt.Fprintf(env.Stderr, "[verbose] runtime dir: %s\n", rtDir)
	}
	socketPath := filepath.Join(rtDir, "bridge.sock")

	// Per-session sandbox temp dir. Bun-built harnesses (opencode) extract
	// an embedded runtime into TMPDIR at startup; the sandbox must grant
	// read+write on it (the nono profile does, via {{tmpdir}}) AND the inner
	// command must see it as TMPDIR (set in `extra` below). We create a
	// fresh, isolated dir per launch and remove it on exit.
	sandboxTmp, err := os.MkdirTemp("", "omac-sandbox-tmp-")
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac start: sandbox temp dir:", err)
		return ExitIOError
	}
	defer os.RemoveAll(sandboxTmp)
	if *verbose {
		fmt.Fprintf(env.Stderr, "[verbose] sandbox TMPDIR: %s\n", sandboxTmp)
	}

	// 5. Spawn sidecars.
	sup := supervisor.New(lc.Facade.BaseEnvPassthrough)
	defer func() {
		if !*keepRunning {
			sup.ShutdownAll(5 * time.Second)
		}
	}()
	specs := make([]supervisor.SidecarSpec, 0, len(armed))
	for _, s := range armed {
		health := config.HealthSpec{}
		if s.meta.Sidecar.Health != nil {
			health = *s.meta.Sidecar.Health
		}
		specs = append(specs, supervisor.SidecarSpec{
			Name:             s.entry.Name,
			SkillDir:         s.abs,
			Command:          s.meta.Sidecar.Command,
			EnvPassthrough:   s.meta.Sidecar.EnvPassthrough,
			Secrets:          s.secrets,
			Config:           s.config,
			Health:           health.Defaults(),
			LogPath:          filepath.Join(rtDir, "logs", s.entry.Name+".log"),
			Workdir:          env.Workdir,
			HarnessSkillsDir: harness.WorkdirSkillsDir(),
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	running, err := sup.StartAll(ctx, specs)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac start:", err)
		return ExitSidecarHealthcheckFail
	}

	// 6. Build facade routes.
	routes := make([]facade.Route, 0, len(running))
	mounts := make([]string, 0, len(running))
	for i, r := range running {
		mount := armed[i].meta.Sidecar.MountOrDefault(r.Name)
		var maxBody int64
		var idle time.Duration
		if lim := armed[i].meta.Sidecar.Limits; lim != nil {
			maxBody = lim.MaxBodyBytes
			idle = time.Duration(lim.IdleTimeoutSecs) * time.Second
		}
		routes = append(routes, facade.Route{
			Mount:        mount,
			UpstreamPort: r.Port,
			MaxBodyBytes: maxBody,
			IdleTimeout:  idle,
			Skill:        r.Name,
			SkillDir:     armed[i].abs,
		})
		mounts = append(mounts, mount)
	}

	// 7. Open both listeners (Unix socket + ephemeral 127.0.0.1 TCP) and
	//    mount routes. We always bind both so clients can pick whichever
	//    transport their environment permits — see internal/facade for
	//    the rationale (proxy-mode Seatbelt blocks AF_UNIX connect on
	//    macOS, and `--open-port` is the documented escape hatch).
	f := facade.New(
		socketPath,
		"127.0.0.1:0",
		routes,
		lc.Facade.MaxBodyBytes,
		time.Duration(lc.Facade.IdleTimeoutSecs)*time.Second,
		filepath.Join(rtDir, "logs", "facade.log"),
		env.Version,
	)
	if err := f.Start(ctx); err != nil {
		fmt.Fprintln(env.Stderr, "omac start: facade:", err)
		return ExitIOError
	}
	defer f.Close()
	tcpPort := f.TCPPort()
	if *verbose {
		fmt.Fprintf(env.Stderr, "[verbose] facade listening on %s and 127.0.0.1:%d (%d route(s))\n",
			socketPath, tcpPort, len(routes))
	}

	// Live-reload control plane: lets `omac register` from an outside
	// terminal mount a new skill onto this running session without a
	// restart (mirrors serve). Non-fatal if it can't bind.
	reloader := &startReloader{
		env: env, facade: f, sup: sup, ctx: ctx,
		rtDir: rtDir, socket: socketPath, tcpPort: tcpPort, verbose: *verbose,
		mounted: map[string]string{},
	}
	for i, a := range armed {
		reloader.markMounted(a.entry.Name, a.meta.Sidecar.MountOrDefault(running[i].Name))
	}
	controlURL, closeControl, controlOK := startControlPlane(reloader)
	defer closeControl()
	if controlOK && *verbose {
		fmt.Fprintf(env.Stderr, "[verbose] control plane: %s\n", controlURL)
	}

	// 7b. Sync declarative skill manifest (.opencode/skills.yaml) if present.
	// This runs after sidecars are healthy so the marketplace sidecar is
	// reachable. Non-fatal: a missing marketplace or absent secrets are warned
	// and skipped; the session proceeds regardless.
	if code := maybeSync(env, discovered, reg, tcpPort, *prune, harness); code != ExitOK {
		return code
	}

	// 8. Build sandbox argv and exec.
	//
	// Resolve the inner command for the selected harness: an explicit
	// --inner override wins, else the profile's inner_cmd, else the
	// harness's default InnerCmd (config.Harness.ResolveInnerCmd).
	inner := harness.ResolveInnerCmd(prof.InnerCmd, *innerCmdOverride)
	if len(innerArgs) > 0 {
		inner = append(inner, innerArgs...)
	}

	var argv []string
	if *noSandbox {
		argv = inner
	} else {
		argv, err = sandbox.Expand(prof, sandbox.Inputs{
			Workdir:  env.Workdir,
			Socket:   socketPath,
			TCPPort:  tcpPort,
			Mounts:   mounts,
			InnerCmd: inner,
			TmpDir:   sandboxTmp,
		})
		if err != nil {
			fmt.Fprintln(env.Stderr, "omac start: sandbox argv:", err)
			return ExitConfigInvalid
		}
		// Whitelist the control-plane port into the sandbox so the inner
		// command (and the omac plugin inside it) can reach
		// OMAC_CONTROL_BASE for live reloads.
		if controlOK {
			if _, port, perr := net.SplitHostPort(controlURL[len("http://"):]); perr == nil {
				argv = injectOpenPort(argv, port)
			}
		}
	}
	if *verbose {
		fmt.Fprintf(env.Stderr, "[verbose] sandbox argv: %v\n", argv)
	}

	// Signal handling is owned by sandbox.Exec: it places the inner
	// command in its own process group, hands the terminal foreground to
	// it (so Ctrl-C goes there directly), and forwards SIGINT/SIGTERM/
	// SIGHUP/SIGQUIT delivered to omac itself onto the child's pgid.
	// When the child exits the deferred cleanups below tear down the
	// facade and the supervised sidecars in order.

	// Extra env passed into the sandbox runtime's own process environment.
	// The runtime is expected to propagate parent env to the inner process
	// (nono's default behavior; controllable via the profile's
	// `environment.allow_vars` field — if set, OMAC_* must be in it).
	//
	// Both transports are advertised to the sandbox. Clients should
	// prefer OMAC_<SKILL>_BASE (TCP-based by default; that is what works
	// under nono proxy mode), and fall back to OMAC_<SKILL>_SOCKET_BASE
	// for environments that prefer Unix sockets.
	extra := map[string]string{
		"OMAC_SOCKET":             socketPath,
		"OMAC_HOST":               "127.0.0.1",
		"OMAC_PORT":               fmt.Sprintf("%d", tcpPort),
		"OMAC_BASE":               fmt.Sprintf("http://127.0.0.1:%d/", tcpPort),
		"OMAC_SKILLS":             strings.Join(mounts, ","),
		"OMAC_VERSION":            env.Version,
		"OMAC_HARNESS":            harness.Name,
		"OMAC_HARNESS_SKILLS_DIR": harness.WorkdirSkillsDir(),
		// Point the inner command at the sandbox-granted temp dir. The
		// nono profile grants RW on this path via {{tmpdir}}; exporting it
		// as TMPDIR is what makes Bun-built harnesses (opencode) extract
		// their runtime into a writable, allowed location.
		"TMPDIR": sandboxTmp,
	}
	for _, m := range mounts {
		extra[sandbox.OmacEnvName(m)] = sandbox.OmacTCPEnvValue(m, tcpPort)
		extra[sandbox.OmacSocketEnvName(m)] = sandbox.OmacEnvValue(m, socketPath)
	}
	if controlOK {
		extra["OMAC_CONTROL_BASE"] = controlURL
	}

	code, err := sandbox.ExecWithReady(argv, extra, nil)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac start: exec:", err)
		return ExitSandboxAbnormal
	}
	return code
}

// autoDeregisterMissing prunes registry entries whose skill directory
// no longer exists on disk. Returns the names of skills that were
// pruned, in the order they appeared in the registry. Secrets and
// skill-config entries are deliberately NOT touched: an accidental
// `rm -rf` shouldn't lose values.
//
// The `global` flag selects which layer is being reconciled: the
// user-global registry (~/.config/omac/sidecar.json) or the workdir
// registry. Workdir-relative SkillDir paths only occur in the workdir
// layer; global entries always store absolute paths, so joining with
// env.Workdir for a non-absolute path is harmless either way.
//
// Operates under the matching flock so concurrent `omac register`
// calls don't race with us.
func autoDeregisterMissing(env *Env, reg *registry.Registry, global bool) ([]string, error) {
	if len(reg.Registered) == 0 {
		return nil, nil
	}
	var pruned []string
	var keep []registry.Entry
	for _, e := range reg.Registered {
		absDir := e.SkillDir
		if !filepath.IsAbs(absDir) {
			absDir = filepath.Join(env.Workdir, absDir)
		}
		// We require both the directory AND its omac.yaml to still
		// exist; either alone is "broken", but a missing omac.yaml
		// would have been caught later anyway. Treating both cases as
		// "skill is gone" is simpler.
		if _, err := os.Stat(filepath.Join(absDir, config.MetaFileName)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				pruned = append(pruned, e.Name)
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", e.Name, err)
		}
		keep = append(keep, e)
	}
	if len(pruned) == 0 {
		return nil, nil
	}
	reload := func() (*registry.Registry, error) { return registry.Load(env.Workdir) }
	persist := func(r *registry.Registry) error { return registry.Save(env.Workdir, r) }
	lock := func(fn func() error) error { return registry.WithLock(env.Workdir, fn) }
	if global {
		reload = registry.LoadGlobal
		persist = registry.SaveGlobal
		lock = registry.WithGlobalLock
	}
	if err := lock(func() error {
		// Re-load under the lock and re-apply the prune. Don't reuse
		// the in-memory reg (it might be stale relative to a parallel
		// `omac register`).
		fresh, err := reload()
		if err != nil {
			return err
		}
		for _, name := range pruned {
			fresh.Remove(name)
		}
		return persist(fresh)
	}); err != nil {
		return nil, err
	}
	// Update caller's view so subsequent steps don't iterate pruned skills.
	reg.Registered = keep
	return pruned, nil
}

// mergeRegistries returns a registry whose entries are the union of the
// global and workdir layers, with the workdir entry winning on a name
// collision (matching skillsource's "workdir wins" precedence). Neither
// input is mutated.
func mergeRegistries(global, workdir *registry.Registry) *registry.Registry {
	out := &registry.Registry{Version: registry.SchemaVersion}
	seen := map[string]struct{}{}
	for _, e := range workdir.Registered {
		out.Registered = append(out.Registered, e)
		seen[e.Name] = struct{}{}
	}
	for _, e := range global.Registered {
		if _, dup := seen[e.Name]; dup {
			continue
		}
		out.Registered = append(out.Registered, e)
		seen[e.Name] = struct{}{}
	}
	return out
}

// mergeConfig returns a store whose (skill, field) values are the union
// of the global and workdir layers, with workdir values overriding
// global ones field-by-field. Neither input is mutated.
func mergeConfig(global, workdir *skillconfig.Store) *skillconfig.Store {
	out := &skillconfig.Store{Version: skillconfig.SchemaVersion, Skills: map[string]map[string]string{}}
	for skill, fields := range global.Skills {
		for field, val := range fields {
			out.Set(skill, field, val)
		}
	}
	for skill, fields := range workdir.Skills {
		for field, val := range fields {
			out.Set(skill, field, val)
		}
	}
	return out
}

// findUnregisteredSkills returns the names of every skill discovered
// across every source omac knows about and that has a omac.yaml but
// is NOT in the registry. Names are sorted for deterministic output.
//
// Sources include the workdir-local roots (<workdir>/.agents/skills
// and <workdir>/.opencode/skills, with .agents winning on collision)
// and every user-global root that exists on disk (XDG-style and
// legacy flat layouts under both `agents/` and `opencode/`). See the
// skillsource package for the full precedence list. Workdir-local
// skills always win over user-global ones with the same name;
// skillsource.Discover handles dedup internally.
// filterRegistryByHarness returns a copy of reg keeping only entries whose
// skill directory is in the active harness's scope. A relative SkillDir (as
// stored for workdir-local skills) is classified by its path segments; an
// absolute one (global skills) likewise. Entries under no recognizable skills
// base are kept (custom locations are not silently dropped).
func filterRegistryByHarness(reg *registry.Registry, workdir string, harness config.Harness) *registry.Registry {
	if reg == nil {
		return reg
	}
	out := &registry.Registry{Version: reg.Version}
	for _, e := range reg.Registered {
		dir := e.SkillDir
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(workdir, dir)
		}
		if skillsource.DirInHarnessScope(dir, harness) {
			out.Registered = append(out.Registered, e)
		}
	}
	return out
}

func findUnregisteredSkills(workdir string, harness config.Harness, reg *registry.Registry) ([]skillsource.Entry, []string, error) {
	discovered, err := skillsource.Discover(workdir, harness)
	if err != nil {
		return nil, nil, err
	}
	registered := map[string]struct{}{}
	for _, e := range reg.Registered {
		registered[e.Name] = struct{}{}
	}
	var out []string
	for _, e := range discovered {
		if _, ok := registered[e.Name]; !ok {
			out = append(out, e.Name)
		}
	}
	sort.Strings(out)
	return discovered, out, nil
}

// createRuntimeDir creates ${TMPDIR}/omac-<workdir-hash>/{logs,pids}.
func createRuntimeDir(workdir string) (string, error) {
	tmp := os.TempDir()
	sum := sha256.Sum256([]byte(workdir))
	name := "omac-" + hex.EncodeToString(sum[:6])
	dir := filepath.Join(tmp, name)
	// Clean stale directory if present.
	if _, err := os.Stat(dir); err == nil {
		_ = os.RemoveAll(dir)
	}
	for _, sub := range []string{"", "logs", "pids"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

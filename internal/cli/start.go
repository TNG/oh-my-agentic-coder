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
	"os/exec"
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
	"github.com/tngtech/oh-my-agentic-coder/internal/session"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillconfig"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillsource"
	"github.com/tngtech/oh-my-agentic-coder/internal/supervisor"
)

// launchOpts carries everything runLaunch needs: the resolved harness, the
// parsed start-family flags, and the inner args to append to the resolved
// inner command. `omac start`, `omac continue`, and `omac resume` all build
// one of these and call runLaunch, so the launch pipeline has a single
// implementation.
type launchOpts struct {
	// label is the invoking subcommand name ("start", "continue", "resume").
	// runLaunch uses it to prefix diagnostics so a failure surfaced via
	// `omac continue`/`omac resume` is not mislabeled as `omac start:`.
	label              string
	harness            config.Harness
	profile            string
	innerCmdOverride   string
	noSandbox          bool
	keepRunning        bool
	acceptSkillChanges bool
	skipSecretPattern  bool
	verbose            bool
	// sessionID, when non-empty, selects a specific session to continue by id
	// (`omac continue -s <id>`). Empty means "most recent" (the default).
	sessionID string
	// innerArgs are appended to the resolved inner command (user-supplied
	// trailing `-- args` plus any command-specific flags like --continue).
	innerArgs []string
}

// parseLaunchArgs parses the shared start-family command line: an optional
// leading positional harness token, the start flags, and trailing `-- inner
// args`. cmdName is used in usage/error text (e.g. "start", "continue"). It
// returns the assembled opts and true on success; on a parse/usage error it
// prints to stderr and returns false (callers map that to ExitMisuse).
func parseLaunchArgs(cmdName string, args []string, env *Env) (launchOpts, bool) {
	fs := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	var (
		profile            = fs.String("sandbox", "", "Name of a sandbox profile from the launcher config.")
		innerCmdOverride   = fs.String("inner", "", "Override inner_cmd's executable.")
		noSandbox          = fs.Bool("no-sandbox", false, "Run inner command directly, without a sandbox (debug only).")
		keepRunning        = fs.Bool("keep-running", false, "Do not stop sidecars when the inner command exits.")
		acceptSkillChanges = fs.Bool("accept-skill-changes", false, "Tolerate bundle_hash drift in registered skills (proceed even if the on-disk skill differs from what was registered).")
		skipSecretPattern  = fs.Bool("skip-secret-pattern", false, "Do not enforce a secret's pattern against an env_passthrough-supplied value (escape hatch for an outdated pattern; the raw value is still passed through).")
		verbose            = fs.Bool("verbose", false, "Verbose lifecycle logging.")
		sessionID          = fs.String("session", "", "Continue a specific session by id instead of the most recent one. (shorthand: -s)")
	)
	// -s is the documented shorthand for --session (opencode mirrors this
	// with `opencode -s <id>`; claude uses --resume, so its shorthand is
	// different, but `omac -s` is harness-agnostic).
	fs.StringVar(sessionID, "s", "", "Shorthand for --session.")
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: omac %s [harness] [flags] [-- inner args...]\n", cmdName)
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
		fmt.Fprintf(env.Stderr, "omac %s: %v\n", cmdName, err)
		return launchOpts{}, false
	}
	if err := fs.Parse(reorderFlagsFirst(ourArgs)); err != nil {
		return launchOpts{}, false
	}
	innerArgs = append(fs.Args(), innerArgs...)
	return launchOpts{
		label:              cmdName,
		harness:            harness,
		profile:            *profile,
		innerCmdOverride:   *innerCmdOverride,
		noSandbox:          *noSandbox,
		keepRunning:        *keepRunning,
		acceptSkillChanges: *acceptSkillChanges,
		skipSecretPattern:  *skipSecretPattern,
		verbose:            *verbose,
		sessionID:          *sessionID,
		innerArgs:          innerArgs,
	}, true
}

// checkInnerBinary verifies the harness's inner command binary is on $PATH.
// Returns ExitOK when found, ExitPrerequisiteMissing when missing, ExitOK
// when InnerCmd is empty (defensive skip). Called by runLaunch and runServe.
func checkInnerBinary(harness config.Harness, prefix string, env *Env) int {
	if len(harness.InnerCmd) == 0 {
		return ExitOK
	}
	if _, err := exec.LookPath(harness.InnerCmd[0]); err != nil {
		fmt.Fprintf(env.Stderr, "%s: harness binary %q not found on $PATH; install it or pass --inner-cmd <path>\n", prefix, harness.InnerCmd[0])
		return ExitPrerequisiteMissing
	}
	return ExitOK
}

func runStart(args []string, env *Env) int {
	opts, ok := parseLaunchArgs("start", args, env)
	if !ok {
		return ExitMisuse
	}
	return runLaunch(env, opts)
}

// runLaunch is the shared start pipeline: load config, reconcile the registry,
// spawn sidecars + facade + control plane, build the sandbox argv, and exec
// the inner command. It is invoked by `start`, `continue`, and `resume`.
func runLaunch(env *Env, opts launchOpts) int {
	harness := opts.harness
	profile := opts.profile
	innerCmdOverride := opts.innerCmdOverride
	noSandbox := opts.noSandbox
	keepRunning := opts.keepRunning
	acceptSkillChanges := opts.acceptSkillChanges
	skipSecretPattern := opts.skipSecretPattern
	verbose := opts.verbose
	innerArgs := opts.innerArgs

	// Diagnostics are prefixed with the invoking subcommand so a failure
	// surfaced through `omac continue`/`omac resume` is not mislabeled.
	label := opts.label
	if label == "" {
		label = "start"
	}
	prefix := "omac " + label

	// 1. Load launcher config.
	lc, cfgPath, err := config.LoadLauncher(env.Workdir)
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+": launcher config:", err)
		return ExitConfigInvalid
	}
	if verbose && cfgPath != "" {
		fmt.Fprintf(env.Stderr, "[verbose] loaded launcher config: %s\n", cfgPath)
	}
	profName := profile
	if profName == "" {
		profName = lc.Sandbox.DefaultProfile
	}
	prof, ok := lc.Sandbox.Profiles[profName]
	if !ok && !noSandbox {
		fmt.Fprintf(env.Stderr, prefix+": unknown sandbox profile %q\n", profName)
		return ExitConfigInvalid
	}

	// 1b. Pre-flight: inner harness binary must be on $PATH.
	if innerCmdOverride == "" {
		if code := checkInnerBinary(harness, prefix, env); code != ExitOK {
			return code
		}
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
		fmt.Fprintln(env.Stderr, prefix+": registry:", err)
		return ExitIOError
	}
	globalReg, err := registry.LoadGlobal()
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+": global registry:", err)
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
		fmt.Fprintln(env.Stderr, prefix+": auto-deregister:", err)
		return ExitIOError
	}
	globalPruned, err := autoDeregisterMissing(env, globalReg, true)
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+": auto-deregister (global):", err)
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

	// 2b. Refuse if any unregistered skill exists under any of the
	//     skill source roots (workdir-local .agents/skills and
	//     .opencode/skills, plus the user-global layers — see the
	//     skillsource package for the full list).
	//     "Skill" here means a directory with a omac.yaml. The user
	//     must explicitly register each one (so registration prompts,
	//     keychain seeding, etc. don't get silently skipped).
	unregistered, err := findUnregisteredSkills(env.Workdir, harness, reg)
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+": scan skills:", err)
		return ExitIOError
	}
	if len(unregistered) > 0 {
		fmt.Fprintln(env.Stderr, prefix+": unregistered skills found in this workdir:")
		for _, name := range unregistered {
			fmt.Fprintf(env.Stderr, "  %s — register with: omac register %s\n", name, name)
		}
		fmt.Fprintln(env.Stderr, "\nA skill you no longer want can be deleted with `omac deregister <skill>` (add --global for a user-global skill).")
		return ExitPrerequisiteMissing
	}

	if len(reg.Registered) == 0 {
		fmt.Fprintln(env.Stderr,
			prefix+": no skills registered in this workdir; "+
				"starting sandbox without sidecars (run `omac register` to add some)")
	}

	// Idempotently provision omac's built-in skills for this harness so they
	// are available with no separate setup step. Quiet when already current;
	// never blocks the launch.
	ensureBuiltinSkills(env, harness)

	// Likewise provision the OpenCode bridge plugin that carries the briefing.
	ensureOpenCodePlugin(env, harness)

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
		fmt.Fprintln(env.Stderr, prefix+": skill-config:", err)
		return ExitIOError
	}
	globalCfg, err := skillconfig.LoadGlobal()
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+": global skill-config:", err)
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
	type invalidSecretProblem struct{ skill, secret, reason string }
	type missingFieldProblem struct {
		skill  string
		fields []string
	}
	type metaProblem struct{ skill, msg string }

	var bundleDrifts []bundleDriftProblem
	var missingSecrets []missingSecretProblem
	var invalidSecrets []invalidSecretProblem
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
		if !acceptSkillChanges {
			bundle, err := config.BundleHash(absDir)
			if err != nil {
				// I/O errors during hashing are class-level (we can't
				// produce useful per-skill diagnostics if the directory
				// is unreadable). Abort immediately.
				fmt.Fprintln(env.Stderr, prefix+": bundle hash:", err)
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
		// A secret listed under env_passthrough is allowed to come from the
		// host environment instead of the keychain — the documented fallback
		// for keychain-less environments (CI runners, headless servers). The
		// supervisor injects these at spawn time (see supervisor.buildEnv),
		// so a required secret absent from the keychain is NOT missing when
		// the shell already exports it. Without this check the preflight
		// would refuse to start before runtime injection ever happens.
		envPassthrough := map[string]struct{}{}
		for _, k := range m.Sidecar.EnvPassthrough {
			envPassthrough[k] = struct{}{}
		}
		secMap := map[string]secrets.Secret{}
		for _, spec := range m.Sidecar.Secrets {
			val, err := keychain.GetWithFallback(secScope, e.Name, spec.Name)
			if err != nil {
				if errors.Is(err, keychain.ErrNotFound) {
					// Not in the keychain — see whether env_passthrough
					// supplies it from the host at runtime. A value found
					// there must still match the secret's pattern: keychain
					// values were validated at register time, so this is the
					// only chance to vet an env-supplied one before the
					// sidecar gets a malformed token. --skip-secret-pattern
					// is the escape hatch for a stale pattern: the raw value
					// is still passed through, just not vetted here.
					if envVal, ok := secretFromEnv(spec.Name, envPassthrough); ok {
						if !skipSecretPattern {
							if perr := validatePattern(spec, envVal); perr != nil {
								invalidSecrets = append(invalidSecrets,
									invalidSecretProblem{skill: e.Name, secret: spec.Name, reason: perr.Error()})
							}
						}
						continue
					}
					if spec.IsRequired() {
						missingSecrets = append(missingSecrets,
							missingSecretProblem{skill: e.Name, secret: spec.Name})
					}
					continue
				}
				// Keychain I/O failure is class-level (likely auth
				// rejection on macOS); the user fixes it once and
				// retries. No point continuing.
				fmt.Fprintln(env.Stderr, prefix+": keychain:", err)
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
	if len(bundleDrifts) > 0 || len(missingSecrets) > 0 || len(invalidSecrets) > 0 || len(missingFields) > 0 || len(metaProblems) > 0 {
		total := len(bundleDrifts) + len(missingSecrets) + len(invalidSecrets) + len(missingFields) + len(metaProblems)
		fmt.Fprintf(env.Stderr, prefix+": refusing to start, found %d problem(s):\n", total)

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
		if len(invalidSecrets) > 0 {
			fmt.Fprintln(env.Stderr, "\n  secret from environment failed pattern validation (pass --skip-secret-pattern if the pattern is outdated):")
			for _, p := range invalidSecrets {
				fmt.Fprintf(env.Stderr, "    %s/%s — %s (fix the exported value, or run omac secrets set %s %s)\n",
					p.skill, p.secret, p.reason, p.skill, p.secret)
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
		if len(missingSecrets) > 0 || len(invalidSecrets) > 0 || len(missingFields) > 0 {
			return ExitSecretRefused
		}
		return ExitConfigInvalid
	}

	// 4. Create runtime directory.
	rtDir, err := createRuntimeDir(env.Workdir)
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+": runtime dir:", err)
		return ExitIOError
	}
	if verbose {
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
		fmt.Fprintln(env.Stderr, prefix+": sandbox temp dir:", err)
		return ExitIOError
	}
	defer os.RemoveAll(sandboxTmp)
	if verbose {
		fmt.Fprintf(env.Stderr, "[verbose] sandbox TMPDIR: %s\n", sandboxTmp)
	}

	// 5. Spawn sidecars.
	sup := supervisor.New(lc.Facade.BaseEnvPassthrough)
	defer func() {
		if !keepRunning {
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
		fmt.Fprintln(env.Stderr, prefix+":", err)
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
		fmt.Fprintln(env.Stderr, prefix+": facade:", err)
		return ExitIOError
	}
	defer f.Close()
	tcpPort := f.TCPPort()
	if verbose {
		fmt.Fprintf(env.Stderr, "[verbose] facade listening on %s and 127.0.0.1:%d (%d route(s))\n",
			socketPath, tcpPort, len(routes))
	}

	// Live-reload control plane: lets `omac register` from an outside
	// terminal mount a new skill onto this running session without a
	// restart (mirrors serve). Non-fatal if it can't bind.
	reloader := &startReloader{
		env: env, facade: f, sup: sup, ctx: ctx,
		rtDir: rtDir, socket: socketPath, tcpPort: tcpPort, verbose: verbose,
		mounted: map[string]string{},
	}
	for i, a := range armed {
		reloader.markMounted(a.entry.Name, a.meta.Sidecar.MountOrDefault(running[i].Name))
	}
	controlURL, closeControl, controlOK := startControlPlane(reloader)
	defer closeControl()
	if controlOK && verbose {
		fmt.Fprintf(env.Stderr, "[verbose] control plane: %s\n", controlURL)
	}

	// 8. Build sandbox argv and exec.
	//
	// Resolve the inner command for the selected harness: an explicit
	// --inner override wins, else the profile's inner_cmd, else the
	// harness's default InnerCmd (config.Harness.ResolveInnerCmd).
	inner := harness.ResolveInnerCmd(prof.InnerCmd, innerCmdOverride)
	// Inject the sandbox briefing: Claude via its --append-system-prompt flag
	// (SystemContextArgs), OpenCode via OMAC_SANDBOX_BRIEFING set below.
	briefingText, injectBriefing := briefingInjection(noSandbox, inner, harness, lc.Sandbox.Briefing)
	if injectBriefing && harness.SystemContextArgs != nil {
		inner = append(inner, harness.SystemContextArgs(briefingText)...)
	}
	if len(innerArgs) > 0 {
		inner = append(inner, innerArgs...)
	}

	var argv []string
	if noSandbox {
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
			fmt.Fprintln(env.Stderr, prefix+": sandbox argv:", err)
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
		// Grant the selected harness's runtime dirs (config, state,
		// sessions) read+write — only for the selected harness, not all
		// harnesses.
		argv = injectSandboxDirs(argv, harness.SandboxDirs)
	}
	if verbose {
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
	if injectBriefing {
		// The OpenCode plugin reads this and pushes it into the system prompt;
		// Claude ignores it (it gets the briefing via the flag above).
		extra["OMAC_SANDBOX_BRIEFING"] = briefingText
		// Harnesses without a CLI flag (copilot) deliver the briefing via
		// an env-var + file mechanism (e.g. COPILOT_CUSTOM_INSTRUCTIONS_DIRS).
		if harness.BriefingEnvFunc != nil {
			for k, v := range harness.BriefingEnvFunc(briefingText, sandboxTmp) {
				extra[k] = v
			}
		}
	}

	code, err := sandbox.ExecWithReady(argv, extra, nil)
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+": exec:", err)
		return ExitSandboxAbnormal
	}
	printContinueHint(env, harness)
	return code
}

// continueHintToken returns the harness token to embed in the post-exit
// `omac continue` hint, or "" when the harness is the default (so the hint
// reads `omac continue` with no token, matching what the user typed).
// For non-default harnesses the first alias is preferred — it is the
// shortest spelling users type (e.g. "claude" over "claude-code").
func continueHintToken(h config.Harness) string {
	if h.Name == config.DefaultHarness().Name {
		return ""
	}
	for _, a := range h.Aliases {
		if a != "" {
			return " " + a
		}
	}
	return " " + h.Name
}

// printContinueHint emits a one-line `omac continue [harness]` hint to stderr
// after the inner command exits, but only when this harness supports
// continuing AND a session for this workdir is resumable. Best-effort: a
// missing harness CLI, an unreadable session store, or a harness with no
// session strategy yields no sessions → no hint (never an error, never
// blocks). Reuses session.List — the same path `omac resume` takes — so the
// hint only appears when continue would actually find a session.
func printContinueHint(env *Env, harness config.Harness) {
	if harness.Session == nil || len(harness.Session.ContinueArgs) == 0 {
		return
	}
	// session.List may shell out to the harness CLI (opencode: ~500ms) or
	// read many JSONL files (claude). Run it with a deadline so the hint
	// never blocks exit by more than hintTimeout; if it doesn't finish in
	// time we simply skip the hint (best-effort, never user-visible delay).
	type result struct {
		sessions []session.Session
		err      error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := session.List(harness, env.Workdir)
		ch <- result{s, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil || len(r.sessions) == 0 {
			return
		}
		tok := continueHintToken(harness)
		fmt.Fprintf(env.Stderr, "\nTo resume this session: omac continue%s -s %s\n", tok, r.sessions[0].ID)
	case <-time.After(hintTimeout):
		// Timed out — skip the hint rather than blocking exit.
	}
}

// hintTimeout bounds how long printContinueHint will block exit waiting for
// the harness's session list. opencode shells out to its CLI; 2s is plenty
// on a warm cache and a sane ceiling for the pathological slow case.
const hintTimeout = 2 * time.Second

// secretFromEnv returns the host value that will satisfy a keychain-absent
// secret at runtime, if any. It returns ok=true only when the secret is
// listed under sidecar.env_passthrough AND the shell exports a non-empty
// value for it. The supervisor passes env_passthrough vars to the sidecar
// at spawn time (see supervisor.buildEnv) whenever the var is present, even
// if empty; here we deliberately treat an empty value as not satisfying a
// required secret, since an empty token is no token. In that case the
// secret is genuinely usable at runtime and the preflight must not refuse
// to start. See the skainet skills' omac.yaml for the canonical "keychain
// or env_passthrough" fallback contract.
func secretFromEnv(name string, envPassthrough map[string]struct{}) (string, bool) {
	if _, ok := envPassthrough[name]; !ok {
		return "", false
	}
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return "", false
	}
	return v, true
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

func findUnregisteredSkills(workdir string, harness config.Harness, reg *registry.Registry) ([]string, error) {
	discovered, err := skillsource.Discover(workdir, harness)
	if err != nil {
		return nil, err
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
	return out, nil
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

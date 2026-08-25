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
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/facade"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandbox"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxrun"
	"github.com/tngtech/oh-my-agentic-coder/internal/session"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillconfig"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillsource"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillstate"
	"github.com/tngtech/oh-my-agentic-coder/internal/supervisor"
	"github.com/tngtech/oh-my-agentic-coder/internal/toolcache"
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
	ephemeralCache     bool
	cacheScope         string
	keepRunning        bool
	acceptSkillChanges bool
	skipSecretPattern  bool
	verbose            bool
	// autoRegisterSkills, when set, silently registers discovered
	// workdir-local skills whose required values resolve without prompting,
	// mirroring `omac serve`'s autoRegister (serve.go). Skills with a
	// required value that cannot resolve are NOT auto-registered — they
	// still surface in the unregistered list so the user is prompted for
	// values. Used by launchers (e.g. the `oco` wrapper) that run `omac
	// start` against a freshly cloned workdir whose workdir-local skills
	// are committed but whose per-workdir registry
	// (.opencode/sidecar.json) is gitignored and therefore empty on every
	// fresh checkout.
	autoRegisterSkills bool
	// audit flags (see internal/audit). auditLog overrides the config/default
	// path; noAudit disables; auditStrict makes an unwritable log fatal.
	auditLog    string
	noAudit     bool
	auditStrict bool
	// sessionID, when non-empty, selects a specific session to continue by id
	// (`omac continue -s <id>`). Empty means "most recent" (the default).
	sessionID string
	// openPorts are extra loopback ports from --open-port (repeatable),
	// typically a local webServer port for browser tests.
	openPorts []int
	// innerArgs are appended to the resolved inner command (user-supplied
	// trailing `-- args` plus any command-specific flags like --continue).
	innerArgs []string
}

// parseLaunchArgs parses the shared start-family command line: an optional
// leading positional harness token, the start flags, and trailing `-- inner
// args`. cmdName is used in usage/error text (e.g. "start", "continue").
// On success it returns ExitOK; on help, ExitOK with empty opts after printing
// usage; on a parse/usage error it prints to stderr and returns ExitMisuse.
func parseLaunchArgs(cmdName string, args []string, env *Env) (launchOpts, int) {
	fs := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	var (
		profile            = fs.String("sandbox", "", "Name of a sandbox profile from the launcher config.")
		innerCmdOverride   = fs.String("inner", "", "Override inner_cmd's executable.")
		noSandbox          = fs.Bool("no-sandbox", false, "Run inner command directly, without a sandbox (debug only).")
		ephemeralCache     = fs.Bool("ephemeral-cache", false, "Use a per-launch cache instead of the persistent cache.")
		cacheScope         = fs.String("cache-scope", "", "Persistent cache scope: global, config, or workdir. Overrides config (default: global).")
		keepRunning        = fs.Bool("keep-running", false, "Do not stop sidecars when the inner command exits.")
		acceptSkillChanges = fs.Bool("accept-skill-changes", false, "Tolerate bundle_hash drift in registered skills (proceed even if the on-disk skill differs from what was registered).")
		skipSecretPattern  = fs.Bool("skip-secret-pattern", false, "Do not enforce a secret's pattern against an env_passthrough-supplied value (escape hatch for an outdated pattern; the raw value is still passed through).")
		verbose            = fs.Bool("verbose", false, "Verbose lifecycle logging.")
		autoRegisterSkills = fs.Bool("auto-register-skills", false, "Silently register discovered workdir-local skills whose required values resolve without prompting, instead of refusing to start. Skills with unresolved required values still prompt via `omac register`.")
		auditLog           = fs.String("audit-log", "", "Path to the audit log (default: persistent central location). Overrides config.")
		noAudit            = fs.Bool("no-audit", false, "Disable the security audit trail.")
		auditStrict        = fs.Bool("audit-strict", false, "Fail-closed: abort if the audit log cannot be written.")
		sessionID          = fs.String("session", "", "Continue a specific session by id instead of the most recent one. (shorthand: -s)")
	)
	var openPorts intMultiFlag
	fs.Var(&openPorts, "open-port", "Allow the sandboxed process to bind and connect on this TCP port (repeatable). Useful for a local app/dev server the agent or its tools talk to — e.g. Playwright/Vite/Next on :3000. On Linux, Landlock cannot limit that to loopback: outbound TCP to any host on the same port is also allowed.")
	// -s is the documented shorthand for --session (opencode mirrors this
	// with `opencode -s <id>`; claude uses --resume, so its shorthand is
	// different, but `omac -s` is harness-agnostic).
	fs.StringVar(sessionID, "s", "", "Shorthand for --session.")
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintf(out, "Usage: omac %s [harness] [flags] [-- inner args...]\n", cmdName)
		fmt.Fprintf(out, "\nharness: one of %s (default: %s)\n\n",
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
		return launchOpts{}, ExitMisuse
	}
	if code, ok := parseFlags(fs, ourArgs, env); !ok {
		return launchOpts{}, code
	}
	if *ephemeralCache && *noSandbox {
		fmt.Fprintf(env.Stderr, "omac %s: --ephemeral-cache cannot be used with --no-sandbox\n", cmdName)
		return launchOpts{}, ExitMisuse
	}
	if *cacheScope != "" {
		if _, err := config.ValidateCacheScope(*cacheScope); err != nil {
			fmt.Fprintf(env.Stderr, "omac %s: %v\n", cmdName, err)
			return launchOpts{}, ExitMisuse
		}
	}
	innerArgs = append(fs.Args(), innerArgs...)
	return launchOpts{
		label:              cmdName,
		harness:            harness,
		profile:            *profile,
		innerCmdOverride:   *innerCmdOverride,
		noSandbox:          *noSandbox,
		ephemeralCache:     *ephemeralCache,
		cacheScope:         *cacheScope,
		keepRunning:        *keepRunning,
		acceptSkillChanges: *acceptSkillChanges,
		skipSecretPattern:  *skipSecretPattern,
		verbose:            *verbose,
		autoRegisterSkills: *autoRegisterSkills,
		auditLog:           *auditLog,
		noAudit:            *noAudit,
		auditStrict:        *auditStrict,
		sessionID:          *sessionID,
		openPorts:          append([]int(nil), openPorts...),
		innerArgs:          innerArgs,
	}, ExitOK
}

// checkInnerBinary verifies the resolved inner command binary is on $PATH.
// Returns ExitOK when found, ExitPrerequisiteMissing when missing, ExitOK
// when empty (defensive skip). Called by runLaunch and runServe.
//
// Validating the wrong binary is what turns a missing harness into the late,
// mislabeled "No such file or directory" from inside Seatbelt. Three inputs
// can steer the check wrong, and all three are unwrapped here: the resolved
// argv may come from a profile-pinned inner_cmd rather than the harness
// default (Harness.ResolveInnerCmd), and it may be wrapped in an
// `env NAME=VALUE ...` prefix (UnwrapEnv) or carry env flags like `-i`
// (skipped below) — each of which would otherwise make the pre-flight check
// `env` itself and report ExitOK regardless of whether the real harness is
// installed.
func checkInnerBinary(innerCmd []string, prefix string, env *Env) int {
	if len(innerCmd) == 0 || innerCmd[0] == "" {
		return ExitOK
	}
	cmd := sandboxrun.UnwrapEnv(innerCmd)
	for len(cmd) > 0 && strings.HasPrefix(cmd[0], "-") {
		cmd = cmd[1:]
	}
	if len(cmd) == 0 || cmd[0] == "" {
		return ExitOK
	}
	if _, err := exec.LookPath(cmd[0]); err != nil {
		fmt.Fprintf(env.Stderr, "%s: harness binary %q not found on $PATH; install it or pass --inner-cmd <path>\n", prefix, cmd[0])
		return ExitPrerequisiteMissing
	}
	return ExitOK
}

func runStart(args []string, env *Env) int {
	opts, code := parseLaunchArgs("start", args, env)
	if code != ExitOK {
		return code
	}
	if opts.label == "" {
		return ExitOK // --help printed usage
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
	autoRegisterSkills := opts.autoRegisterSkills

	// Diagnostics are prefixed with the invoking subcommand so a failure
	// surfaced through `omac continue`/`omac resume` is not mislabeled.
	label := opts.label
	if label == "" {
		label = "start"
	}
	prefix := "omac " + label

	// Reject the audit misuse combination up front (before any work).
	if opts.noAudit && opts.auditStrict {
		fmt.Fprintln(env.Stderr, prefix+": --no-audit cannot be combined with --audit-strict")
		return ExitMisuse
	}

	// 1. Load launcher config.
	lc, cfgPath, err := config.LoadLauncher(env.Workdir)
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+": launcher config:", err)
		return ExitConfigInvalid
	}
	if verbose && cfgPath != "" {
		fmt.Fprintf(env.Stderr, "[verbose] loaded launcher config: %s\n", cfgPath)
	}
	// One resolved sandbox plan for the whole launch: the launcher profile
	// (templated argv) plus, for omac's native backend, its policy profile
	// (grant JSON). Everything downstream reads the plan instead of
	// re-resolving a bare name — see internal/cli/sandboxplan.go.
	plan, planErr := resolveSandboxPlan(lc, profile)
	if planErr != nil && !noSandbox {
		fmt.Fprintln(env.Stderr, prefix+":", planErr)
		return ExitConfigInvalid
	}
	profName := plan.Name
	prof := plan.Launcher

	// 1b. Pre-flight: inner harness binary must be on $PATH. Checked on the
	//     resolved argv (profile inner_cmd, else harness default) — the same
	//     argv step 8 hands to the sandbox. An explicit --inner skips: that
	//     points at an exact binary, which is an escape hatch; sandboxrun
	//     warns non-fatally if it cannot resolve it either.
	if innerCmdOverride == "" {
		if code := checkInnerBinary(harness.ResolveInnerCmd(prof.InnerCmd, ""), prefix, env); code != ExitOK {
			return code
		}
	}

	// 1c. Pre-flight: codex on macOS is incompatible with the omac Seatbelt
	//     sandbox — its Rust HTTP client disconnects mid-stream even with
	//     network=open. Fail loud rather than hang. --no-sandbox disables
	//     the entire omac sandbox (fs/network/secret isolation) so it is
	//     not a safe workaround. See issue #48.
	if runtime.GOOS == "darwin" && harness.Name == "codex" && !noSandbox {
		fmt.Fprintf(env.Stderr, "%s: codex is incompatible with the macOS Seatbelt sandbox "+
			"(its HTTP client disconnects mid-stream even with network=open). "+
			"codex on macOS is not supported under the omac sandbox; use a "+
			"different harness (opencode, claude-code, copilot) or run codex "+
			"on Linux (bwrap works). --no-sandbox is not a safe workaround — "+
			"it disables the entire omac sandbox (filesystem, network, secret "+
			"isolation). See issue #48.\n", prefix)
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

	// Load the config stores once before auto-registration eligibility and
	// runtime resolution. A broken store is a launch-wide I/O failure, not a
	// per-skill condition that can be deferred to registration guidance.
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
	configStore := skillstate.MergeConfig(globalCfg, workdirCfg)

	// 2a-bis. Optional auto-registration of workdir-local skills whose
	//         required values resolve without prompting. Mirrors `omac
	//         serve`'s autoRegister (serve.go): the workdir-local skill
	//         source roots are scanned, and every discovered skill absent
	//         from the registry whose omac.yaml sidecar has all required
	//         values resolved is registered silently. Skills with
	//         unresolved required values are left untouched so the
	//         findUnregisteredSkills gate below still surfaces them and
	//         prompts the user.
	//
	//         This is the path launchers like the `oco` wrapper use to
	//         avoid forcing the user to run `omac register` on every
	//         fresh workdir whose committed skills ship with a
	//         gitignored per-workdir registry (the documented default
	//         for omac's own repo and any repo that ships example
	//         skills).
	if autoRegisterSkills {
		registered, errs, err := startAutoRegisterWorkdirSkills(env, harness, reg, configStore, skipSecretPattern)
		if err != nil {
			fmt.Fprintln(env.Stderr, prefix+": keychain:", err)
			return ExitKeychainError
		}
		for _, r := range registered {
			fmt.Fprintf(env.Stderr, "[ok] auto-registered skill %s (no prompting required)\n", r)
		}
		for _, e := range errs {
			fmt.Fprintln(env.Stderr, prefix+": auto-register:", e)
		}
		if len(registered) > 0 {
			// Reload the workdir registry + re-merge so the gate and
			// the rest of start see the freshly-registered skills.
			workdirReg, err = registry.Load(env.Workdir)
			if err != nil {
				fmt.Fprintln(env.Stderr, prefix+": registry:", err)
				return ExitIOError
			}
			workdirReg = filterRegistryByHarness(workdirReg, env.Workdir, harness)
			reg = mergeRegistries(globalReg, workdirReg)
		}
	}

	// 2a-ter. One-time grandfathering of the already-registered skills
	//         into the host-only approval store (see internal/skilltrust
	//         and skill_approval.go). Only fires the first time (before
	//         the store exists) so upgrading omac never newly breaks a
	//         working setup; thereafter a skill must be approved
	//         explicitly by an out-of-sandbox `omac register`.
	if firstApprovalUpgrade() {
		n, merr := grandfatherOnce(grandfatherScope{workdir: env.Workdir, reg: reg})
		if merr != nil {
			fmt.Fprintln(env.Stderr, prefix+": approval store (non-fatal):", merr)
		}
		if n > 0 {
			fmt.Fprintf(env.Stderr, "%s: migrated %d registered skill(s) to the host-only approval store; "+
				"new skills now require `omac register` on the host to spawn\n", prefix, n)
		}
	}

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
	// The rule — which rungs of the precedence ladder satisfy a declared
	// secret or config field, and what counts as unready — lives in
	// internal/skillstate, shared with serve, live reload, doctor and `config
	// show`. It used to be reimplemented on each of those paths and the copies
	// drifted silently (issue #174). start's only job here is presentation:
	// accumulate problems, then render one consolidated refusal.
	//
	// We accumulate rather than returning on the first problem. The user's
	// complaint when this returned early was that fixing skill A revealed
	// skill B revealed skill C, etc. — N invocations to fix N problems.
	//
	// Secret values are eagerly fetched from the keychain even when we may not
	// end up using them; the deferred Zero wipes them on every path out.
	resolver := skillstate.New(skillstate.Options{
		// Workdir-scoped, with an unscoped fallback inside GetWithFallback, so
		// secrets stored by a serve-aware register (scoped per workdir) and
		// legacy/global ones (unscoped) both resolve. See
		// docs/MULTI_DIR_DESKTOP.md §4.3.
		Scope:             keychain.WorkdirID(env.Workdir),
		AcceptBundleDrift: acceptSkillChanges,
		SkipSecretPattern: skipSecretPattern,
	})
	// resolved holds every Armed we obtained, including skills later refused
	// by the approval gate, purely so the deferred Zero wipes all of it. Never
	// reassign it — the approval gate below filters into a separate slice.
	resolved := make([]skillstate.Armed, 0, len(reg.Registered))
	defer func() {
		for i := range resolved {
			resolved[i].Zero()
		}
	}()

	var problems []skillstate.Problem
	for _, e := range reg.Registered {
		absDir := e.SkillDir
		if !filepath.IsAbs(absDir) {
			absDir = filepath.Join(env.Workdir, absDir)
		}
		a, probs := resolver.Load(e, absDir, configStore)
		resolved = append(resolved, a)
		problems = append(problems, probs...)

		// A bundle-hash I/O failure is launch-wide rather than a per-skill
		// diagnostic: nothing useful can be said about an unreadable skill
		// directory. Under --accept-skill-changes the hash is allowed to fail
		// (step 5a re-derives it and refuses), since that flag's whole point
		// is not to abort on skill-dir state.
		if a.BundleErr != nil && !acceptSkillChanges {
			fmt.Fprintln(env.Stderr, prefix+": bundle hash:", a.BundleErr)
			return ExitIOError
		}
	}

	// If anything went wrong above, render one consolidated report
	// (grouped by problem class) and abort.
	if len(problems) > 0 {
		return renderSkillRefusal(env.Stderr, prefix, problems)
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

	// 4a. Construct the audit trail BEFORE launching the inner command.
	//
	// The log lives at a persistent, central path (audit.DefaultPath) that
	// survives restarts — NOT the per-run rtDir. Under --audit-strict a
	// failure to open/write is fatal: fatalTeardown runs the same cleanup
	// the deferred teardowns do and exits non-zero. We collect the resolved
	// secret VALUES to seed the redactor (belt-and-suspenders; secret names
	// are always logged, values never).
	var secretValues []string
	for _, s := range resolved {
		for _, sec := range s.Secrets {
			secretValues = append(secretValues, sec.ExposeString())
		}
	}
	// fatalTeardown is assigned its real body once sup/facade/control exist;
	// until then a strict write failure (only possible after those are up)
	// cannot occur, but we default it to a plain exit for safety.
	var fatalTeardown func(error)
	auditCfg, misuse := resolveAuditConfig(lc.Audit, auditFlags{
		logPath: opts.auditLog, disable: opts.noAudit, strict: opts.auditStrict,
	}, audit.ModeStart, env.Version, secretValues, func(err error) {
		if fatalTeardown != nil {
			fatalTeardown(err)
		}
	})
	if misuse != "" {
		fmt.Fprintln(env.Stderr, prefix+": "+misuse)
		return ExitMisuse
	}
	auditor, aerr := newAuditor(env, auditCfg)
	if aerr != nil {
		fmt.Fprintln(env.Stderr, prefix+": audit:", aerr)
		return ExitIOError
	}
	defer auditor.Close()

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
	scope, err := resolveCacheScope(lc.Cache, opts.cacheScope)
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+": cache:", err)
		return ExitConfigInvalid
	}
	cacheScope, err := prepareLaunchCache(noSandbox, opts.ephemeralCache, scope, env.Workdir, cfgPath, sandboxTmp)
	if err != nil {
		if opts.ephemeralCache {
			fmt.Fprintln(env.Stderr, prefix+": cache:", err)
		} else {
			fmt.Fprintln(env.Stderr, prefix+": cache:", err,
				"retry with --ephemeral-cache to bypass persistent cache setup")
		}
		return ExitIOError
	}
	defer cacheScope.Close()
	if verbose && cacheScope != nil {
		fmt.Fprintf(env.Stderr, "[verbose] cache mode=%s path=%s\n", cacheScope.Mode, cacheScope.Dir)
	}

	// 5. Spawn sidecars.
	sup := supervisor.New(lc.Facade.BaseEnvPassthrough, auditor, skillSpawnAuthorizer)
	defer func() {
		if !keepRunning {
			sup.ShutdownAll(5 * time.Second)
		}
	}()

	// 5a. Spawn-approval gate. A sidecar runs UNSANDBOXED, so only skills
	//     whose current on-disk code is host-approved (see
	//     internal/skilltrust) may start — and they run from the immutable
	//     approval snapshot, not the agent-writable workdir. Unapproved
	//     skills are mounted as broken routes with the remedy, never spawned.
	var refusedRoutes []facade.Route
	approved := make([]skillstate.Armed, 0, len(resolved))
	for _, a := range resolved {
		snap, refusal := approvedSpawnDir(a.Entry.Name, a.AbsDir, a.Bundle)
		if refusal != nil {
			refusedRoutes = append(refusedRoutes, brokenApprovalRoute(
				a.Mount, a.Entry.Name, a.AbsDir, refusal))
			fmt.Fprintf(env.Stderr, "%s: %s\n", prefix, refusalNotice(refusal))
			continue
		}
		a.AbsDir = snap // spawn (and serve SKILL.md) from the frozen snapshot
		approved = append(approved, a)
	}

	specs := make([]supervisor.SidecarSpec, 0, len(approved))
	for _, s := range approved {
		health := config.HealthSpec{}
		if s.Meta.Sidecar.Health != nil {
			health = *s.Meta.Sidecar.Health
		}
		specs = append(specs, supervisor.SidecarSpec{
			Name:             s.Entry.Name,
			SkillDir:         s.AbsDir,
			Command:          s.Meta.Sidecar.Command,
			EnvPassthrough:   s.Meta.Sidecar.EnvPassthrough,
			Secrets:          s.Secrets,
			Config:           s.Config,
			Health:           health.Defaults(),
			LogPath:          filepath.Join(rtDir, "logs", s.Entry.Name+".log"),
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
		mount := approved[i].Mount
		var maxBody int64
		var idle time.Duration
		if lim := approved[i].Meta.Sidecar.Limits; lim != nil {
			maxBody = lim.MaxBodyBytes
			idle = time.Duration(lim.IdleTimeoutSecs) * time.Second
		}
		routes = append(routes, facade.Route{
			Mount:        mount,
			UpstreamPort: r.Port,
			MaxBodyBytes: maxBody,
			IdleTimeout:  idle,
			Skill:        r.Name,
			SkillDir:     approved[i].AbsDir,
		})
		mounts = append(mounts, mount)
	}
	// Mount unapproved skills as broken routes so a probe gets an
	// actionable 502 instead of a silent 404 (and they never spawn).
	routes = append(routes, refusedRoutes...)

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
	f.SetAuditor(auditor)
	// `omac start` has no --learn (serve-only), so the protected set is
	// always the profile's.
	wireFacadeSandbox(f, noSandbox, false, plan, func(format string, args ...any) {
		fmt.Fprintf(env.Stderr, prefix+": "+format+"\n", args...)
	})
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
		skipSecretPattern: skipSecretPattern,
		mounted:           map[string]string{},
	}
	for _, a := range approved {
		reloader.markMounted(a.Entry.Name, a.Mount)
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
	briefingText, injectBriefing := briefingInjection(noSandbox, inner, harness, lc.Sandbox.Briefing, cacheScope)
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
		if cacheScope != nil {
			argv = injectSandboxFlag(argv, "--allow", cacheScope.Dir)
		}
		// Forward the selected harness's auth env vars through the
		// default profile's restrictive allow_vars filter — only for the
		// selected harness.
		argv = forwardHarnessEnv(env, argv, harness, plan)
		// User --open-port grants (e.g. local Playwright webServer). Additive
		// on top of the profile; no-op on non-native backends (with a warning).
		argv = injectUserOpenPorts(env, argv, opts.openPorts, prof)
		// Pass the resolved audit path down to `omac sandbox run` so the
		// network-filter subprocess appends net.decision events to the
		// same persistent log. Inherit the parent's run_id + mode so the
		// subprocess's events correlate with the parent's (same run_id,
		// continuing seq).
		if ap := audit.EffectivePath(auditCfg); ap != "" {
			argv = injectSandboxFlag(argv, "--audit-log", ap)
			argv = injectSandboxFlag(argv, "--audit-run-id", auditor.RunID())
			argv = injectSandboxFlag(argv, "--audit-mode", string(auditCfg.Mode))
		}
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
	if cacheScope != nil {
		extra["OMAC_CACHE_DIR"] = cacheScope.Dir
		extra["OMAC_CACHE_MODE"] = string(cacheScope.Mode)
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
		// Harnesses that load always-on context only from files in the
		// workspace tree (codewhale) get the briefing written into the workdir;
		// omac removes it (and any empty dirs it created) when the run exits.
		if harness.BriefingFileFunc != nil {
			if rel, werr := harness.BriefingFileFunc(briefingText, env.Workdir); werr != nil {
				fmt.Fprintln(env.Stderr, prefix+": briefing file:", werr)
			} else if rel != "" {
				// Keep git from committing the briefing (persists across a
				// SIGKILL); remove the file itself on a clean exit.
				gitExcludeBriefing(env.Workdir, rel)
				defer removeBriefingFile(filepath.Join(env.Workdir, rel))
			}
		}
	}

	// Now that the supervisor, facade, and control plane exist, give the
	// strict-mode fatal handler a real teardown: stop sidecars, close the
	// facade + control plane, then exit non-zero. A strict-mode audit write
	// failure mid-run lands here.
	fatalTeardown = func(ferr error) {
		fmt.Fprintln(env.Stderr, prefix+": audit (strict) write failed, aborting:", ferr)
		if !keepRunning {
			sup.ShutdownAll(5 * time.Second)
		}
		_ = f.Close()
		closeControl()
		os.Exit(ExitIOError)
	}

	// session.start is emitted just before the inner command launches. In
	// strict mode a failure to write it invokes fatalTeardown above.
	sandboxed := !noSandbox
	sandboxBackend := ""
	if sandboxed {
		sandboxBackend = profName
	}
	auditor.Emit(audit.SessionStart(env.Version, harness.Name, profName, sandboxBackend))
	auditor.Emit(audit.InnerExec(argv, profName, sandboxed))

	// The post-exit hint needs the id of the session this run created. opencode
	// self-reports it via the control plane (the omac plugin POSTs
	// /__omac__/session), so the pre-exec enumeration is skipped for it:
	// `opencode session list` runs before the inner launches and can block
	// indefinitely. Other harnesses enumerate here — cheap on-disk reads — to
	// tell a fresh session apart from a sibling active in the same workdir (#145).
	selfReportsSession := harness.Session != nil && harness.Session.ListKind == config.SessionListOpenCodeCLI
	var priorSessions map[string]struct{}
	if harness.Session != nil && len(harness.Session.ContinueArgs) > 0 && !selfReportsSession {
		// Bounded: the snapshot only sharpens the resume hint, so it must never
		// delay the inner launch. `omac start` waits at most hintTimeout for it.
		priorSessions = boundedKnownIDs(func() map[string]struct{} {
			return session.KnownIDs(harness, env.Workdir)
		}, hintTimeout)
	}

	code, err := sandbox.ExecWithReady(argv, extra, nil)
	auditor.Emit(audit.SessionStop(code))
	if err != nil {
		fmt.Fprintln(env.Stderr, prefix+": exec:", err)
		return ExitSandboxAbnormal
	}
	resumed := opts.sessionID
	if resumed == "" && selfReportsSession {
		resumed = reloader.reportedSession()
	}
	printContinueHint(env, harness, resumed, priorSessions)
	return code
}

func prepareLaunchCache(noSandbox, ephemeral bool, scope config.CacheScope, workdir, cfgPath, sandboxTmp string) (*toolcache.Scope, error) {
	if noSandbox {
		return nil, nil
	}
	if ephemeral {
		return toolcache.PrepareEphemeral(sandboxTmp)
	}
	switch scope {
	case config.CacheScopeWorkdir:
		return toolcache.PreparePersistent(toolcache.DomainWorkdir, workdir)
	case config.CacheScopeConfig:
		if cfgPath != "" {
			return toolcache.PreparePersistent(toolcache.DomainConfig, cfgPath)
		}
		return toolcache.PrepareShared()
	default:
		return toolcache.PrepareShared()
	}
}

// resolveCacheScope merges the config's cache scope with an optional
// --cache-scope flag override (precedence: flag > config > default global).
func resolveCacheScope(cfg config.CacheConfig, override string) (config.CacheScope, error) {
	if override != "" {
		return config.ValidateCacheScope(override)
	}
	return cfg.Resolve()
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
//
// resumedID is the session this run is known to have re-entered (omac continue
// -s / omac resume), or "" for a fresh session. prior is the set of session
// ids that existed before launch (see hintSessionID for how both steer the
// selection).
func printContinueHint(env *Env, harness config.Harness, resumedID string, prior map[string]struct{}) {
	if harness.Session == nil || len(harness.Session.ContinueArgs) == 0 {
		return
	}
	// When the session that ran is already known — an explicit resume, or a
	// harness that self-reported its id via the control plane — advertise it
	// directly. No enumeration needed (and for opencode none is attempted, so a
	// slow/blocking `session list` never runs).
	if resumedID != "" {
		fmt.Fprintf(env.Stderr, "\nTo resume this session: omac continue%s -s %s\n",
			continueHintToken(harness), resumedID)
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
		if r.err != nil {
			return
		}
		id, ok := hintSessionID(r.sessions, resumedID, prior)
		if !ok {
			return
		}
		tok := continueHintToken(harness)
		fmt.Fprintf(env.Stderr, "\nTo resume this session: omac continue%s -s %s\n", tok, id)
	case <-time.After(hintTimeout):
		// Timed out — skip the hint rather than blocking exit.
	}
}

// hintSessionID picks the session id to advertise in the continue hint.
//
// When this run resumed a known session (omac continue -s / omac resume),
// resumedID is advertised verbatim — it is exactly the session that ran.
// Otherwise the run created a fresh session: the most-recent session absent
// from prior (the snapshot of ids taken before launch) is that new session, so
// a sibling session that stayed active in the same workdir is never advertised
// (issue #141). When nothing is new — the snapshot was unavailable, or the
// harness reused an id — it falls back to the most-recent session, preserving
// the previous best-effort behavior.
//
// sessions is newest-first, as session.List guarantees. Returns ok=false only
// when there is nothing resumable. Kept pure so the selection is unit-testable
// without printContinueHint's goroutine and timeout.
func hintSessionID(sessions []session.Session, resumedID string, prior map[string]struct{}) (string, bool) {
	if resumedID != "" {
		return resumedID, true
	}
	for _, s := range sessions {
		if s.ID == "" {
			continue
		}
		if _, seen := prior[s.ID]; !seen {
			return s.ID, true
		}
	}
	if len(sessions) == 0 || sessions[0].ID == "" {
		return "", false
	}
	return sessions[0].ID, true
}

// hintTimeout bounds how long printContinueHint will block exit waiting for
// the harness's session list. opencode shells out to its CLI; 2s is plenty
// on a warm cache and a sane ceiling for the pathological slow case.
const hintTimeout = 2 * time.Second

// boundedKnownIDs runs the prior-session enumeration but never waits longer
// than d for it. The snapshot only sharpens the post-exit resume hint (#145),
// so a slow or stuck session store must never stall the inner launch: on
// timeout we return an empty snapshot and carry on (best-effort, matching
// printContinueHint). This is the structural guarantee that `omac start` never
// waits indefinitely on session listing — the regression guard for the
// opencode-session-list hang. enumerate is injected so the bound is testable
// without real session I/O.
func boundedKnownIDs(enumerate func() map[string]struct{}, d time.Duration) map[string]struct{} {
	ch := make(chan map[string]struct{}, 1)
	go func() { ch <- enumerate() }()
	select {
	case ids := <-ch:
		return ids
	case <-time.After(d):
		return nil
	}
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

// startAutoRegisterWorkdirSkills silently registers every discovered
// workdir-local skill (Kind == "workdir") that is absent from reg AND
// whose omac.yaml sidecar has every required value resolved without
// prompting. Mirrors `omac serve`'s autoRegister (serve.go), but
// scoped to values that are resolved at launch so a skill with a missing
// keychain secret or config value still surfaces through
// findUnregisteredSkills and prompts the user — auto-registration never
// silently skips a registration prompt that would otherwise have asked
// for a value.
//
// Returns the names of skills that were registered (sorted), per-skill
// diagnostics for metadata/registration failures, and a fatal keychain read
// error. Metadata errors remain diagnostic so one malformed skill does not
// hide actionable results for other skills.
func startAutoRegisterWorkdirSkills(env *Env, harness config.Harness, reg *registry.Registry, configStore *skillconfig.Store, skipSecretPattern bool) ([]string, []string, error) {
	discovered, err := skillsource.Discover(env.Workdir, harness)
	if err != nil {
		return nil, []string{fmt.Sprintf("scan skills: %v", err)}, nil
	}
	registered := map[string]struct{}{}
	for _, e := range reg.Registered {
		registered[e.Name] = struct{}{}
	}
	var done []string
	var errs []string
	for _, ent := range discovered {
		if ent.Kind != "workdir" {
			continue // user-global skills are registered once, globally
		}
		if _, ok := registered[ent.Name]; ok {
			continue
		}
		meta, err := config.LoadMeta(filepath.Join(ent.Dir, config.MetaFileName))
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", ent.Name, err))
			continue
		}
		eligible, err := skillEligibleForAutoRegister(env.Workdir, ent.Name, meta, configStore, skipSecretPattern)
		if err != nil {
			return done, errs, err
		}
		if !eligible {
			// A required secret/field is the user's signal to run
			// `omac register` themselves; we don't auto-register so
			// the findUnregisteredSkills gate below still surfaces it.
			continue
		}
		if _, err := startAutoRegisterOne(env.Workdir, ent); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", ent.Name, err))
			continue
		}
		done = append(done, ent.Name)
	}
	sort.Strings(done)
	return done, errs, nil
}

// skillEligibleForAutoRegister reports whether parsed metadata is eligible
// for silent auto-registration: it must have a sidecar block, and every
// declared secret/config field must be satisfiable at start time WITHOUT
// prompting the user.
//
// "Satisfiable without prompting" is by definition whatever runLaunch's
// preflight would accept, so this asks internal/skillstate rather than
// restating the precedence ladder — before #174 it was a sixth hand-rolled
// copy of it, in the same file as the first.
//
// A skill with at least one required-and-unsatisfiable secret/field is
// NOT eligible — the findUnregisteredSkills gate surfaces it so the
// user is prompted for the missing value.
func skillEligibleForAutoRegister(workdir, skillName string, m *config.Meta, configStore *skillconfig.Store, skipSecretPattern bool) (eligible bool, err error) {
	if m.Sidecar == nil {
		return false, nil
	}
	// SkipBundleHash: the skill is by definition not registered yet, so there
	// is no recorded hash for drift to be measured against.
	r := skillstate.New(skillstate.Options{
		Scope:             keychain.WorkdirID(workdir),
		SkipSecretPattern: skipSecretPattern,
		SkipBundleHash:    true,
	})
	armed, problems := r.Resolve(m, registry.Entry{Name: skillName}, "", configStore)
	armed.Zero()

	// A dead keychain is deliberately "not eligible" rather than an error:
	// --auto-register-skills on a headless box has always just declined to
	// auto-register (GetWithFallback reported the missing backend as
	// ErrNotFound), and turning that into a hard ExitKeychainError would break
	// launches that work today. Only an opaque keychain failure — the case that
	// was already fatal here — still propagates.
	if p := skillstate.First(problems, skillstate.KeychainUnavailable); p != nil {
		if !errors.Is(p.Cause, keychain.ErrNotFound) {
			return false, p.Cause
		}
		return false, nil
	}
	for _, p := range problems {
		// An OPTIONAL secret whose exported value fails its pattern must not
		// block registration: eligibility asks whether the skill's REQUIRED
		// values resolve without prompting, and registering anyway lets start's
		// preflight report the malformed value with its --skip-secret-pattern
		// hint — a better outcome than the findUnregisteredSkills gate's
		// "run omac register", which would not fix it either.
		if p.Kind == skillstate.InvalidSecret && !secretRequired(m, p.Field) {
			continue
		}
		return false, nil
	}
	return true, nil
}

// secretRequired reports whether m declares the named secret as required.
// Unknown names are treated as required (fail closed).
func secretRequired(m *config.Meta, name string) bool {
	for _, sp := range m.Sidecar.Secrets {
		if sp.Name == name {
			return sp.IsRequired()
		}
	}
	return true
}

// startAutoRegisterOne writes a registry entry for a discovered
// workdir-local skill without prompting, mirroring serve.go's autoRegister.
func startAutoRegisterOne(workdir string, ent skillsource.Entry) (*registry.Entry, error) {
	bundle, err := config.BundleHash(ent.Dir)
	if err != nil {
		return nil, err
	}
	m, err := config.LoadMeta(filepath.Join(ent.Dir, config.MetaFileName))
	if err != nil {
		return nil, err
	}
	declared := make([]string, 0, len(m.Sidecar.Secrets))
	for _, sp := range m.Sidecar.Secrets {
		declared = append(declared, sp.Name)
	}
	var out *registry.Entry
	err = registry.WithLock(workdir, func() error {
		reg, err := registry.Load(workdir)
		if err != nil {
			return err
		}
		stored := ent.Dir
		if rel, rerr := filepath.Rel(workdir, ent.Dir); rerr == nil {
			stored = rel
		}
		reg.Upsert(registry.Entry{
			Name:                ent.Name,
			SkillDir:            stored,
			BundleHash:          bundle,
			RegisteredAt:        time.Now().UTC(),
			DeclaredSecretNames: declared,
		})
		if err := registry.Save(workdir, reg); err != nil {
			return err
		}
		e, _ := reg.Find(ent.Name)
		out = e
		return nil
	})
	if err != nil {
		return nil, err
	}
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

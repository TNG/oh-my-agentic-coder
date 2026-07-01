package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/facade"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/opencodestate"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandbox"
	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillconfig"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillsource"
	"github.com/tngtech/oh-my-agentic-coder/internal/supervisor"
)

// runServe implements `omac serve` — the long-lived, multi-directory mode
// behind OpenCode Desktop. It wraps `opencode serve` (the inner command),
// keeps the facade + supervisor mutable for the process lifetime, and
// activates a directory's skills lazily on request. See
// docs/MULTI_DIR_DESKTOP.md.
//
// This implementation focuses on the omac-side control/data plane and is
// directly drivable over the control-plane HTTP API (so it can be tested
// without OpenCode). When --workdir is given, that one directory is
// auto-activated at cold start (§5.5).
func runServe(args []string, env *Env) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	var (
		workdir          = fs.String("workdir", "", "Auto-activate this one directory at cold start (single-dir convenience, §5.5).")
		controlAddr      = fs.String("control-addr", "127.0.0.1:0", "Bind address for the control-plane HTTP server.")
		acceptChanges    = fs.Bool("accept-skill-changes", false, "Tolerate bundle_hash drift in registered skills.")
		profile          = fs.String("sandbox", "", "Name of a sandbox profile from the launcher config.")
		innerCmdOverride = fs.String("inner", "", "Override inner_cmd's executable (default: opencode serve).")
		noSandbox        = fs.Bool("no-sandbox", false, "Run the inner command directly, without a sandbox (debug only).")
		noInner          = fs.Bool("no-inner", false, "Do not launch any inner command; run the control plane only (testing/headless).")
		verbose          = fs.Bool("verbose", false, "Verbose lifecycle logging.")
		forDesktop       = fs.Bool("for-opencode-desktop", false, "Grant every project worktree from the local OpenCode state (Desktop projects) read+write in the sandbox.")
		learn            = fs.Bool("learn", false, "Learn mode: do not restrict filesystem access; record folders used and offer to add them to the sandbox profile at session end.")
	)
	var roots multiFlag
	fs.Var(&roots, "root", "Pre-declared root directory under which projects may be activated (§5.4 Option B). Repeatable. Empty = allow any directory.")
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac serve [harness] [flags] [-- inner args...]")
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
	// `omac serve claude`) before flag parsing.
	harness, ourArgs, err := splitHarnessToken(ourArgs)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac serve:", err)
		return ExitMisuse
	}
	if err := fs.Parse(reorderFlagsFirst(ourArgs)); err != nil {
		return ExitMisuse
	}
	innerArgs = append(fs.Args(), innerArgs...)

	lc, _, err := config.LoadLauncher(env.Workdir)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac serve: launcher config:", err)
		return ExitConfigInvalid
	}
	profName := *profile
	if profName == "" {
		profName = lc.Sandbox.DefaultProfile
	}
	prof, profOK := lc.Sandbox.Profiles[profName]
	if !profOK && !*noSandbox && !*noInner {
		fmt.Fprintf(env.Stderr, "omac serve: unknown sandbox profile %q\n", profName)
		return ExitConfigInvalid
	}

	// Pre-flight: inner harness binary must be on $PATH (unless --no-inner
	// or --inner override).
	if !*noInner && *innerCmdOverride == "" {
		if code := checkInnerBinary(harness, "omac serve", env); code != ExitOK {
			return code
		}
	}

	// Normalize pre-declared roots to absolute paths (§5.4 Option B).
	absRoots := make([]string, 0, len(roots))
	for _, r := range roots {
		ar, err := filepath.Abs(r)
		if err != nil {
			fmt.Fprintln(env.Stderr, "omac serve: --root:", err)
			return ExitMisuse
		}
		absRoots = append(absRoots, ar)
	}

	rtDir, err := createRuntimeDirServe(env.Workdir)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac serve: runtime dir:", err)
		return ExitIOError
	}
	socketPath := filepath.Join(rtDir, "bridge.sock")

	// Per-session sandbox temp dir exported as TMPDIR; the nono profile
	// grants RW on it via {{tmpdir}} so Bun-built harnesses (opencode) can
	// extract their embedded runtime. Removed on exit. See start.go.
	sandboxTmp, err := os.MkdirTemp("", "omac-sandbox-tmp-")
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac serve: sandbox temp dir:", err)
		return ExitIOError
	}
	defer os.RemoveAll(sandboxTmp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sup := supervisor.New(lc.Facade.BaseEnvPassthrough)
	defer sup.ShutdownAll(5 * time.Second)

	f := facade.New(
		socketPath,
		"127.0.0.1:0",
		nil, // empty initial route table
		lc.Facade.MaxBodyBytes,
		time.Duration(lc.Facade.IdleTimeoutSecs)*time.Second,
		filepath.Join(rtDir, "logs", "facade.log"),
		env.Version,
	)
	if err := f.Start(ctx); err != nil {
		fmt.Fprintln(env.Stderr, "omac serve: facade:", err)
		return ExitIOError
	}
	defer f.Close()

	srv := &serveServer{
		env:           env,
		harness:       harness,
		facade:        f,
		sup:           sup,
		ctx:           ctx,
		rtDir:         rtDir,
		sandboxTmp:    sandboxTmp,
		socketPath:    socketPath,
		tcpPort:       f.TCPPort(),
		acceptChanges: *acceptChanges,
		verbose:       *verbose,
		roots:         absRoots,
		dirs:          map[string]*dirState{},
		byToken:       map[string]*dirState{},
		global:        map[string]*skillRoute{},
	}

	// Cold start: global skills are a fixed, known set, so — unlike the lazy
	// workdir-local skills — we validate them up front and refuse to start on
	// drift, mirroring `omac start`. Workdir skills stay lazy/auto-registered.
	if code := srv.checkGlobalDrift(); code != ExitOK {
		return code
	}

	// Activate user-global skills once, under /__global__/ (§5.1).
	if err := srv.activateGlobals(); err != nil {
		fmt.Fprintln(env.Stderr, "omac serve: activate globals:", err)
		return ExitIOError
	}

	// --workdir convenience: pre-activate exactly one directory (§5.5).
	if *workdir != "" {
		abs, err := filepath.Abs(*workdir)
		if err != nil {
			fmt.Fprintln(env.Stderr, "omac serve: --workdir:", err)
			return ExitMisuse
		}
		if _, err := srv.activate(abs); err != nil {
			fmt.Fprintln(env.Stderr, "omac serve: pre-activate", abs, ":", err)
			return ExitIOError
		}
	}

	// Control-plane HTTP server (host-side; distinct from the facade).
	cln, err := net.Listen("tcp", *controlAddr)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac serve: control listen:", err)
		return ExitIOError
	}
	controlURL := fmt.Sprintf("http://%s", cln.Addr().String())
	srv.controlBase = controlURL
	// Publish the control URL so other omac CLI invocations (register,
	// deregister, secrets, config) can notify this running serve to reload a
	// directory after they change on-disk state. Best-effort.
	if err := writeControlInfo(controlURL); err != nil && *verbose {
		fmt.Fprintln(env.Stderr, "[verbose] could not write control-info file:", err)
	}
	defer removeControlInfo()
	httpSrv := &http.Server{Handler: srv.controlMux()}
	go func() {
		if err := httpSrv.Serve(cln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(env.Stderr, "omac serve: control server:", err)
		}
	}()
	defer httpSrv.Close()

	if *verbose {
		fmt.Fprintf(env.Stderr, "[verbose] facade tcp=127.0.0.1:%d socket=%s\n", srv.tcpPort, socketPath)
		fmt.Fprintf(env.Stderr, "[verbose] control plane: %s\n", controlURL)
		if len(absRoots) > 0 {
			fmt.Fprintf(env.Stderr, "[verbose] allowed roots: %v\n", absRoots)
		}
	}
	fmt.Fprintf(env.Stdout, "omac serve: control plane on %s; facade on 127.0.0.1:%d\n", controlURL, srv.tcpPort)

	// --no-inner: run the control plane only (testing / headless drivers).
	if *noInner {
		fmt.Fprintf(env.Stdout, "OMAC_CONTROL_BASE=%s\n", controlURL)
		<-ctx.Done()
		return ExitOK
	}

	// Warn (once) when the harness's client-side bridge plugin is missing
	// from this workdir: without it, OpenCode Desktop won't talk to the
	// control plane and skills won't surface. The user may continue, abort,
	// or silence the warning permanently. Aborting returns before any inner
	// command is launched.
	if proceed := warnPluginMissing(env, harness); !proceed {
		fmt.Fprintln(env.Stderr, "omac serve: aborted by user (plugin not installed)")
		return ExitOK
	}

	// Idempotently provision omac's built-in skills for this harness so they
	// are available with no separate setup step. Quiet when already current;
	// never blocks the launch.
	ensureBuiltinSkills(env, harness)

	// Likewise provision the OpenCode bridge plugin that carries the briefing.
	ensureOpenCodePlugin(env, harness)

	// Build the inner argv. serve mode runs the selected harness's *server*
	// form: the inner executable is resolved from the profile (or --inner, or
	// the harness default), then the harness's ServerLaunch convention is
	// applied — e.g. OpenCode gets `serve` inserted unless a subcommand is
	// already present, while Claude Code (no server convention) runs as-is.
	profileInner := prof.InnerCmd
	if !profOK {
		profileInner = nil
	}
	// Resolve the inner command for the selected harness: --inner override
	// wins, else the profile's inner_cmd, else the harness default.
	inner := harness.ResolveInnerCmd(profileInner, *innerCmdOverride)
	// Apply the harness's server-launch convention (e.g. OpenCode injects
	// `serve` when no subcommand is present). Harnesses without a server
	// mode leave the inner command unchanged.
	inner = harness.ApplyServerLaunch(inner, innerArgs)
	// Inject the sandbox briefing on the same terms as `omac start` (see there).
	briefingText, injectBriefing := briefingInjection(*noSandbox, inner, harness, lc.Sandbox.Briefing)
	if injectBriefing && harness.SystemContextArgs != nil {
		inner = append(inner, harness.SystemContextArgs(briefingText)...)
	}
	if len(innerArgs) > 0 {
		inner = append(inner, innerArgs...)
	}

	// Extra env injected into the inner process (§5.1 step 7). The
	// per-skill OMAC_G_*/OMAC_D_* vars are added on top by serve as routes
	// come and go; the static globals are set here once.
	extra := srv.baseEnv()
	if injectBriefing {
		// Consumed by the OpenCode plugin; see start.go.
		extra["OMAC_SANDBOX_BRIEFING"] = briefingText
		// Harnesses without a CLI flag (copilot) deliver the briefing via
		// an env-var + file mechanism (e.g. COPILOT_CUSTOM_INSTRUCTIONS_DIRS).
		if harness.BriefingEnvFunc != nil {
			for k, v := range harness.BriefingEnvFunc(briefingText, srv.sandboxTmp) {
				extra[k] = v
			}
		}
	}

	var argv []string
	if *noSandbox {
		argv = inner
	} else {
		argv, err = sandbox.Expand(prof, sandbox.Inputs{
			Workdir:  env.Workdir,
			Socket:   socketPath,
			TCPPort:  srv.tcpPort,
			Mounts:   srv.facadeMounts(),
			InnerCmd: inner,
			TmpDir:   srv.sandboxTmp,
		})
		if err != nil {
			fmt.Fprintln(env.Stderr, "omac serve: sandbox argv:", err)
			return ExitConfigInvalid
		}
		// The control-plane port is distinct from the facade TCP port and
		// is NOT whitelisted by the profile's `--open-port {{tcp_port}}`.
		// Without opening it, the sandboxed `opencode serve` (and the plugin
		// inside it) cannot reach OMAC_CONTROL_BASE — nono denies the
		// loopback connect (FailedToOpenSocket). Whitelist it too.
		if cp := controlPortOf(cln); cp != "" {
			argv = injectOpenPort(argv, cp)
		}
		// Grant the selected harness's runtime dirs (config, state,
		// sessions) read+write — only for the selected harness.
		argv = injectSandboxDirs(argv, harness.SandboxDirs)
		// --for-opencode-desktop: grant every project worktree OpenCode
		// knows about. The kernel sandbox cannot grow after launch, so
		// folders opened for the first time during this session still
		// need a restart (or --learn).
		if *forDesktop {
			worktrees, skipped, derr := opencodestate.Worktrees()
			if derr != nil {
				fmt.Fprintln(env.Stderr, "omac serve: --for-opencode-desktop:", derr)
				return ExitIOError
			}
			for _, wt := range skipped {
				fmt.Fprintf(env.Stderr, "omac serve: skipping stale OpenCode worktree %s (no longer exists)\n", wt)
			}
			if len(worktrees) == 0 {
				fmt.Fprintln(env.Stderr, "omac serve: --for-opencode-desktop: no OpenCode projects found")
			} else {
				fmt.Fprintf(env.Stderr, "omac serve: granting %d OpenCode project folder(s):\n", len(worktrees))
				for _, wt := range worktrees {
					fmt.Fprintf(env.Stderr, "  r+w %s\n", wt)
					argv = injectSandboxFlag(argv, "--allow", wt)
				}
			}
		}
		// --learn: pass through to the built-in sandbox, which lifts
		// filesystem restrictions and records folder usage.
		if *learn {
			argv = injectSandboxFlag(argv, "--learn", "")
		}
	}
	if *verbose {
		fmt.Fprintf(env.Stderr, "[verbose] inner argv: %v\n", argv)
	}

	// Run the inner command (opencode serve) in the sandbox, with the
	// control plane already serving. ExecWithReady blocks until the child
	// exits; the deferred facade/supervisor/control teardown then runs
	// (§5.3). The onReady hook is where any post-launch work would go; the
	// control plane is already up, so it's a no-op marker here.
	code, err := sandbox.ExecWithReady(argv, extra, func() {
		if *verbose {
			fmt.Fprintln(env.Stderr, "[verbose] inner command started; control plane live")
		}
	})
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac serve: exec:", err)
		return ExitSandboxAbnormal
	}
	return code
}

// controlPortOf returns the port the control-plane listener is bound to,
// as a string, or "" if it can't be determined.
func controlPortOf(ln net.Listener) string {
	if ta, ok := ln.Addr().(*net.TCPAddr); ok {
		return fmt.Sprintf("%d", ta.Port)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return ""
	}
	return port
}

// injectOpenPort splices `--open-port <port>` into a sandbox argv so the
// sandboxed inner command may connect to that loopback port.
func injectOpenPort(argv []string, port string) []string {
	return injectSandboxFlag(argv, "--open-port", port)
}

// injectSandboxDirs splices --allow flags (read+write) for each
// harness-declared runtime directory into the sandbox argv, before the
// -- separator. Empty/nil dirs is a no-op.
func injectSandboxDirs(argv []string, dirs []string) []string {
	for _, d := range dirs {
		if d == "" {
			continue
		}
		argv = injectSandboxFlag(argv, "--allow", d)
	}
	return argv
}

// injectSandboxFlag splices a sandbox flag (with optional value; pass
// "" for boolean flags) into a sandbox argv. It inserts right before
// the `--` argument separator (the conventional boundary between
// sandbox flags and the inner command); if there is no `--`, it falls
// back to inserting just after argv[0].
func injectSandboxFlag(argv []string, flagName, value string) []string {
	ins := []string{flagName}
	if value != "" {
		ins = append(ins, value)
	}
	for i, a := range argv {
		if a == "--" {
			out := make([]string, 0, len(argv)+len(ins))
			out = append(out, argv[:i]...)
			out = append(out, ins...)
			out = append(out, argv[i:]...)
			return out
		}
	}
	// No `--` separator: insert just after the sandbox executable.
	if len(argv) == 0 {
		return argv
	}
	out := make([]string, 0, len(argv)+len(ins))
	out = append(out, argv[0])
	out = append(out, ins...)
	out = append(out, argv[1:]...)
	return out
}

// multiFlag collects a repeatable string flag (e.g. --root a --root b).
type multiFlag []string

func (m *multiFlag) String() string { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// ---- server state (docs/MULTI_DIR_DESKTOP.md §7) ----

type skillRoute struct {
	Name      string
	Mount     string
	Namespace string // dir token or facade.GlobalNamespace
	SkillDir  string // skill's on-disk dir; source of SKILL.md auto-discovery
	State     facade.RouteState
	Detail    string
	Missing   []string
}

type dirState struct {
	Dir    string
	Token  string
	State  string // activating|active|active_partial
	Skills map[string]*skillRoute
	mu     sync.Mutex
}

type serveServer struct {
	env           *Env
	harness       config.Harness // active harness; scopes skill discovery
	facade        *facade.Facade
	sup           *supervisor.Supervisor
	ctx           context.Context
	rtDir         string
	sandboxTmp    string // host temp dir granted RW + exported as TMPDIR
	socketPath    string
	tcpPort       int
	controlBase   string
	acceptChanges bool
	verbose       bool
	roots         []string // §5.4 Option B; empty = allow any directory

	mu      sync.RWMutex
	dirs    map[string]*dirState   // abs dir -> state
	byToken map[string]*dirState   // token -> dir
	global  map[string]*skillRoute // mount -> global skill

	// actMu serializes per-dir activation so two concurrent activate
	// calls for the same directory coalesce (§5.2 step 1) without holding
	// the coarse `mu` across discovery / spawning.
	actMu   sync.Mutex
	actLock map[string]*sync.Mutex

	// flatAliases tracks the §5.5 single-dir flat facade aliases currently
	// installed (mount -> present), so they can be torn down when a second
	// directory activates.
	flatAliasMu sync.Mutex
	flatAliases map[string]struct{}
}

// dirAllowed reports whether absDir may be activated under the configured
// roots policy (§5.4 Option B). An empty roots list allows any directory.
func (s *serveServer) dirAllowed(absDir string) bool {
	if len(s.roots) == 0 {
		return true
	}
	for _, root := range s.roots {
		if absDir == root {
			return true
		}
		rel, err := filepath.Rel(root, absDir)
		if err != nil {
			continue
		}
		if rel != ".." && !startsWithDotDot(rel) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}

func startsWithDotDot(rel string) bool {
	return rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)
}

// dirActLock returns the per-dir activation mutex, creating it on first use.
func (s *serveServer) dirActLock(absDir string) *sync.Mutex {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	if s.actLock == nil {
		s.actLock = map[string]*sync.Mutex{}
	}
	m, ok := s.actLock[absDir]
	if !ok {
		m = &sync.Mutex{}
		s.actLock[absDir] = m
	}
	return m
}

// ---- cold-start: global skills ----

// checkGlobalDrift validates the user-global skill layer at cold start and
// refuses to start (mirroring `omac start`) if any global skill is:
//   - present on disk under a global skills root but NOT registered, or
//   - registered but its bundle hash has drifted since register (unless
//     --accept-skill-changes), or
//   - registered but its omac.yaml is now broken/missing a sidecar block.
//
// Global skills are a fixed, known-at-startup set, so surfacing these here
// (with the same actionable `omac register` hints as start) is appropriate;
// workdir-local skills remain lazy and auto-registered on activation.
//
// Returns ExitOK when clean, or a non-zero exit code to abort serve.
func (s *serveServer) checkGlobalDrift() int {
	gReg, err := registry.LoadGlobal()
	if err != nil {
		fmt.Fprintln(s.env.Stderr, "omac serve: global registry:", err)
		return ExitIOError
	}
	registered := map[string]struct{}{}
	for _, e := range gReg.Registered {
		registered[e.Name] = struct{}{}
	}

	// 1. Unregistered global skills on disk. Discover everything visible to
	//    the active harness, keep only the user-global ones, and flag any
	//    that aren't in the global registry.
	discovered, derr := skillsource.Discover(s.env.Workdir, s.harness)
	if derr != nil {
		fmt.Fprintln(s.env.Stderr, "omac serve: scan global skills:", derr)
		return ExitIOError
	}
	var unregistered []string
	for _, e := range discovered {
		if e.Kind != "user-global" {
			continue
		}
		if _, ok := registered[e.Name]; !ok {
			unregistered = append(unregistered, e.Name)
		}
	}
	sort.Strings(unregistered)

	// 2. Bundle-hash drift + broken meta on registered globals.
	var drifted, brokenMeta []string
	for _, e := range gReg.Registered {
		absDir := e.SkillDir
		if !filepath.IsAbs(absDir) {
			continue
		}
		if !skillsource.DirInHarnessScope(absDir, s.harness) {
			continue // not loadable by this harness; activateGlobals skips it too
		}
		m, merr := config.LoadMeta(filepath.Join(absDir, config.MetaFileName))
		if merr != nil || m.Sidecar == nil {
			brokenMeta = append(brokenMeta, e.Name)
			continue
		}
		if !s.acceptChanges {
			if bundle, herr := config.BundleHash(absDir); herr == nil && bundle != e.BundleHash {
				drifted = append(drifted, e.Name)
			}
		}
	}
	sort.Strings(drifted)
	sort.Strings(brokenMeta)

	if len(unregistered) == 0 && len(drifted) == 0 && len(brokenMeta) == 0 {
		return ExitOK
	}

	total := len(unregistered) + len(drifted) + len(brokenMeta)
	fmt.Fprintf(s.env.Stderr, "omac serve: refusing to start, %d global-skill problem(s):\n", total)
	if len(brokenMeta) > 0 {
		fmt.Fprintln(s.env.Stderr, "\n  "+config.MetaFileName+" broken:")
		for _, n := range brokenMeta {
			fmt.Fprintf(s.env.Stderr, "    %s — re-register: omac register --force %s\n", n, n)
		}
	}
	if len(unregistered) > 0 {
		fmt.Fprintln(s.env.Stderr, "\n  global skill present but not registered:")
		for _, n := range unregistered {
			fmt.Fprintf(s.env.Stderr, "    %s — run: omac register %s\n", n, n)
		}
	}
	if len(drifted) > 0 {
		fmt.Fprintln(s.env.Stderr, "\n  bundle changed since register (re-register, or pass --accept-skill-changes):")
		for _, n := range drifted {
			fmt.Fprintf(s.env.Stderr, "    %s — omac register --force %s\n", n, n)
		}
	}
	fmt.Fprintln(s.env.Stderr)
	if len(unregistered) > 0 || len(brokenMeta) > 0 {
		return ExitPrerequisiteMissing
	}
	return ExitConfigInvalid
}

func (s *serveServer) activateGlobals() error {
	gReg, err := registry.LoadGlobal()
	if err != nil {
		return err
	}
	gCfg, err := skillconfig.LoadGlobal()
	if err != nil {
		return err
	}
	for _, e := range gReg.Registered {
		absDir := e.SkillDir
		if !filepath.IsAbs(absDir) {
			// Global entries should be absolute; skip otherwise.
			continue
		}
		// Harness scoping: a global skill registered under another harness's
		// dir (e.g. ~/.config/opencode/skills while running claude) is not
		// loadable by the active harness, so omac does not activate it.
		if !skillsource.DirInHarnessScope(absDir, s.harness) {
			if s.verbose {
				fmt.Fprintf(s.env.Stderr, "[verbose] global skill %s skipped (out of %s harness scope: %s)\n", e.Name, s.harness.Name, absDir)
			}
			continue
		}
		// Global skill: no single project, so OMAC_WORKDIR defaults to the
		// server's launch workdir.
		sr := s.bringUp(e, absDir, s.env.Workdir, facade.GlobalNamespace, "" /* unscoped secrets */, gCfg)
		s.mu.Lock()
		s.global[sr.Mount] = sr
		s.mu.Unlock()
		if s.verbose {
			fmt.Fprintf(s.env.Stderr, "[verbose] global skill %s mounted under /__global__/%s state=%s\n", sr.Name, sr.Mount, sr.State)
		}
	}
	return nil
}

// reloadGlobals tears down every currently-mounted global skill (routes +
// sidecars) and re-runs activateGlobals, so a newly registered/deregistered
// global skill is picked up without restarting serve. This is the global
// counterpart to deactivate+activate for a directory.
func (s *serveServer) reloadGlobals() error {
	// Snapshot and clear the current global set under the lock.
	s.mu.Lock()
	old := s.global
	s.global = map[string]*skillRoute{}
	s.mu.Unlock()

	// Tear down old routes + sidecars (outside the lock; StopSidecar and
	// RemoveRoute take their own locks).
	for mount, sr := range old {
		s.facade.RemoveRoute(facade.GlobalNamespace, mount)
		if sr.State == facade.RouteReady {
			s.sup.StopSidecar(facade.GlobalNamespace+"/"+sr.Name, 5*time.Second)
		}
	}

	// Re-activate from the (now-current) global registry.
	return s.activateGlobals()
}

// ---- lazy activation ----

// activate brings a directory online (idempotent) and returns its manifest.
func (s *serveServer) activate(absDir string) (map[string]any, error) {
	// Per-dir coalescing (§5.2 step 1): concurrent activate calls for the
	// same directory serialize here, so only the first does the work and
	// the rest observe the finished state.
	lock := s.dirActLock(absDir)
	lock.Lock()
	defer lock.Unlock()

	s.mu.RLock()
	existing, already := s.dirs[absDir]
	s.mu.RUnlock()
	if already {
		// Already active: re-discover so a skill installed/registered since
		// this dir was first activated is mounted now — no manual reload or
		// restart needed. Existing healthy skills are left untouched.
		s.rediscover(existing)
		return s.manifestFor(existing), nil
	}

	// Validate it's a real directory.
	if info, err := os.Stat(absDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", absDir)
	}
	// Enforce the pre-declared roots policy (§5.4 Option B).
	if !s.dirAllowed(absDir) {
		return nil, fmt.Errorf("directory %s is not under any allowed --root", absDir)
	}

	token := mintToken()
	d := &dirState{Dir: absDir, Token: token, State: "activating", Skills: map[string]*skillRoute{}}
	s.mu.Lock()
	s.dirs[absDir] = d
	s.byToken[token] = d
	s.mu.Unlock()

	discovered, err := skillsource.Discover(absDir, s.harness)
	if err != nil {
		return nil, err
	}

	wReg, err := registry.Load(absDir)
	if err != nil {
		return nil, err
	}
	wCfg, err := skillconfig.Load(absDir)
	if err != nil {
		return nil, err
	}
	workdirID := keychain.WorkdirID(absDir)

	partial := false
	for _, ent := range discovered {
		if ent.Kind != "workdir" {
			// user-global skill: already activated at cold start under
			// /__global__/. Not re-registered or re-spawned here (§5.2).
			continue
		}
		// Auto-register workdir-local skills not yet in this dir's registry.
		e, _ := wReg.Find(ent.Name)
		if e == nil {
			ne, rerr := s.autoRegister(absDir, ent)
			if rerr != nil {
				// Surface as a broken route rather than failing the whole dir.
				sr := &skillRoute{Name: ent.Name, Mount: ent.Name, Namespace: token,
					State: facade.RouteBroken, Detail: rerr.Error()}
				s.facade.AddRoute(facade.Route{Mount: sr.Mount, Namespace: token, Skill: sr.Name, State: sr.State, Detail: sr.Detail})
				d.mu.Lock()
				d.Skills[sr.Mount] = sr
				d.mu.Unlock()
				partial = true
				continue
			}
			e = ne
		}
		// workdir-local skill: OMAC_WORKDIR is the activated project dir
		// (absDir), not the skill's own directory (ent.Dir).
		sr := s.bringUp(*e, ent.Dir, absDir, token, workdirID, wCfg)
		if sr.State != facade.RouteReady {
			partial = true
		}
		d.mu.Lock()
		d.Skills[sr.Mount] = sr
		d.mu.Unlock()
	}

	d.mu.Lock()
	if partial {
		d.State = "active_partial"
	} else {
		d.State = "active"
	}
	d.mu.Unlock()

	s.refreshSingleDirAliases()
	return s.manifestFor(d), nil
}

// rediscover re-scans an already-active directory and brings up workdir-local
// skills that are not yet mounted, or that are mounted in a non-ready state
// (broken/pending-credentials) and may now succeed (e.g. after a chmod fix or
// a newly-supplied secret). Skills that are already ready are left untouched,
// so this is cheap to call on every agent turn and never churns healthy
// sidecars. It is the mechanism that makes "install a skill -> it appears"
// work without a manual reload.
func (s *serveServer) rediscover(d *dirState) {
	absDir := d.Dir
	token := d.Token
	discovered, err := skillsource.Discover(absDir, s.harness)
	if err != nil {
		return
	}
	wReg, err := registry.Load(absDir)
	if err != nil {
		return
	}
	wCfg, err := skillconfig.Load(absDir)
	if err != nil {
		return
	}
	workdirID := keychain.WorkdirID(absDir)

	for _, ent := range discovered {
		if ent.Kind != "workdir" {
			continue // globals handled separately
		}
		// Skip skills that are already mounted AND ready — never touch a
		// working route/sidecar (removing it even momentarily is what caused
		// a healthy skill to 404 mid-session). d.Skills is keyed by MOUNT
		// (matching how every other path stores it), so look up by mount.
		mnt := ent.Name
		if m, merr := config.LoadMeta(filepath.Join(ent.Dir, config.MetaFileName)); merr == nil && m.Sidecar != nil {
			mnt = m.Sidecar.MountOrDefault(ent.Name)
		}
		d.mu.Lock()
		cur, mounted := d.Skills[mnt]
		ready := mounted && cur.State == facade.RouteReady
		d.mu.Unlock()
		if ready {
			continue
		}
		// A previously non-ready skill is being retried. We do NOT pre-remove
		// its stub route: bringUp installs the new route via facade.AddRoute,
		// which replaces the entry by key atomically, so there is never a
		// window with no route. (A non-ready route has no live sidecar to
		// stop.)
		e, _ := wReg.Find(ent.Name)
		if e == nil {
			ne, rerr := s.autoRegister(absDir, ent)
			if rerr != nil {
				sr := &skillRoute{Name: ent.Name, Mount: ent.Name, Namespace: token,
					State: facade.RouteBroken, Detail: rerr.Error()}
				s.installRoute(sr, 0)
				d.mu.Lock()
				d.Skills[sr.Mount] = sr
				d.mu.Unlock()
				continue
			}
			e = ne
		}
		sr := s.bringUp(*e, ent.Dir, absDir, token, workdirID, wCfg)
		d.mu.Lock()
		d.Skills[sr.Mount] = sr
		d.mu.Unlock()
	}

	// Recompute aggregate state.
	d.mu.Lock()
	partial := false
	for _, sr := range d.Skills {
		if sr.State != facade.RouteReady {
			partial = true
			break
		}
	}
	if partial {
		d.State = "active_partial"
	} else {
		d.State = "active"
	}
	d.mu.Unlock()
}

// autoRegister writes a registry entry for a discovered workdir-local skill
// without prompting (serve mode has no human at the keyboard). Mirrors the
// non-interactive parts of `omac register`.
func (s *serveServer) autoRegister(absDir string, ent skillsource.Entry) (*registry.Entry, error) {
	metaPath := filepath.Join(ent.Dir, config.MetaFileName)
	m, err := config.LoadMeta(metaPath)
	if err != nil {
		return nil, err
	}
	if m.Sidecar == nil {
		return nil, fmt.Errorf("skill %q has no sidecar block", ent.Name)
	}
	bundle, err := config.BundleHash(ent.Dir)
	if err != nil {
		return nil, err
	}
	declared := make([]string, 0, len(m.Sidecar.Secrets))
	for _, sp := range m.Sidecar.Secrets {
		declared = append(declared, sp.Name)
	}
	var out *registry.Entry
	err = registry.WithLock(absDir, func() error {
		reg, err := registry.Load(absDir)
		if err != nil {
			return err
		}
		stored := ent.Dir
		if rel, rerr := filepath.Rel(absDir, ent.Dir); rerr == nil {
			stored = rel
		}
		reg.Upsert(registry.Entry{
			Name:                ent.Name,
			SkillDir:            stored,
			BundleHash:          bundle,
			RegisteredAt:        time.Now().UTC(),
			DeclaredSecretNames: declared,
		})
		if err := registry.Save(absDir, reg); err != nil {
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

// bringUp resolves a registered skill's secrets/config, and either spawns a
// live sidecar (and live route) or installs a stub route (pending-credentials
// / broken). secretScope is "" for global skills (unscoped keychain) or the
// workdir-id for workdir-local skills.
// bringUp resolves and (if ready) spawns a skill's sidecar.
//
//   - absDir is the skill's own directory (used as the sidecar's cwd and
//     for bundle hashing).
//   - workdir is the value exposed to the sidecar as OMAC_WORKDIR, i.e. the
//     project directory the skill should operate on. For a workdir-local
//     skill this is the activated project; for a global skill there is no
//     single project, so the server's launch workdir is used as a default.
func (s *serveServer) bringUp(e registry.Entry, absDir, workdir, namespace, secretScope string, cfg *skillconfig.Store) *skillRoute {
	metaPath := filepath.Join(absDir, config.MetaFileName)
	m, err := config.LoadMeta(metaPath)
	if err != nil || m.Sidecar == nil {
		sr := &skillRoute{Name: e.Name, Mount: e.Name, Namespace: namespace, SkillDir: absDir, State: facade.RouteBroken, Detail: "omac.yaml invalid or missing sidecar"}
		s.installRoute(sr, 0)
		return sr
	}
	mount := m.Sidecar.MountOrDefault(e.Name)

	if !s.acceptChanges {
		if bundle, herr := config.BundleHash(absDir); herr == nil && bundle != e.BundleHash {
			sr := &skillRoute{Name: e.Name, Mount: mount, Namespace: namespace, SkillDir: absDir, State: facade.RouteBroken,
				Detail: "bundle changed since register; re-register or pass --accept-skill-changes"}
			s.installRoute(sr, 0)
			return sr
		}
	}

	// Resolve secrets.
	secMap := map[string]secrets.Secret{}
	var missing []string
	for _, spec := range m.Sidecar.Secrets {
		val, gerr := keychain.GetWithFallback(secretScope, e.Name, spec.Name)
		if gerr == nil {
			secMap[spec.Name] = val
			continue
		}
		if errors.Is(gerr, keychain.ErrNotFound) {
			if spec.IsRequired() {
				missing = append(missing, spec.Name)
			}
			continue
		}
		// keychain I/O error -> broken
		sr := &skillRoute{Name: e.Name, Mount: mount, Namespace: namespace, SkillDir: absDir, State: facade.RouteBroken, Detail: gerr.Error()}
		s.installRoute(sr, 0)
		return sr
	}

	// Resolve config.
	cfgMap := map[string]string{}
	for _, spec := range m.Sidecar.Config {
		if v, ok := cfg.Get(e.Name, spec.Name); ok {
			cfgMap[spec.Name] = v
			continue
		}
		if spec.Default != "" {
			cfgMap[spec.Name] = spec.Default
			continue
		}
		if spec.DefaultFromEnv != "" {
			if ev, ok := os.LookupEnv(spec.DefaultFromEnv); ok && ev != "" {
				cfgMap[spec.Name] = ev
				continue
			}
		}
		if spec.IsRequired() {
			missing = append(missing, spec.Name)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		sr := &skillRoute{Name: e.Name, Mount: mount, Namespace: namespace, SkillDir: absDir,
			State: facade.RoutePendingCredentials, Missing: missing,
			Detail: fmt.Sprintf("missing required values: %v", missing)}
		s.installRoute(sr, 0)
		return sr
	}

	// Spawn.
	health := config.HealthSpec{}
	if m.Sidecar.Health != nil {
		health = *m.Sidecar.Health
	}
	spec := supervisor.SidecarSpec{
		Name:             namespace + "/" + e.Name, // unique tracking key across dirs
		SkillName:        e.Name,                   // plain name -> SIDECAR_SKILL (no slash)
		SkillDir:         absDir,
		Command:          m.Sidecar.Command,
		EnvPassthrough:   m.Sidecar.EnvPassthrough,
		Secrets:          secMap,
		Config:           cfgMap,
		Health:           health.Defaults(),
		LogPath:          filepath.Join(s.rtDir, "logs", namespace+"-"+e.Name+".log"),
		Workdir:          workdir, // -> OMAC_WORKDIR (the project, not the skill dir)
		HarnessSkillsDir: s.harness.WorkdirSkillsDir(),
	}
	running, serr := s.sup.AddSidecar(s.ctx, spec)
	// Wipe secret material now that the sidecar has been spawned (its env
	// was built synchronously inside AddSidecar). Secret holds a []byte, so
	// zeroing the map's stored value wipes the shared backing array.
	for name := range spec.Secrets {
		sec := spec.Secrets[name]
		sec.Zero()
		spec.Secrets[name] = sec
	}
	if serr != nil {
		sr := &skillRoute{Name: e.Name, Mount: mount, Namespace: namespace, SkillDir: absDir, State: facade.RouteBroken, Detail: serr.Error()}
		s.installRoute(sr, 0)
		return sr
	}
	sr := &skillRoute{Name: e.Name, Mount: mount, Namespace: namespace, SkillDir: absDir, State: facade.RouteReady}
	s.installRoute(sr, running.Port)
	return sr
}

// baseEnv returns the environment overlaid onto the inner `opencode serve`
// process at launch (§5.1 step 7). Only values known at cold start are
// injected: the facade transports, the control-plane URL, and the global
// (shared) skills' OMAC_G_* vars + OMAC_SKILLS list. Per-directory skills
// are activated lazily *after* the child is running, so their OMAC_D_*
// vars cannot be injected into an already-exec'd process; the agent
// discovers those dynamically by calling OMAC_CONTROL_BASE /__omac__/...
// and reading the per-dir manifest (§6.3).
func (s *serveServer) baseEnv() map[string]string {
	extra := map[string]string{
		"OMAC_SOCKET":             s.socketPath,
		"OMAC_HOST":               "127.0.0.1",
		"OMAC_PORT":               fmt.Sprintf("%d", s.tcpPort),
		"OMAC_BASE":               fmt.Sprintf("http://127.0.0.1:%d/", s.tcpPort),
		"OMAC_VERSION":            s.env.Version,
		"OMAC_CONTROL_BASE":       s.controlBase,
		"OMAC_HARNESS":            s.harness.Name,
		"OMAC_HARNESS_SKILLS_DIR": s.harness.WorkdirSkillsDir(),
		// Sandbox-granted temp dir exported as TMPDIR (see start.go and
		// the nono profile's {{tmpdir}} grant).
		"TMPDIR": s.sandboxTmp,
	}
	// Global skills are known at cold start (§4.5/§5.1): inject their base
	// URLs and list their mounts in OMAC_SKILLS.
	//
	// We emit BOTH names for each global skill:
	//   - OMAC_G_<MOUNT>_BASE  — the serve-mode global form (§4.5);
	//   - OMAC_<MOUNT>_BASE    — the flat form that single-workdir `start`
	//                            emits, which existing SKILL.md files hardcode.
	// A global skill's mount is unique server-wide (it lives under the
	// reserved __global__ namespace), so the flat alias is unambiguous — no
	// collision risk. Emitting both means a skill authored for `start`
	// (e.g. skill-marketplace reading OMAC_SKILL_MARKETPLACE_BASE) works
	// unchanged under serve.
	s.mu.RLock()
	mounts := make([]string, 0, len(s.global))
	for mount, sr := range s.global {
		if sr.State != facade.RouteReady {
			continue
		}
		url := sandbox.OmacTCPEnvValueNS(facade.GlobalNamespace, mount, s.tcpPort)
		extra[sandbox.OmacGlobalEnvName(mount)] = url
		extra[sandbox.OmacEnvName(mount)] = url // flat alias for start-mode skills
		mounts = append(mounts, facade.GlobalNamespace+"/"+mount)
	}
	s.mu.RUnlock()
	sort.Strings(mounts)
	extra["OMAC_SKILLS"] = joinCSV(mounts)
	return extra
}

// facadeMounts returns the set of facade mount segments to advertise to the
// sandbox profile's {{skills_csv}} / {{per_skill_env_flags}} template. At
// cold start this is the global skills' namespaced keys; per-dir mounts are
// added lazily and not known here.
func (s *serveServer) facadeMounts() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.global))
	for mount, sr := range s.global {
		if sr.State == facade.RouteReady {
			out = append(out, facade.GlobalNamespace+"/"+mount)
		}
	}
	sort.Strings(out)
	return out
}

func joinCSV(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

func (s *serveServer) installRoute(sr *skillRoute, port int) {
	s.facade.AddRoute(facade.Route{
		Mount:        sr.Mount,
		Namespace:    sr.Namespace,
		UpstreamPort: port,
		Skill:        sr.Name,
		SkillDir:     sr.SkillDir,
		State:        sr.State,
		Detail:       sr.Detail,
	})
}

// refreshSingleDirAliases maintains the §5.5 single-directory compatibility
// aliases. When exactly one directory is active, each of its ready
// workdir-local skills also gets a FLAT facade route (Namespace="",
// /<mount>) pointing at the same upstream, so a skill that hardcodes the
// start-mode path /<mount> (and OMAC_<SKILL>_BASE) keeps working under
// serve. As soon as a second directory activates, the flat aliases are
// torn down (flat names would be ambiguous across dirs — exactly the
// collision §4.1 namespacing prevents).
//
// The aliases are pure facade routes that reuse the per-dir sidecar's
// upstream port; they spawn nothing. Global skills are never aliased flat
// (they already live under the stable /__global__/ namespace).
func (s *serveServer) refreshSingleDirAliases() {
	s.mu.RLock()
	dirCount := len(s.dirs)
	var only *dirState
	for _, d := range s.dirs {
		only = d
	}
	s.mu.RUnlock()

	// Always clear any stale flat aliases first.
	s.clearFlatAliases()

	if dirCount != 1 || only == nil {
		return
	}
	only.mu.Lock()
	defer only.mu.Unlock()
	for _, sr := range only.Skills {
		if sr.State != facade.RouteReady {
			continue
		}
		// Mirror the namespaced route as a flat one. We re-resolve the
		// upstream by reading the namespaced route's port from the facade
		// via a fresh AddRoute that copies the upstream; since we don't
		// retain the port in skillRoute, look it up through the facade.
		port := s.facade.UpstreamPort(sr.Namespace, sr.Mount)
		if port == 0 {
			continue
		}
		s.facade.AddRoute(facade.Route{Mount: sr.Mount, Namespace: "", UpstreamPort: port, Skill: sr.Name, SkillDir: sr.SkillDir, State: facade.RouteReady})
		s.flatAliasMu.Lock()
		if s.flatAliases == nil {
			s.flatAliases = map[string]struct{}{}
		}
		s.flatAliases[sr.Mount] = struct{}{}
		s.flatAliasMu.Unlock()
	}
}

func (s *serveServer) clearFlatAliases() {
	s.flatAliasMu.Lock()
	for mount := range s.flatAliases {
		s.facade.RemoveRoute("", mount)
	}
	s.flatAliases = map[string]struct{}{}
	s.flatAliasMu.Unlock()
}

// ---- manifest ----

func (s *serveServer) manifestFor(d *dirState) map[string]any {
	d.mu.Lock()
	state := d.State
	// Non-nil so an empty manifest serializes as `"skills": []`, not null
	// (clients otherwise crash on a null skills list).
	skills := make([]map[string]any, 0, len(d.Skills))
	for _, sr := range d.Skills {
		skills = append(skills, s.skillJSON(sr, "workdir"))
	}
	d.mu.Unlock()

	s.mu.RLock()
	for _, sr := range s.global {
		skills = append(skills, s.skillJSON(sr, "global"))
	}
	s.mu.RUnlock()

	sort.Slice(skills, func(i, j int) bool {
		return skills[i]["name"].(string) < skills[j]["name"].(string)
	})
	return map[string]any{
		"dir":       d.Dir,
		"dir_token": d.Token,
		"state":     state,
		"skills":    skills,
	}
}

func (s *serveServer) skillJSON(sr *skillRoute, scope string) map[string]any {
	out := map[string]any{
		"name":  sr.Name,
		"scope": scope,
		"mount": sr.Mount,
		"state": string(sr.State),
	}
	if sr.State == facade.RouteReady {
		out["base"] = sandbox.OmacTCPEnvValueNS(sr.Namespace, sr.Mount, s.tcpPort)
		out["socket_base"] = sandbox.OmacEnvValueNS(sr.Namespace, sr.Mount, s.socketPath)
	}
	if len(sr.Missing) > 0 {
		out["missing"] = sr.Missing
	}
	if sr.Detail != "" {
		out["detail"] = sr.Detail
	}
	return out
}

// ---- control plane ----

func (s *serveServer) controlMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/__omac__/activate", s.handleActivate)
	mux.HandleFunc("/__omac__/deactivate", s.handleDeactivate)
	mux.HandleFunc("/__omac__/reload", s.handleReload)
	mux.HandleFunc("/__omac__/reload-global", s.handleReloadGlobal)
	mux.HandleFunc("/__omac__/dirs", s.handleDirs)
	mux.HandleFunc("/__omac__/global", s.handleGlobal)
	return mux
}

// handleReloadGlobal re-activates the user-global skill layer (POST). Used
// after a global `omac register`/`deregister` so the change takes effect
// without restarting serve. Returns the refreshed global skill list.
func (s *serveServer) handleReloadGlobal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	if err := s.reloadGlobals(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.handleGlobal(w, r) // respond with the refreshed global list
}

type dirReq struct {
	Dir string `json:"dir"`
}

func decodeDir(r *http.Request) (string, error) {
	var req dirReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return "", fmt.Errorf("bad json body: %w", err)
	}
	if req.Dir == "" {
		return "", fmt.Errorf("missing 'dir'")
	}
	return filepath.Abs(req.Dir)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *serveServer) handleActivate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	abs, err := decodeDir(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	manifest, err := s.activate(abs)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, manifest)
}

func (s *serveServer) handleDeactivate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	abs, err := decodeDir(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.deactivate(abs)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deactivated", "dir": abs})
}

func (s *serveServer) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	abs, err := decodeDir(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Reload = re-run activation logic for an already-known dir: drop and
	// re-activate so pending-credentials skills get promoted.
	s.deactivate(abs)
	manifest, err := s.activate(abs)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, manifest)
}

func (s *serveServer) handleDirs(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	dirs := make([]map[string]string, 0, len(s.dirs))
	for _, d := range s.dirs {
		d.mu.Lock()
		dirs = append(dirs, map[string]string{"dir": d.Dir, "dir_token": d.Token, "state": d.State})
		d.mu.Unlock()
	}
	s.mu.RUnlock()
	sort.Slice(dirs, func(i, j int) bool { return dirs[i]["dir"] < dirs[j]["dir"] })
	writeJSON(w, http.StatusOK, map[string]any{"dirs": dirs})
}

func (s *serveServer) handleGlobal(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	skills := make([]map[string]any, 0, len(s.global))
	for _, sr := range s.global {
		skills = append(skills, s.skillJSON(sr, "global"))
	}
	s.mu.RUnlock()
	sort.Slice(skills, func(i, j int) bool {
		return skills[i]["name"].(string) < skills[j]["name"].(string)
	})
	writeJSON(w, http.StatusOK, map[string]any{"skills": skills})
}

func (s *serveServer) deactivate(absDir string) {
	s.mu.Lock()
	d, ok := s.dirs[absDir]
	if ok {
		delete(s.dirs, absDir)
		delete(s.byToken, d.Token)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	d.mu.Lock()
	for _, sr := range d.Skills {
		s.facade.RemoveRoute(sr.Namespace, sr.Mount)
		if sr.State == facade.RouteReady {
			s.sup.StopSidecar(sr.Namespace+"/"+sr.Name, 5*time.Second)
		}
	}
	d.mu.Unlock()
	s.refreshSingleDirAliases()
}

// ---- helpers ----

func mintToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Extremely unlikely; fall back to a time-based value.
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

// createRuntimeDirServe creates ${TMPDIR}/omac-serve-<hash>/{logs}.
func createRuntimeDirServe(serverRoot string) (string, error) {
	tmp := os.TempDir()
	sum := sha256.Sum256([]byte("serve:" + serverRoot))
	name := "omac-serve-" + hex.EncodeToString(sum[:6])
	dir := filepath.Join(tmp, name)
	if _, err := os.Stat(dir); err == nil {
		_ = os.RemoveAll(dir)
	}
	for _, sub := range []string{"", "logs"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

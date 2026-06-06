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
	"sync"
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
	)
	var roots multiFlag
	fs.Var(&roots, "root", "Pre-declared root directory under which projects may be activated (§5.4 Option B). Repeatable. Empty = allow any directory.")
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac serve [flags] [-- inner args...]")
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
		facade:        f,
		sup:           sup,
		ctx:           ctx,
		rtDir:         rtDir,
		socketPath:    socketPath,
		tcpPort:       f.TCPPort(),
		acceptChanges: *acceptChanges,
		verbose:       *verbose,
		roots:         absRoots,
		dirs:          map[string]*dirState{},
		byToken:       map[string]*dirState{},
		global:        map[string]*skillRoute{},
	}

	// Cold start: activate user-global skills once, under /__global__/ (§5.1).
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

	// Build the inner argv. serve mode runs the OpenCode *server*, not the
	// TUI — but the launcher profiles ship inner_cmd ["opencode"] (sized for
	// `omac start`'s TUI). So serve resolves the executable from the profile
	// (or --inner), then ensures the `serve` subcommand is present unless the
	// caller already specified a subcommand via trailing `-- args`.
	inner := prof.InnerCmd
	if !profOK {
		inner = nil
	}
	if *innerCmdOverride != "" {
		if len(inner) == 0 {
			inner = []string{*innerCmdOverride}
		} else {
			inner = append([]string{*innerCmdOverride}, inner[1:]...)
		}
	}
	if len(inner) == 0 {
		// No profile inner_cmd and no override: default to `opencode`.
		inner = []string{"opencode"}
	}
	// Ensure the opencode server is launched. If the inner executable is
	// `opencode` and no explicit subcommand follows (in the profile's
	// inner_cmd tail or in trailing args), insert `serve`.
	inner = ensureServeSubcommand(inner, innerArgs)
	if len(innerArgs) > 0 {
		inner = append(inner, innerArgs...)
	}

	// Extra env injected into the inner process (§5.1 step 7). The
	// per-skill OMAC_G_*/OMAC_D_* vars are added on top by serve as routes
	// come and go; the static globals are set here once.
	extra := srv.baseEnv()

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

// ensureServeSubcommand makes sure an `opencode` inner command launches the
// server. If inner[0] is "opencode" (or ends in "/opencode") and neither the
// inner tail nor the trailing args already begin with a non-flag token (a
// subcommand like "serve", "run", "tui"), it inserts "serve" right after the
// executable. For any other executable it is a no-op.
func ensureServeSubcommand(inner, trailing []string) []string {
	if len(inner) == 0 {
		return inner
	}
	exe := inner[0]
	base := exe
	if i := lastSlash(exe); i >= 0 {
		base = exe[i+1:]
	}
	if base != "opencode" {
		return inner
	}
	if hasSubcommand(inner[1:]) || hasSubcommand(trailing) {
		return inner
	}
	out := make([]string, 0, len(inner)+1)
	out = append(out, inner[0], "serve")
	out = append(out, inner[1:]...)
	return out
}

// hasSubcommand reports whether args begins with a non-flag positional
// (i.e. a subcommand). Leading flags mean "no subcommand yet".
func hasSubcommand(args []string) bool {
	for _, a := range args {
		if a == "" {
			continue
		}
		if a[0] == '-' {
			// A flag; keep scanning is wrong — a subcommand always comes
			// first in CLI convention, so a leading flag means none.
			return false
		}
		return true
	}
	return false
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
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
// sandboxed inner command may connect to that loopback port. It inserts the
// flag right before the `--` argument separator (the conventional boundary
// between sandbox flags and the inner command); if there is no `--`, it
// appends before the first inner-command token is impossible to locate
// reliably, so it falls back to inserting at the front after argv[0].
func injectOpenPort(argv []string, port string) []string {
	for i, a := range argv {
		if a == "--" {
			out := make([]string, 0, len(argv)+2)
			out = append(out, argv[:i]...)
			out = append(out, "--open-port", port)
			out = append(out, argv[i:]...)
			return out
		}
	}
	// No `--` separator: insert just after the sandbox executable.
	if len(argv) == 0 {
		return argv
	}
	out := make([]string, 0, len(argv)+2)
	out = append(out, argv[0], "--open-port", port)
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
	facade        *facade.Facade
	sup           *supervisor.Supervisor
	ctx           context.Context
	rtDir         string
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
		sr := s.bringUp(e, absDir, facade.GlobalNamespace, "" /* unscoped secrets */, gCfg)
		s.mu.Lock()
		s.global[sr.Mount] = sr
		s.mu.Unlock()
		if s.verbose {
			fmt.Fprintf(s.env.Stderr, "[verbose] global skill %s mounted under /__global__/%s state=%s\n", sr.Name, sr.Mount, sr.State)
		}
	}
	return nil
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
	if d, ok := s.dirs[absDir]; ok {
		s.mu.RUnlock()
		return s.manifestFor(d), nil
	}
	s.mu.RUnlock()

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

	discovered, err := skillsource.Discover(absDir)
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
		sr := s.bringUp(*e, ent.Dir, token, workdirID, wCfg)
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
func (s *serveServer) bringUp(e registry.Entry, absDir, namespace, secretScope string, cfg *skillconfig.Store) *skillRoute {
	metaPath := filepath.Join(absDir, config.MetaFileName)
	m, err := config.LoadMeta(metaPath)
	if err != nil || m.Sidecar == nil {
		sr := &skillRoute{Name: e.Name, Mount: e.Name, Namespace: namespace, State: facade.RouteBroken, Detail: "omac.yaml invalid or missing sidecar"}
		s.installRoute(sr, 0)
		return sr
	}
	mount := m.Sidecar.MountOrDefault(e.Name)

	if !s.acceptChanges {
		if bundle, herr := config.BundleHash(absDir); herr == nil && bundle != e.BundleHash {
			sr := &skillRoute{Name: e.Name, Mount: mount, Namespace: namespace, State: facade.RouteBroken,
				Detail: "bundle changed since register; re-register or pass --accept-skill-changes"}
			s.installRoute(sr, 0)
			return sr
		}
	}

	// Resolve secrets.
	secMap := map[string]secrets.Secret{}
	var missing []string
	for _, spec := range m.Sidecar.Secrets {
		val, gerr := keychain.GetScoped(secretScope, e.Name, spec.Name)
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
		sr := &skillRoute{Name: e.Name, Mount: mount, Namespace: namespace, State: facade.RouteBroken, Detail: gerr.Error()}
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
		sr := &skillRoute{Name: e.Name, Mount: mount, Namespace: namespace,
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
		Name:           namespace + "/" + e.Name, // unique across dirs
		SkillDir:       absDir,
		Command:        m.Sidecar.Command,
		EnvPassthrough: m.Sidecar.EnvPassthrough,
		Secrets:        secMap,
		Config:         cfgMap,
		Health:         health.Defaults(),
		LogPath:        filepath.Join(s.rtDir, "logs", namespace+"-"+e.Name+".log"),
		Workdir:        absDir,
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
		sr := &skillRoute{Name: e.Name, Mount: mount, Namespace: namespace, State: facade.RouteBroken, Detail: serr.Error()}
		s.installRoute(sr, 0)
		return sr
	}
	sr := &skillRoute{Name: e.Name, Mount: mount, Namespace: namespace, State: facade.RouteReady}
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
		"OMAC_SOCKET":       s.socketPath,
		"OMAC_HOST":         "127.0.0.1",
		"OMAC_PORT":         fmt.Sprintf("%d", s.tcpPort),
		"OMAC_BASE":         fmt.Sprintf("http://127.0.0.1:%d/", s.tcpPort),
		"OMAC_VERSION":      s.env.Version,
		"OMAC_CONTROL_BASE": s.controlBase,
	}
	// Global skills are known at cold start (§4.5/§5.1): inject their
	// OMAC_G_<SKILL>_BASE vars and list their mounts in OMAC_SKILLS.
	s.mu.RLock()
	mounts := make([]string, 0, len(s.global))
	for mount, sr := range s.global {
		if sr.State != facade.RouteReady {
			continue
		}
		extra[sandbox.OmacGlobalEnvName(mount)] = sandbox.OmacTCPEnvValueNS(facade.GlobalNamespace, mount, s.tcpPort)
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
		s.facade.AddRoute(facade.Route{Mount: sr.Mount, Namespace: "", UpstreamPort: port, Skill: sr.Name, State: facade.RouteReady})
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
	mux.HandleFunc("/__omac__/dirs", s.handleDirs)
	mux.HandleFunc("/__omac__/global", s.handleGlobal)
	return mux
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

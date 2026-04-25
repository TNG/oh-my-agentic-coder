package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/facade"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandbox"
	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillconfig"
	"github.com/tngtech/oh-my-agentic-coder/internal/supervisor"
)

func runStart(args []string, env *Env) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	var (
		profile           = fs.String("sandbox", "", "Name of a sandbox profile from the launcher config.")
		innerCmdOverride  = fs.String("inner", "", "Override inner_cmd's executable.")
		noSandbox         = fs.Bool("no-sandbox", false, "Run inner command directly, without a sandbox (debug only).")
		keepRunning       = fs.Bool("keep-running", false, "Do not stop sidecars when the inner command exits.")
		acceptMetaChanges = fs.Bool("accept-meta-changes", false, "Tolerate meta_hash drift.")
		verbose           = fs.Bool("verbose", false, "Verbose lifecycle logging.")
	)
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac start [flags] [-- inner args...]")
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

	// 2. Load registry and every meta.
	//
	// An empty registry is NOT an error: omac is still useful as a thin
	// sandbox launcher even before any skills have been registered. In
	// that case there's nothing to spawn (no sidecars, no facade routes)
	// but the rest of the pipeline (facade listener, sandbox exec) still
	// makes sense and the inner command runs as configured by the
	// sandbox profile. We just emit a one-line notice so the user
	// understands why no facade traffic will work.
	reg, err := registry.Load(env.Workdir)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac start: registry:", err)
		return ExitIOError
	}
	if len(reg.Registered) == 0 {
		fmt.Fprintln(env.Stderr,
			"omac start: no skills registered in this workdir; "+
				"starting sandbox without sidecars (run `omac register` to add some)")
	}

	type resolved struct {
		entry registry.Entry
		meta  *config.Meta
		abs   string // absolute skill dir
	}
	skills := make([]resolved, 0, len(reg.Registered))
	for _, e := range reg.Registered {
		absDir := e.SkillDir
		if !filepath.IsAbs(absDir) {
			absDir = filepath.Join(env.Workdir, absDir)
		}
		metaPath := filepath.Join(absDir, "meta.yaml")
		m, err := config.LoadMeta(metaPath)
		if err != nil {
			fmt.Fprintf(env.Stderr, "omac start: %s: %v\n", e.Name, err)
			return ExitConfigInvalid
		}
		if m.Sidecar == nil {
			fmt.Fprintf(env.Stderr, "omac start: %s: meta no longer has a sidecar block\n", e.Name)
			return ExitConfigInvalid
		}
		hash, err := config.HashMetaFile(metaPath)
		if err != nil {
			fmt.Fprintln(env.Stderr, "omac start: hash:", err)
			return ExitIOError
		}
		if hash != e.MetaHash && !*acceptMetaChanges {
			fmt.Fprintf(env.Stderr, "omac start: %s: meta_hash drifted since register (pass --accept-meta-changes to continue)\n", e.Name)
			return ExitConfigInvalid
		}
		skills = append(skills, resolved{entry: e, meta: m, abs: absDir})
	}

	// 3. Resolve secrets from keychain and non-secret config from
	//    .opencode/skill-config.json. Both feed the sidecar's env;
	//    secrets win on collision (validated at meta load).
	type withSecrets struct {
		resolved
		secrets map[string]secrets.Secret
		config  map[string]string
	}
	armed := make([]withSecrets, 0, len(skills))
	// Zero-on-exit; collect all Secrets so we can wipe them at the end.
	var allSecrets []secrets.Secret
	defer func() {
		for i := range allSecrets {
			allSecrets[i].Zero()
		}
	}()
	var configStore *skillconfig.Store
	if len(skills) > 0 {
		configStore, err = skillconfig.Load(env.Workdir)
		if err != nil {
			fmt.Fprintln(env.Stderr, "omac start: skill-config:", err)
			return ExitIOError
		}
	}
	for _, s := range skills {
		m := map[string]secrets.Secret{}
		for _, spec := range s.meta.Sidecar.Secrets {
			val, err := keychain.Get(s.entry.Name, spec.Name)
			if err != nil {
				if errors.Is(err, keychain.ErrNotFound) {
					if spec.IsRequired() {
						fmt.Fprintf(env.Stderr,
							"omac start: %s: required secret %s missing. Run: omac secrets set %s %s\n",
							s.entry.Name, spec.Name, s.entry.Name, spec.Name)
						return ExitSecretRefused
					}
					continue
				}
				fmt.Fprintln(env.Stderr, "omac start: keychain:", err)
				return ExitKeychainError
			}
			m[spec.Name] = val
			allSecrets = append(allSecrets, val)
		}

		cfg := map[string]string{}
		for _, spec := range s.meta.Sidecar.Config {
			v, ok := configStore.Get(s.entry.Name, spec.Name)
			if !ok {
				if spec.IsRequired() {
					fmt.Fprintf(env.Stderr,
						"omac start: %s: required config field %s missing. Re-run: omac register --reprompt-fields %s\n",
						s.entry.Name, spec.Name, s.entry.Name)
					return ExitSecretRefused
				}
				continue
			}
			cfg[spec.Name] = v
		}

		armed = append(armed, withSecrets{resolved: s, secrets: m, config: cfg})
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
			Name:           s.entry.Name,
			SkillDir:       s.abs,
			Command:        s.meta.Sidecar.Command,
			EnvPassthrough: s.meta.Sidecar.EnvPassthrough,
			Secrets:        s.secrets,
			Config:         s.config,
			Health:         health.Defaults(),
			LogPath:        filepath.Join(rtDir, "logs", s.entry.Name+".log"),
			Workdir:        env.Workdir,
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

	// 8. Build sandbox argv and exec.
	inner := prof.InnerCmd
	if *innerCmdOverride != "" {
		if len(inner) == 0 {
			inner = []string{*innerCmdOverride}
		} else {
			inner = append([]string{*innerCmdOverride}, inner[1:]...)
		}
	}
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
		})
		if err != nil {
			fmt.Fprintln(env.Stderr, "omac start: sandbox argv:", err)
			return ExitConfigInvalid
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
		"OMAC_SOCKET":  socketPath,
		"OMAC_HOST":    "127.0.0.1",
		"OMAC_PORT":    fmt.Sprintf("%d", tcpPort),
		"OMAC_BASE":    fmt.Sprintf("http://127.0.0.1:%d/", tcpPort),
		"OMAC_SKILLS":  strings.Join(mounts, ","),
		"OMAC_VERSION": env.Version,
	}
	for _, m := range mounts {
		extra[sandbox.OmacEnvName(m)] = sandbox.OmacTCPEnvValue(m, tcpPort)
		extra[sandbox.OmacSocketEnvName(m)] = sandbox.OmacEnvValue(m, socketPath)
	}

	code, err := sandbox.Exec(argv, extra)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac start: exec:", err)
		return ExitSandboxAbnormal
	}
	return code
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

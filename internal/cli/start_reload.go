package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/facade"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillconfig"
	"github.com/tngtech/oh-my-agentic-coder/internal/supervisor"
)

// startReloader gives single-workdir `omac start` the same live-reload that
// serve has: a control plane that, on POST /__omac__/reload, re-discovers the
// workdir and mounts any newly-registered skill onto the running facade
// (flat mounts, matching start's namespace-less scheme) — so you can install
// + register a skill from an outside terminal and keep working in the same
// TUI session without restarting.
//
// It deliberately only ADDS missing skills; it never disturbs a skill that is
// already mounted (so a healthy route is never dropped mid-session).
type startReloader struct {
	env     *Env
	facade  *facade.Facade
	sup     *supervisor.Supervisor
	ctx     context.Context
	rtDir   string
	socket  string
	tcpPort int
	verbose bool

	mu      sync.Mutex
	mounted map[string]struct{} // skill names already mounted
}

// startControlPlane binds a loopback control-plane HTTP server for start and
// publishes its URL via the shared control-info file. Returns the listener,
// the control URL, and a close func. On bind failure it returns ok=false and
// start proceeds without live reload (non-fatal).
func startControlPlane(r *startReloader) (controlURL string, closeFn func(), ok bool) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", func() {}, false
	}
	controlURL = fmt.Sprintf("http://%s", ln.Addr().String())
	mux := http.NewServeMux()
	mux.HandleFunc("/__omac__/reload", r.handleReload)
	mux.HandleFunc("/__omac__/dirs", r.handleDirs)
	srv := &http.Server{Handler: mux}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(r.env.Stderr, "omac start: control server:", err)
		}
	}()
	_ = writeControlInfo(controlURL)
	return controlURL, func() {
		srv.Close()
		removeControlInfo()
	}, true
}

func (r *startReloader) markMounted(names ...string) {
	r.mu.Lock()
	if r.mounted == nil {
		r.mounted = map[string]struct{}{}
	}
	for _, n := range names {
		r.mounted[n] = struct{}{}
	}
	r.mu.Unlock()
}

func (r *startReloader) isMounted(name string) bool {
	r.mu.Lock()
	_, ok := r.mounted[name]
	r.mu.Unlock()
	return ok
}

func (r *startReloader) handleDirs(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	names := make([]string, 0, len(r.mounted))
	for n := range r.mounted {
		names = append(names, n)
	}
	r.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":    "start",
		"workdir": r.env.Workdir,
		"mounted": names,
	})
}

func (r *startReloader) handleReload(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	added := r.reload()
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":    "start",
		"workdir": r.env.Workdir,
		"added":   added,
	})
}

// reload scans the workdir for registered skills that aren't mounted yet and
// brings them up on the running facade. Returns the names newly mounted.
func (r *startReloader) reload() []string {
	wReg, err := registry.Load(r.env.Workdir)
	if err != nil {
		return nil
	}
	gReg, err := registry.LoadGlobal()
	if err != nil {
		return nil
	}
	reg := mergeRegistries(gReg, wReg)

	wCfg, _ := skillconfig.Load(r.env.Workdir)
	gCfg, _ := skillconfig.LoadGlobal()
	cfgStore := mergeConfig(gCfg, wCfg)

	secScope := keychain.WorkdirID(r.env.Workdir)
	var added []string

	for _, e := range reg.Registered {
		if r.isMounted(e.Name) {
			continue
		}
		absDir := e.SkillDir
		if !filepath.IsAbs(absDir) {
			absDir = filepath.Join(r.env.Workdir, absDir)
		}
		m, err := config.LoadMeta(filepath.Join(absDir, config.MetaFileName))
		if err != nil || m.Sidecar == nil {
			continue
		}
		mount := m.Sidecar.MountOrDefault(e.Name)

		// Resolve secrets (workdir-scoped, unscoped fallback) + config.
		secMap := map[string]secrets.Secret{}
		missing := false
		for _, spec := range m.Sidecar.Secrets {
			val, gerr := keychain.GetWithFallback(secScope, e.Name, spec.Name)
			if gerr == nil {
				secMap[spec.Name] = val
				continue
			}
			if spec.IsRequired() {
				missing = true
			}
		}
		if missing {
			continue // not ready yet; a later reload (after secrets set) gets it
		}
		cfgMap := map[string]string{}
		cfgMissing := false
		for _, spec := range m.Sidecar.Config {
			if v, ok := cfgStore.Get(e.Name, spec.Name); ok {
				cfgMap[spec.Name] = v
			} else if spec.Default != "" {
				cfgMap[spec.Name] = spec.Default
			} else if spec.IsRequired() {
				cfgMissing = true
			}
		}
		if cfgMissing {
			continue
		}

		health := config.HealthSpec{}
		if m.Sidecar.Health != nil {
			health = *m.Sidecar.Health
		}
		spec := supervisor.SidecarSpec{
			Name:           e.Name,
			SkillName:      e.Name,
			SkillDir:       absDir,
			Command:        m.Sidecar.Command,
			EnvPassthrough: m.Sidecar.EnvPassthrough,
			Secrets:        secMap,
			Config:         cfgMap,
			Health:         health.Defaults(),
			LogPath:        filepath.Join(r.rtDir, "logs", e.Name+".log"),
			Workdir:        r.env.Workdir,
		}
		running, serr := r.sup.AddSidecar(r.ctx, spec)
		for name := range spec.Secrets {
			sec := spec.Secrets[name]
			sec.Zero()
			spec.Secrets[name] = sec
		}
		if serr != nil {
			if r.verbose {
				fmt.Fprintf(r.env.Stderr, "[verbose] reload: %s failed: %v\n", e.Name, serr)
			}
			continue
		}
		r.facade.AddRoute(facade.Route{
			Mount:        mount,
			UpstreamPort: running.Port,
			Skill:        e.Name,
			State:        facade.RouteReady,
		})
		r.markMounted(e.Name)
		added = append(added, mount)
		if r.verbose {
			fmt.Fprintf(r.env.Stderr, "[verbose] reload: mounted %s at /%s\n", e.Name, mount)
		}
	}
	return added
}

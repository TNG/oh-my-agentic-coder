package cli

import (
	"context"
	"encoding/json"
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
	"github.com/tngtech/oh-my-agentic-coder/internal/sandbox"
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

	mu          sync.Mutex
	mounted     map[string]string // skill name -> mount, for skills mounted on the facade
	lastSession string            // most-recent session id the harness plugin reported (see handleSession)
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
	// The omac plugin (built for serve) calls activate/deactivate. In the
	// single-workdir start model "activate <dir>" maps to a reload of our
	// one workdir; we accept it and return a serve-shaped manifest so the
	// plugin works unchanged instead of 404-spamming.
	mux.HandleFunc("/__omac__/activate", r.handleActivate)
	mux.HandleFunc("/__omac__/deactivate", r.handleActivate) // no-op deactivate, same response
	mux.HandleFunc("/__omac__/reload-global", r.handleReloadGlobalStart)
	// The harness plugin reports the id of the session it created here, so the
	// post-exit "resume" hint is exact without enumerating sessions.
	mux.HandleFunc("/__omac__/session", r.handleSession)
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

// markMounted records a skill (name -> facade mount) as mounted.
func (r *startReloader) markMounted(name, mount string) {
	r.mu.Lock()
	if r.mounted == nil {
		r.mounted = map[string]string{}
	}
	r.mounted[name] = mount
	r.mu.Unlock()
}

func (r *startReloader) isMounted(name string) bool {
	r.mu.Lock()
	_, ok := r.mounted[name]
	r.mu.Unlock()
	return ok
}

// handleSession records the session id the harness plugin reports for this
// run (opencode posts it on session.created). Best-effort: a malformed or
// empty body is ignored. The recorded id feeds the post-exit continue hint,
// replacing the need to enumerate sessions to guess which one this run created.
func (r *startReloader) handleSession(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	var body struct {
		Session string `json:"session"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err == nil && body.Session != "" {
		r.mu.Lock()
		r.lastSession = body.Session
		r.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// reportedSession returns the last session id reported via handleSession, or
// "" if the plugin never reported one (control plane down, or a harness with
// no such plugin).
func (r *startReloader) reportedSession() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastSession
}

func (r *startReloader) handleDirs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":    "start",
		"workdir": r.env.Workdir,
		"dirs":    []map[string]any{{"dir": r.env.Workdir, "state": "active"}},
	})
}

func (r *startReloader) handleReload(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	r.reload()
	writeJSON(w, http.StatusOK, r.manifest())
}

// handleActivate accepts the plugin's activate/deactivate calls. start has a
// single fixed workdir, so we treat activate as a reload of that workdir and
// reply with a serve-shaped manifest. A request for a different directory
// gets an empty manifest (start only knows its own workdir).
func (r *startReloader) handleActivate(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	dir, _ := decodeDir(req)
	if dir != "" && dir != r.env.Workdir {
		// Not our workdir — report it as having no start-managed skills.
		writeJSON(w, http.StatusOK, map[string]any{
			"dir": dir, "dir_token": "", "state": "active",
			"skills": []map[string]any{},
		})
		return
	}
	r.reload()
	writeJSON(w, http.StatusOK, r.manifest())
}

func (r *startReloader) handleReloadGlobalStart(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	// start has no separate global layer; a reload covers everything.
	r.reload()
	writeJSON(w, http.StatusOK, map[string]any{"skills": []map[string]any{}})
}

// manifest renders start's mounted skills in the serve manifest shape the
// plugin expects. start uses FLAT mounts (no dir token), so base URLs are
// http://127.0.0.1:<tcp>/<mount> and dir_token is empty.
func (r *startReloader) manifest() map[string]any {
	r.mu.Lock()
	pairs := make([][2]string, 0, len(r.mounted))
	for name, mount := range r.mounted {
		pairs = append(pairs, [2]string{name, mount})
	}
	r.mu.Unlock()

	skills := make([]map[string]any, 0, len(pairs))
	for _, p := range pairs {
		name, mount := p[0], p[1]
		skills = append(skills, map[string]any{
			"name":  name,
			"scope": "workdir",
			"mount": mount,
			"state": "ready",
			"base":  sandbox.OmacTCPEnvValue(mount, r.tcpPort),
		})
	}
	return map[string]any{
		"dir":       r.env.Workdir,
		"dir_token": "",
		"state":     "active",
		"skills":    skills,
	}
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
		r.markMounted(e.Name, mount)
		added = append(added, mount)
		if r.verbose {
			fmt.Fprintf(r.env.Stderr, "[verbose] reload: mounted %s at /%s\n", e.Name, mount)
		}
	}
	return added
}

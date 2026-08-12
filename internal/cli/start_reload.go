package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"sync"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/facade"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandbox"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillconfig"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillstate"
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
	// skipSecretPattern carries `omac start --skip-secret-pattern` into the
	// reload loop. Reload began pattern-checking env-supplied secrets when it
	// adopted the shared readiness rule, so without this a session started
	// explicitly to tolerate an outdated pattern would still refuse to mount a
	// skill registered mid-session.
	skipSecretPattern bool

	mu      sync.Mutex
	mounted map[string]string // skill name -> mount, for skills mounted on the facade
	// notReady holds the skills a reload could not bring up, keyed by skill
	// name, so /__omac__/reload's manifest reports them instead of omitting
	// them. Before #174 an unready skill was silently `continue`d: no message,
	// no route, no diagnostic, and no way for the agent to learn why the skill
	// it just registered never appeared.
	notReady map[string]*notReadySkill
	// warned records the last reason reported to stderr per skill, so a
	// repeated reload of the same unready skill does not spam the human's
	// terminal while a CHANGED reason still gets through. Reset implicitly when
	// the process restarts.
	warned      map[string]string
	lastSession string // most-recent session id the harness plugin reported (see handleSession)
}

// notReadySkill is a skill that resolved to a stub route rather than a live
// sidecar, mirroring the fields serve reports for the same states.
type notReadySkill struct {
	Mount   string
	State   facade.RouteState
	Missing []string
	Detail  string
}

// reloadStubRoute maps skillstate problems to the stub route reload should
// install, or nil when the skill is ready to spawn. It is reload's
// presentation of the shared readiness rule, and mirrors serve's bringUp: a
// supplyable credential is pending-credentials, anything the agent cannot fix
// by supplying a value is broken.
func reloadStubRoute(mount string, problems []skillstate.Problem) *notReadySkill {
	if len(problems) == 0 {
		return nil
	}
	// A dead keychain or a malformed env-supplied value is not "missing": the
	// remedy is not `omac secrets set`, so don't route it as if it were.
	if p := skillstate.First(problems, skillstate.KeychainUnavailable, skillstate.InvalidSecret); p != nil {
		detail := p.Detail
		if p.Fix != "" {
			detail += " — " + p.Fix
		}
		return &notReadySkill{Mount: mount, State: facade.RouteBroken, Detail: detail}
	}
	if missing := skillstate.MissingFields(problems); len(missing) > 0 {
		return &notReadySkill{
			Mount:   mount,
			State:   facade.RoutePendingCredentials,
			Missing: missing,
			Detail:  fmt.Sprintf("missing required values: %v", missing),
		}
	}
	// Anything left (bundle drift) is a re-register, not a credential.
	p := problems[0]
	detail := p.Detail
	if p.Fix != "" {
		detail += " — " + p.Fix
	}
	return &notReadySkill{Mount: mount, State: facade.RouteBroken, Detail: detail}
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

// markMounted records a skill (name -> facade mount) as mounted, clearing any
// not-ready state it had from an earlier reload (it has just been promoted).
func (r *startReloader) markMounted(name, mount string) {
	r.mu.Lock()
	if r.mounted == nil {
		r.mounted = map[string]string{}
	}
	r.mounted[name] = mount
	delete(r.notReady, name)
	r.mu.Unlock()
}

// markNotReady records why a skill could not be brought up, for the manifest.
func (r *startReloader) markNotReady(name string, st *notReadySkill) {
	r.mu.Lock()
	if r.notReady == nil {
		r.notReady = map[string]*notReadySkill{}
	}
	r.notReady[name] = st
	r.mu.Unlock()
}

// reportNotReady is the whole not-ready treatment for one skill: a stub route
// so a probe gets an actionable status instead of a 404, a manifest entry so the
// agent can see it, and one stderr line per distinct reason for the human.
// Deliberately does NOT markMounted, so a later reload promotes the skill once
// the cause is fixed.
func (r *startReloader) reportNotReady(name, absDir string, st *notReadySkill) {
	r.facade.AddRoute(facade.Route{
		Mount: st.Mount, Skill: name, SkillDir: absDir,
		State: st.State, Detail: st.Detail,
	})
	r.markNotReady(name, st)
	if r.warnOnce(name, st.Detail) {
		fmt.Fprintf(r.env.Stderr, "omac start: skill %s not mounted — %s\n", name, st.Detail)
	}
}

// pruneNotReady drops not-ready state (and its stub route) for skills that have
// left the registry, so a `omac deregister` mid-session doesn't leave the
// manifest advertising a skill that no longer exists.
func (r *startReloader) pruneNotReady(reg *registry.Registry) {
	registered := make(map[string]struct{}, len(reg.Registered))
	for _, e := range reg.Registered {
		registered[e.Name] = struct{}{}
	}
	r.mu.Lock()
	var dropped []*notReadySkill
	for name, st := range r.notReady {
		if _, ok := registered[name]; !ok {
			dropped = append(dropped, st)
			delete(r.notReady, name)
			delete(r.warned, name)
		}
	}
	r.mu.Unlock()
	for _, st := range dropped {
		r.facade.RemoveRoute("", st.Mount)
	}
}

// warnOnce reports true the first time it is called for (name, reason), so a
// repeated reload of the same unready skill does not spam the human's stderr
// while a skill whose reason CHANGED (an approval refusal that became a missing
// secret) still reports the new one.
func (r *startReloader) warnOnce(name, reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, seen := r.warned[name]; seen && last == reason {
		return false
	}
	if r.warned == nil {
		r.warned = map[string]string{}
	}
	r.warned[name] = reason
	return true
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
	stuck := make(map[string]notReadySkill, len(r.notReady))
	for name, st := range r.notReady {
		stuck[name] = *st
	}
	r.mu.Unlock()

	skills := make([]map[string]any, 0, len(pairs)+len(stuck))
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
	// Report unready skills too, in serve's shape (see serve.skillJSON): no
	// base, plus missing/detail. internal/manifest already renders
	// pending-credentials with the `omac secrets set` hint, so the agent's
	// briefing explains a skill it cannot reach instead of pretending it
	// doesn't exist.
	for name, st := range stuck {
		entry := map[string]any{
			"name":  name,
			"scope": "workdir",
			"mount": st.Mount,
			"state": string(st.State),
		}
		if len(st.Missing) > 0 {
			entry["missing"] = st.Missing
		}
		if st.Detail != "" {
			entry["detail"] = st.Detail
		}
		skills = append(skills, entry)
	}
	sort.Slice(skills, func(i, j int) bool {
		return skills[i]["name"].(string) < skills[j]["name"].(string)
	})
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

	// Drop not-ready state for skills that are no longer registered, so
	// `omac deregister` while a session runs doesn't leave the manifest
	// advertising a phantom skill (and internal/manifest telling the agent to
	// run `omac secrets set` for something that no longer exists).
	r.pruneNotReady(reg)

	// MergeConfig tolerates a nil layer, which matters here: the load errors
	// are discarded, and skill-config.yaml is agent-writable.
	wCfg, _ := skillconfig.Load(r.env.Workdir)
	gCfg, _ := skillconfig.LoadGlobal()
	cfgStore := skillstate.MergeConfig(gCfg, wCfg)

	// Same readiness rule as start's preflight and serve's bringUp
	// (internal/skillstate). Reload's own copy used to omit the
	// $default_from_env rung and the env_passthrough fallback entirely and to
	// read any keychain error as "absent", so a skill that started fine under
	// `omac start` could never be brought up live — silently (issue #174,
	// Failure 1). SkipBundleHash: reload has always mounted the current
	// on-disk code and left drift to the approval gate below, which re-derives
	// the hash itself.
	resolver := skillstate.New(skillstate.Options{
		Scope:             keychain.WorkdirID(r.env.Workdir),
		SkipBundleHash:    true,
		SkipSecretPattern: r.skipSecretPattern,
	})
	var added []string

	for _, e := range reg.Registered {
		if r.isMounted(e.Name) {
			continue
		}
		absDir := e.SkillDir
		if !filepath.IsAbs(absDir) {
			absDir = filepath.Join(r.env.Workdir, absDir)
		}

		// Inspect first, Fill after the approval gate: a sidecar that is about
		// to be refused must not cost a keychain read (one blocking
		// authorization prompt per refused skill, on macOS) or have its
		// credentials materialized for nothing.
		armed, problems := resolver.Inspect(e, absDir)
		mount := armed.Mount

		// A broken omac.yaml gets a route and a message like every other
		// not-ready cause. Dropping it silently is the exact bug notReady
		// exists to fix — an agent that had just edited the file would see it
		// vanish from the manifest with no explanation.
		if p := skillstate.First(problems, skillstate.MetaBroken); p != nil {
			r.reportNotReady(e.Name, absDir, &notReadySkill{
				Mount: mount, State: facade.RouteBroken,
				Detail: config.MetaFileName + ": " + p.Detail,
			})
			continue
		}

		// Spawn-approval gate. A live reload is exactly the path a
		// confined agent would use to bring up a skill it authored in the
		// workdir (author code -> forge .opencode/sidecar.json -> POST
		// /__omac__/reload). A sidecar runs UNSANDBOXED, so refuse unless
		// the current on-disk code is host-approved (see
		// internal/skilltrust) and runs from the immutable approval snapshot,
		// not the workdir. Mount a broken route (no markMounted, so a later
		// reload after `omac register` can still bring it up).
		snapDir, refusal := approvedSpawnDir(e.Name, absDir, "")
		if refusal != nil {
			r.facade.AddRoute(brokenApprovalRoute(mount, e.Name, absDir, refusal))
			r.markNotReady(e.Name, &notReadySkill{
				Mount: mount, State: facade.RouteBroken, Detail: refusal.Error(),
			})
			if r.warnOnce(e.Name, refusal.Error()) {
				fmt.Fprintf(r.env.Stderr, "omac start: %s\n", refusalNotice(refusal))
			}
			continue
		}

		problems = append(problems, resolver.Fill(&armed, cfgStore)...)

		// Not ready: install a stub route and say so, rather than skipping in
		// silence. markNotReady deliberately does NOT markMounted, so a later
		// reload (after `omac secrets set`) promotes it — facade.AddRoute
		// overwrites by mount, so promotion needs no teardown.
		if st := reloadStubRoute(mount, problems); st != nil {
			armed.Zero()
			r.reportNotReady(e.Name, absDir, st)
			continue
		}

		m := armed.Meta
		health := config.HealthSpec{}
		if m.Sidecar.Health != nil {
			health = *m.Sidecar.Health
		}
		spec := supervisor.SidecarSpec{
			Name:           e.Name,
			SkillName:      e.Name,
			SkillDir:       snapDir, // run the frozen snapshot, not the workdir
			Command:        m.Sidecar.Command,
			EnvPassthrough: m.Sidecar.EnvPassthrough,
			Secrets:        armed.Secrets,
			Config:         armed.Config,
			Health:         health.Defaults(),
			LogPath:        filepath.Join(r.rtDir, "logs", e.Name+".log"),
			Workdir:        r.env.Workdir,
		}
		running, serr := r.sup.AddSidecar(r.ctx, spec)
		armed.Zero() // the sidecar's env was built synchronously inside AddSidecar
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

package cli

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
	"github.com/TNG/oh-my-agentic-coder/internal/supervisor"
)

const (
	mcpMaxRespawns   = 5
	mcpRespawnWindow = 10 * time.Minute
)

// mcpRespawnBackoff is the wait before the first respawn; it doubles with
// every respawn still inside the window.
var mcpRespawnBackoff = time.Second

// mcpServer is one injected server: the entry it was started from, its
// gateway process, and the respawn history.
type mcpServer struct {
	p    *mcpPlan
	name string

	mu      sync.Mutex
	entry   *mcpreg.Entry
	spec    supervisor.SidecarSpec
	running *supervisor.Running
	recent  []time.Time

	// revive wakes a watcher that gave up, after a refresh restarted the gateway.
	revive chan struct{}
}

// route is the facade route for the current gateway. Callers hold s.mu or
// have exclusive access.
func (s *mcpServer) route() facade.Route {
	return facade.Route{
		Namespace: mcpreg.FacadeNamespace,
		Mount:     s.name,
		Upstream:  s.running.Upstream,
		Skill:     s.name,
		Owner:     "mcp:" + s.name,
		MCP:       s.entry.Def.Policy(),
		State:     facade.RouteReady,
	}
}

// spawn starts the gateway with a fresh secret and port. It first reaps the
// previous process of the same name.
func (s *mcpServer) spawn(ctx context.Context) error {
	s.p.sup.StopSidecar(s.spec.Name, mcpGatewayShutdownTimeout)
	running, err := s.p.sup.AddSidecar(ctx, s.spec)
	if err != nil {
		return err
	}
	s.running = running
	s.spec.KeepLog = true
	return nil
}

func (s *mcpServer) logf(format string, args ...any) {
	f, err := os.OpenFile(s.spec.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[omac] "+format+"\n", args...)
}

// markBroken replaces the route with a broken one and records it in the audit
// trail. Callers hold s.mu.
func (s *mcpServer) markBroken(f *facade.Facade, detail string) {
	r := s.route()
	r.Upstream.Secret = ""
	r.State = facade.RouteBroken
	r.Detail = detail
	f.AddRoute(r)
	s.p.auditor.Emit(audit.RouteStateEvent(s.name, mcpreg.FacadeNamespace, string(facade.RouteBroken), detail))
	s.logf("gave up: %s", detail)
	fmt.Fprintf(s.p.stderr, "%s: MCP server %q gateway is down: %s\n", s.p.prefix, s.name, detail)
}

// watch restarts gateways that die while the session runs. It returns at
// once; the goroutines end when ctx is cancelled or the plan shuts down.
func (p *mcpPlan) watch(ctx context.Context, f *facade.Facade) {
	if p == nil {
		return
	}
	for _, s := range p.servers {
		p.watchers.Add(1)
		go func() {
			defer p.watchers.Done()
			s.supervise(ctx, f)
		}()
	}
}

func (s *mcpServer) supervise(ctx context.Context, f *facade.Facade) {
	for {
		s.mu.Lock()
		cur := s.running
		s.mu.Unlock()
		select {
		case <-cur.Done():
		case <-ctx.Done():
			return
		case <-s.p.stop:
			return
		}
		switch s.recover(ctx, f, cur) {
		case recoverStopped:
			return
		case recoverGaveUp:
			select {
			case <-s.revive:
			case <-ctx.Done():
				return
			case <-s.p.stop:
				return
			}
		}
	}
}

type recoverResult int

const (
	recoverOK recoverResult = iota
	recoverGaveUp
	recoverStopped
)

// recover handles the death of gateway dead: back off, then respawn.
func (s *mcpServer) recover(ctx context.Context, f *facade.Facade, dead *supervisor.Running) recoverResult {
	s.mu.Lock()
	if s.running != dead {
		s.mu.Unlock()
		return recoverOK
	}
	now := time.Now()
	kept := s.recent[:0]
	for _, t := range s.recent {
		if now.Sub(t) < mcpRespawnWindow {
			kept = append(kept, t)
		}
	}
	s.recent = kept
	if len(s.recent) >= mcpMaxRespawns {
		s.markBroken(f, fmt.Sprintf("the gateway died %d times within %s; restart the session", mcpMaxRespawns+1, mcpRespawnWindow))
		s.mu.Unlock()
		return recoverGaveUp
	}
	delay := mcpRespawnBackoff << len(s.recent)
	s.recent = append(s.recent, now)
	s.logf("gateway exited; respawning in %s (respawn %d of %d)", delay, len(s.recent), mcpMaxRespawns)
	s.mu.Unlock()

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return recoverStopped
	case <-s.p.stop:
		return recoverStopped
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running != dead {
		return recoverOK
	}
	select {
	case <-s.p.stop:
		return recoverStopped
	default:
	}
	if err := s.spawn(ctx); err != nil {
		s.logf("respawn failed: %v", err)
		return recoverOK
	}
	f.AddRoute(s.route())
	s.logf("gateway respawned on port %d", s.running.Upstream.Port)
	return recoverOK
}

// restart replaces the gateway with one started from entry.
func (s *mcpServer) restart(ctx context.Context, f *facade.Facade, entry *mcpreg.Entry, spec supervisor.SidecarSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prevEntry, prevSpec := s.entry, s.spec
	s.entry, s.spec = entry, spec
	s.spec.KeepLog = true
	if err := s.spawn(ctx); err != nil {
		s.entry, s.spec = prevEntry, prevSpec
		return err
	}
	s.recent = nil
	f.AddRoute(s.route())
	select {
	case s.revive <- struct{}{}:
	default:
	}
	s.logf("gateway restarted for the changed registration (%s)", entry.BundleHash)
	return nil
}

// refresh brings the running gateways in line with the registry and returns
// notices for the user. A server registered after launch is only reported: the
// harness reads its MCP config once at launch.
func (p *mcpPlan) refresh(ctx context.Context, f *facade.Facade) []string {
	if p == nil || p.sup == nil {
		return nil
	}
	entries, _, err := mcpreg.Effective(p.workdir, p.harness)
	if err != nil {
		return []string{fmt.Sprintf("MCP registry: %v", err)}
	}
	current := map[string]*mcpreg.Entry{}
	for _, e := range entries {
		current[e.Name] = e
	}
	var notices []string
	started := map[string]bool{}
	for _, s := range p.servers {
		started[s.name] = true
		entry, ok := current[s.name]
		if !ok {
			if n := p.once(s.name+"/gone", fmt.Sprintf("MCP server %q is no longer registered; its gateway keeps running until the session ends", s.name)); n != "" {
				notices = append(notices, n)
			}
			continue
		}
		s.mu.Lock()
		unchanged := s.entry.BundleHash == entry.BundleHash
		s.mu.Unlock()
		if unchanged {
			continue
		}
		serverSecrets, ok := p.loadSecrets(&Env{Stderr: p.stderr}, p.prefix, entry)
		if !ok {
			notices = append(notices, fmt.Sprintf("MCP server %q changed but was not restarted: a secret is missing", s.name))
			continue
		}
		if err := s.restart(ctx, f, entry, p.spec(entry, serverSecrets)); err != nil {
			notices = append(notices, fmt.Sprintf("MCP server %q changed but its gateway did not restart: %v", s.name, err))
			continue
		}
		notices = append(notices, fmt.Sprintf("MCP server %q restarted with its updated registration", s.name))
	}
	for _, e := range entries {
		if started[e.Name] {
			continue
		}
		if n := p.once(e.Name+"/new", "restart the session to add "+e.Name); n != "" {
			notices = append(notices, n)
		}
	}
	return notices
}

// once returns msg the first time key is seen and "" afterwards.
func (p *mcpPlan) once(key, msg string) string {
	p.noticeMu.Lock()
	defer p.noticeMu.Unlock()
	if p.noticed == nil {
		p.noticed = map[string]bool{}
	}
	if p.noticed[key] {
		return ""
	}
	p.noticed[key] = true
	return msg
}

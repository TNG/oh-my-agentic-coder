// Package containerproxy implements the mediated Docker-compatible endpoint
// for the JVM build executor (ADR 0002, ticket 08). The executor receives a
// filtered loopback HTTP proxy as DOCKER_HOST=tcp://127.0.0.1:<port>; the
// proxy forwards only the ticket-02 measured allowlist to the existing
// Docker/Colima daemon and fails closed on everything else.
//
// Discipline mirrors internal/credproxy: a loopback HTTP forward server
// with a policy gate, NOT a netproxy CONNECT tunnel. The Docker API needs
// an HTTP-aware filter (read the request to rewrite PortBindings, inject
// the ownership label, validate the create body, enforce the allowlist);
// netproxy.Server is a CONNECT raw-byte tunnel that never reads HTTP, so
// it cannot be reused here (same observation the ticket-06 credential-lift
// proxy made).
//
// The proxy runs host-side, unsandboxed (the daemon socket is host-side;
// the executor never sees it). It authenticates by ownership, not token:
// the DOCKER_HOST URL carries no userinfo — follow-up ops are gated on
// the container carrying this executor's omac.executor=<id> label.
//
// v1 posture: started on macOS (Shape A, env-only network) only, when the
// approved manifest declares container images. On Linux the build executor
// is kernel-blocked, so the proxy is not started. A standard Gradle project
// with no approved images skips the proxy entirely.
package containerproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/stableport"
)

// DefaultUpstreamSocket is the default Docker/Colima daemon socket the
// proxy forwards to when Config.Upstream is empty. Colima on macOS exposes
// the daemon at ~/.colima/default/docker.sock.
const DefaultUpstreamSocket = "unix://" + defaultSocketPath

// defaultSocketPath is the path below HOME to the Colima daemon socket;
// New resolves HOME at call time (HOME + "/" + defaultSocketPath). It is
// a const so the path is stable; only HOME is read at call time.
const defaultSocketPath = ".colima/default/docker.sock"

// portFileName is the .omac-control control-state file recording the
// assigned container-proxy port.
const portFileName = "containerproxy-port"

// Config configures a Proxy.
type Config struct {
	// Upstream is the daemon endpoint the proxy forwards allowed requests
	// to. Empty selects DefaultUpstreamSocket (Colima). May be an
	// http(s):// URL (tests inject an httptest server) or a unix:// URL
	// (production Colima socket).
	Upstream string
	// ApprovedImages is the frozen-for-session manifest image set. The
	// create-body Image field and images/{ref}/json refs must be in this
	// set.
	ApprovedImages []string
	// ExecutorID is the unforgeable ownership label value injected on
	// every create (omac.executor=<id>). Follow-up ops are gated on it.
	ExecutorID string
	// Auditor receives container create/denial/cleanup events. nil → Nop.
	Auditor audit.Auditor
	// Logf is the structured log sink (proxy decisions only; never env
	// values or bodies). nil → discard.
	Logf func(format string, args ...any)
	// WorktreePath is the canonical worktree root the proxy serves. When
	// non-empty, Start derives a STABLE loopback port from it (via
	// stableport.For) so the warm Gradle daemon's cached DOCKER_HOST stays
	// valid across runs — the bug being fixed: a random ephemeral port
	// each run left the warm daemon pointing at a dead port. Empty
	// preserves the legacy random-port behavior.
	WorktreePath string
	// ControlLeaf is the OMAC cache leaf (GRADLE_USER_HOME) where the
	// assigned port is recorded at .omac-control/containerproxy-port so
	// the next run can prefer it. The file is written and read by the
	// SUPERVISOR (unsandboxed); the executor never sees it. Empty
	// disables cross-run port persistence (the port is still stable
	// within a process via the worktree hash, but not across a daemon
	// recycle that re-runs Start).
	ControlLeaf string
}

// Proxy is the mediated Docker endpoint. It binds 127.0.0.1:0, serves the
// v1 allowlist to the executor, and forwards allowed requests to the
// upstream daemon, rewriting PortBindings to loopback and injecting the
// ownership label. It tracks created container IDs and the executor-owned
// network for ownership enforcement and cleanup.
type Proxy struct {
	cfg       Config
	ln        net.Listener
	upstream  *url.URL
	transport *http.Transport
	auditor   audit.Auditor
	logf      func(string, ...any)
	// boundPort is the loopback port Start bound. Tracked so shutdown /
	// diagnostics can report it without re-reading the listener.
	boundPort int

	mu          sync.Mutex
	containers  map[string]containerMeta // id -> metadata (owned)
	networkID   string                   // executor-owned internal network id
	networkName string                   // executor-owned internal network name
	createdNet  bool
	stopOnce    sync.Once
	// buildRequestID is the active build request id threaded from runBuild
	// (ticket 09, spec §254). Non-empty only during a build; set via
	// SetBuildRequestID before the first proxied request so denials carry
	// the correlation prefix naming the active request. The startup
	// scavenger runs WITHOUT a build request id (no active build).
	buildRequestID string
}

// containerMeta is the cached metadata for a container this executor owns.
type containerMeta struct {
	id    string
	image string
	ports []PortMapping
}

// New validates the config and builds a Proxy (does NOT start it — call
// Start). ExecutorID and at least one ApprovedImage are required (the CLI
// skips starting the proxy when no images are approved).
func New(cfg Config) (*Proxy, error) {
	if cfg.ExecutorID == "" {
		return nil, fmt.Errorf("containerproxy: empty executor id")
	}
	if len(cfg.ApprovedImages) == 0 {
		return nil, fmt.Errorf("containerproxy: no approved images")
	}
	up := cfg.Upstream
	if up == "" {
		home, _ := os.UserHomeDir()
		up = "unix://" + home + "/" + defaultSocketPath
	}
	u, err := url.Parse(up)
	if err != nil {
		return nil, fmt.Errorf("containerproxy: parse upstream %q: %w", up, err)
	}
	aud := cfg.Auditor
	if aud == nil {
		aud = audit.Nop()
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	transport := &http.Transport{}
	if u.Scheme == "unix" {
		// Docker over a unix socket: dial the socket path, request URL is
		// http://localhost/<path>.
		sock := u.Path
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}
	}
	return &Proxy{
		cfg:        cfg,
		upstream:   u,
		transport:  transport,
		auditor:    aud,
		logf:       logf,
		containers: map[string]containerMeta{},
	}, nil
}

// SetBuildRequestID threads the active build request id into the proxy so
// container-policy denials are correlated with the active build request
// (ticket 09, spec §254). Call before the first proxied request (the
// build.request event is emitted just before RunBuild). Empty clears it
// (e.g. between builds); the proxy serves one build at a time per worktree.
func (p *Proxy) SetBuildRequestID(id string) {
	p.mu.Lock()
	p.buildRequestID = id
	p.mu.Unlock()
}

// Scavenge removes abandoned executor-owned resources from a PREVIOUS
// crashed executor (same executor id) WITHOUT touching unrelated or
// currently-active resources (ticket 09, checkbox 6). It queries the daemon
// for containers and networks labeled omac.executor=<this-executor-id> and
// DELETEs the matches. It does NOT list untracked resources, trust client
// labels, or touch volumes (volumes are sidecar-owned per ADR 0002 and not
// created by the v1 allowlist).
//
// Safety: the label filter scopes every DELETE to this executor's resources
// only. Start runs Scavenge BEFORE binding the listener, so no client can
// connect until the daemon state is clean — the scavenger cannot race this
// proxy's own in-session tracking. A second proxy with the same id is
// excluded by the per-worktree flock in runBuild (one build at a time per
// worktree), so a same-id proxy racing a scavenge is not a v1 scenario.
//
// Best-effort: daemon errors are logged and audited but do not abort the
// scan. Returns the counts of containers and networks removed.
func (p *Proxy) Scavenge() (containersRemoved, networksRemoved int) {
	containersRemoved = p.scavengeContainers()
	networksRemoved = p.scavengeNetworks()
	p.auditor.Emit(audit.ControlMutation("container.scavenge.summary", "",
		fmt.Sprintf("executor=%s containers=%d networks=%d force=true", p.cfg.ExecutorID, containersRemoved, networksRemoved)))
	return
}

// scavengeContainers removes abandoned containers labeled with this
// executor's ownership label. It uses GET /containers/json with a
// server-side label filter (the same ownership-label convention as the
// runtime proxy) so ONLY this executor's abandoned containers are returned;
// unrelated host containers are never listed and never deleted.
func (p *Proxy) scavengeContainers() int {
	filters := map[string][]string{"label": {OwnershipLabelKey + "=" + p.cfg.ExecutorID}}
	encoded, _ := json.Marshal(filters)
	q := url.Values{}
	q.Set("all", "true")
	q.Set("filters", string(encoded))
	req, err := http.NewRequest(http.MethodGet, p.upstreamURL("/containers/json?"+q.Encode()), nil)
	if err != nil {
		return 0
	}
	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		p.logf("containerproxy: scavenge containers list: %v", err)
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		p.logf("containerproxy: scavenge containers list status %d", resp.StatusCode)
		return 0
	}
	b, _ := io.ReadAll(resp.Body)
	var listed []struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(b, &listed); err != nil {
		p.logf("containerproxy: scavenge containers parse: %v", err)
		return 0
	}
	removed := 0
	for _, c := range listed {
		if c.ID == "" {
			continue
		}
		p.deleteContainer(c.ID, true)
		p.auditor.Emit(audit.ControlMutation("container.scavenge", "",
			fmt.Sprintf("executor=%s id=%s result=removed force=true", p.cfg.ExecutorID, c.ID)))
		removed++
	}
	return removed
}

// scavengeNetworks removes abandoned networks labeled with this executor's
// ownership label (the same label ensureNetwork sets on the executor-owned
// internal network). A crashed prior run may have left its network behind;
// this reclaims it so the new run can create a fresh one (ensureNetwork
// treats a name-conflict 409 as a soft failure and would otherwise leave
// containers on the default bridge).
func (p *Proxy) scavengeNetworks() int {
	filters := map[string][]string{"label": {OwnershipLabelKey + "=" + p.cfg.ExecutorID}}
	encoded, _ := json.Marshal(filters)
	q := url.Values{}
	q.Set("filters", string(encoded))
	req, err := http.NewRequest(http.MethodGet, p.upstreamURL("/networks?"+q.Encode()), nil)
	if err != nil {
		return 0
	}
	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		p.logf("containerproxy: scavenge networks list: %v", err)
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		p.logf("containerproxy: scavenge networks list status %d", resp.StatusCode)
		return 0
	}
	b, _ := io.ReadAll(resp.Body)
	var listed []struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(b, &listed); err != nil {
		p.logf("containerproxy: scavenge networks parse: %v", err)
		return 0
	}
	removed := 0
	for _, n := range listed {
		if n.ID == "" {
			continue
		}
		p.removeNetwork(n.ID)
		p.auditor.Emit(audit.ControlMutation("container.scavenge", "",
			fmt.Sprintf("executor=%s network=%s result=removed force=true", p.cfg.ExecutorID, n.ID)))
		removed++
	}
	return removed
}

// Start runs the startup scavenger (ticket 09, checkbox 6 — removes
// abandoned resources from a PREVIOUS crashed executor with the same id,
// without touching unrelated resources), THEN binds the loopback listener
// and serves in a goroutine. Scavenging BEFORE the bind eliminates the
// race between the scavenger and the new session's first request: once
// net.Listen returns, the kernel queues inbound connections immediately,
// so a client (Testcontainers) racing to connect could dispatch a
// /containers/create → attachToNetwork while the scavenger's stale
// /networks snapshot is still being iterated — and the network scavenger
// could removeNetwork a network the just-attached container is on.
// Scavenging before the bind closes that window entirely: no client can
// connect until the daemon state is clean. A bind failure is independent
// of the daemon, so the error path is unaffected.
//
// Returns the DOCKER_HOST URL the executor is pointed at
// (tcp://127.0.0.1:<port>) and a stop func that tears down the listener
// and runs Cleanup (best-effort removal of this session's owned containers
// + the executor network). Scavenger errors are best-effort: a daemon
// that is down at startup is logged and the proxy still starts (the build
// will fail fast on the first proxied request instead).
func (p *Proxy) Start() (dockerHost string, stop func(), err error) {
	// Scavenge BEFORE binding so no client can connect until the daemon
	// state is clean (eliminates the first-request/scavenger race — see
	// the doc comment above). Best-effort; logged + audited.
	cRemoved, nRemoved := p.Scavenge()
	if cRemoved > 0 || nRemoved > 0 {
		p.logf("containerproxy: scavenged %d container(s) and %d network(s) from a previous executor", cRemoved, nRemoved)
	}
	port, fallback := p.choosePort()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		// The chosen port (stable or random) was not bindable; retry once
		// with a kernel-assigned ephemeral port so a transient bind race
		// or a stale control file pointing at an in-use port never wedges
		// the build. Correctness over determinism.
		if port != 0 {
			p.logf("containerproxy: bind on stable port %d failed (%v); falling back to a random ephemeral port", port, err)
			ln, err = net.Listen("tcp", "127.0.0.1:0")
		}
		if err != nil {
			return "", nil, fmt.Errorf("containerproxy: bind listener: %w", err)
		}
		fallback = true
	}
	p.ln = ln
	p.boundPort = ln.Addr().(*net.TCPAddr).Port
	if fallback {
		p.logf("containerproxy: using fallback ephemeral port %d (stable window unavailable; warm-daemon DOCKER_HOST may drift on next run)", p.boundPort)
	}
	// Persist the assigned port so the next run can prefer it. Any port
	// inside [StablePortMin, StablePortMax) — preferred OR a scanned
	// neighbor — is persisted so the next run prefers exactly what this run
	// bound, breaking the permanent warn-loop where a scanned neighbor is
	// treated as "fallback" and never persisted (issue #191). A true
	// out-of-window random/ephemeral port is NOT persisted because it would
	// poison the control file — the next run would prefer a dead-ephemeral
	// or out-of-range value and destabilize again. Best-effort: a write
	// failure degrades cross-run stability but does not fail the build (the
	// port is valid for this run).
	if p.cfg.ControlLeaf != "" && !fallback {
		if werr := stableport.WritePreferred(p.cfg.ControlLeaf, portFileName, p.boundPort); werr != nil {
			p.logf("containerproxy: could not persist port file: %v", werr)
		}
	}
	go p.acceptLoop()
	dockerHost = fmt.Sprintf("tcp://127.0.0.1:%d", p.boundPort)
	return dockerHost, p.shutdown, nil
}

// choosePort resolves the loopback port Start should bind. It prefers, in
// order: (1) a previously-assigned port read from the control-state file
// (so the port stays stable even after the listener is torn down between
// runs); (2) a fresh stable port derived from the worktree path; (3) a
// fallback random ephemeral port when the whole stable window is occupied.
// Returns the chosen port and a fallback flag (true when the chosen port
// is NOT the deterministic stable one — the caller logs a warning so the
// user understands the warm-daemon bug may resurface in the rare collision
// case). When WorktreePath is empty the legacy random-port behavior is
// used (port 0, not flagged as fallback — that is the documented v1 path).
func (p *Proxy) choosePort() (port int, fallback bool) {
	if p.cfg.WorktreePath == "" {
		// Legacy random-port behavior preserved for callers that did not
		// wire the worktree path.
		return 0, false
	}
	preferred := 0
	if p.cfg.ControlLeaf != "" {
		preferred = stableport.ReadPreferred(p.cfg.ControlLeaf, portFileName)
	}
	if preferred == 0 {
		preferred = stableport.For(p.cfg.WorktreePath)
	}
	chosen := stableport.Select(preferred, stableport.IsFree, stableport.RandomFree)
	if chosen == 0 {
		// stableport.Select exhausted the window AND the random fallback failed.
		// Let the kernel pick (Start retries on 127.0.0.1:0).
		return 0, true
	}
	// "Fallback" means we are outside the stable port window
	// [StablePortMin, StablePortMax) — i.e. a random kernel-assigned port.
	// A scanned neighbor inside the window is NOT a fallback: it is
	// persisted so the next run prefers exactly what this run bound,
	// breaking the permanent warn-loop (issue #191). A true out-of-window
	// random port is NOT persisted because it would poison the control
	// file for the next run.
	return chosen, chosen < stableport.StablePortMin || chosen >= stableport.StablePortMax
}

// shutdown is the stop func returned by Start. It closes the listener and
// runs Cleanup (best-effort). Safe to call more than once.
func (p *Proxy) shutdown() {
	p.stopOnce.Do(func() {
		if p.ln != nil {
			_ = p.ln.Close()
		}
		p.Cleanup()
	})
}

func (p *Proxy) acceptLoop() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			p.handle(conn)
		}()
	}
}

// requestTimeout bounds a single proxied request end-to-end. Image pulls
// can be slow on a cold daemon; keep it generous.
const requestTimeout = 10 * time.Minute

// handle serves one HTTP/1.1 request from the executor (origin-form).
// Docker clients send one request per connection (HTTP/1.1 keep-alive is
// not required for Testcontainers), so we read one request head + body,
// dispatch, and close.
func (p *Proxy) handle(conn net.Conn) {
	conn.SetDeadline(time.Now().Add(requestTimeout))
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	body, _ := io.ReadAll(req.Body)
	req.Body.Close()
	p.serve(conn, req, body)
}

// serve is the policy gate + forwarder. It decides the allowlist verdict,
// validates/rewrites the create body, enforces ownership on follow-up ops,
// rewrites the containers/json filter, and forwards allowed requests to
// the upstream daemon. Denials are rendered as a JSON Docker-API-style
// error response with an omac message field AND a typed
// *ContainerPolicyError emitted to the audit trail.
func (p *Proxy) serve(conn net.Conn, req *http.Request, body []byte) {
	d := decideAllowlist(req.Method, req.URL.Path)
	if !d.allowed {
		p.deny(conn, req, &ContainerPolicyError{
			Kind:   KindUnknownEndpoint,
			Reason: req.Method + " " + req.URL.Path,
		})
		return
	}

	// /images/create: allow only when fromImage ∈ approved set; deny
	// X-Registry-Auth.
	if d.rule == "images.create" {
		fromImage := req.URL.Query().Get("fromImage")
		if isRyukImage(fromImage) {
			p.deny(conn, req, &ContainerPolicyError{Kind: KindRyukForbidden, Image: fromImage})
			return
		}
		if !imageApproved(fromImage, p.cfg.ApprovedImages) {
			p.deny(conn, req, &ContainerPolicyError{Kind: KindUnapprovedImage, Image: fromImage})
			return
		}
		if req.Header.Get("X-Registry-Auth") != "" {
			p.deny(conn, req, &ContainerPolicyError{Kind: KindRegistryAuthForbidden, Reason: "X-Registry-Auth denied"})
			return
		}
		p.forward(conn, req, body, d)
		return
	}

	// /images/{ref}/json: allow only for approved refs. When the ref is
	// a digest (sha256:...), the daemon has already resolved an approved
	// tag to that digest (the pull via /images/create is the security
	// boundary, validated above); resolve the digest back to its RepoTags
	// via a daemon sub-request and allow if ANY RepoTag matches the
	// approved set. This handles Testcontainers' inspect-by-digest flow.
	if d.rule == "image.inspect" {
		if isRyukImage(d.imageRef) {
			p.deny(conn, req, &ContainerPolicyError{Kind: KindRyukForbidden, Image: d.imageRef})
			return
		}
		if !imageApproved(d.imageRef, p.cfg.ApprovedImages) {
			// Digest ref: resolve via the daemon's image metadata.
			// The ref is the image's content digest; the daemon knows
			// which RepoTags point at it. If any approved tag matches,
			// the inspect is for an approved image.
			if strings.HasPrefix(d.imageRef, "sha256:") {
				if !p.digestApprovedByRepoTags(d.imageRef) {
					p.deny(conn, req, &ContainerPolicyError{Kind: KindUnapprovedImage, Image: d.imageRef})
					return
				}
			} else {
				p.deny(conn, req, &ContainerPolicyError{Kind: KindUnapprovedImage, Image: d.imageRef})
				return
			}
		}
		p.forward(conn, req, body, d)
		return
	}

	// /containers/json: rewrite the label filter server-side.
	if d.rule == "containers.list" {
		req.URL.RawQuery = rewriteContainersListFilter(req.URL.RawQuery, p.cfg.ExecutorID)
		p.forward(conn, req, body, d)
		return
	}

	// /networks/prune: inject the ownership label filter so only THIS
	// executor's networks are pruned. The client's filter (if any) is
	// dropped and replaced, matching the containers.list ownership model
	// (client filters are forgeable; the proxy enforces, not trusts).
	if d.rule == "networks.prune" {
		req.URL.RawQuery = rewriteNetworksPruneFilter(req.URL.RawQuery, p.cfg.ExecutorID)
		p.forward(conn, req, body, d)
		return
	}

	// /volumes/prune: same JVMHookResourceReaper shutdown hook, same
	// ownership-label-filter scoping as /networks/prune. Only THIS
	// executor's volumes are pruned.
	if d.rule == "volumes.prune" {
		req.URL.RawQuery = rewriteVolumesPruneFilter(req.URL.RawQuery, p.cfg.ExecutorID)
		p.forward(conn, req, body, d)
		return
	}

	// /images/prune: third prune endpoint the JVMHookResourceReaper
	// shutdown hook calls. Same ownership-label-filter scoping.
	if d.rule == "images.prune" {
		req.URL.RawQuery = rewriteImagesPruneFilter(req.URL.RawQuery, p.cfg.ExecutorID)
		p.forward(conn, req, body, d)
		return
	}

	// /containers/create: validate + rewrite the body.
	if d.rule == "containers.create" {
		rewritten, perr := validateCreateBody(body, p.cfg.ApprovedImages, p.cfg.ExecutorID)
		if perr != nil {
			p.deny(conn, req, perr)
			return
		}
		// Forward the rewritten body; capture the created Id to track
		// ownership and register ports.
		p.forwardCreate(conn, req, rewritten)
		return
	}

	// Ownership-scoped rules: start/kill/wait/inspect/logs/delete.
	if isOwnershipScopedRule(d.rule) {
		if !p.owned(d.containerID, req) {
			p.deny(conn, req, &ContainerPolicyError{
				Kind:        KindNotOwnedByExecutor,
				ContainerID: d.containerID,
			})
			return
		}
		p.forward(conn, req, body, d)
		return
	}

	// ping / version / info / images.json: pass through.
	p.forward(conn, req, body, d)
}

// owned reports whether the container id carries this executor's ownership
// label. It consults the in-memory created-containers map first (fast
// path); if absent it queries the daemon via GET /containers/{id}/json and
// caches the result. One executor cannot reach another's containers.
func (p *Proxy) owned(id string, req *http.Request) bool {
	p.mu.Lock()
	if _, ok := p.containers[id]; ok {
		p.mu.Unlock()
		return true
	}
	p.mu.Unlock()
	// Inspect via the daemon. Use a fresh request (not the caller's).
	inspectReq, err := http.NewRequest(http.MethodGet, p.upstreamURL("/containers/"+id+"/json"), nil)
	if err != nil {
		return false
	}
	resp, err := p.transport.RoundTrip(inspectReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return false
	}
	inspectBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// Docker's GET /containers/{id}/json (inspect) nests labels at
	// Config.Labels ONLY — top-level Labels is absent on inspect
	// responses. (The create response carries no Labels at all — it is
	// just {"Id":...,"Warnings":[]}, and the in-memory p.containers fast
	// path handles ownership for proxy-created containers without parsing.)
	// Parsing top-level Labels as a fallback would let a fake/buggy daemon
	// satisfy ownership with a forgeable top-level label, so we read
	// Config.Labels ONLY and fail closed if it is absent. This is the
	// critical parse fix (review critical #1): the previous code read
	// top-level Labels and false-denied every non-cached inspect against a
	// real daemon; this reads Config.Labels and correctly enforces.
	var meta struct {
		Config struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(inspectBody, &meta); err != nil {
		return false
	}
	labels := meta.Config.Labels
	if labels[OwnershipLabelKey] != p.cfg.ExecutorID {
		return false
	}
	p.mu.Lock()
	p.containers[id] = containerMeta{id: id, image: meta.Config.Image, ports: extractPublishedPorts(inspectBody)}
	p.mu.Unlock()
	return true
}

// forwardCreate forwards a (rewritten) create body and, on a 2xx response,
// captures the created container Id, registers its published ports, and
// attaches it to the executor-owned internal network.
func (p *Proxy) forwardCreate(conn net.Conn, req *http.Request, body []byte) {
	upReq, err := http.NewRequest(req.Method, p.upstreamURL("/containers/create"), strings.NewReader(string(body)))
	if err != nil {
		p.deny(conn, req, &ContainerPolicyError{Kind: KindUnknownEndpoint, Reason: "build upstream create request"})
		return
	}
	copyForwardHeaders(upReq.Header, req.Header)
	upReq.Header.Set("Content-Type", "application/json")
	resp, err := p.transport.RoundTrip(upReq)
	if err != nil {
		p.logf("containerproxy: upstream create error: %v", err)
		p.deny(conn, req, &ContainerPolicyError{Kind: KindUnknownEndpoint, Reason: "upstream unreachable"})
		return
	}
	defer resp.Body.Close()
	// Stream the response back to the client first.
	respBytes, _ := io.ReadAll(resp.Body)
	writeRawResponse(conn, resp.Status, resp.Header, respBytes)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	// Capture the created Id.
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(respBytes, &created); err != nil || created.ID == "" {
		return
	}
	// Register the id in p.containers SYNCHRONOUSLY (under the lock)
	// BEFORE the post-response inspect/attach so Cleanup cannot orphan it
	// and a concurrent follow-up op (start/inspect) sees it. The metadata
	// is enriched (image, ports) after the inspect below; a "pending"
	// entry with an empty image is safe — the audit redacts an empty
	// image and the ownership fast-path only needs the id present.
	p.mu.Lock()
	p.containers[created.ID] = containerMeta{id: created.ID}
	p.mu.Unlock()
	// Inspect to get the published ports + image. Done after tracking so
	// the tracked metadata is complete; imageForUnlocked is called under
	// the lock below (NOT imageFor — sync.Mutex is not reentrant).
	ports, image := p.inspectAndRegister(created.ID)
	p.mu.Lock()
	if entry, ok := p.containers[created.ID]; ok {
		entry.image = image
		entry.ports = ports
		p.containers[created.ID] = entry
	}
	p.mu.Unlock()
	p.auditor.Emit(audit.ControlMutation("container.create", "", fmt.Sprintf(
		"executor=%s image=%s id=%s ports=%s",
		p.cfg.ExecutorID, redactImage(image), created.ID, fmtPortMappings(ports))))
	// Attach to the executor-owned internal network. If attach fails the
	// container MUST NOT run on the default bridge (which has an outbound
	// route) — kill + delete it and audit the denial (checkbox 5).
	if err := p.attachToNetwork(created.ID); err != nil {
		p.logf("containerproxy: network attach failed for %s, killing+removing: %v", created.ID, err)
		p.deleteContainer(created.ID, true)
		p.mu.Lock()
		delete(p.containers, created.ID)
		p.mu.Unlock()
		p.auditor.Emit(audit.ControlMutation("container.denied", "", fmt.Sprintf(
			"executor=%s id=%s kind=%v reason=network attach failed: %v",
			p.cfg.ExecutorID, created.ID, KindHostNamespaceForbidden, err)))
	}
}

// inspectAndRegister fetches the container's published ports and image
// from the daemon. Best-effort: a failed inspect yields empty values.
func (p *Proxy) inspectAndRegister(id string) (ports []PortMapping, image string) {
	inspectReq, err := http.NewRequest(http.MethodGet, p.upstreamURL("/containers/"+id+"/json"), nil)
	if err != nil {
		return nil, ""
	}
	resp, err := p.transport.RoundTrip(inspectReq)
	if err != nil {
		return nil, ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var meta struct {
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
	}
	_ = json.Unmarshal(b, &meta)
	return extractPublishedPorts(b), meta.Config.Image
}

// imageFor returns the cached image for a container id.
func (p *Proxy) imageFor(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.imageForUnlocked(id)
}

// imageForUnlocked is imageFor without the lock; callers already holding
// p.mu must use this (sync.Mutex is not reentrant).
func (p *Proxy) imageForUnlocked(id string) string {
	if m, ok := p.containers[id]; ok {
		return m.image
	}
	return ""
}

// forward proxies a request to the upstream daemon verbatim (the body was
// already validated/rewritten where applicable). For streaming responses
// (upstream uses chunked transfer encoding OR no Content-Length, which is
// what GET /containers/{id}/logs?follow=true returns), the response body
// is streamed to the client with chunked transfer encoding instead of
// being buffered. Buffering a live stream (io.ReadAll) blocks forever
// waiting for EOF, so Testcontainers' log-follow never receives any data
// and times out. The logs endpoint is the primary streaming case; other
// endpoints return finite bodies and take the buffered path.
func (p *Proxy) forward(conn net.Conn, req *http.Request, body []byte, d endpointDecision) {
	upReq, err := http.NewRequest(req.Method, p.upstreamURL(req.URL.Path), strings.NewReader(string(body)))
	if err != nil {
		p.deny(conn, req, &ContainerPolicyError{Kind: KindUnknownEndpoint, Reason: "build upstream request"})
		return
	}
	upReq.URL.RawQuery = req.URL.RawQuery
	copyForwardHeaders(upReq.Header, req.Header)
	if len(body) > 0 {
		upReq.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.transport.RoundTrip(upReq)
	if err != nil {
		p.logf("containerproxy: upstream error %s %s: %v", req.Method, req.URL.Path, err)
		p.deny(conn, req, &ContainerPolicyError{Kind: KindUnknownEndpoint, Reason: "upstream unreachable"})
		return
	}
	defer resp.Body.Close()
	// Streaming response: the daemon uses chunked encoding (no
	// Content-Length) for /logs?follow=true and similar endpoints.
	// Stream the body to the client with chunked transfer encoding
	// instead of buffering (which would block forever on a live stream).
	// A response with a Content-Length header is finite → buffer it.
	// Go's http.Transport strips Transfer-Encoding from resp.Header and
	// presents a streaming resp.Body; the signature of a chunked upstream
	// is the ABSENCE of Content-Length.
	if resp.Header.Get("Content-Length") == "" {
		p.streamResponse(conn, resp)
		return
	}
	respBytes, _ := io.ReadAll(resp.Body)
	writeRawResponse(conn, resp.Status, resp.Header, respBytes)
}

// streamResponse writes the response status + headers (minus hop-by-hop
// headers, same as writeRawResponse) and then streams the response body
// to the client using HTTP/1.1 chunked transfer encoding. Used for
// streaming endpoints (logs?follow=true) where the upstream body has no
// Content-Length and may stay open indefinitely. The conn deadline set in
// handle() bounds the stream; Testcontainers closes the connection when
// it has seen enough log output, which causes the copy to return.
func (p *Proxy) streamResponse(conn net.Conn, resp *http.Response) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "HTTP/1.1 %s\r\n", resp.Status)
	for k, vs := range resp.Header {
		switch strings.ToLower(k) {
		case "connection", "keep-alive", "te", "trailer",
			"transfer-encoding", "upgrade", "content-length":
			continue
		}
		for _, v := range vs {
			fmt.Fprintf(&sb, "%s: %s\r\n", k, v)
		}
	}
	sb.WriteString("X-Omac-Sandbox: denied\r\n")
	sb.WriteString("Transfer-Encoding: chunked\r\n")
	sb.WriteString("Connection: close\r\n\r\n")
	_, _ = conn.Write([]byte(sb.String()))
	// Stream chunks: for each read, write the chunk size (hex) + CRLF +
	// data + CRLF. An empty read (EOF) writes the terminating 0-length
	// chunk. Errors are best-effort (the client may have closed first).
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			fmt.Fprintf(conn, "%x\r\n", n)
			_, _ = conn.Write(buf[:n])
			_, _ = conn.Write([]byte("\r\n"))
		}
		if err != nil {
			_, _ = conn.Write([]byte("0\r\n\r\n"))
			return
		}
	}
}

// upstreamURL builds the URL for an upstream request. For a unix socket
// the host is "localhost" (the DialContext ignores it); for an http(s)
// upstream it is the real host.
func (p *Proxy) upstreamURL(path string) string {
	if p.upstream.Scheme == "unix" {
		return "http://localhost" + path
	}
	return p.upstream.String() + path
}

// digestApprovedByRepoTags resolves an image content digest (sha256:...)
// back to its RepoTags via a daemon GET /images/{digest}/json sub-request,
// then reports whether ANY RepoTag matches the approved image set. This
// backs the image.inspect path: Testcontainers inspects by digest after
// the daemon resolved an approved tag to that digest. The pull (via
// /images/create, validated at the security boundary) is what authorized
// the image; this resolution just maps the digest back to the tag the
// manifest approved. Best-effort: on a daemon error it returns false
// (fail-closed — the inspect is denied, which surfaces as a clear
// Testcontainers failure rather than a silent allow).
func (p *Proxy) digestApprovedByRepoTags(digest string) bool {
	req, err := http.NewRequest(http.MethodGet, p.upstreamURL("/images/"+digest+"/json"), nil)
	if err != nil {
		return false
	}
	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		p.logf("containerproxy: digest RepoTags lookup failed for %s: %v", digest, err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		p.logf("containerproxy: digest RepoTags lookup for %s: daemon status %d", digest, resp.StatusCode)
		return false
	}
	var meta struct {
		RepoTags []string `json:"RepoTags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return false
	}
	for _, tag := range meta.RepoTags {
		if imageApproved(tag, p.cfg.ApprovedImages) {
			return true
		}
	}
	return false
}

// deny writes a JSON Docker-API-style error response to the client with an
// `omac` message field, marks the response X-Omac-Sandbox, AND emits the
// typed *ContainerPolicyError to the audit trail (spec §254 — correlate
// low-level denials with the active build request). Never credential values.
//
// Ticket 09: the active build request id is stamped onto the error so the
// rendered diagnostic (and the audit event) names the active request. The
// correlation prefix is the FIRST line of the rendered message so a
// Gradle/log reader sees the OMAC cause before any Testcontainers wrapping.
func (p *Proxy) deny(conn net.Conn, req *http.Request, perr *ContainerPolicyError) {
	p.mu.Lock()
	perr.BuildRequestID = p.buildRequestID
	p.mu.Unlock()
	p.logf("containerproxy: DENY %s %s: %s", req.Method, req.URL.Path, perr.Render())
	p.auditor.Emit(audit.ControlMutation("container.denied", "",
		fmt.Sprintf("executor=%s request=%s method=%s path=%s kind=%s image=%s id=%s",
			p.cfg.ExecutorID, perr.BuildRequestID, req.Method, req.URL.Path, perr.Kind, redactImage(perr.Image), perr.ContainerID)))
	payload := map[string]any{
		"message": perr.Render(),
		"omac":    perr.Render(),
	}
	body, _ := json.Marshal(payload)
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	writeRawResponse(conn, "403 Forbidden", hdr, body)
}

// copyForwardHeaders copies request headers that should reach upstream,
// dropping hop-by-hop headers AND credential-bearing headers build code
// must not send in v1. Mirrors credproxy.copyForwardHeaders for the
// hop-by-hop set; additionally strips X-Registry-Auth so a build cannot
// leak a private-registry credential to the daemon on /containers/create
// (the images/create path denies it explicitly with a structured error;
// stripping here closes the create-container bypass — private registry
// auth is issue #92 territory and the v1 filter denies it everywhere).
func copyForwardHeaders(dst, src http.Header) {
	for k, vs := range src {
		switch strings.ToLower(k) {
		case "connection", "keep-alive", "te", "trailer",
			"transfer-encoding", "upgrade", "host":
			continue
		case "x-registry-auth":
			// Credential-bearing header; never forwarded in v1.
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// writeRawResponse writes a full HTTP/1.1 response back to the client
// (status line, headers, body). Connection: close — no keep-alive.
func writeRawResponse(conn net.Conn, status string, hdr http.Header, body []byte) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "HTTP/1.1 %s\r\n", status)
	for k, vs := range hdr {
		switch strings.ToLower(k) {
		case "connection", "keep-alive", "te", "trailer",
			"transfer-encoding", "upgrade":
			continue
		}
		for _, v := range vs {
			fmt.Fprintf(&sb, "%s: %s\r\n", k, v)
		}
	}
	sb.WriteString("X-Omac-Sandbox: denied\r\n")
	// Only set Content-Length if the upstream did not set it (avoid
	// duplicate). A chunked upstream (no Content-Length) gets a computed
	// one here so the client can read the body.
	if hdr.Get("Content-Length") == "" {
		fmt.Fprintf(&sb, "Content-Length: %d\r\n", len(body))
	}
	sb.WriteString("Connection: close\r\n\r\n")
	_, _ = conn.Write([]byte(sb.String()))
	_, _ = conn.Write(body)
}

// Cleanup removes executor-owned containers and the executor-owned internal
// network without touching unrelated resources (checkbox 7). Best-effort:
// errors are logged but do not abort the cleanup loop. Safe to call with
// a nil receiver or after shutdown.
func (p *Proxy) Cleanup() {
	if p == nil {
		return
	}
	p.mu.Lock()
	ids := make([]containerMeta, 0, len(p.containers))
	for _, m := range p.containers {
		ids = append(ids, m)
	}
	netID := p.networkID
	p.containers = map[string]containerMeta{}
	p.mu.Unlock()
	// Remove each owned container (force=true, v=true so a running
	// container is killed and its volumes removed).
	for _, m := range ids {
		p.deleteContainer(m.id, true)
		p.auditor.Emit(audit.ControlMutation("container.cleanup", "",
			fmt.Sprintf("executor=%s id=%s result=removed", p.cfg.ExecutorID, m.id)))
	}
	// Disconnect + remove the executor-owned network.
	if netID != "" {
		p.removeNetwork(netID)
	}
}

// deleteContainer sends DELETE /containers/{id}?force=true&v=true to the
// daemon. Best-effort.
func (p *Proxy) deleteContainer(id string, force bool) {
	q := url.Values{}
	if force {
		q.Set("force", "true")
		q.Set("v", "true")
	}
	path := "/containers/" + id
	if q := q.Encode(); q != "" {
		path += "?" + q
	}
	req, err := http.NewRequest(http.MethodDelete, p.upstreamURL(path), nil)
	if err != nil {
		return
	}
	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		p.logf("containerproxy: cleanup delete %s: %v", id, err)
		return
	}
	resp.Body.Close()
}

// --- executor-owned internal network (checkbox 5) -----------------------

// ensureNetwork creates the executor-owned internal network (no outbound
// route, labeled omac.executor=<id>) if it does not yet exist. Called
// lazily by attachToNetwork. The network endpoints are NOT exposed to the
// executor's allowlist (they are host-side proxy operations).
func (p *Proxy) ensureNetwork() {
	p.mu.Lock()
	if p.createdNet {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	name := "omac-" + p.cfg.ExecutorID
	body := map[string]any{
		"Name":       name,
		"Labels":     map[string]string{OwnershipLabelKey: p.cfg.ExecutorID},
		"Internal":   true,
		"EnableIPv6": false,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, p.upstreamURL("/networks/create"), strings.NewReader(string(raw)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		p.logf("containerproxy: create executor network: %v", err)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		p.logf("containerproxy: create executor network status %d: %s", resp.StatusCode, b)
		return
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(b, &created); err != nil || created.ID == "" {
		return
	}
	p.mu.Lock()
	p.networkID = created.ID
	p.networkName = name
	p.createdNet = true
	p.mu.Unlock()
}

// attachToNetwork connects a container to the executor-owned network.
// Returns an error if the container could not be attached (network
// missing or daemon refused); the caller (forwardCreate) MUST kill+delete
// the container on error so it cannot run on the default bridge, which
// has an outbound route — violating checkbox 5 ("internal network with no
// outbound route"). Silent fallback to the default bridge is a security
// failure, not an acceptable best-effort.
func (p *Proxy) attachToNetwork(containerID string) error {
	p.ensureNetwork()
	p.mu.Lock()
	netID := p.networkID
	p.mu.Unlock()
	if netID == "" {
		return fmt.Errorf("executor-owned internal network unavailable")
	}
	body := map[string]any{"Container": containerID}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, p.upstreamURL("/networks/"+netID+"/connect"), strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		return fmt.Errorf("attach %s to network %s: %w", containerID, netID, err)
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("attach %s to network %s: daemon status %d", containerID, netID, resp.StatusCode)
	}
	return nil
}

// removeNetwork disconnects containers and removes the executor-owned network.
func (p *Proxy) removeNetwork(netID string) {
	req, err := http.NewRequest(http.MethodDelete, p.upstreamURL("/networks/"+netID), nil)
	if err != nil {
		return
	}
	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		p.logf("containerproxy: remove network %s: %v", netID, err)
		return
	}
	resp.Body.Close()
}

// redactImage is a placeholder for env-value redaction in audit: image
// refs are non-secret, so we pass them through. Env VALUES (POSTGRES_PASSWORD
// etc.) are never audited — only the image ref and port mappings are.
func redactImage(image string) string { return image }

// queryEscape / queryUnescape wrap net/url for the policy package.
func queryEscape(s string) string            { return url.QueryEscape(s) }
func queryUnescape(s string) (string, error) { return url.QueryUnescape(s) }

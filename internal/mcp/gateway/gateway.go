// Package gateway exposes a local stdio MCP server as Streamable HTTP
// (MCP revision 2025-06-18) behind the omac facade.
package gateway

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
	"github.com/TNG/oh-my-agentic-coder/internal/sidecarauth"
)

const (
	sessionHeader     = "Mcp-Session-Id"
	maxBodyBytes      = 16 << 20
	eventQueueSize    = 256
	keepaliveInterval = 15 * time.Second
	killWait          = 2 * time.Second

	defaultMaxSessions = 32
	defaultIdleTimeout = 30 * time.Minute
)

// Options configures a gateway for one stdio MCP server.
type Options struct {
	Def *mcpreg.Definition
	// Policy is enforced on every message and compared with each session's
	// live tool list. It is required and is not modified.
	Policy *mcppolicy.Policy
	// Dir is the working directory of every child process.
	Dir    string
	Secret string
	// ChildEnv is the environment the children derive theirs from. OMAC_* and
	// SIDECAR_* variables are removed; everything else, including declared
	// secrets the caller added, reaches the child.
	ChildEnv    []string
	Log         io.Writer
	MaxSessions int
	IdleTimeout time.Duration
}

type gateway struct {
	opts      Options
	env       []string
	mu        sync.Mutex
	sessions  map[string]*session
	closed    bool
	stopReap  chan struct{}
	keepalive time.Duration
}

// Handler returns the HTTP handler and a function that ends all sessions and
// kills their children.
func Handler(o Options) (http.Handler, func(), error) {
	g, err := newGateway(o)
	if err != nil {
		return nil, nil, err
	}
	h, err := g.handler()
	if err != nil {
		return nil, nil, err
	}
	go g.reap()
	return h, g.shutdown, nil
}

func newGateway(o Options) (*gateway, error) {
	if o.Def == nil || len(o.Def.Server.Command) == 0 {
		return nil, errors.New("gateway: definition without a server command")
	}
	if o.Policy == nil {
		return nil, errors.New("gateway: no tool policy")
	}
	if o.MaxSessions <= 0 {
		o.MaxSessions = defaultMaxSessions
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = defaultIdleTimeout
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	return &gateway{
		opts:      o,
		env:       childEnv(o),
		sessions:  map[string]*session{},
		stopReap:  make(chan struct{}),
		keepalive: keepaliveInterval,
	}, nil
}

func (g *gateway) handler() (http.Handler, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", g.post)
	mux.HandleFunc("GET /mcp", g.get)
	mux.HandleFunc("DELETE /mcp", g.delete)
	mux.HandleFunc("GET /status", g.status)
	return sidecarauth.Require(g.opts.Secret, mux)
}

func childEnv(o Options) []string {
	var env []string
	for _, kv := range o.ChildEnv {
		if strings.HasPrefix(kv, "OMAC_") || strings.HasPrefix(kv, "SIDECAR_") {
			continue
		}
		env = append(env, kv)
	}
	add := func(name, value string) {
		if !sidecarauth.Reserved(name) {
			env = append(env, name+"="+value)
		}
	}
	for name, value := range o.Def.Server.Env {
		add(name, value)
	}
	if m := o.Def.Mode; m != nil {
		add(m.Name, m.Value)
	}
	return env
}

func (g *gateway) shutdown() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	close(g.stopReap)
	all := make([]*session, 0, len(g.sessions))
	for _, s := range g.sessions {
		all = append(all, s)
	}
	g.sessions = map[string]*session{}
	g.mu.Unlock()
	for _, s := range all {
		s.close()
	}
}

func (g *gateway) reap() {
	interval := g.opts.IdleTimeout / 4
	if interval > time.Minute {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-g.stopReap:
			return
		case <-t.C:
			for _, s := range g.expired() {
				fmt.Fprintf(g.opts.Log, "mcp gateway: session %s idle, ending\n", s.id)
				s.close()
			}
		}
	}
}

func (g *gateway) expired() []*session {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []*session
	for id, s := range g.sessions {
		if s.idleFor() > g.opts.IdleTimeout {
			delete(g.sessions, id)
			out = append(out, s)
		}
	}
	return out
}

func (g *gateway) lookup(w http.ResponseWriter, r *http.Request) *session {
	id := r.Header.Get(sessionHeader)
	if id == "" {
		http.Error(w, "missing "+sessionHeader+" header", http.StatusBadRequest)
		return nil
	}
	g.mu.Lock()
	s := g.sessions[id]
	g.mu.Unlock()
	if s == nil {
		refuseHTTP(w, http.StatusNotFound, mcppolicy.ReasonUnknownSession, "unknown session")
		return nil
	}
	return s
}

func (g *gateway) remove(s *session) {
	g.mu.Lock()
	if g.sessions[s.id] == s {
		delete(g.sessions, s.id)
	}
	g.mu.Unlock()
}

func (g *gateway) open() (*session, int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, http.StatusServiceUnavailable, errors.New("gateway shutting down")
	}
	if len(g.sessions) >= g.opts.MaxSessions {
		return nil, http.StatusServiceUnavailable, errors.New("too many sessions")
	}
	id, err := newSessionID()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s, err := startSession(id, g.opts, g.env, g.remove)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	g.sessions[id] = s
	return s, 0, nil
}

func newSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (g *gateway) status(w http.ResponseWriter, _ *http.Request) {
	g.mu.Lock()
	n := len(g.sessions)
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","sessions":%d,"review":%q}`, n, g.reviewState())
}

func (g *gateway) delete(w http.ResponseWriter, r *http.Request) {
	s := g.lookup(w, r)
	if s == nil {
		return
	}
	g.remove(s)
	s.close()
	w.WriteHeader(http.StatusNoContent)
}

type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

func (m message) hasID() bool { return len(m.ID) > 0 && string(m.ID) != "null" }

func (g *gateway) post(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	body = bytes.TrimSpace(body)
	var msg message
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, "body must be a single JSON-RPC message", http.StatusBadRequest)
		return
	}
	call, canonical, err := mcppolicy.ParseRequest(body)
	if err != nil {
		refuseHTTP(w, http.StatusBadRequest, mcppolicy.ReasonUnparseable, "refused: "+err.Error())
		return
	}
	body = canonical
	if msg.hasID() && isInternalID(msg.ID) {
		http.Error(w, "request id is reserved", http.StatusBadRequest)
		return
	}

	var s *session
	if msg.Method == "initialize" && msg.hasID() {
		var status int
		s, status, err = g.open()
		if err != nil {
			fmt.Fprintf(g.opts.Log, "mcp gateway: initialize: %v\n", err)
			http.Error(w, err.Error(), status)
			return
		}
		w.Header().Set(sessionHeader, s.id)
	} else if s = g.lookup(w, r); s == nil {
		return
	}
	s.begin()
	defer s.end()

	if ref := g.enforce(r.Context(), s, call); ref != nil {
		if msg.Method == "" || !msg.hasID() {
			refuseHTTP(w, http.StatusForbidden, ref.reason, ref.message())
			return
		}
		writeReply(w, r, policyErrorReply(msg.ID, *ref))
		return
	}

	if msg.Method == "" || !msg.hasID() {
		if err := s.send(body); err != nil {
			http.Error(w, "server process unavailable", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	key := idKey(msg.ID)
	reply, err := s.register(key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer s.unregister(key)
	if err := s.send(body); err != nil {
		http.Error(w, "server process unavailable", http.StatusBadGateway)
		return
	}

	rewrite := func(resp []byte) []byte { return resp }
	if msg.Method == "tools/list" {
		rewrite = func(resp []byte) []byte { return g.filterToolsList(msg.ID, resp) }
	}
	if acceptsEventStream(r) {
		g.answerSSE(w, r, s, msg.ID, reply, rewrite)
		return
	}
	select {
	case resp := <-reply:
		w.Header().Set("Content-Type", "application/json")
		w.Write(rewrite(resp))
	case <-s.done:
		writeJSONRPCExit(w, msg.ID)
	case <-r.Context().Done():
	}
}

func (g *gateway) answerSSE(w http.ResponseWriter, r *http.Request, s *session, id json.RawMessage, reply <-chan []byte, rewrite func([]byte) []byte) {
	sse := startSSE(w)
	tick := time.NewTicker(g.keepalive)
	defer tick.Stop()
	for {
		select {
		case resp := <-reply:
			sse.event(rewrite(resp))
			return
		case <-s.done:
			sse.event(exitResponse(id))
			return
		case <-tick.C:
			sse.comment("keepalive")
		case <-r.Context().Done():
			return
		}
	}
}

func (g *gateway) get(w http.ResponseWriter, r *http.Request) {
	if !acceptsEventStream(r) {
		http.Error(w, "Accept must include text/event-stream", http.StatusNotAcceptable)
		return
	}
	s := g.lookup(w, r)
	if s == nil {
		return
	}
	if !s.claimStream() {
		http.Error(w, "event stream already open for this session", http.StatusConflict)
		return
	}
	defer s.releaseStream()
	s.begin()
	defer s.end()

	sse := startSSE(w)
	tick := time.NewTicker(g.keepalive)
	defer tick.Stop()
	for {
		select {
		case m := <-s.events:
			sse.event(m)
		case <-tick.C:
			sse.comment("keepalive")
		case <-s.done:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func acceptsEventStream(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept") {
		for _, part := range strings.Split(v, ",") {
			mt := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
			if strings.EqualFold(mt, "text/event-stream") {
				return true
			}
		}
	}
	return false
}

// writeReply answers the request with one JSON-RPC message, as an event when
// the client accepts a stream.
func writeReply(w http.ResponseWriter, r *http.Request, msg []byte) {
	if acceptsEventStream(r) {
		startSSE(w).event(msg)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(msg)
}

func exitResponse(id json.RawMessage) []byte {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32603, "message": "MCP server process exited"},
	})
	return b
}

func writeJSONRPCExit(w http.ResponseWriter, id json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(exitResponse(id))
}

type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func startSSE(w http.ResponseWriter) sseWriter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	s := sseWriter{w: w, f: f}
	s.flush()
	return s
}

func (s sseWriter) event(data []byte) {
	fmt.Fprintf(s.w, "event: message\ndata: %s\n\n", data)
	s.flush()
}

func (s sseWriter) comment(text string) {
	fmt.Fprintf(s.w, ": %s\n\n", text)
	s.flush()
}

func (s sseWriter) flush() {
	if s.f != nil {
		s.f.Flush()
	}
}

func idKey(id json.RawMessage) string { return string(bytes.TrimSpace(id)) }

type session struct {
	id      string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	log     io.Writer
	wmu     sync.Mutex
	done    chan struct{}
	readEnd chan struct{}
	events  chan []byte
	onExit  func(*session)

	mu         sync.Mutex
	waiters    map[string]chan []byte
	active     int
	stream     bool
	lastActive time.Time
	closeOnce  sync.Once

	// checkMu serializes review checks; review, reviewValid and toolsGen are guarded
	// by mu. toolsGen counts the server's tools/list_changed notifications.
	checkMu     sync.Mutex
	review      reviewCheck
	reviewValid bool
	toolsGen    int
}

func startSession(id string, o Options, env []string, onExit func(*session)) (*session, error) {
	argv := o.Def.Server.Command
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = o.Dir
	cmd.Env = env
	cmd.Stderr = o.Log
	cmd.WaitDelay = killWait
	setProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", argv[0], err)
	}
	s := &session{
		id:         id,
		cmd:        cmd,
		stdin:      stdin,
		log:        o.Log,
		done:       make(chan struct{}),
		readEnd:    make(chan struct{}),
		events:     make(chan []byte, eventQueueSize),
		onExit:     onExit,
		waiters:    map[string]chan []byte{},
		lastActive: time.Now(),
	}
	go s.readLoop(stdout)
	return s, nil
}

func (s *session) readLoop(stdout io.Reader) {
	r := bufio.NewReader(stdout)
	for {
		line, err := r.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			s.route(line)
		}
		if err != nil {
			break
		}
	}
	close(s.readEnd)
	s.onExit(s)
	s.close()
}

func (s *session) route(line []byte) {
	if isToolsListChanged(line) {
		s.toolsChanged()
	}
	var m message
	if json.Unmarshal(line, &m) == nil && m.Method == "" && m.hasID() {
		s.mu.Lock()
		ch := s.waiters[idKey(m.ID)]
		s.mu.Unlock()
		if ch == nil {
			fmt.Fprintf(s.log, "mcp gateway: dropping response with unexpected id %s\n", m.ID)
			return
		}
		// Never block the read loop: a server that answers one id twice
		// would otherwise stall every later message of the session.
		select {
		case ch <- append([]byte(nil), line...):
		default:
			fmt.Fprintf(s.log, "mcp gateway: dropping duplicate response for id %s\n", m.ID)
		}
		return
	}
	select {
	case s.events <- append([]byte(nil), line...):
	default:
		fmt.Fprintln(s.log, "mcp gateway: event queue full, dropping server message")
	}
}

func (s *session) register(key string) (<-chan []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.waiters[key]; dup {
		return nil, fmt.Errorf("request id %s already in flight", key)
	}
	ch := make(chan []byte, 1)
	s.waiters[key] = ch
	return ch, nil
}

func (s *session) unregister(key string) {
	s.mu.Lock()
	delete(s.waiters, key)
	s.mu.Unlock()
}

func (s *session) send(msg []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	select {
	case <-s.done:
		return errors.New("session closed")
	default:
	}
	_, err := s.stdin.Write(append(msg, '\n'))
	return err
}

func (s *session) begin() {
	s.mu.Lock()
	s.active++
	s.lastActive = time.Now()
	s.mu.Unlock()
}

func (s *session) end() {
	s.mu.Lock()
	s.active--
	s.lastActive = time.Now()
	s.mu.Unlock()
}

func (s *session) idleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active > 0 {
		return 0
	}
	return time.Since(s.lastActive)
}

func (s *session) claimStream() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stream {
		return false
	}
	s.stream = true
	return true
}

func (s *session) releaseStream() {
	s.mu.Lock()
	s.stream = false
	s.mu.Unlock()
}

// close ends the session and kills the child; it returns after the child has
// been reaped.
func (s *session) close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.wmu.Lock()
		s.stdin.Close()
		s.wmu.Unlock()
		killProcess(s.cmd)
		select {
		case <-s.readEnd:
		case <-time.After(killWait):
		}
		s.cmd.Wait()
	})
}

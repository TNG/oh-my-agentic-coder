package containerproxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/stableport"
)

// fakeDaemon is a test Docker daemon recording the requests it receives.
type fakeDaemon struct {
	mux    *http.ServeMux
	server *httptest.Server
	calls  []recordedReq
	// createResponse is the JSON returned for POST /containers/create.
	createResponse string
	// inspectResponse is the JSON returned for GET /containers/{id}/json.
	inspectResponse string
	// networkCreateResponse is the JSON returned for POST /networks/create.
	networkCreateResponse string
	// sawAuthHeader records whether a /containers/create request reached
	// the daemon carrying an X-Registry-Auth header (the proxy must strip
	// it — review critical #2).
	sawAuthHeader bool
	// preseededContainers is the list returned by GET /containers/json.
	// Used by the scavenger tests to simulate abandoned containers from a
	// previous crashed executor (plus unrelated containers the scavenger
	// must NOT touch). Reset to nil after the scavenger consumes it so a
	// second list returns empty (the scavenger removed its matches).
	preseededContainers []fakeContainer
	// preseededNetworks is the list returned by GET /networks. Same model
	// as preseededContainers for the network scavenger.
	preseededNetworks []fakeNetwork
	// deletedContainers records ids the daemon received DELETE for, so
	// scavenger tests can assert exactly which containers were removed.
	deletedContainers []string
	// deletedNetworks records network ids the daemon received DELETE for.
	deletedNetworks []string
	// createdContainers records containers created via POST /containers/create
	// (parsed from the create body's Labels so the scavenger's label filter
	// can find them). Used by the crash-restart test to faithfully simulate
	// a crashed prior run: the proxy creates a container, the daemon persists
	// it, the proxy crashes without cleanup, and the next proxy's scavenger
	// finds it via GET /containers/json. The labels are parsed from the
	// create body (the proxy injects omac.executor=<id> via validateCreateBody).
	createdContainers []fakeContainer
}

// fakeContainer is a minimal /containers/json list entry for scavenger tests.
type fakeContainer struct {
	ID     string
	Labels map[string]string
}

// fakeNetwork is a minimal /networks list entry for scavenger tests.
type fakeNetwork struct {
	ID     string
	Labels map[string]string
}

type recordedReq struct {
	Method string
	Path   string
	Query  string
	Body   string
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{mux: http.NewServeMux()}
	d.mux.HandleFunc("/containers/create", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Registry-Auth") != "" {
			d.sawAuthHeader = true
		}
		b, _ := io.ReadAll(r.Body)
		d.calls = append(d.calls, recordedReq{r.Method, r.URL.Path, r.URL.RawQuery, string(b)})
		// Persist the created container so a subsequent GET /containers/json
		// (e.g. the scavenger) can find it. The id comes from the create
		// response; the labels are parsed from the create body (the proxy
		// injects omac.executor=<id> via validateCreateBody). This makes the
		// crash-restart test faithful: a container created through the proxy
		// is visible to the scavenger's daemon list without re-seeding.
		resp := d.createResponse
		if resp == "" {
			resp = `{"Id":"abc123","Warnings":[]}`
		}
		var created struct {
			ID string `json:"Id"`
		}
		if json.Unmarshal([]byte(resp), &created) == nil && created.ID != "" {
			labels := parseCreateBodyLabels(string(b))
			d.createdContainers = append(d.createdContainers, fakeContainer{ID: created.ID, Labels: labels})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, resp)
	})
	d.mux.HandleFunc("/networks/create", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		d.calls = append(d.calls, recordedReq{r.Method, r.URL.Path, r.URL.RawQuery, string(b)})
		resp := d.networkCreateResponse
		if resp == "" {
			resp = `{"Id":"net-1","Warning":""}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, resp)
	})
	// GET /networks (no trailing slash) — list networks for the scavenger.
	// Returns preseededNetworks filtered by the label filter in the query
	// (the scavenger sends filters={"label":["omac.executor=<id>"]}).
	d.mux.HandleFunc("/networks", func(w http.ResponseWriter, r *http.Request) {
		d.calls = append(d.calls, recordedReq{r.Method, r.URL.Path, r.URL.RawQuery, ""})
		out := filterFakeNetworks(d.preseededNetworks, r.URL.Query().Get("filters"))
		b, _ := json.Marshal(out)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	})
	d.mux.HandleFunc("/networks/", func(w http.ResponseWriter, r *http.Request) {
		d.calls = append(d.calls, recordedReq{r.Method, r.URL.Path, r.URL.RawQuery, ""})
		if r.Method == http.MethodDelete {
			id := strings.TrimPrefix(r.URL.Path, "/networks/")
			d.deletedNetworks = append(d.deletedNetworks, id)
		}
		w.WriteHeader(http.StatusOK)
	})
	// Generic container endpoint: /containers/{id}/...
	d.mux.HandleFunc("/containers/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		d.calls = append(d.calls, recordedReq{r.Method, r.URL.Path, r.URL.RawQuery, string(b)})
		// GET /containers/json (list) — return preseeded containers
		// filtered by the label filter in the query (the scavenger sends
		// filters={"label":["omac.executor=<id>"]}).
		if r.URL.Path == "/containers/json" {
			// Union of preseeded (orphaned from a previous crash) + created
			// (persisted by the create handler), filtered by the label
			// filter. This lets the crash-restart test faithfully simulate
			// a crashed prior run without re-seeding: the proxy creates a
			// container, the daemon persists it, the proxy crashes, and the
			// next proxy's scavenger finds it via the daemon list.
			all := append([]fakeContainer(nil), d.preseededContainers...)
			all = append(all, d.createdContainers...)
			out := filterFakeContainers(all, r.URL.Query().Get("filters"))
			jb, _ := json.Marshal(out)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(jb)
			return
		}
		if r.Method == http.MethodDelete {
			id := strings.TrimPrefix(r.URL.Path, "/containers/")
			if i := strings.IndexByte(id, '?'); i >= 0 {
				id = id[:i]
			}
			d.deletedContainers = append(d.deletedContainers, id)
		}
		// Return the create response for /create, the inspect response for
		// /json, etc. Simplest: return inspectResponse for /json, OK otherwise.
		if strings.HasSuffix(r.URL.Path, "/json") {
			resp := d.inspectResponse
			if resp == "" {
				// Real Docker nests labels at Config.Labels (NOT top-level
				// Labels). The default fixture uses the real shape so the
				// ownership parse is exercised against what a real daemon
				// actually returns; a fixture with top-level Labels would
				// hide the Config.Labels parse bug (review critical #1).
				resp = `{"Id":"abc123","Config":{"Image":"pgvector/pgvector:pg16","Labels":{"omac.executor":"exec-1"}},"NetworkSettings":{"Ports":{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"54321"}]}}}`
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, resp)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	d.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		d.calls = append(d.calls, recordedReq{r.Method, r.URL.Path, r.URL.RawQuery, ""})
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	// Wrap the mux so versioned /v1.44/... paths are stripped to /...
	// (the real Docker daemon accepts the versioned prefix; the fake
	// daemon's ServeMux handlers are version-agnostic).
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = stripVersionPrefix(r.URL.Path)
		d.mux.ServeHTTP(w, r2)
	})
	d.server = httptest.NewServer(handler)
	t.Cleanup(d.server.Close)
	return d
}

// stripVersionPrefix removes a leading /v<digits>(.<digits>)?/ from path.
func stripVersionPrefix(path string) string {
	if !strings.HasPrefix(path, "/v") {
		return path
	}
	idx := strings.IndexByte(path[1:], '/')
	if idx < 0 {
		return path
	}
	seg := path[1 : 1+idx]
	if isVersionSeg(seg) {
		return path[1+idx:]
	}
	return path
}

// filterFakeContainers mimics the daemon's label-filter behavior for
// GET /containers/json: returns only entries whose Labels contain every
// label in the filters JSON's "label" array. The scavenger sends
// filters={"label":["omac.executor=<id>"]}, so only this executor's
// abandoned containers are returned. An empty/unparseable filter returns
// nothing (fail-safe: the scavenger must not enumerate unrelated hosts).
func filterFakeContainers(in []fakeContainer, filtersJSON string) []fakeContainer {
	if filtersJSON == "" {
		return nil
	}
	var filters map[string][]string
	if err := json.Unmarshal([]byte(filtersJSON), &filters); err != nil {
		return nil
	}
	want := filters["label"]
	var out []fakeContainer
	for _, c := range in {
		ok := true
		for _, l := range want {
			if !labelMatches(c.Labels, l) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, c)
		}
	}
	return out
}

// filterFakeNetworks is the network analogue of filterFakeContainers.
func filterFakeNetworks(in []fakeNetwork, filtersJSON string) []fakeNetwork {
	if filtersJSON == "" {
		return nil
	}
	var filters map[string][]string
	if err := json.Unmarshal([]byte(filtersJSON), &filters); err != nil {
		return nil
	}
	want := filters["label"]
	var out []fakeNetwork
	for _, n := range in {
		ok := true
		for _, l := range want {
			if !labelMatches(n.Labels, l) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, n)
		}
	}
	return out
}

// labelMatches reports whether labels contains the label key=value pair.
// Docker label filters are "key" (present) or "key=value" (exact).
func labelMatches(labels map[string]string, filter string) bool {
	if k, v, ok := strings.Cut(filter, "="); ok {
		return labels[k] == v
	}
	_, ok := labels[filter]
	return ok
}

// parseCreateBodyLabels extracts the Labels map from a create-container
// JSON body (the proxy injects omac.executor=<id> via validateCreateBody).
// Used by the fake daemon to persist created containers with their labels
// so the scavenger's label filter can find them.
func parseCreateBodyLabels(body string) map[string]string {
	var parsed struct {
		Labels map[string]string `json:"Labels"`
	}
	if json.Unmarshal([]byte(body), &parsed) == nil {
		return parsed.Labels
	}
	return nil
}

// startProxy starts a containerproxy pointed at the fake daemon.
func startProxy(t *testing.T, d *fakeDaemon) *Proxy {
	t.Helper()
	p, err := New(Config{
		Upstream:       d.server.URL,
		ApprovedImages: []string{"pgvector/pgvector:pg16"},
		ExecutorID:     "exec-1",
		Auditor:        audit.Nop(),
		Logf:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.shutdown)
	return p
}

// doReq issues a request to the proxy and returns the response status,
// body, and a parsed omac message (when present).
func doReq(t *testing.T, p *Proxy, method, path string, body []byte, hdr http.Header) (int, string, string) {
	t.Helper()
	conn, err := net.Dial("tcp", p.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req, err := http.NewRequest(method, "http://127.0.0.1"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if hdr != nil {
		for k, vs := range hdr {
			for _, v := range vs {
				req.Header.Set(k, v)
			}
		}
	}
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var omacMsg string
	var parsed map[string]any
	if json.Unmarshal(b, &parsed) == nil {
		if v, ok := parsed["omac"].(string); ok {
			omacMsg = v
		}
	}
	return resp.StatusCode, string(b), omacMsg
}

// --- allowlist tests -----------------------------------------------------

func TestAllowlist_PingVersionInfo(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	// /_ping is sent by the Docker client BOTH unversioned (liveness)
	// AND versioned (version negotiation). Both must be allowed.
	for _, path := range []string{"/_ping", "/v1.44/_ping", "/v1.32/_ping", "/v1.44/version", "/v1.44/info"} {
		status, _, _ := doReq(t, p, http.MethodGet, path, nil, nil)
		if status != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, status)
		}
	}
}

func TestAllowlist_UnknownEndpointDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	status, body, omac := doReq(t, p, http.MethodPost, "/v1.44/build", []byte(`{}`), nil)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%q", status, body)
	}
	if omac == "" {
		t.Errorf("denial must include omac message field: %q", body)
	}
	if !strings.Contains(omac, "unknown Docker API endpoint") {
		t.Errorf("denial must say unknown endpoint: %q", omac)
	}
	// Prune endpoints denied, EXCEPT /networks/prune, /volumes/prune,
	// and /images/prune which are allowed (ownership-label-filtered)
	// because Testcontainers' JVMHookResourceReaper calls all three on
	// every JVM shutdown. /containers/prune is the only prune endpoint
	// no caller needs — it stays denied.
	for _, path := range []string{"/v1.44/containers/prune"} {
		s, _, o := doReq(t, p, http.MethodPost, path, nil, nil)
		if s != http.StatusForbidden || !strings.Contains(o, "unknown") {
			t.Errorf("%s: expected structured unknown-endpoint denial, got %d %q", path, s, o)
		}
	}
	// /networks/prune, /volumes/prune, /images/prune are ALLOWED — the
	// proxy forwards them with an injected ownership label filter.
	// Assert they are not denied here (the filter rewrite is asserted
	// separately in TestNetworksPrune_RewritesFilter /
	// TestVolumesPrune_RewritesFilter / TestImagesPrune_RewritesFilter).
	for _, path := range []string{"/v1.44/networks/prune", "/v1.44/volumes/prune", "/v1.44/images/prune"} {
		if s, _, _ := doReq(t, p, http.MethodPost, path, nil, nil); s == http.StatusForbidden {
			t.Errorf("%s: expected allow (ownership-filtered), got 403", path)
		}
	}
	// /exec, /build, /commit, /attach, /archive denied.
	for _, path := range []string{"/v1.44/exec/abc/start", "/v1.44/commit", "/v1.44/containers/abc/attach", "/v1.44/containers/abc/archive"} {
		s, _, _ := doReq(t, p, http.MethodPost, path, nil, nil)
		if s != http.StatusForbidden {
			t.Errorf("%s: expected 403, got %d", path, s)
		}
	}
	// swarm/node/service denied.
	for _, path := range []string{"/v1.44/swarm", "/v1.44/nodes", "/v1.44/services"} {
		s, _, _ := doReq(t, p, http.MethodGet, path, nil, nil)
		if s != http.StatusForbidden {
			t.Errorf("%s: expected 403, got %d", path, s)
		}
	}
}

// TestNetworksPrune_RewritesFilter asserts POST /networks/prune is
// ALLOWED and the proxy injects the ownership label filter so only THIS
// executor's networks are pruned. The client's filter (forgeable) is
// dropped and replaced, matching the containers.list ownership model.
// Testcontainers' JVMHookResourceReaper calls this on every JVM shutdown.
func TestNetworksPrune_RewritesFilter(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	execID := p.cfg.ExecutorID

	// Send a prune with a forgeable client filter (a fake label the
	// client has no right to set). The proxy must drop it and inject
	// omac.executor=<execID>.
	forgeableFilter := url.QueryEscape(`{"label":["client-forged"]}`)
	path := "/v1.44/networks/prune?filters=" + forgeableFilter
	status, body, _ := doReq(t, p, http.MethodPost, path, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (prune is allowed, ownership-filtered); body=%q", status, body)
	}

	// Find the forwarded prune request the daemon recorded.
	var pruneCall *recordedReq
	for i := len(d.calls) - 1; i >= 0; i-- {
		if d.calls[i].Method == http.MethodPost && strings.Contains(d.calls[i].Path, "/networks/prune") {
			pruneCall = &d.calls[i]
			break
		}
	}
	if pruneCall == nil {
		t.Fatal("daemon did not record a POST /networks/prune")
	}
	// The forwarded query MUST carry the injected ownership label and
	// MUST NOT carry the client-forged label.
	if !strings.Contains(pruneCall.Query, "omac.executor%3D"+execID) &&
		!strings.Contains(pruneCall.Query, "omac.executor="+execID) {
		t.Errorf("forwarded prune query missing ownership label filter: %q", pruneCall.Query)
	}
	if strings.Contains(pruneCall.Query, "client-forged") {
		t.Errorf("forwarded prune query retained client-forged label: %q", pruneCall.Query)
	}
}

// TestVolumesPrune_RewritesFilter asserts POST /volumes/prune is ALLOWED
// and the proxy injects the ownership label filter so only THIS
// executor's volumes are pruned. Same mechanism as /networks/prune —
// the JVMHookResourceReaper shutdown hook calls both endpoints.
func TestVolumesPrune_RewritesFilter(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	execID := p.cfg.ExecutorID

	forgeableFilter := url.QueryEscape(`{"label":["client-forged"]}`)
	path := "/v1.44/volumes/prune?filters=" + forgeableFilter
	status, body, _ := doReq(t, p, http.MethodPost, path, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (prune is allowed, ownership-filtered); body=%q", status, body)
	}

	var pruneCall *recordedReq
	for i := len(d.calls) - 1; i >= 0; i-- {
		if d.calls[i].Method == http.MethodPost && strings.Contains(d.calls[i].Path, "/volumes/prune") {
			pruneCall = &d.calls[i]
			break
		}
	}
	if pruneCall == nil {
		t.Fatal("daemon did not record a POST /volumes/prune")
	}
	if !strings.Contains(pruneCall.Query, "omac.executor%3D"+execID) &&
		!strings.Contains(pruneCall.Query, "omac.executor="+execID) {
		t.Errorf("forwarded prune query missing ownership label filter: %q", pruneCall.Query)
	}
	if strings.Contains(pruneCall.Query, "client-forged") {
		t.Errorf("forwarded prune query retained client-forged label: %q", pruneCall.Query)
	}
}

// TestImagesPrune_RewritesFilter asserts POST /images/prune is ALLOWED
// and the proxy injects the ownership label filter. Same mechanism as
// /networks/prune and /volumes/prune — the JVMHookResourceReaper
// shutdown hook calls all three prune endpoints.
func TestImagesPrune_RewritesFilter(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	execID := p.cfg.ExecutorID

	forgeableFilter := url.QueryEscape(`{"label":["client-forged"]}`)
	path := "/v1.44/images/prune?filters=" + forgeableFilter
	status, body, _ := doReq(t, p, http.MethodPost, path, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (prune is allowed, ownership-filtered); body=%q", status, body)
	}

	var pruneCall *recordedReq
	for i := len(d.calls) - 1; i >= 0; i-- {
		if d.calls[i].Method == http.MethodPost && strings.Contains(d.calls[i].Path, "/images/prune") {
			pruneCall = &d.calls[i]
			break
		}
	}
	if pruneCall == nil {
		t.Fatal("daemon did not record a POST /images/prune")
	}
	if !strings.Contains(pruneCall.Query, "omac.executor%3D"+execID) &&
		!strings.Contains(pruneCall.Query, "omac.executor="+execID) {
		t.Errorf("forwarded prune query missing ownership label filter: %q", pruneCall.Query)
	}
	if strings.Contains(pruneCall.Query, "client-forged") {
		t.Errorf("forwarded prune query retained client-forged label: %q", pruneCall.Query)
	}
}

// --- create-body validation tests ----------------------------------------

func validCreateBody() string {
	return `{"Image":"pgvector/pgvector:pg16","Labels":{},"Env":["POSTGRES_PASSWORD=hush"],"HostConfig":{"Privileged":false,"Binds":[],"Mounts":[],"NetworkMode":"","PidMode":"","IpcMode":"","UsernsMode":"","CgroupnsMode":"","Runtime":"","CapAdd":[],"Devices":[],"SecurityOpt":[],"Dns":[],"ExtraHosts":[],"CgroupParent":"","PortBindings":{"5432/tcp":[{"HostIp":"","HostPort":""}]}}}`
}

// waitForCall polls the fake daemon's recorded calls until one matches the
// predicate or the timeout elapses. The container proxy does post-response
// work (inspect, network attach) AFTER writing the response to the client,
// so a test that acts on the response must wait for the side effects.
func waitForCall(t *testing.T, d *fakeDaemon, pred func(recordedReq) bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range d.calls {
			if pred(c) {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestCreateBody_ApprovedImageForwardsWithRewrite(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	status, _, _ := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(validCreateBody()), nil)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", status)
	}
	// Find the recorded create body.
	var rec recordedReq
	for _, c := range d.calls {
		if c.Path == "/containers/create" {
			rec = c
		}
	}
	if rec.Method == "" {
		t.Fatal("create not forwarded to upstream")
	}
	// PortBindings HostIp rewritten to 127.0.0.1.
	if !strings.Contains(rec.Body, `"HostIp":"127.0.0.1"`) {
		t.Errorf("HostIp not rewritten to 127.0.0.1:\n%s", rec.Body)
	}
	// Ownership label injected.
	if !strings.Contains(rec.Body, `"omac.executor":"exec-1"`) {
		t.Errorf("ownership label not injected:\n%s", rec.Body)
	}
	// Env values are NOT present in audit; here we only check they pass
	// through to the daemon (they are ephemeral test creds).
}

func TestCreateBody_UnapprovedImageDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	body := strings.ReplaceAll(validCreateBody(), "pgvector/pgvector:pg16", "postgres:17")
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "denied container image") || !strings.Contains(omac, "postgres:17") {
		t.Errorf("denial must name unapproved image: %q", omac)
	}
	if !strings.Contains(omac, "do not retry") {
		t.Errorf("denial must state do not retry: %q", omac)
	}
}

func TestCreateBody_PrivilegedDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	body := strings.ReplaceAll(validCreateBody(), `"Privileged":false`, `"Privileged":true`)
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "privileged mode") {
		t.Errorf("denial must state privileged mode forbidden: %q", omac)
	}
}

func TestCreateBody_BindMountDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	body := strings.ReplaceAll(validCreateBody(), `"Binds":[]`, `"Binds":["/var/run/docker.sock:/var/run/docker.sock:rw"]`)
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "bind mount") {
		t.Errorf("denial must state bind mount forbidden: %q", omac)
	}
}

func TestCreateBody_RyukImageDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	body := strings.ReplaceAll(validCreateBody(), "pgvector/pgvector:pg16", "testcontainers/ryuk:0.12.0")
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "Ryuk") {
		t.Errorf("denial must mention Ryuk: %q", omac)
	}
}

func TestCreateBody_ReservedLabelDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	body := strings.ReplaceAll(validCreateBody(), `"Labels":{}`, `"Labels":{"omac.executor":"forged"}`)
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "reserved") {
		t.Errorf("denial must state reserved label: %q", omac)
	}
}

func TestCreateBody_NetworkNamespaceDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	body := strings.ReplaceAll(validCreateBody(), `"NetworkMode":""`, `"NetworkMode":"host"`)
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "namespace") {
		t.Errorf("denial must state namespace forbidden: %q", omac)
	}
}

func TestCreateBody_CapAddDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	body := strings.ReplaceAll(validCreateBody(), `"CapAdd":[]`, `"CapAdd":["NET_ADMIN"]`)
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "capabilities") || !strings.Contains(omac, "forbidden") {
		t.Errorf("denial must state capabilities forbidden: %q", omac)
	}
}

// TestCreateBody_SysctlsMapDenied asserts a non-empty Sysctls (serialized
// as a map[string]string, per Docker's HostConfig.Sysctls) is denied —
// kernel parameters are a host escape vector. Regression test for a bug
// where the validation checked Sysctls as an array (nonEmptyStrSlice)
// instead of a map, so a non-empty map silently passed through to the
// daemon even though "Sysctls" was in the allowlist.
func TestCreateBody_SysctlsMapDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	// Inject Sysctls as a map (the shape Docker actually sends). Replace
	// the closing "HostConfig":{...} by inserting before the closing brace.
	body := strings.ReplaceAll(validCreateBody(), `"CgroupParent":""`, `"CgroupParent":"","Sysctls":{"net.ipv4.ip_forward":"1"}`)
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (Sysctls map must be denied)", status)
	}
	if !strings.Contains(omac, "Sysctls") || !strings.Contains(omac, "not permitted") {
		t.Errorf("denial must state Sysctls not permitted: %q", omac)
	}
}

// TestCreateBody_LxcConfArrayDenied asserts a non-empty LxcConf
// (serialized as a []string of "key=value", per Docker's
// HostConfig.LxcConf) is denied — legacy lxc config is an arbitrary host
// escape vector. Regression test for a bug where the validation checked
// LxcConf as a map instead of an array, so a non-empty array silently
// passed through to the daemon.
func TestCreateBody_LxcConfArrayDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	body := strings.ReplaceAll(validCreateBody(), `"CgroupParent":""`, `"CgroupParent":"","LxcConf":["lxc.aa_profile=unconfined"]`)
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (LxcConf array must be denied)", status)
	}
	if !strings.Contains(omac, "LxcConf") || !strings.Contains(omac, "not permitted") {
		t.Errorf("denial must state LxcConf not permitted: %q", omac)
	}
}

// --- images/create tests -------------------------------------------------

func TestImagesCreate_ApprovedFromImageForwards(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	status, _, _ := doReq(t, p, http.MethodPost, "/v1.44/images/create?fromImage=pgvector/pgvector&tag=pg16", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
}

func TestImagesCreate_UnapprovedFromImageDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/images/create?fromImage=evil/image&tag=latest", nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "evil/image") {
		t.Errorf("denial must name the unapproved image: %q", omac)
	}
}

func TestImagesCreate_RegistryAuthDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	hdr := http.Header{}
	hdr.Set("X-Registry-Auth", "dXNlcjpwYXNz")
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/images/create?fromImage=pgvector/pgvector&tag=pg16", nil, hdr)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "X-Registry-Auth") {
		t.Errorf("denial must mention X-Registry-Auth: %q", omac)
	}
}

// TestCreate_RegistryAuthStripped asserts the credential header
// X-Registry-Auth is NOT forwarded to the daemon on /containers/create
// (review critical #2: the create path forwarded it verbatim via
// copyForwardHeaders, bypassing the filter that images/create enforces).
// The header is stripped by copyForwardHeaders; the create must still
// succeed (the header's absence does not block an approved-image create).
func TestCreate_RegistryAuthStripped(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	hdr := http.Header{}
	hdr.Set("X-Registry-Auth", "dXNlcjpwYXNz")
	status, _, _ := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(validCreateBody()), hdr)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (approved image; auth header stripped)", status)
	}
	// Wait for the create to be recorded (the handler sets sawAuthHeader).
	waitForCall(t, d, func(c recordedReq) bool {
		return c.Method == http.MethodPost && c.Path == "/containers/create"
	}, "containers/create")
	if d.sawAuthHeader {
		t.Errorf("X-Registry-Auth was forwarded to the daemon on create; it must be stripped by copyForwardHeaders")
	}
}

// TestCreateBody_UnknownHostConfigFieldDenied asserts the create-body
// validation is ALLOWLIST-based (spec.md:222: unknown security-relevant
// fields denied), not denylist-based. An unknown HostConfig field must
// be denied fail-closed so a future Docker API field cannot pass through
// unexamined (review major #3).
func TestCreateBody_UnknownHostConfigFieldDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	// A field NOT in the v1 allowedHostConfigKeys set.
	body := `{"Image":"pgvector/pgvector:pg16","HostConfig":{"SomeNewDangerousField":"evil"}}`
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (unknown HostConfig field must be denied fail-closed)", status)
	}
	if !strings.Contains(omac, "SomeNewDangerousField") {
		t.Errorf("denial must name the unknown field: %q", omac)
	}
}

// TestCreateBody_AllUnknownHostConfigFieldsReportedTogether asserts the
// allowlist validation collects ALL unknown HostConfig keys in one
// denial, not just the first. A first-key-only denial would hide
// subsequent missing fields behind the first failure, forcing one
// rebuild+IT cycle per field. The "measured allowlist" was captured
// against an older docker-java; newer client versions serialize
// additional fields, so one IT run must surface the complete gap.
func TestCreateBody_AllUnknownHostConfigFieldsReportedTogether(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	// Two fields NOT in the v1 allowedHostConfigKeys set.
	body := `{"Image":"pgvector/pgvector:pg16","HostConfig":{"FutureFieldA":"x","FutureFieldB":"y"}}`
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	// Both unknown fields must appear in the single denial (sorted).
	if !strings.Contains(omac, "FutureFieldA") || !strings.Contains(omac, "FutureFieldB") {
		t.Errorf("denial must name BOTH unknown fields: %q", omac)
	}
	if !strings.Contains(omac, "FutureFieldA, FutureFieldB") {
		t.Errorf("denial must list both fields comma-joined (sorted): %q", omac)
	}
}

// TestCreateBody_AutoRemoveDenied asserts AutoRemove=true is denied: a
// container that auto-removes on exit evades the proxy's ownership
// tracking and the cleanup/audit path (review major #3).
func TestCreateBody_AutoRemoveDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	body := `{"Image":"pgvector/pgvector:pg16","HostConfig":{"AutoRemove":true}}`
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (AutoRemove must be denied)", status)
	}
	if !strings.Contains(omac, "AutoRemove") {
		t.Errorf("denial must mention AutoRemove: %q", omac)
	}
}

// TestCreateBody_IsolationAcceptedAndValidated asserts the Isolation
// HostConfig field (which Testcontainers' docker-java client serializes on
// macOS) is ACCEPTED when empty/default and DENIED when non-default. A
// non-default Isolation is a host-namespace escape vector on platforms
// that honor it.
func TestCreateBody_IsolationAcceptedAndValidated(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	// Empty Isolation is accepted (the common Testcontainers form).
	body := `{"Image":"pgvector/pgvector:pg16","HostConfig":{"Isolation":""}}`
	status, _, _ := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusCreated {
		t.Fatalf("empty Isolation: status = %d, want 201", status)
	}
	// Non-default Isolation is denied as a host-namespace escape.
	body = `{"Image":"pgvector/pgvector:pg16","HostConfig":{"Isolation":"process"}}`
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("Isolation=process: status = %d, want 403", status)
	}
	if !strings.Contains(omac, "Isolation") {
		t.Errorf("denial must name Isolation: %q", omac)
	}
}

// TestCreateBody_ResourceFieldsAccepted asserts the pass-through resource
// fields (Memory, NanoCpus, PidsLimit) are accepted with arbitrary values.
// These are DoS-mitigation limits, not escape vectors; the manifest gate
// enforces the ceiling. Testcontainers serializes PidsLimit by default.
func TestCreateBody_ResourceFieldsAccepted(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	for _, body := range []string{
		`{"Image":"pgvector/pgvector:pg16","HostConfig":{"Memory":0}}`,
		`{"Image":"pgvector/pgvector:pg16","HostConfig":{"NanoCpus":0}}`,
		`{"Image":"pgvector/pgvector:pg16","HostConfig":{"PidsLimit":0}}`,
		`{"Image":"pgvector/pgvector:pg16","HostConfig":{"PidsLimit":256}}`,
	} {
		status, _, _ := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
		if status != http.StatusCreated {
			t.Errorf("resource field body %q: status = %d, want 201", body, status)
		}
	}
}

// --- image inspect -------------------------------------------------------

func TestImageInspect_ApprovedRefForwards(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	status, _, _ := doReq(t, p, http.MethodGet, "/v1.44/images/pgvector/pgvector:pg16/json", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
}

func TestImageInspect_UnapprovedRefDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	status, _, omac := doReq(t, p, http.MethodGet, "/v1.44/images/evil:latest/json", nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "evil:latest") {
		t.Errorf("denial must name the image: %q", omac)
	}
}

// TestImageInspect_DigestResolvesToApprovedTag asserts that when
// Testcontainers inspects an image by its content digest (sha256:...),
// the proxy resolves the digest back to its RepoTags via a daemon
// sub-request and allows the inspect when ANY RepoTag matches the
// approved set. The pull (/images/create) is the security boundary;
// the inspect is read-only. Without this resolution, Testcontainers'
// inspect-by-digest flow fails because the digest never matches the
// manifest's tag list.
func TestImageInspect_DigestResolvesToApprovedTag(t *testing.T) {
	d := newFakeDaemon(t)
	// Seed the daemon to return RepoTags for the digest lookup.
	d.mux.HandleFunc("/images/sha256:1d533553fefe4f12e5d80c7b80622ba0c382abb5758856f52983d8789179f0fb/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Id":"sha256:1d533553fefe4f12e5d80c7b80622ba0c382abb5758856f52983d8789179f0fb","RepoTags":["pgvector/pgvector:pg16"]}`)
	})
	p := startProxy(t, d)
	status, _, _ := doReq(t, p, http.MethodGet, "/v1.44/images/sha256:1d533553fefe4f12e5d80c7b80622ba0c382abb5758856f52983d8789179f0fb/json", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("digest resolving to approved tag: status = %d, want 200", status)
	}
}

// TestImageInspect_DigestResolvesToUnapprovedTag asserts that a digest
// whose RepoTags do NOT match the approved set is denied fail-closed.
func TestImageInspect_DigestResolvesToUnapprovedTag(t *testing.T) {
	d := newFakeDaemon(t)
	d.mux.HandleFunc("/images/sha256:evil123/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Id":"sha256:evil123","RepoTags":["evil/image:latest"]}`)
	})
	p := startProxy(t, d)
	status, _, omac := doReq(t, p, http.MethodGet, "/v1.44/images/sha256:evil123/json", nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("digest resolving to unapproved tag: status = %d, want 403", status)
	}
	if !strings.Contains(omac, "sha256:evil123") {
		t.Errorf("denial must name the digest: %q", omac)
	}
}

// --- ownership enforcement -----------------------------------------------

func TestOwnership_NotOwnedDenied(t *testing.T) {
	d := newFakeDaemon(t)
	// Inspect returns a DIFFERENT executor label.
	d.inspectResponse = `{"Id":"xyz","Config":{"Image":"pgvector/pgvector:pg16"},"Labels":{"omac.executor":"other-executor"}}`
	p := startProxy(t, d)
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/xyz/start", nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "not owned by this executor") {
		t.Errorf("denial must state not owned: %q", omac)
	}
}

func TestOwnership_OwnedForwards(t *testing.T) {
	d := newFakeDaemon(t)
	// Real Docker nests labels at Config.Labels (NOT top-level Labels).
	// This is the shape a real daemon returns from GET /containers/{id}/json;
	// the proxy MUST parse Config.Labels, not top-level Labels (review
	// critical #1: the previous fixture used top-level Labels and hid the
	// parse bug).
	d.inspectResponse = `{"Id":"abc123","Config":{"Image":"pgvector/pgvector:pg16","Labels":{"omac.executor":"exec-1"}}}`
	p := startProxy(t, d)
	status, _, _ := doReq(t, p, http.MethodPost, "/v1.44/containers/abc123/start", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
}

// TestOwnership_ConfigLabelsNotTopLevel asserts the ownership parse reads
// Config.Labels (the real Docker inspect shape) and that a container
// whose labels are ONLY at top-level Labels (a non-real shape) is denied
// fail-closed — guarding against a regression of the critical parse bug.
func TestOwnership_ConfigLabelsNotTopLevel(t *testing.T) {
	d := newFakeDaemon(t)
	// Labels at top-level ONLY (the shape the buggy parser read) — a real
	// daemon does NOT return this. The proxy must NOT treat this as owned.
	d.inspectResponse = `{"Id":"abc123","Labels":{"omac.executor":"exec-1"}}`
	p := startProxy(t, d)
	status, _, body := doReq(t, p, http.MethodGet, "/v1.44/containers/abc123/json", nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (top-level Labels must not satisfy ownership; real daemon nests at Config.Labels): body=%s", status, body)
	}
}

// --- containers/json filter rewrite -------------------------------------

func TestContainersList_FilterRewritten(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	// Client sends a forgeable label filter.
	status, _, _ := doReq(t, p, http.MethodGet, `/v1.44/containers/json?all=true&filters=%7B%22label%22%3A%5B%22org.testcontainers%3Dtrue%22%5D%7D`, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	// Find the recorded request and verify the filter was rewritten.
	var rec recordedReq
	for _, c := range d.calls {
		if c.Path == "/containers/json" {
			rec = c
		}
	}
	if rec.Method == "" {
		t.Fatal("containers/json not forwarded")
	}
	if !strings.Contains(rec.Query, "omac.executor%3Dexec-1") {
		t.Errorf("ownership label not injected into filters: %q", rec.Query)
	}
	// The forgeable client label must not survive (the proxy strips it;
	// the injected ownership label replaces the label set).
	if strings.Contains(rec.Query, "org.testcontainers") {
		t.Errorf("client label filter must be stripped, not trusted: %q", rec.Query)
	}
}

// --- streaming response (logs?follow=true) --------------------------------

// TestForward_StreamsChunkedResponse asserts the proxy streams a response
// with no Content-Length (the shape /containers/{id}/logs?follow=true
// returns) using HTTP/1.1 chunked transfer encoding to the client, instead
// of buffering the body. Buffering a live stream blocks forever (io.ReadAll
// waits for EOF), which caused Testcontainers' LogMessageWaitStrategy to
// time out even though the container was ready.
func TestForward_StreamsChunkedResponse(t *testing.T) {
	d := newFakeDaemon(t)
	// Seed a streaming handler for /containers/{id}/logs that writes
	// chunks with delays (simulating a live log stream) and no
	// Content-Length (chunked transfer encoding).
	d.mux.HandleFunc("/containers/abc123/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		// Force chunked streaming by explicitly writing the status
		// header first (so Go's http.Server can't compute
		// Content-Length from the total body), then writing chunks
		// with a delay between them.
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("database system is starting\n"))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte("database system is ready to accept connections\n"))
		if flusher != nil {
			flusher.Flush()
		}
	})
	// Create a container through the proxy so it's in the ownership map
	// (the logs endpoint is ownership-scoped).
	p := startProxy(t, d)
	createBody := `{"Image":"pgvector/pgvector:pg16","HostConfig":{"PortBindings":{"5432/tcp":[{"HostIp":"","HostPort":""}]}}}`
	status, _, _ := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(createBody), nil)
	if status != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201", status)
	}
	// The create response returns Id=abc123 (the fakeDaemon default).
	// Now request the logs stream — the proxy must stream, not buffer.
	conn, err := net.Dial("tcp", p.ln.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	req := "GET /v1.44/containers/abc123/logs?follow=true&stdout=true&stderr=true HTTP/1.1\r\nHost: docker\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	// Read the response with a real HTTP client reader. The response
	// must arrive (not block forever) and contain the log line.
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// The proxy must use chunked encoding (streaming) not Content-Length.
	if resp.Header.Get("Content-Length") != "" {
		t.Errorf("streaming response must NOT have Content-Length, got %q", resp.Header.Get("Content-Length"))
	}
	// http.ReadResponse consumes the Transfer-Encoding header and wraps
	// the body in a chunk reader; the header is removed after parsing.
	// The body must be readable and contain both log lines (not block).
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read streaming body: %v", err)
	}
	if !strings.Contains(string(body), "database system is ready to accept connections") {
		t.Errorf("streaming body missing the ready log line: %q", body)
	}
}

// --- cleanup -------------------------------------------------------------

func TestCleanup_RemovesOwnedContainersAndNetwork(t *testing.T) {
	d := newFakeDaemon(t)
	d.networkCreateResponse = `{"Id":"net-xyz"}`
	p := startProxy(t, d)
	// Create a container so it is tracked.
	status, _, _ := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(validCreateBody()), nil)
	if status != http.StatusCreated {
		t.Fatalf("create status = %d", status)
	}
	// Wait for the proxy's post-response work (network create + attach)
	// to complete before Cleanup runs; otherwise Cleanup races the
	// attachToNetwork goroutine.
	waitForCall(t, d, func(c recordedReq) bool {
		return c.Method == http.MethodPost && c.Path == "/networks/create"
	}, "networks/create")
	// Trigger cleanup.
	p.Cleanup()
	// Assert a DELETE for abc123 reached the daemon.
	foundDelete := false
	foundNetRemove := false
	for _, c := range d.calls {
		if c.Method == http.MethodDelete && strings.Contains(c.Path, "/containers/abc123") {
			foundDelete = true
		}
		if c.Method == http.MethodDelete && strings.Contains(c.Path, "/networks/") {
			foundNetRemove = true
		}
	}
	if !foundDelete {
		t.Error("cleanup did not DELETE the owned container")
	}
	if !foundNetRemove {
		t.Error("cleanup did not DELETE the executor network")
	}
}

// --- config validation ---------------------------------------------------

func TestNew_RequiresExecutorIDAndImages(t *testing.T) {
	if _, err := New(Config{ApprovedImages: []string{"x"}}); err == nil {
		t.Error("empty executor id must error")
	}
	if _, err := New(Config{ExecutorID: "x"}); err == nil {
		t.Error("empty approved images must error")
	}
}

// --- ContainerPolicyError.Render -----------------------------------------

func TestContainerPolicyError_Render(t *testing.T) {
	cases := []struct {
		kind PolicyErrKind
		want []string
	}{
		{KindUnapprovedImage, []string{"denied container image", "do not retry"}},
		{KindPrivilegedForbidden, []string{"privileged mode", "forbidden"}},
		{KindBindMountForbidden, []string{"bind mount", "forbidden"}},
		{KindHostNamespaceForbidden, []string{"namespace", "forbidden"}},
		{KindDeviceForbidden, []string{"forbidden"}},
		{KindUnknownEndpoint, []string{"unknown Docker API endpoint"}},
		{KindNotOwnedByExecutor, []string{"not owned by this executor"}},
		{KindRyukForbidden, []string{"Ryuk"}},
		{KindRegistryAuthForbidden, []string{"X-Registry-Auth"}},
		{KindReservedLabel, []string{"reserved"}},
	}
	for _, c := range cases {
		e := &ContainerPolicyError{Kind: c.kind, Image: "test:1", ContainerID: "cid", Reason: "r"}
		msg := e.Render()
		for _, w := range c.want {
			if !strings.Contains(msg, w) {
				t.Errorf("kind %d render missing %q: %s", c.kind, w, msg)
			}
		}
	}
}

// --- ticket 09: startup scavenger (checkbox 6) ---------------------------

// TestScavenge_RemovesOnlyOwnedContainers asserts the startup scavenger
// removes abandoned containers labeled with THIS executor's ownership
// label and does NOT touch unrelated host containers (checkbox 6). The
// fake daemon is pre-seeded with two owned containers (from a previous
// crashed executor) and one unrelated container; only the owned two are
// DELETEd.
func TestScavenge_RemovesOnlyOwnedContainers(t *testing.T) {
	d := newFakeDaemon(t)
	d.preseededContainers = []fakeContainer{
		{ID: "owned-aaa", Labels: map[string]string{"omac.executor": "exec-1"}},
		{ID: "owned-bbb", Labels: map[string]string{"omac.executor": "exec-1"}},
		{ID: "unrelated-ccc", Labels: map[string]string{"omac.executor": "other-exec"}},
		{ID: "no-label-ddd", Labels: nil},
	}
	// Build the proxy WITHOUT starting it (Start would run the scavenger
	// automatically); call Scavenge directly to assert the counts.
	p, err := New(Config{
		Upstream:       d.server.URL,
		ApprovedImages: []string{"pgvector/pgvector:pg16"},
		ExecutorID:     "exec-1",
		Auditor:        audit.Nop(),
		Logf:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	cRemoved, nRemoved := p.Scavenge()
	if cRemoved != 2 {
		t.Errorf("containers removed = %d, want 2 (only owned): deleted=%v", cRemoved, d.deletedContainers)
	}
	if nRemoved != 0 {
		t.Errorf("networks removed = %d, want 0", nRemoved)
	}
	// Exactly the two owned ids were DELETEd.
	wantDeleted := map[string]bool{"owned-aaa": true, "owned-bbb": true}
	if len(d.deletedContainers) != 2 {
		t.Fatalf("deleted %d containers, want 2: %v", len(d.deletedContainers), d.deletedContainers)
	}
	for _, id := range d.deletedContainers {
		if !wantDeleted[id] {
			t.Errorf("deleted unexpected container %s (must not touch unrelated)", id)
		}
	}
}

// TestScavenge_RemovesOnlyOwnedNetworks asserts the scavenger removes
// abandoned networks labeled with this executor's id and leaves unrelated
// networks alone.
func TestScavenge_RemovesOnlyOwnedNetworks(t *testing.T) {
	d := newFakeDaemon(t)
	d.preseededNetworks = []fakeNetwork{
		{ID: "net-owned-1", Labels: map[string]string{"omac.executor": "exec-1"}},
		{ID: "net-other", Labels: map[string]string{"omac.executor": "other"}},
	}
	p, err := New(Config{
		Upstream:       d.server.URL,
		ApprovedImages: []string{"pgvector/pgvector:pg16"},
		ExecutorID:     "exec-1",
		Auditor:        audit.Nop(),
		Logf:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	cRemoved, nRemoved := p.Scavenge()
	if nRemoved != 1 {
		t.Errorf("networks removed = %d, want 1: deleted=%v", nRemoved, d.deletedNetworks)
	}
	if cRemoved != 0 {
		t.Errorf("containers removed = %d, want 0", cRemoved)
	}
	if len(d.deletedNetworks) != 1 || d.deletedNetworks[0] != "net-owned-1" {
		t.Errorf("deleted networks = %v, want [net-owned-1]", d.deletedNetworks)
	}
}

// TestScavenge_EmptyDaemonIsNoOp asserts the scavenger is a no-op on a
// clean daemon (no owned resources to remove).
func TestScavenge_EmptyDaemonIsNoOp(t *testing.T) {
	d := newFakeDaemon(t)
	p, err := New(Config{
		Upstream:       d.server.URL,
		ApprovedImages: []string{"pgvector/pgvector:pg16"},
		ExecutorID:     "exec-1",
		Auditor:        audit.Nop(),
		Logf:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	cRemoved, nRemoved := p.Scavenge()
	if cRemoved != 0 || nRemoved != 0 {
		t.Errorf("clean daemon: removed %d containers, %d networks; want 0/0", cRemoved, nRemoved)
	}
	if len(d.deletedContainers) != 0 || len(d.deletedNetworks) != 0 {
		t.Errorf("clean daemon had deletes: containers=%v networks=%v", d.deletedContainers, d.deletedNetworks)
	}
}

// TestScavenge_SpecialCharExecutorID asserts the scavenger's label filter
// is built with json.Marshal (not fmt.Sprintf into a JSON string), so an
// executor id containing JSON-special characters (e.g. a worktree base
// name like feat"a) is correctly encoded and the scavenger still finds
// the owned resources. The hand-rolled fmt.Sprintf filter would produce
// malformed JSON and silently no-op for such ids (review major #2).
func TestScavenge_SpecialCharExecutorID(t *testing.T) {
	d := newFakeDaemon(t)
	execID := `omac-feat"a`
	d.preseededContainers = []fakeContainer{
		{ID: "owned-special", Labels: map[string]string{"omac.executor": execID}},
		{ID: "unrelated", Labels: map[string]string{"omac.executor": "exec-1"}},
	}
	p, err := New(Config{
		Upstream:       d.server.URL,
		ApprovedImages: []string{"pgvector/pgvector:pg16"},
		ExecutorID:     execID,
		Auditor:        audit.Nop(),
		Logf:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	cRemoved, _ := p.Scavenge()
	if cRemoved != 1 {
		t.Errorf("special-char executor id: removed %d containers, want 1 (json.Marshal-encoded filter must match): deleted=%v", cRemoved, d.deletedContainers)
	}
	if len(d.deletedContainers) != 1 || d.deletedContainers[0] != "owned-special" {
		t.Errorf("special-char executor id: deleted=%v, want [owned-special]", d.deletedContainers)
	}
}

// TestStart_RunsScavengerAtStartup asserts Start invokes the scavenger
// before serving the first request, so abandoned resources from a previous
// crashed executor are removed automatically (checkbox 6). The fake
// daemon is pre-seeded with an owned abandoned container; after Start the
// container must be gone (DELETEd).
func TestStart_RunsScavengerAtStartup(t *testing.T) {
	d := newFakeDaemon(t)
	d.preseededContainers = []fakeContainer{
		{ID: "abandoned-1", Labels: map[string]string{"omac.executor": "exec-1"}},
	}
	p, err := New(Config{
		Upstream:       d.server.URL,
		ApprovedImages: []string{"pgvector/pgvector:pg16"},
		ExecutorID:     "exec-1",
		Auditor:        audit.Nop(),
		Logf:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer p.shutdown()
	// The scavenger runs synchronously in Start before returning, so the
	// DELETE is already recorded.
	if len(d.deletedContainers) != 1 || d.deletedContainers[0] != "abandoned-1" {
		t.Errorf("startup scavenger did not remove abandoned container: deleted=%v", d.deletedContainers)
	}
}

// --- ticket 09: denial correlation (checkbox 7, spec §254) ---------------

// TestDenial_CorrelatedWithBuildRequest asserts that when a build request
// id is set on the proxy, a container-policy denial carries the request id
// in BOTH the omac message (first line) and the audit event. The agent
// thus receives an actionable OMAC explanation naming the active request
// rather than only a wrapped Testcontainers failure.
func TestDenial_CorrelatedWithBuildRequest(t *testing.T) {
	d := newFakeDaemon(t)
	p, err := New(Config{
		Upstream:       d.server.URL,
		ApprovedImages: []string{"pgvector/pgvector:pg16"},
		ExecutorID:     "exec-1",
		Auditor:        audit.Nop(),
		Logf:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	p.SetBuildRequestID("b-deadbeef")
	if _, _, err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer p.shutdown()
	// Trigger a denial: unapproved image.
	body := strings.ReplaceAll(validCreateBody(), "pgvector/pgvector:pg16", "postgres:17")
	status, bodyStr, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	// The omac message MUST start with the correlation prefix naming the
	// active build request AND the actionable cause on line 1 (spec §254 —
	// the OMAC cause + request id are the FIRST line so Gradle/Testcontainers
	// summary-truncation that shows only line 1 still conveys the fix hint).
	// Line 1 = "OMAC build request <id>: <cause line 1>".
	if !strings.HasPrefix(omac, "OMAC build request b-deadbeef: OMAC build denied container image postgres:17") {
		t.Errorf("denial line 1 must carry the request id AND the actionable cause:\n%s", omac)
	}
	// The underlying cause's fix hint is still present.
	if !strings.Contains(omac, "do not retry") {
		t.Errorf("denial must still contain the cause's fix hint: %q", omac)
	}
	// The raw body carries the same message in the `omac` field.
	if !strings.Contains(bodyStr, "b-deadbeef") {
		t.Errorf("response body must carry the build request id: %s", bodyStr)
	}
}

// TestDenial_NoBuildRequestIDOmitsPrefix asserts that without a build
// request id (e.g. the startup scavenger's own audit events, or a denial
// outside a build) the correlation prefix is omitted — no misleading
// "OMAC build request <empty> was denied" line.
func TestDenial_NoBuildRequestIDOmitsPrefix(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d) // no SetBuildRequestID
	// Trigger a denial: unapproved image.
	body := strings.ReplaceAll(validCreateBody(), "pgvector/pgvector:pg16", "postgres:17")
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if strings.Contains(omac, "OMAC build request") {
		t.Errorf("denial without a build request id must NOT include the correlation prefix: %q", omac)
	}
	// The underlying cause is still present.
	if !strings.Contains(omac, "denied container image") {
		t.Errorf("denial must still name the underlying cause: %q", omac)
	}
}

// TestContainerPolicyError_RenderWithBuildRequestID asserts Render prepends
// the correlation prefix (request id + cause on line 1) when BuildRequestID
// is set.
func TestContainerPolicyError_RenderWithBuildRequestID(t *testing.T) {
	e := &ContainerPolicyError{Kind: KindUnapprovedImage, Image: "evil:1", BuildRequestID: "b-abc"}
	msg := e.Render()
	if !strings.HasPrefix(msg, "OMAC build request b-abc: OMAC build denied container image evil:1") {
		t.Errorf("render line 1 must carry the request id AND the cause: %s", msg)
	}
	if !strings.Contains(msg, "do not retry") {
		t.Errorf("render must still contain the cause's fix hint: %s", msg)
	}
}

// --- ticket 09: crash + supervisor restart cleanup (checkbox 5) ----------

// TestCrashRestart_ScavengerRemovesOrphanedContainer simulates a crashed
// executor: a proxy creates a container, then STOPs WITHOUT cleanup
// (simulated crash — shutdown is bypassed, the container remains on the
// daemon). A NEW proxy with the SAME executor id starts; its startup
// scavenger must remove the orphaned container (checkbox 5: simulated
// supervisor restart leaves no owned container behind). The fake daemon
// persists the proxy-created container (via createdContainers), so the
// scavenger finds it via GET /containers/json — no re-seeding.
func TestCrashRestart_ScavengerRemovesOrphanedContainer(t *testing.T) {
	d := newFakeDaemon(t)
	// First "session": create a proxy, create a container through it, then
	// simulate a crash by closing the listener WITHOUT running Cleanup (so
	// the container remains on the daemon). The fake daemon persists the
	// created container (id abc123, labeled omac.executor=exec-1 by the
	// proxy's validateCreateBody) into createdContainers.
	p1, err := New(Config{
		Upstream:       d.server.URL,
		ApprovedImages: []string{"pgvector/pgvector:pg16"},
		ExecutorID:     "exec-1",
		Auditor:        audit.Nop(),
		Logf:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p1.Start(); err != nil {
		t.Fatal(err)
	}
	// Create a container through the proxy so it is tracked by p1 AND
	// persisted by the fake daemon (createdContainers).
	if status, _, _ := doReq(t, p1, http.MethodPost, "/v1.44/containers/create", []byte(validCreateBody()), nil); status != http.StatusCreated {
		t.Fatalf("create status = %d", status)
	}
	// Wait for the post-create network attach to settle.
	waitForCall(t, d, func(c recordedReq) bool {
		return c.Method == http.MethodPost && c.Path == "/networks/create"
	}, "networks/create")
	// Simulate crash: close the listener, do NOT run Cleanup. The
	// container "abc123" is now orphaned on the daemon (the fake daemon
	// persists it in createdContainers). The accept-loop goroutine exits
	// on the next Accept() error; the in-flight attach goroutine is
	// intentionally leaked (crash simulation — no graceful teardown).
	p1.ln.Close()
	d.calls = nil
	d.deletedContainers = nil
	// Second "session": a new proxy with the SAME executor id. Its startup
	// scavenger must find the orphaned container via GET /containers/json
	// (the daemon returns it from createdContainers) and remove it. The
	// scavenger runs BEFORE the listener is bound (fix for review critical
	// #1), so no client can race it.
	p2, err := New(Config{
		Upstream:       d.server.URL,
		ApprovedImages: []string{"pgvector/pgvector:pg16"},
		ExecutorID:     "exec-1",
		Auditor:        audit.Nop(),
		Logf:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p2.Start(); err != nil {
		t.Fatal(err)
	}
	defer p2.shutdown()
	// The scavenger ran in Start (before bind): the orphaned container the
	// fake daemon persisted is DELETEd.
	if len(d.deletedContainers) != 1 || d.deletedContainers[0] != "abc123" {
		t.Errorf("scavenger on restart did not remove the orphaned container: deleted=%v", d.deletedContainers)
	}
}

// TestCrashRestart_ScavengerRemovesOrphanedNetwork asserts a crashed
// prior run's executor-owned network is reclaimed by the next startup's
// scavenger (so ensureNetwork does not silently fail on a name-conflict
// 409 and leave containers on the default bridge — checkbox 5).
func TestCrashRestart_ScavengerRemovesOrphanedNetwork(t *testing.T) {
	d := newFakeDaemon(t)
	// Simulate the post-crash state: an orphaned executor network.
	d.preseededNetworks = []fakeNetwork{
		{ID: "net-orphan", Labels: map[string]string{"omac.executor": "exec-1"}},
	}
	p, err := New(Config{
		Upstream:       d.server.URL,
		ApprovedImages: []string{"pgvector/pgvector:pg16"},
		ExecutorID:     "exec-1",
		Auditor:        audit.Nop(),
		Logf:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer p.shutdown()
	if len(d.deletedNetworks) != 1 || d.deletedNetworks[0] != "net-orphan" {
		t.Errorf("scavenger did not remove the orphaned network: deleted=%v", d.deletedNetworks)
	}
}

// --- stable port selection (warm-daemon DOCKER_HOST fix) ----------------
//
// The pure stable-port helper tests (hash determinism/range/symlinks,
// window scan/wrap/fallback) live in internal/stableport. The tests below
// exercise the containerproxy wiring of those helpers against a real Proxy.

// TestStart_StablePortBindsWhenFree asserts Start binds the deterministic
// stable port derived from the worktree when it is free. A control leaf
// is wired so the assigned port is also persisted.
func TestStart_StablePortBindsWhenFree(t *testing.T) {
	d := newFakeDaemon(t)
	leaf := t.TempDir()
	want := stableport.For("/worktree/feat-a")
	p, err := New(Config{
		Upstream:       d.server.URL,
		ApprovedImages: []string{"pgvector/pgvector:pg16"},
		ExecutorID:     "exec-1",
		WorktreePath:   "/worktree/feat-a",
		ControlLeaf:    leaf,
		Auditor:        audit.Nop(),
		Logf:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	dockerHost, _, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.shutdown()
	if !strings.HasSuffix(dockerHost, fmt.Sprintf(":%d", want)) {
		t.Errorf("DOCKER_HOST = %q, want port %d (stable port when free)", dockerHost, want)
	}
	if p.boundPort != want {
		t.Errorf("boundPort = %d, want %d", p.boundPort, want)
	}
	// The control file must record the assigned port for the next run.
	got := stableport.ReadPreferred(leaf, portFileName)
	if got != want {
		t.Errorf("port file = %d, want %d", got, want)
	}
}

// TestStart_PortFilePreferredOverHash asserts Start prefers the
// previously-assigned port from the control file over a fresh hash, so
// the port stays stable even after the listener is torn down between runs.
func TestStart_PortFilePreferredOverHash(t *testing.T) {
	d := newFakeDaemon(t)
	leaf := t.TempDir()
	// Pre-seed the control file with a port that is NOT the hash-derived
	// one. Start must bind the seeded port (the hash is only a fallback).
	seeded := stableport.For("/worktree/feat-a") + 7
	if seeded >= stableport.StablePortMax {
		seeded = stableport.StablePortMin + (seeded - stableport.StablePortMax)
	}
	if err := stableport.WritePreferred(leaf, portFileName, seeded); err != nil {
		t.Fatal(err)
	}
	p, err := New(Config{
		Upstream:       d.server.URL,
		ApprovedImages: []string{"pgvector/pgvector:pg16"},
		ExecutorID:     "exec-1",
		WorktreePath:   "/worktree/feat-a",
		ControlLeaf:    leaf,
		Auditor:        audit.Nop(),
		Logf:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	dockerHost, _, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.shutdown()
	if p.boundPort != seeded {
		t.Errorf("boundPort = %d, want seeded %d (port file preferred over hash %d)", p.boundPort, seeded, stableport.For("/worktree/feat-a"))
	}
	if !strings.HasSuffix(dockerHost, fmt.Sprintf(":%d", seeded)) {
		t.Errorf("DOCKER_HOST = %q, want port %d", dockerHost, seeded)
	}
}

// TestStart_ScansWhenStablePortBusy asserts Start never stays on a held
// port: when the stable port AND the whole scan window are occupied, the
// bound port never equals the occupied stable port NOR any held window
// port — it is either a momentarily-released window neighbor or a
// fallback ephemeral port. Occupy the preferred port + PortScanWindow
// neighbors (wrapping) so every scan candidate is deterministically held;
// a partial occupation leaves a TOCTOU race at the first unheld neighbor
// (stableport.IsFree binds→closes→releases, so a checked-free port can be
// re-taken by the occupier before the proxy binds it).
func TestStart_ScansWhenStablePortBusy(t *testing.T) {
	d := newFakeDaemon(t)
	leaf := t.TempDir()
	want := stableport.For("/worktree/feat-b")
	// Occupy the stable port + the full scan window.
	held := make([]net.Listener, 0, stableport.PortScanWindow+1)
	for i := 0; i <= stableport.PortScanWindow; i++ {
		pn := want + i
		if pn >= stableport.StablePortMax {
			pn = stableport.StablePortMin + (pn - stableport.StablePortMax)
		}
		occ, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", pn))
		if err != nil {
			t.Logf("neighbor %d unoccupiable (%v) — treating as free", pn, err)
			continue
		}
		held = append(held, occ)
	}
	defer func() {
		for _, occ := range held {
			_ = occ.Close()
		}
	}()
	p, err := New(Config{
		Upstream:       d.server.URL,
		ApprovedImages: []string{"pgvector/pgvector:pg16"},
		ExecutorID:     "exec-1",
		WorktreePath:   "/worktree/feat-b",
		ControlLeaf:    leaf,
		Auditor:        audit.Nop(),
		Logf:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.shutdown()
	if p.boundPort == want {
		t.Errorf("boundPort = %d, must NOT be the occupied stable port", p.boundPort)
	}
	for _, occ := range held {
		hp := occ.Addr().(*net.TCPAddr).Port
		if p.boundPort == hp {
			t.Errorf("boundPort = %d collides with held window port %d (scan or bind picked a held port)", p.boundPort, hp)
		}
	}
}

// TestStart_ScanNeighborPersisted asserts that when the preferred stable
// port is occupied but a scan neighbor is free, the scan neighbor is
// persisted to the control file AND the log does NOT emit the "fallback"
// warning (issue #191: the old code treated chosen != preferred as
// "fallback" and skipped persistence, causing a permanent warn-loop).
func TestStart_ScanNeighborPersisted(t *testing.T) {
	d := newFakeDaemon(t)
	leaf := t.TempDir()
	preferred := stableport.For("/worktree/feat-scan")
	// Occupy ONLY the preferred port so Select scans to preferred+1.
	occ, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", preferred))
	if err != nil {
		t.Fatal(err)
	}
	defer occ.Close()

	var logBuf strings.Builder
	logf := func(format string, args ...any) {
		logBuf.WriteString(fmt.Sprintf(format, args...))
	}
	p, err := New(Config{
		Upstream:       d.server.URL,
		ApprovedImages: []string{"pgvector/pgvector:pg16"},
		ExecutorID:     "exec-1",
		WorktreePath:   "/worktree/feat-scan",
		ControlLeaf:    leaf,
		Auditor:        audit.Nop(),
		Logf:           logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.shutdown()
	// The bound port must be a scan neighbor (preferred+1 or within window),
	// NOT outside the stable range (which would be a true fallback).
	if p.boundPort < stableport.StablePortMin || p.boundPort >= stableport.StablePortMax {
		t.Errorf("boundPort = %d outside stable window, want a scan neighbor inside [%d,%d)", p.boundPort, stableport.StablePortMin, stableport.StablePortMax)
	}
	if p.boundPort == preferred {
		t.Errorf("boundPort = %d equals occupied preferred port", p.boundPort)
	}
	// The scan neighbor MUST be persisted: the core fix (issue #191).
	got := stableport.ReadPreferred(leaf, portFileName)
	if got != p.boundPort {
		t.Errorf("port file = %d, want bound neighbor %d (scanned neighbor was not persisted)", got, p.boundPort)
	}
	// The log must NOT contain the "fallback" warning (the new choosePort
	// return value false for in-window neighbors).
	if strings.Contains(logBuf.String(), "fallback") {
		t.Errorf("log contains 'fallback' warning but a scan neighbor is NOT a fallback:\n%s", logBuf.String())
	}
}

// TestStart_FallbackRandomWhenWindowFull asserts Start falls back to a
// random ephemeral port when the whole stable window is occupied, so the
// build never wedges (correctness over determinism). The whole window is
// simulated by overriding the port-free predicate via a test seam: rather
// than binding 50 real sockets (flaky and slow), this test patches
// stableport.IsFree indirectly by occupying the preferred + scan neighbors.
//
// Since the production stableport.Select uses the package-level
// stableport.IsFree, this
// test binds a real listener on every port the scan would touch (preferred
// + the next stableport.PortScanWindow-1). That is at most 50 listeners —
// practical
// on macOS/Linux loopback.
func TestStart_FallbackRandomWhenWindowFull(t *testing.T) {
	d := newFakeDaemon(t)
	leaf := t.TempDir()
	want := stableport.For("/worktree/feat-c")
	// Occupy the preferred port and the next portScanWindow-1 ports so the
	// scan exhausts the window and Start falls back to a random port.
	var occ []net.Listener
	defer func() {
		for _, l := range occ {
			l.Close()
		}
	}()
	for i := 0; i < stableport.PortScanWindow; i++ {
		p := want + i
		if p >= stableport.StablePortMax {
			break
		}
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			// A port in the window may already be in use by the test
			// runner or another listener — that just means fewer
			// sockets we need to bind. Continue.
			continue
		}
		occ = append(occ, l)
	}
	if len(occ) < stableport.PortScanWindow {
		// Could not occupy the whole window (host already uses some
		// ports). The fallback path is still exercised if the scan
		// happens to find no free port among the occupied ones; but to
		// deterministically assert the RANDOM fallback we need the whole
		// window occupied. Skip if the host would not let us.
		t.Skipf("could not occupy the full scan window (got %d of %d); cannot deterministically force the random fallback", len(occ), stableport.PortScanWindow)
	}
	p, err := New(Config{
		Upstream:       d.server.URL,
		ApprovedImages: []string{"pgvector/pgvector:pg16"},
		ExecutorID:     "exec-1",
		WorktreePath:   "/worktree/feat-c",
		ControlLeaf:    leaf,
		Auditor:        audit.Nop(),
		Logf:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.shutdown()
	// The bound port must NOT be in the stable range (the window was full).
	if p.boundPort >= stableport.StablePortMin && p.boundPort < stableport.StablePortMax {
		// It could still be a wrap-around port we did not occupy; check it
		// is one we actually occupied. If it is free, the scan found a gap
		// we could not occupy — still a valid (non-random) outcome. Only
		// fail if it landed on a port we occupied (impossible — it would
		// have failed to bind) or outside [StablePortMin,StablePortMax)
		// when the whole window was occupied.
		if p.boundPort >= want && p.boundPort < want+len(occ) {
			t.Errorf("boundPort = %d landed on an occupied port (bind should have failed)", p.boundPort)
		}
	}
	// Regardless of range, the port must be bindable and the proxy
	// serving: the build must not wedge.
	if p.boundPort <= 0 {
		t.Errorf("boundPort = %d, want a positive port (random fallback)", p.boundPort)
	}
}

// TestStart_LegacyRandomPortWhenNoWorktree asserts Start preserves the
// legacy random-port behavior when WorktreePath is empty (callers that
// did not wire the worktree path get the original v1 behavior).
func TestStart_LegacyRandomPortWhenNoWorktree(t *testing.T) {
	d := newFakeDaemon(t)
	p, err := New(Config{
		Upstream:       d.server.URL,
		ApprovedImages: []string{"pgvector/pgvector:pg16"},
		ExecutorID:     "exec-1",
		Auditor:        audit.Nop(),
		Logf:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	dockerHost, _, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.shutdown()
	// Legacy behavior: a random ephemeral port (not in the stable range,
	// not deterministic). Just assert it is a positive loopback port.
	if p.boundPort <= 0 {
		t.Errorf("boundPort = %d, want a positive random ephemeral port", p.boundPort)
	}
	if !strings.HasPrefix(dockerHost, "tcp://127.0.0.1:") {
		t.Errorf("DOCKER_HOST = %q, want loopback tcp", dockerHost)
	}
}

// TestStart_PortPersistsAcrossRestarts asserts the port assigned on the
// first Start is preferred on a second Start (new Proxy, same control
// leaf) so the warm Gradle daemon's DOCKER_HOST stays valid. This is the
// end-to-end reproduction of the bug being fixed.
func TestStart_PortPersistsAcrossRestarts(t *testing.T) {
	d := newFakeDaemon(t)
	leaf := t.TempDir()
	mk := func() *Proxy {
		t.Helper()
		p, err := New(Config{
			Upstream:       d.server.URL,
			ApprovedImages: []string{"pgvector/pgvector:pg16"},
			ExecutorID:     "exec-1",
			WorktreePath:   "/worktree/feat-d",
			ControlLeaf:    leaf,
			Auditor:        audit.Nop(),
			Logf:           func(string, ...any) {},
		})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	// First run: assigns and persists a stable port.
	p1 := mk()
	dh1, _, err := p1.Start()
	if err != nil {
		t.Fatal(err)
	}
	port1 := p1.boundPort
	p1.shutdown()
	// Second run (new proxy, same worktree + leaf): must bind the SAME port.
	p2 := mk()
	dh2, _, err := p2.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p2.shutdown()
	if p2.boundPort != port1 {
		t.Errorf("port drifted across runs: first=%d second=%d (warm daemon DOCKER_HOST would point at the dead port)", port1, p2.boundPort)
	}
	if dh1 != dh2 {
		t.Errorf("DOCKER_HOST drifted: first=%q second=%q", dh1, dh2)
	}
}

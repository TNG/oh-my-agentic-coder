package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
	"github.com/TNG/oh-my-agentic-coder/internal/sidecarauth"
)

const secret = "s3cret"

func TestMain(m *testing.M) {
	if os.Getenv("GATEWAY_FAKE_SERVER") == "1" {
		fakeServer()
		return
	}
	os.Exit(m.Run())
}

// fakeServer speaks line-delimited JSON-RPC on stdio.
func fakeServer() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(in.Bytes(), &m) != nil {
			continue
		}
		reply := func(result any) {
			out.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
		}
		switch m.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": "2025-06-18"})
		case "tools/list":
			var p struct {
				Cursor string `json:"cursor"`
			}
			var req struct {
				Params json.RawMessage `json:"params"`
			}
			json.Unmarshal(in.Bytes(), &req)
			json.Unmarshal(req.Params, &p)
			reply(fakeToolsPage(p.Cursor))
		case "tools/call":
			var req struct {
				Params struct {
					Name string `json:"name"`
				} `json:"params"`
			}
			json.Unmarshal(in.Bytes(), &req)
			switch req.Params.Name {
			case "debug":
				wd, _ := os.Getwd()
				reply(map[string]any{"pid": os.Getpid(), "cwd": wd, "env": os.Environ()})
			case "relist":
				out.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
				reply(map[string]any{"content": []any{}})
			default:
				reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": "called " + req.Params.Name}}})
			}
		case "notifications/initialized":
			out.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
		}
	}
}

// fakeToolsPage serves the tools listed in the JSON file GATEWAY_FAKE_TOOLS,
// re-read on every call, GATEWAY_FAKE_PAGE tools per page (cursor = index).
func fakeToolsPage(cursor string) map[string]any {
	var tools []json.RawMessage
	if b, err := os.ReadFile(os.Getenv("GATEWAY_FAKE_TOOLS")); err == nil {
		json.Unmarshal(b, &tools)
	}
	start, _ := strconv.Atoi(cursor)
	size, _ := strconv.Atoi(os.Getenv("GATEWAY_FAKE_PAGE"))
	if size <= 0 {
		size = len(tools) + 1
	}
	end := min(start+size, len(tools))
	res := map[string]any{"tools": tools[min(start, end):end]}
	if end < len(tools) {
		res["nextCursor"] = strconv.Itoa(end)
	}
	return res
}

type fakeTool struct{ Name, Description string }

func writeTools(t *testing.T, path string, tools ...fakeTool) {
	t.Helper()
	var raws []map[string]any
	for _, ft := range tools {
		raws = append(raws, map[string]any{"name": ft.Name, "description": ft.Description, "inputSchema": map[string]any{"type": "object"}})
	}
	b, _ := json.Marshal(raws)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func hashOf(t *testing.T, ft fakeTool) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"name": ft.Name, "description": ft.Description, "inputSchema": map[string]any{"type": "object"}})
	h, err := mcppolicy.ToolHash(raw)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

var baseTools = []fakeTool{{"echo", "echoes"}, {"wipe", "deletes"}, {"hidden", "off"}, {"debug", "dbg"}, {"relist", "relists"}}

func reviewOf(t *testing.T, tools ...fakeTool) *mcppolicy.Review {
	t.Helper()
	per := map[string]string{}
	for _, ft := range tools {
		per[ft.Name] = hashOf(t, ft)
	}
	return &mcppolicy.Review{ToolsHash: mcppolicy.ListHash(per), Tools: per, At: time.Now()}
}

func testOptions(t *testing.T) Options {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	toolsFile := filepath.Join(t.TempDir(), "tools.json")
	writeTools(t, toolsFile, baseTools...)
	o := Options{
		Def: &mcpreg.Definition{Name: "fake", Server: mcpreg.ServerSpec{Command: []string{exe}, Env: map[string]string{
			"GATEWAY_FAKE_SERVER": "1", "GATEWAY_FAKE_TOOLS": toolsFile,
		}}},
		Policy: &mcppolicy.Policy{
			Server: "fake",
			Access: mcppolicy.ReadWriteDelete,
			Tools: map[string]mcppolicy.Class{
				"echo": mcppolicy.Read, "wipe": mcppolicy.Delete, "hidden": mcppolicy.Off,
				"debug": mcppolicy.Read, "relist": mcppolicy.Read,
			},
			Approvals: mcppolicy.DefaultApprovals(),
			Review:    reviewOf(t, baseTools...),
		},
		Dir:    t.TempDir(),
		Secret: secret,
		ChildEnv: []string{
			"PATH=" + os.Getenv("PATH"),
			"OMAC_BASE=x",
			"SIDECAR_PORT=1234",
			sidecarauth.EnvVar + "=" + secret,
			"DECLARED_SECRET=hunter2",
		},
	}
	return o
}

func newServer(t *testing.T, mutate func(*Options)) *httptest.Server {
	t.Helper()
	o := testOptions(t)
	if mutate != nil {
		mutate(&o)
	}
	h, shutdown, err := Handler(o)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); shutdown() })
	return srv
}

func do(t *testing.T, method, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(sidecarauth.Header, secret)
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

var sseAccept = map[string]string{"Accept": "application/json, text/event-stream", "Content-Type": "application/json"}

func withSession(id string) map[string]string {
	h := map[string]string{"Accept": "application/json, text/event-stream", "Content-Type": "application/json", sessionHeader: id}
	return h
}

// readEvent returns the data of the next SSE message event.
func readEvent(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("reading event stream: %v", err)
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			return strings.TrimSpace(data)
		}
	}
}

func initialize(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp := do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, sseAccept)
	if resp.StatusCode != 200 {
		t.Fatalf("initialize status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}
	id := resp.Header.Get(sessionHeader)
	if id == "" {
		t.Fatal("no session id")
	}
	if ev := readEvent(t, bufio.NewReader(resp.Body)); !strings.Contains(ev, "2025-06-18") {
		t.Fatalf("initialize result = %s", ev)
	}
	return id
}

func debugInfo(t *testing.T, srv *httptest.Server, sid string) (info struct {
	PID int      `json:"pid"`
	CWD string   `json:"cwd"`
	Env []string `json:"env"`
}) {
	t.Helper()
	resp := do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"debug"}}`, withSession(sid))
	var msg struct {
		Result json.RawMessage `json:"result"`
	}
	ev := readEvent(t, bufio.NewReader(resp.Body))
	if err := json.Unmarshal([]byte(ev), &msg); err != nil || msg.Result == nil {
		t.Fatalf("debug reply %s: %v", ev, err)
	}
	if err := json.Unmarshal(msg.Result, &info); err != nil {
		t.Fatal(err)
	}
	return info
}

func TestRequiresSecret(t *testing.T) {
	srv := newServer(t, nil)
	for _, tc := range []struct{ name, value string }{{"missing", ""}, {"wrong", "nope"}} {
		for _, path := range []string{"/mcp", "/status"} {
			resp := do(t, "POST", srv.URL+path, "{}", map[string]string{sidecarauth.Header: tc.value})
			if resp.StatusCode != 401 {
				t.Errorf("%s %s: status = %d, want 401", tc.name, path, resp.StatusCode)
			}
		}
	}
	if resp := do(t, "GET", srv.URL+"/status", "", nil); resp.StatusCode != 200 {
		t.Fatalf("status with secret = %d", resp.StatusCode)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	srv := newServer(t, nil)
	sid := initialize(t, srv)

	resp := do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, withSession(sid))
	if resp.StatusCode != 202 {
		t.Fatalf("notification status = %d, want 202", resp.StatusCode)
	}
	resp = do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","id":"abc","method":"tools/list"}`, withSession(sid))
	if ev := readEvent(t, bufio.NewReader(resp.Body)); !strings.Contains(ev, `"id":"abc"`) || !strings.Contains(ev, "tools") {
		t.Fatalf("tools/list result = %s", ev)
	}
}

func TestPlainJSONAnswer(t *testing.T) {
	srv := newServer(t, nil)
	sid := initialize(t, srv)
	resp := do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`,
		map[string]string{"Accept": "application/json", "Content-Type": "application/json", sessionHeader: sid})
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q", ct)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"id":7`) {
		t.Fatalf("body = %s", b)
	}
}

func TestSessionIDErrors(t *testing.T) {
	srv := newServer(t, nil)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	if resp := do(t, "POST", srv.URL+"/mcp", body, sseAccept); resp.StatusCode != 400 {
		t.Errorf("no session id: status = %d, want 400", resp.StatusCode)
	}
	if resp := do(t, "POST", srv.URL+"/mcp", body, withSession("nope")); resp.StatusCode != 404 {
		t.Errorf("unknown session id: status = %d, want 404", resp.StatusCode)
	}
	if resp := do(t, "GET", srv.URL+"/mcp", "", withSession("nope")); resp.StatusCode != 404 {
		t.Errorf("GET unknown session: status = %d, want 404", resp.StatusCode)
	}
	if resp := do(t, "DELETE", srv.URL+"/mcp", "", withSession("nope")); resp.StatusCode != 404 {
		t.Errorf("DELETE unknown session: status = %d, want 404", resp.StatusCode)
	}
}

func TestConcurrentSessionsHaveOwnChild(t *testing.T) {
	srv := newServer(t, nil)
	a, b := initialize(t, srv), initialize(t, srv)
	if a == b {
		t.Fatal("sessions share an id")
	}
	if pa, pb := debugInfo(t, srv, a).PID, debugInfo(t, srv, b).PID; pa == pb {
		t.Fatalf("both sessions use child %d", pa)
	}
}

func TestServerNotificationOnGetStream(t *testing.T) {
	srv := newServer(t, nil)
	sid := initialize(t, srv)

	stream := do(t, "GET", srv.URL+"/mcp", "", withSession(sid))
	if stream.StatusCode != 200 || stream.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("GET stream: %d %q", stream.StatusCode, stream.Header.Get("Content-Type"))
	}
	if second := do(t, "GET", srv.URL+"/mcp", "", withSession(sid)); second.StatusCode != 409 {
		t.Fatalf("second stream status = %d, want 409", second.StatusCode)
	}
	do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, withSession(sid))
	if ev := readEvent(t, bufio.NewReader(stream.Body)); !strings.Contains(ev, "tools/list_changed") {
		t.Fatalf("event = %s", ev)
	}
}

func TestKeepaliveOnGetStream(t *testing.T) {
	g, err := newGateway(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	g.keepalive = 20 * time.Millisecond
	h, err := g.handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); g.shutdown() })
	sid := initialize(t, srv)
	stream := do(t, "GET", srv.URL+"/mcp", "", withSession(sid))
	line, err := bufio.NewReader(stream.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, ":") {
		t.Fatalf("first line = %q, err %v", line, err)
	}
}

func TestDeleteKillsChild(t *testing.T) {
	srv := newServer(t, nil)
	sid := initialize(t, srv)
	pid := debugInfo(t, srv, sid).PID
	if resp := do(t, "DELETE", srv.URL+"/mcp", "", withSession(sid)); resp.StatusCode != 204 {
		t.Fatalf("DELETE status = %d", resp.StatusCode)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("child %d still alive after DELETE", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resp := do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, withSession(sid)); resp.StatusCode != 404 {
		t.Fatalf("session after DELETE: status = %d, want 404", resp.StatusCode)
	}
}

func TestMaxSessions(t *testing.T) {
	srv := newServer(t, func(o *Options) { o.MaxSessions = 1 })
	sid := initialize(t, srv)
	resp := do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, sseAccept)
	if resp.StatusCode != 503 {
		t.Fatalf("second initialize status = %d, want 503", resp.StatusCode)
	}
	do(t, "DELETE", srv.URL+"/mcp", "", withSession(sid))
	initialize(t, srv)
}

func TestIdleSessionExpires(t *testing.T) {
	srv := newServer(t, func(o *Options) { o.IdleTimeout = 200 * time.Millisecond })
	sid := initialize(t, srv)
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp := do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","method":"notifications/x"}`, withSession(sid))
		if resp.StatusCode == 404 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("idle session did not expire")
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func TestChildEnvironmentAndDir(t *testing.T) {
	dir := t.TempDir()
	srv := newServer(t, func(o *Options) {
		o.Dir = dir
		o.Def.Mode = &mcpreg.ModeSpec{Name: "FAKE_MODE", Value: "readonly"}
	})
	info := debugInfo(t, srv, initialize(t, srv))
	env := strings.Join(info.Env, "\n")
	for _, bad := range []string{sidecarauth.EnvVar, secret, "OMAC_BASE", "SIDECAR_PORT"} {
		if strings.Contains(env, bad) {
			t.Errorf("child environment contains %q", bad)
		}
	}
	for _, want := range []string{"DECLARED_SECRET=hunter2", "FAKE_MODE=readonly", "PATH="} {
		if !strings.Contains(env, want) {
			t.Errorf("child environment lacks %q", want)
		}
	}
	// the temp dir may be reached through a symlink (macOS /var), so compare resolved paths
	if got, want := realPath(t, info.CWD), realPath(t, dir); got != want {
		t.Errorf("child cwd = %q, want %q", got, want)
	}
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A server that answers one request twice must not stall the session's read
// loop: later messages still reach their waiters.
func TestDuplicateResponseDoesNotBlockTheReadLoop(t *testing.T) {
	s := &session{log: io.Discard, waiters: map[string]chan []byte{}, events: make(chan []byte, 1)}
	reply, err := s.register("1")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		s.route([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		s.route([]byte(`{"jsonrpc":"2.0","id":1,"result":{"again":true}}`))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("route blocked on a duplicate response")
	}
	if got := string(<-reply); !strings.Contains(got, `"result":{}`) {
		t.Errorf("first response lost: %s", got)
	}
}

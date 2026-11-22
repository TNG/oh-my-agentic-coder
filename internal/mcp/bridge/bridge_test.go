package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorded struct {
	Method string
	Path   string
	Header http.Header
	Body   string
}

type fakeServer struct {
	mu       sync.Mutex
	requests []recorded
	// reply decides the POST answer; defaults to a JSON echo of the request.
	reply func(w http.ResponseWriter, r *http.Request, body []byte)
	// events are written to the GET stream.
	events chan string
}

func newFake() *fakeServer { return &fakeServer{events: make(chan string, 4)} }

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recorded{r.Method, r.URL.Path, r.Header.Clone(), string(body)})
	f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for {
			select {
			case ev := <-f.events:
				fmt.Fprintf(w, "data: %s\n\n", ev)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
	default:
		if f.reply != nil {
			f.reply(w, r, body)
			return
		}
		jsonReply(w, body)
	}
}

func jsonReply(w http.ResponseWriter, body []byte) {
	var m message
	_ = json.Unmarshal(body, &m)
	if m.Method == "initialize" {
		w.Header().Set("Mcp-Session-Id", "sess-1")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18"}}`, m.ID)
		return
	}
	if !m.isRequest() {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, m.ID)
}

func (f *fakeServer) byMethod(method string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.requests {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	text := strings.TrimSuffix(s.b.String(), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

const initLine = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`

func runOnce(t *testing.T, getenv func(string) string, input string) []string {
	t.Helper()
	var out syncBuffer
	if err := Run(context.Background(), "demo", strings.NewReader(input), &out, getenv); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return out.lines()
}

func TestJSONReplySessionAndDelete(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f)
	defer srv.Close()
	input := initLine + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"

	lines := runOnce(t, env(map[string]string{"OMAC_BASE": srv.URL + "/", "OMAC_FACADE_TOKEN": "tok"}), input)

	if len(lines) != 2 {
		t.Fatalf("stdout lines = %q", lines)
	}
	posts := f.byMethod(http.MethodPost)
	if len(posts) != 3 {
		t.Fatalf("posts = %d", len(posts))
	}
	for _, p := range posts {
		if p.Path != "/__mcp__/demo/mcp" {
			t.Errorf("path = %q", p.Path)
		}
		if p.Header.Get("X-Omac-Facade-Token") != "tok" {
			t.Errorf("token header missing: %v", p.Header)
		}
		if got := p.Header.Get("Accept"); got != "application/json, text/event-stream" {
			t.Errorf("Accept = %q", got)
		}
	}
	if posts[0].Header.Get("Mcp-Session-Id") != "" {
		t.Error("initialize must not carry a session id")
	}
	for _, p := range posts[1:] {
		if p.Header.Get("Mcp-Session-Id") != "sess-1" || p.Header.Get("MCP-Protocol-Version") != "2025-06-18" {
			t.Errorf("later request headers = %v", p.Header)
		}
	}
	del := f.byMethod(http.MethodDelete)
	if len(del) != 1 || del[0].Header.Get("Mcp-Session-Id") != "sess-1" {
		t.Errorf("delete = %+v", del)
	}
}

func TestSSEReplyWritesEachEventUntilResponse(t *testing.T) {
	f := newFake()
	f.reply = func(w http.ResponseWriter, r *http.Request, body []byte) {
		var m message
		_ = json.Unmarshal(body, &m)
		if m.Method == "initialize" {
			jsonReply(w, body)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,\n", m.ID)
		fmt.Fprint(w, "data: \"result\":{}}\n\n")
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"after\"}\n\n")
	}
	srv := httptest.NewServer(f)
	defer srv.Close()

	lines := runOnce(t, env(map[string]string{"OMAC_BASE": srv.URL}),
		initLine+"\n"+`{"jsonrpc":"2.0","id":7,"method":"tools/call"}`+"\n")

	want := []string{
		`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/progress"}`,
		`{"jsonrpc":"2.0","id":7,"result":{}}`,
	}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %q", lines)
	}
}

func TestAcceptedReplyWritesNothing(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f)
	defer srv.Close()
	lines := runOnce(t, env(map[string]string{"OMAC_BASE": srv.URL}),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	if len(lines) != 0 {
		t.Fatalf("lines = %q", lines)
	}
	if len(f.byMethod(http.MethodDelete)) != 0 {
		t.Error("no session, so no DELETE expected")
	}
}

func TestGETStreamForwardsServerMessages(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f)
	defer srv.Close()
	pr, pw := io.Pipe()
	var out syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), "demo", pr, &out, env(map[string]string{"OMAC_BASE": srv.URL}))
	}()

	fmt.Fprintln(pw, initLine)
	waitFor(t, func() bool { return len(f.byMethod(http.MethodGet)) == 1 })
	get := f.byMethod(http.MethodGet)[0]
	if get.Header.Get("Mcp-Session-Id") != "sess-1" || get.Header.Get("MCP-Protocol-Version") != "2025-06-18" {
		t.Errorf("GET headers = %v", get.Header)
	}
	f.events <- `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`
	waitFor(t, func() bool { return len(out.lines()) == 2 })
	if got := out.lines()[1]; got != `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}` {
		t.Errorf("forwarded = %q", got)
	}

	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(f.byMethod(http.MethodDelete)) != 1 {
		t.Error("expected DELETE on EOF")
	}
}

func TestUnixSocketFallbackSendsNoToken(t *testing.T) {
	f := newFake()
	socket := filepath.Join(t.TempDir(), "o.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	srv := &http.Server{Handler: f}
	go srv.Serve(l)
	defer srv.Close()

	lines := runOnce(t, env(map[string]string{
		"OMAC_BASE": "not a url", "OMAC_SOCKET": socket, "OMAC_FACADE_TOKEN": "tok",
	}), `{"jsonrpc":"2.0","id":3,"method":"ping"}`+"\n")

	if len(lines) != 1 {
		t.Fatalf("lines = %q", lines)
	}
	posts := f.byMethod(http.MethodPost)
	if len(posts) != 1 || posts[0].Path != "/__mcp__/demo/mcp" {
		t.Fatalf("posts = %+v", posts)
	}
	if posts[0].Header.Get("X-Omac-Facade-Token") != "" {
		t.Error("token must not be sent over the socket")
	}
}

func TestHTTPFailureBecomesJSONRPCError(t *testing.T) {
	f := newFake()
	f.reply = func(w http.ResponseWriter, r *http.Request, body []byte) {
		http.Error(w, "boom", http.StatusBadGateway)
	}
	srv := httptest.NewServer(f)
	defer srv.Close()

	lines := runOnce(t, env(map[string]string{"OMAC_BASE": srv.URL}),
		`{"jsonrpc":"2.0","id":"abc","method":"tools/call"}`+"\n"+
			`{"jsonrpc":"2.0","method":"notifications/cancelled"}`+"\n")

	if len(lines) != 1 {
		t.Fatalf("lines = %q", lines)
	}
	var got struct {
		ID    string `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "abc" || got.Error.Code != jsonRPCInternalError || !strings.Contains(got.Error.Message, "502") {
		t.Errorf("error response = %s", lines[0])
	}
}

func TestConnectionFailureBecomesJSONRPCError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	lines := runOnce(t, env(map[string]string{"OMAC_BASE": base}), `{"jsonrpc":"2.0","id":9,"method":"ping"}`+"\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"id":9`) || !strings.Contains(lines[0], `"error"`) {
		t.Fatalf("lines = %q", lines)
	}
}

func TestProxyEnvironmentIgnored(t *testing.T) {
	proxyHit := make(chan struct{}, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHit <- struct{}{}
		http.Error(w, "proxy", http.StatusTeapot)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")

	f := newFake()
	srv := httptest.NewServer(f)
	defer srv.Close()
	// 127.0.0.1 is bypassed by Go's default proxy rules, so use a hostname that
	// only the target resolves to via its own address.
	base := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)

	lines := runOnce(t, env(map[string]string{"OMAC_BASE": base}), `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n")
	if len(lines) != 1 || strings.Contains(lines[0], "error") {
		t.Fatalf("lines = %q", lines)
	}
	select {
	case <-proxyHit:
		t.Fatal("request went through the proxy")
	default:
	}
}

func TestNoExtraHeaders(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f)
	defer srv.Close()
	runOnce(t, env(map[string]string{"OMAC_BASE": srv.URL}), `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n")
	allowed := map[string]bool{"Accept": true, "Content-Type": true, "Content-Length": true, "Accept-Encoding": true}
	for name := range f.byMethod(http.MethodPost)[0].Header {
		if !allowed[name] {
			t.Errorf("unexpected header %q", name)
		}
	}
}

func TestStdoutOnlyProtocolLines(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f)
	defer srv.Close()
	lines := runOnce(t, env(map[string]string{"OMAC_BASE": srv.URL}),
		"not json\n\n"+initLine+"\n"+`{"jsonrpc":"2.0","id":2,"method":"ping"}`+"\n")
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil || m["jsonrpc"] != "2.0" {
			t.Errorf("non-protocol stdout line %q", l)
		}
	}
	if len(lines) != 2 {
		t.Errorf("lines = %q", lines)
	}
}

func TestMissingEndpointIsError(t *testing.T) {
	err := Run(context.Background(), "demo", strings.NewReader(""), io.Discard, env(nil))
	if err == nil {
		t.Fatal("expected error")
	}
}

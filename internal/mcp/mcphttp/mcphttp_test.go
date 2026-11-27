package mcphttp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is a minimal Streamable HTTP MCP server. It records the headers of
// every request.
type fakeServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []http.Header
	methods  []string
	expired  bool
}

func newFakeServer(t *testing.T, bearer string) *fakeServer {
	f := &fakeServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Header.Clone())
		f.methods = append(f.methods, r.Method)
		expired := f.expired
		f.mu.Unlock()
		if bearer != "" && r.Header.Get("Authorization") != "Bearer "+bearer {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodGet:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if expired {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &m)
		switch m.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(m.ID)+`,"result":{"protocolVersion":"2025-06-18","capabilities":{}}}`)
		case "tools/list":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":"+string(m.ID)+",\"result\":{\"tools\":[{\"name\":\"echo\"}]}}\n\n")
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) header(i int, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i].Get(name)
}

type collector struct {
	ch chan string
}

func newCollector() *collector   { return &collector{ch: make(chan string, 8)} }
func (c *collector) on(b []byte) { c.ch <- string(b) }
func (c *collector) next(t *testing.T) string {
	t.Helper()
	select {
	case m := <-c.ch:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
		return ""
	}
}

func TestHandshakeCarriesSessionVersionAndBearer(t *testing.T) {
	f := newFakeServer(t, "tok")
	c := newCollector()
	tr, err := New(Config{URL: f.URL, Bearer: "tok", Headers: map[string]string{"X-Team": "a"}}, c.on, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	if err := tr.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	if got := c.next(t); !strings.Contains(got, `"protocolVersion":"2025-06-18"`) {
		t.Fatalf("reply = %s", got)
	}
	if err := tr.Send([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		t.Fatal(err)
	}
	if err := tr.Send([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)); err != nil {
		t.Fatal(err)
	}
	if got := c.next(t); !strings.Contains(got, `"echo"`) {
		t.Fatalf("SSE reply = %s", got)
	}
	if got := f.header(0, "MCP-Protocol-Version"); got != "" {
		t.Errorf("initialize carried a protocol version: %q", got)
	}
	if got := f.header(1, "Mcp-Session-Id"); got != "s1" {
		t.Errorf("session id = %q", got)
	}
	if got := f.header(1, "MCP-Protocol-Version"); got != "2025-06-18" {
		t.Errorf("protocol version = %q", got)
	}
	if got := f.header(1, "Authorization"); got != "Bearer tok" {
		t.Errorf("authorization = %q", got)
	}
	if got := f.header(1, "X-Team"); got != "a" {
		t.Errorf("literal header = %q", got)
	}
}

func TestWrongBearerIsAnError(t *testing.T) {
	f := newFakeServer(t, "tok")
	tr, err := New(Config{URL: f.URL, Bearer: "wrong"}, newCollector().on, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	err = tr.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Error("the error names the token")
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	other := newFakeServer(t, "")
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer redirect.Close()
	tr, err := New(Config{URL: redirect.URL, Bearer: "tok"}, newCollector().on, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	err = tr.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("err = %v", err)
	}
	other.mu.Lock()
	defer other.mu.Unlock()
	if len(other.requests) != 0 {
		t.Error("the redirect target received a request, with the credentials")
	}
}

func TestExpiredSessionEndsTheTransport(t *testing.T) {
	f := newFakeServer(t, "")
	closed := make(chan struct{})
	c := newCollector()
	tr, err := New(Config{URL: f.URL}, c.on, func() { close(closed) })
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)); err != nil {
		t.Fatal(err)
	}
	c.next(t)
	f.mu.Lock()
	f.expired = true
	f.mu.Unlock()
	if err := tr.Send([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)); err != ErrSessionExpired {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("onClose not called")
	}
}

func TestCloseDeletesTheSession(t *testing.T) {
	f := newFakeServer(t, "")
	c := newCollector()
	tr, _ := New(Config{URL: f.URL}, c.on, nil)
	if err := tr.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)); err != nil {
		t.Fatal(err)
	}
	c.next(t)
	tr.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	if last := f.methods[len(f.methods)-1]; last != http.MethodDelete {
		t.Errorf("last request = %s, want DELETE", last)
	}
}

func TestValidate(t *testing.T) {
	good := []string{"https://mcp.example.com/mcp", "http://localhost:8080/mcp", "http://127.0.0.1/mcp", "http://[::1]:9/mcp"}
	bad := map[string]string{
		"http://mcp.example.com/mcp":    "loopback",
		"ftp://example.com":             "scheme",
		"https://user:pw@example.com":   "user info",
		"https://example.com/mcp?k=tok": "query",
		"https://example.com/mcp#frag":  "fragment",
		"not a url":                     "valid URL",
		"https:///nohost":               "valid URL",
	}
	for _, u := range good {
		if err := Validate(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	for u, want := range bad {
		if err := Validate(u); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want mention of %q", u, err, want)
		}
	}
}

func TestAddressGuard(t *testing.T) {
	cases := []struct {
		addr               string
		loopbackOK, privOK bool
		wantErr            bool
	}{
		{"93.184.216.34:443", false, false, false},
		{"127.0.0.1:80", true, false, false},
		{"127.0.0.1:80", false, false, true},
		{"10.1.2.3:443", false, false, true},
		{"10.1.2.3:443", false, true, false},
		{"192.168.0.5:443", false, false, true},
		{"172.16.0.1:443", false, false, true},
		{"169.254.169.254:80", false, false, true},
		{"169.254.169.254:80", false, true, false},
		{"100.64.0.1:443", false, false, true},
		{"0.0.0.0:80", false, false, true},
		{"[fe80::1]:443", false, false, true},
		{"[fd00::1]:443", false, false, true},
		{"[::1]:80", false, false, true},
		{"[2606:4700::1111]:443", false, false, false},
	}
	for _, c := range cases {
		err := addressGuard(c.loopbackOK, c.privOK)("tcp", c.addr, nil)
		if (err != nil) != c.wantErr {
			t.Errorf("%s loopbackOK=%v privOK=%v: err = %v, wantErr %v", c.addr, c.loopbackOK, c.privOK, err, c.wantErr)
		}
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{"localhost": true, "LOCALHOST": true, "127.0.0.1": true, "::1": true, "evil.example.com": false, "10.0.0.1": false, "localhost.evil.com": false} {
		if got := IsLoopbackHost(host); got != want {
			t.Errorf("%s: %v, want %v", host, got, want)
		}
	}
}

package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
)

// upstream is a Streamable HTTP MCP server that answers with plain JSON.
type upstream struct {
	*httptest.Server
	mu    sync.Mutex
	calls []string
	auth  []string
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &m)
		u.mu.Lock()
		u.auth = append(u.auth, r.Header.Get("Authorization"))
		if m.Method == "tools/call" {
			u.calls = append(u.calls, m.Params.Name)
		}
		u.mu.Unlock()
		reply := func(result any) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
		}
		switch m.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "up1")
			reply(map[string]any{"protocolVersion": "2025-06-18"})
		case "tools/list":
			var tools []map[string]any
			for _, ft := range baseTools {
				tools = append(tools, map[string]any{"name": ft.Name, "description": ft.Description, "inputSchema": map[string]any{"type": "object"}})
			}
			reply(map[string]any{"tools": tools})
		case "tools/call":
			reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": "called " + m.Params.Name}}})
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func remoteGateway(t *testing.T, up *upstream, mutate func(*Options)) *httptest.Server {
	return newServer(t, func(o *Options) {
		o.Def = &mcpreg.Definition{Name: "fake", Secrets: []string{"UP_TOKEN"}, Server: mcpreg.ServerSpec{URL: up.URL, BearerSecret: "UP_TOKEN"}}
		o.ChildEnv = append(o.ChildEnv, "UP_TOKEN=up-secret-token")
		if mutate != nil {
			mutate(o)
		}
	})
}

func TestRemoteServerThroughTheGateway(t *testing.T) {
	up := newUpstream(t)
	srv := remoteGateway(t, up, nil)
	sid := initialize(t, srv)

	resp := do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, withSession(sid))
	list := readEvent(t, bufio.NewReader(resp.Body))
	if !strings.Contains(list, `"echo"`) || strings.Contains(list, `"hidden"`) {
		t.Fatalf("the tool list must show echo and hide the off tool: %s", list)
	}

	resp = do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo"}}`, withSession(sid))
	if got := readEvent(t, bufio.NewReader(resp.Body)); !strings.Contains(got, "called echo") {
		t.Fatalf("echo reply = %s", got)
	}

	resp = do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"hidden"}}`, withSession(sid))
	if got := readEvent(t, bufio.NewReader(resp.Body)); !strings.Contains(got, `"error"`) {
		t.Fatalf("a tool classed off must be refused: %s", got)
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	for _, c := range up.calls {
		if c == "hidden" {
			t.Error("the refused call reached the remote server")
		}
	}
	for _, a := range up.auth {
		if a != "Bearer up-secret-token" {
			t.Errorf("authorization = %q", a)
		}
	}
}

func TestRemoteBearerNeverReachesTheAgentSide(t *testing.T) {
	up := newUpstream(t)
	srv := remoteGateway(t, up, nil)
	resp := do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, sseAccept)
	body, _ := io.ReadAll(resp.Body)
	for k, v := range resp.Header {
		if strings.Contains(strings.Join(v, " "), "up-secret-token") {
			t.Errorf("header %s carries the token", k)
		}
	}
	if strings.Contains(string(body), "up-secret-token") {
		t.Error("the body carries the token")
	}
}

func TestRemoteWithoutItsSecretIsRefusedAtInitialize(t *testing.T) {
	up := newUpstream(t)
	srv := remoteGateway(t, up, func(o *Options) {
		o.ChildEnv = o.ChildEnv[:len(o.ChildEnv)-1]
	})
	resp := do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, sseAccept)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.auth) != 0 {
		t.Error("a request went out without the token")
	}
}

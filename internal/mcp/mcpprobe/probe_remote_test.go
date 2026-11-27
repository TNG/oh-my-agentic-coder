package mcpprobe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
)

func TestListToolsOfAURLServerSendsTheBearer(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &m)
		w.Header().Set("Content-Type", "application/json")
		switch m.Method {
		case "initialize":
			io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(m.ID)+`,"result":{"protocolVersion":"2025-06-18"}}`)
		case "tools/list":
			io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(m.ID)+`,"result":{"tools":[{"name":"echo","description":"d","inputSchema":{"type":"object"}}]}}`)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()

	def := &mcpreg.Definition{Name: "r", Secrets: []string{"T"}, Server: mcpreg.ServerSpec{URL: srv.URL, BearerSecret: "T"}}
	sec := map[string]secrets.Secret{"T": secrets.NewSecretString("tok")}
	tools, tail, err := ListTools(context.Background(), &mcpreg.Entry{Def: def}, sec, nil, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" || tail != "" {
		t.Fatalf("tools = %+v, tail = %q", tools, tail)
	}
	for _, a := range auth {
		if a != "Bearer tok" {
			t.Errorf("authorization = %q", a)
		}
	}
	if _, _, err := ListTools(context.Background(), &mcpreg.Entry{Def: def}, nil, nil, time.Second); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("a missing secret must fail: %v", err)
	}
}

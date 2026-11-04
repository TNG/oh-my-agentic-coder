package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
	"github.com/TNG/oh-my-agentic-coder/internal/supervisor"
)

func TestSecurityServeGlobalListRequiresControlToken(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)
	s.sup = supervisor.New(nil, audit.Nop(), nil)
	s.global["probe"] = &skillRoute{
		Name: "probe", Mount: "probe", Namespace: facade.GlobalNamespace, State: facade.RouteReady,
	}
	srv := httptest.NewServer(s.controlMux())
	t.Cleanup(srv.Close)

	want := map[string]any{"skills": []any{map[string]any{
		"name": "probe", "mount": "probe", "scope": "global", "state": "ready",
		"base":        fmt.Sprintf("http://127.0.0.1:%d/__global__/probe", s.tcpPort),
		"socket_base": fmt.Sprintf("http+unix://%s/__global__/probe", url.PathEscape(s.socketPath)),
	}}}
	for _, tc := range []struct {
		name, token string
		status      int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong", "wrong", http.StatusUnauthorized},
		{"valid", s.controlToken, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := controlRoundTrip(t, http.MethodGet, srv.URL+"/__omac__/global", tc.token, "")
			if status != tc.status {
				t.Fatalf("status = %d, want %d; body=%s", status, tc.status, body)
			}
			if status == http.StatusOK {
				var got map[string]any
				if err := json.Unmarshal([]byte(body), &got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("global list = %#v, want %#v", got, want)
				}
			}
		})
	}

	// With an empty registry, a real reload removes the old global entry.
	status, body := controlRoundTrip(t, http.MethodPost, srv.URL+"/__omac__/reload-global", s.controlToken, "{}")
	if status != http.StatusOK || body != "{\"skills\":[]}\n" {
		t.Fatalf("reload-global = %d %s, want refreshed empty list", status, body)
	}
	status, body = controlRoundTrip(t, http.MethodGet, srv.URL+"/__omac__/global", s.controlToken, "")
	if status != http.StatusOK || body != "{\"skills\":[]}\n" {
		t.Fatalf("global after reload = %d %s, want refreshed empty list", status, body)
	}
}

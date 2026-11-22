package facade

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
	"github.com/TNG/oh-my-agentic-coder/internal/sidecarauth"
)

const (
	routeSecret   = "minted-secret"
	facadeToken   = "agent-facade-token"
	forgedSecret  = "forged-by-agent"
	echoedSecret  = "echoed-by-sidecar"
	plainReply    = "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nX-Omac-Sidecar-Secret: " + echoedSecret + "\r\n\r\n"
	upgradeReply  = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nX-Omac-Sidecar-Secret: " + echoedSecret + "\r\n\r\n"
	refusedUpgrad = "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nX-Omac-Sidecar-Secret: " + echoedSecret + "\r\n\r\n"
)

const agentHeaders = "X-Omac-Facade-Token: " + facadeToken + "\r\n" +
	"X-Omac-Sidecar-Secret: " + forgedSecret + "\r\n" +
	"x_omac_sidecar_secret: " + forgedSecret + "\r\n" +
	"X_Omac_Facade_Token: " + facadeToken + "\r\n"

func startSecretFacade(t *testing.T, s *fakeSidecar) int {
	t.Helper()
	f := New("", "127.0.0.1:0", []Route{{
		Mount:    "skill",
		Upstream: sidecarauth.Upstream{Port: s.port, Secret: routeSecret},
		State:    RouteReady,
	}}, 0, time.Minute, "", "test")
	f.FacadeToken = facadeToken
	if err := f.Start(context.Background()); err != nil {
		t.Fatalf("facade start: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f.TCPPort()
}

func assertSidecarSawOnlyRouteSecret(t *testing.T, saw string) {
	t.Helper()
	lower := strings.ToLower(saw)
	for _, bad := range []string{"x-omac-facade-token", "x_omac_facade_token", "x_omac_sidecar_secret", forgedSecret, facadeToken} {
		if strings.Contains(lower, bad) {
			t.Errorf("sidecar saw %q:\n%s", bad, saw)
		}
	}
	if got := strings.Count(lower, "x-omac-sidecar-secret: "+routeSecret); got != 1 {
		t.Errorf("sidecar saw the route secret %d times, want 1:\n%s", got, saw)
	}
	if got := strings.Count(lower, "x-omac-sidecar-secret"); got != 1 {
		t.Errorf("sidecar saw %d secret headers, want 1:\n%s", got, saw)
	}
}

func readAll(t *testing.T, c interface {
	io.Reader
	SetReadDeadline(time.Time) error
}) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, _ := io.ReadAll(c)
	return string(b)
}

func TestSidecarSeesOnlyRouteSecretOverHTTP(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	var saw http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw = r.Header.Clone()
		w.Header().Set(sidecarauth.Header, echoedSecret)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)
	port := up.Listener.Addr().(*net.TCPAddr).Port
	facadePort := startSecretFacade(t, &fakeSidecar{port: port})
	c := dialFacade(t, facadePort, "GET /skill/x HTTP/1.1\r\nHost: facade\r\n"+agentHeaders+"Connection: close\r\n\r\n")
	resp := readAll(t, c)

	if got := saw.Values(sidecarauth.Header); len(got) != 1 || got[0] != routeSecret {
		t.Errorf("sidecar saw secret values %v, want only the route secret", got)
	}
	for name, values := range saw {
		lower := strings.ToLower(name)
		if lower == "x-omac-sidecar-secret" {
			continue
		}
		if strings.Contains(lower, "omac") && lower != "x-forwarded-prefix" {
			t.Errorf("sidecar saw header %s=%v", name, values)
		}
	}
	if strings.Contains(strings.ToLower(resp), "x-omac-sidecar-secret") || strings.Contains(resp, echoedSecret) {
		t.Errorf("client saw an echoed secret header:\n%s", resp)
	}
}

func TestSidecarSeesOnlyRouteSecretOverUpgrade(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	for name, reply := range map[string]string{"switching": upgradeReply, "refused": refusedUpgrad} {
		t.Run(name, func(t *testing.T) {
			s := startSidecar(t, reply)
			port := startSecretFacade(t, s)
			c := dialFacade(t, port, "GET /skill/ws HTTP/1.1\r\nHost: facade\r\nConnection: upgrade\r\nUpgrade: websocket\r\n"+agentHeaders+"\r\n")
			resp := readAll(t, c)
			time.Sleep(100 * time.Millisecond)

			assertSidecarSawOnlyRouteSecret(t, s.saw())
			if strings.Contains(strings.ToLower(resp), "x-omac-sidecar-secret") || strings.Contains(resp, echoedSecret) {
				t.Errorf("client saw an echoed secret header:\n%s", resp)
			}
		})
	}
}

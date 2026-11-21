package cli

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

const (
	facadeDefaultBody = 10 << 20
	declaredLarge     = 25 << 20
	declaredSmall     = 64 << 10
)

func metaDeclaringBodyLimit(t *testing.T, limit int64) *config.Meta {
	t.Helper()
	path := filepath.Join(t.TempDir(), config.MetaFileName)
	yaml := fmt.Sprintf("name: limited\nsidecar:\n  command: [\"true\"]\n  limits:\n    max_body_bytes: %d\n", limit)
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := config.LoadMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// routeBuilders runs each code path that mounts a ready skill route, with the
// skill's manifest, and returns the route it hands to the facade.
var routeBuilders = []struct {
	name  string
	build func(meta *config.Meta, port int) facade.Route
}{
	{"start", func(m *config.Meta, port int) facade.Route {
		return startRoute("limited", port, "limited", "/skill", m, routeApprovals(m), config.DefaultHealthPath)
	}},
	{"serve", func(m *config.Meta, port int) facade.Route {
		return readySkillRoute("limited", "limited", "tok", "/skill", m).facadeRoute(port)
	}},
	{"serve single-directory alias", func(m *config.Meta, port int) facade.Route {
		return readySkillRoute("limited", "limited", "tok", "/skill", m).flatAliasRoute(port)
	}},
	{"reload", func(m *config.Meta, port int) facade.Route {
		return reloadRoute("limited", port, "limited", m)
	}},
}

func TestRoutesCarryDeclaredMaxBodyBytes(t *testing.T) {
	for _, limit := range []int64{declaredLarge, declaredSmall} {
		meta := metaDeclaringBodyLimit(t, limit)
		for _, b := range routeBuilders {
			t.Run(fmt.Sprintf("%s/%d", b.name, limit), func(t *testing.T) {
				if got := b.build(meta, 1).MaxBodyBytes; got != limit {
					t.Errorf("Route.MaxBodyBytes = %d, want %d", got, limit)
				}
			})
		}
	}
}

func TestRouteMaxBodyBytesUnsetMeansFacadeDefault(t *testing.T) {
	for name, m := range map[string]*config.Meta{
		"nil meta":    nil,
		"no sidecar":  {},
		"no limits":   {Sidecar: &config.SidecarMeta{}},
		"zero limits": {Sidecar: &config.SidecarMeta{Limits: &config.LimitsSpec{}}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := routeMaxBodyBytes(m); got != 0 {
				t.Errorf("routeMaxBodyBytes = %d, want 0", got)
			}
		})
	}
}

func TestRoutesEnforceDeclaredMaxBodyBytes(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	}))
	t.Cleanup(sidecar.Close)
	_, portStr, _ := net.SplitHostPort(sidecar.Listener.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	for _, limit := range []int64{declaredLarge, declaredSmall} {
		meta := metaDeclaringBodyLimit(t, limit)
		for _, b := range routeBuilders {
			t.Run(fmt.Sprintf("%s/%d", b.name, limit), func(t *testing.T) {
				route := b.build(meta, port)
				route.State = facade.RouteReady
				f := facade.New("", "127.0.0.1:0", []facade.Route{route}, facadeDefaultBody, 0, "", "test")
				if err := f.Start(t.Context()); err != nil {
					t.Fatalf("facade start: %v", err)
				}
				t.Cleanup(func() { f.Close() })

				prefix := route.Mount
				if route.Namespace != "" {
					prefix = route.Namespace + "/" + route.Mount
				}
				post := func(size int64, chunked bool) int {
					url := fmt.Sprintf("http://127.0.0.1:%d/%s/ingest", f.TCPPort(), prefix)
					var body io.Reader = bytes.NewReader(make([]byte, size))
					if chunked {
						body = io.MultiReader(body)
					}
					resp, err := http.Post(url, "application/octet-stream", body)
					if err != nil {
						t.Fatalf("post %d bytes: %v", size, err)
					}
					defer resp.Body.Close()
					return resp.StatusCode
				}
				for _, chunked := range []bool{false, true} {
					if got := post(limit, chunked); got != http.StatusOK {
						t.Errorf("chunked=%v: body of exactly %d bytes: status %d, want 200", chunked, limit, got)
					}
					if got := post(limit+1, chunked); got != http.StatusRequestEntityTooLarge {
						t.Errorf("chunked=%v: body of %d bytes: status %d, want 413", chunked, limit+1, got)
					}
				}
			})
		}
	}
}

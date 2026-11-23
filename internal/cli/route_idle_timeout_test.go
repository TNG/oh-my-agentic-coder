package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
)

func TestRouteIdleTimeout(t *testing.T) {
	withLimits := &config.Meta{Sidecar: &config.SidecarMeta{Limits: &config.LimitsSpec{IdleTimeoutSecs: 90}}}
	cases := map[string]struct {
		meta *config.Meta
		want time.Duration
	}{
		"nil meta":      {nil, 0},
		"no sidecar":    {&config.Meta{}, 0},
		"no limits":     {&config.Meta{Sidecar: &config.SidecarMeta{}}, 0},
		"limit not set": {&config.Meta{Sidecar: &config.SidecarMeta{Limits: &config.LimitsSpec{}}}, 0},
		"limit set":     {withLimits, 90 * time.Second},
	}
	for name, c := range cases {
		if got := routeIdleTimeout(c.meta); got != c.want {
			t.Errorf("%s: got %v, want %v", name, got, c.want)
		}
	}
}

func metaDeclaringIdleTimeout(t *testing.T, secs int) *config.Meta {
	t.Helper()
	path := filepath.Join(t.TempDir(), config.MetaFileName)
	yaml := fmt.Sprintf("name: limited\nsidecar:\n  command: [\"true\"]\n  limits:\n    idle_timeout_secs: %d\n", secs)
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := config.LoadMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// idleRouteBuilders runs each code path that mounts a ready skill route, with
// the skill's manifest, and returns the route it hands to the facade.
var idleRouteBuilders = []struct {
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

func TestRoutesCarryDeclaredIdleTimeout(t *testing.T) {
	meta := metaDeclaringIdleTimeout(t, 90)
	for _, b := range idleRouteBuilders {
		t.Run(b.name, func(t *testing.T) {
			if got := b.build(meta, 1).IdleTimeout; got != 90*time.Second {
				t.Errorf("Route.IdleTimeout = %v, want 90s", got)
			}
		})
	}
}

func TestRoutesWithoutIdleTimeoutUseFacadeValue(t *testing.T) {
	for _, b := range idleRouteBuilders {
		t.Run(b.name, func(t *testing.T) {
			if got := b.build(&config.Meta{Sidecar: &config.SidecarMeta{}}, 1).IdleTimeout; got != 0 {
				t.Errorf("Route.IdleTimeout = %v, want 0 (facade value)", got)
			}
		})
	}
}

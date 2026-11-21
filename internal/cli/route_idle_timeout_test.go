package cli

import (
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
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

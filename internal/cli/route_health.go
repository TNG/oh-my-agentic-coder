package cli

import "github.com/TNG/oh-my-agentic-coder/internal/config"

// routeHealthPath returns the health endpoint a skill's manifest declares
// for its sidecar, or the default when it declares none. The facade lets
// requests to this path skip the per-sidecar request cap.
func routeHealthPath(m *config.Meta) string {
	var health *config.HealthSpec
	if m != nil && m.Sidecar != nil {
		health = m.Sidecar.Health
	}
	return health.Defaults().Path
}

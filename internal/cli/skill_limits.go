package cli

import "github.com/TNG/oh-my-agentic-coder/internal/config"

// routeMaxBodyBytes is the request body limit a skill declares with
// limits.max_body_bytes. It returns 0 when the skill sets none, which the
// facade reads as "use the facade-wide default". A negative value would
// remove the cap, so it counts as unset.
func routeMaxBodyBytes(meta *config.Meta) int64 {
	if meta == nil || meta.Sidecar == nil || meta.Sidecar.Limits == nil {
		return 0
	}
	return max(meta.Sidecar.Limits.MaxBodyBytes, 0)
}

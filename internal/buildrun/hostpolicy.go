package buildrun

import (
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
)

// HostPolicy returns the host-controlled authority ceiling the build path
// enforces, derived from the existing build-run defaults: defaultMaxHeap is
// the Gradle daemon -Xmx ceiling, and the --max-duration CLI flag (when set)
// bounds total build wall-clock. The manifest may REQUEST resource values
// within this ceiling but cannot widen it (spec.md:150).
//
// maxDuration is the effective --max-duration for this invocation. A zero
// maxDuration means no per-invocation duration ceiling is set, so a manifest
// resources.maxDuration request is fail-closed denied (the host has not
// authorized a duration ceiling for the request to be checked against).
//
// MaxCPU / MaxProcesses are left zero (not yet wired to concrete host
// limits). A manifest request for those dimensions is fail-closed denied
// with an actionable message (see validateResources) until later tickets
// populate them from real host limits — this is honest: spec.md:150 says
// OMAC "provides" ceilings, so an unset dimension rejects requests rather
// than letting any value through.
//
// The returned buildmanifest.HostPolicy is what the CLI passes to
// buildmanifest.Validate and buildmanifest.Gate.
func HostPolicy(maxDuration time.Duration) buildmanifest.HostPolicy {
	return buildmanifest.HostPolicy{
		MaxHeap:     defaultMaxHeap,
		MaxDuration: maxDuration,
		// MaxCPU / MaxProcesses intentionally zero: not wired to real host
		// limits yet. validateResources fail-closes a manifest request for
		// these dimensions until a later ticket populates them.
	}
}

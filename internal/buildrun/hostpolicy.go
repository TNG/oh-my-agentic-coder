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
// CPU and process-count ceilings are not exposed: they are not wired to
// concrete host limits in v1, so the manifest cannot request them (only
// maxHeap and maxDuration are requestable — see ResourceRequests).
//
// The returned buildmanifest.HostPolicy is what the CLI passes to
// buildmanifest.Validate and buildmanifest.Gate.
func HostPolicy(maxDuration time.Duration) buildmanifest.HostPolicy {
	return buildmanifest.HostPolicy{
		MaxHeap:     defaultMaxHeap,
		MaxDuration: maxDuration,
	}
}

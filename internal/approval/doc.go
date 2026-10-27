// Package approval holds the pure, I/O-free rules for sidecar endpoint
// approvals: the match-pattern syntax of the omac.yaml `approvals` block, the
// most-specific-wins matcher, and the request-path normalization the facade
// applies before it matches and forwards.
//
// It imports nothing from omac, so internal/config can validate a declaration
// with it and internal/facade can enforce one without an import cycle.
package approval

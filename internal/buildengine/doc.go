// Package buildengine owns one complete build or stop invocation behind a
// transport-independent function.
//
// It absorbs the orchestration that previously lived in internal/cli's
// build commands: manifest gating (digest verification against an
// immutable approved-policy snapshot), cache-leaf preparation, proxy
// startup, grants derivation, per-leaf locking, restricted-executor
// launch, staged cancellation, post-build daemon recycle, and cleanup.
// Both brokered requests (the future internal/buildbroker, mounted on
// the start/serve parent's loopback control plane) and direct
// host-terminal invocation call through one engine function.
//
// The engine accepts a canonical worktree, an immutable approved-policy
// snapshot, the raw arguments after `omac build`, stdout/stderr writers,
// and graceful/forced cancellation signals. It reloads the manifest only
// to verify its digest still matches the snapshot, reparses the raw
// arguments with the existing command parser (internal/buildrun), and
// returns an explicit ResultClass plus exit code. The result class is
// assigned where the outcome occurs; callers never infer it from a
// numeric code.
//
// A narrow SnapshotProvider seam has two adapters: a parent-owned
// snapshot for an authorized worktree (the broker path) and an
// invocation-scoped snapshot resolved from the durable approval record
// (the direct host path). The engine cannot write approvals or replace
// snapshots.
//
// The interface exposes no proxy constructors, daemon endpoints,
// credential values, cache paths, sandbox grants, or host-policy
// internals — a concrete function plus an options value, not a
// speculative exported interface hierarchy. Tests inject only the
// dependencies that actually vary (snapshot provider, cancellation
// signals, stdout/stderr writers, and the proxy starter seams).
//
// This gate (ticket 04) is a behavior-preserving prefactor: no ordering,
// exit-code, lock-location, or direct-host-semantics change. Every
// existing internal/buildrun and internal/cli/build_integration_test.go
// test stays green against the extracted engine.
package buildengine

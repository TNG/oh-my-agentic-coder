package cli

import (
	"fmt"
	"io"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildcontrol"
)

// reconcileDaemonOwnership is the parent-startup reconciliation step
// for ticket 07 (spec.md §239: "At parent startup, pending and active
// records are reconciled before accepting builds"). The parent (`omac
// start` / `omac serve`) calls this BEFORE mounting the build broker
// so a parent that crashed between daemon creation and ownership
// registration does not leave stale records that the next build trips
// over (the parent-crash window the pending-to-active handshake
// closes — see buildcontrol/reconcile.go for the per-record policy).
//
// cacheRoot is the shared cache root (parent of cache-scope dirs) under
// which the host-only build-control root lives; empty (no cache scope
// prepared → no host-only build-control root) makes reconciliation a
// no-op (there are no records to reconcile and no path to write them).
//
// stderr receives a one-line warning when reconciliation fails.
// Reconciliation is BEST-EFFORT at startup: a failure (e.g. the
// daemons/ directory cannot be read) leaves stale records that the
// build-time handshake or the next startup will catch — so the parent
// does NOT abort startup on a reconciliation error. This is the
// documented fail-soft-at-startup / fail-closed-at-build-time split:
// the build-time handshake fails closed (a build that cannot establish
// ownership does not start), but startup reconciliation is advisory.
//
// The production verifier (procidentity.Verify) is wired via the
// buildcontrol package-level daemonVerify seam (Phase 1). This helper
// does NOT take a verifier parameter: the seam is package-internal so
// tests swap it directly inside the buildcontrol package, and the cli
// layer always uses the production path.
func reconcileDaemonOwnership(cacheRoot string, stderr io.Writer) {
	if cacheRoot == "" {
		// No cache scope prepared → no host-only build-control root →
		// nothing to reconcile. The broker will fail closed for a
		// brokered build that needs the cache root (the gate guard in
		// brokerEngineInvoker), but a parent with no cache scope is a
		// legitimate configuration (e.g. `--no-sandbox`).
		return
	}
	if err := buildcontrol.ReconcileDaemonRecords(cacheRoot); err != nil {
		// Fail-soft: log and continue. The parent must still accept
		// builds; the build-time handshake and the next startup
		// reconciliation will catch any stale records the sweep missed.
		fmt.Fprintf(stderr, "omac: warning: daemon ownership reconciliation failed (continuing): %v\n", err)
	}
}

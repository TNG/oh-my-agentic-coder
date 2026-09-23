package ephemeraldocker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// EndpointPollInterval is the pause between endpoint polls while waiting
// for the guest docker daemon.
const EndpointPollInterval = 2 * time.Second

// EndpointState is the terminal state of the endpoint wait machine.
// Waiting is the initial state; the machine never returns it.
type EndpointState int

const (
	EndpointWaiting EndpointState = iota
	// EndpointReady: the guest docker endpoint answered over the host
	// forward. This — not the limactl exit code — is the authoritative
	// boot success criterion (spec, protob precedent).
	EndpointReady
	// EndpointTimedOut: the deadline expired without the endpoint ever
	// answering. The machine is terminal: no further probes run.
	EndpointTimedOut
)

// EndpointProbe reports whether the guest docker endpoint answers on the
// host forward. Injected in tests; production uses the HTTP probe.
type EndpointProbe func(ctx context.Context, hostPort int) bool

// DefaultEndpointProbe issues GET http://127.0.0.1:<P>/version with a
// short per-attempt timeout. Any HTTP response counts as up — the daemon
// is listening; connection errors do not.
func DefaultEndpointProbe(ctx context.Context, hostPort int) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/version", hostPort), nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return true
}

// WaitForEndpoint polls the probe until the endpoint answers (Ready) or
// the context deadline hits (TimedOut). A cancelled/expired context is
// checked before every probe, so a timed-out machine never probes again.
func WaitForEndpoint(ctx context.Context, hostPort int, interval time.Duration, probe EndpointProbe) EndpointState {
	if probe == nil {
		probe = DefaultEndpointProbe
	}
	if interval <= 0 {
		interval = EndpointPollInterval
	}
	for {
		if ctx.Err() != nil {
			return EndpointTimedOut
		}
		if probe(ctx, hostPort) {
			return EndpointReady
		}
		select {
		case <-ctx.Done():
			return EndpointTimedOut
		case <-time.After(interval):
		}
	}
}

// VerifyFirewall proves the omac-vmguard table is loaded in the guest.
// The provision step already fails the whole boot when nft cannot load
// the ruleset; this is the host-side backstop. `nft list table` exits
// non-zero when the table is missing, and limactl shell propagates the
// guest exit code — both a failed shell call and a missing table land
// here as errors, and the session must not start in either case.
func VerifyFirewall(run LimaCtl, limaHome, vmName string) error {
	if err := run(limaHome, "shell", vmName, "--", "nft", "list", "table", "inet", "omac-vmguard"); err != nil {
		return fmt.Errorf("ephemeral-docker: firewall verification failed for %s: %w", vmName, err)
	}
	return nil
}

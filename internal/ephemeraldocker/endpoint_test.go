package ephemeraldocker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWaitForEndpointReadyOnFirstHit(t *testing.T) {
	calls := 0
	probe := func(context.Context, int) bool { calls++; return true }
	state := WaitForEndpoint(context.Background(), 12345, 0, probe)
	if state != EndpointReady {
		t.Errorf("state = %v, want EndpointReady", state)
	}
	if calls != 1 {
		t.Errorf("probe calls = %d, want 1", calls)
	}
}

func TestWaitForEndpointTimesOutWithoutHit(t *testing.T) {
	interval := 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 3*interval)
	defer cancel()
	calls := 0
	probe := func(context.Context, int) bool { calls++; return false }
	state := WaitForEndpoint(ctx, 42424, interval, probe)
	if state != EndpointTimedOut {
		t.Errorf("state = %v, want EndpointTimedOut", state)
	}
	if calls == 0 {
		t.Error("the machine must poll before the deadline hits")
	}
}

func TestWaitForEndpointNeverProbesAfterTimeout(t *testing.T) {
	// A context that is already cancelled must not run a single probe:
	// after TimedOut the machine is terminal.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	probe := func(context.Context, int) bool { calls++; return true }
	state := WaitForEndpoint(ctx, 42424, time.Millisecond, probe)
	if state != EndpointTimedOut {
		t.Errorf("state = %v, want EndpointTimedOut", state)
	}
	if calls != 0 {
		t.Errorf("probe calls = %d, want 0 (TimedOut is terminal)", calls)
	}
}

func TestWaitForEndpointRetriesUntilHit(t *testing.T) {
	interval := 2 * time.Millisecond
	calls := 0
	// Answer on the third poll.
	probe := func(context.Context, int) bool {
		calls++
		return calls >= 3
	}
	state := WaitForEndpoint(context.Background(), 42424, interval, probe)
	if state != EndpointReady {
		t.Fatalf("state = %v, want EndpointReady", state)
	}
	if calls != 3 {
		t.Errorf("probe calls = %d, want 3", calls)
	}
}

func TestVerifyFirewallSuccess(t *testing.T) {
	var got limaCall
	run := func(home string, args ...string) error {
		got = limaCall{home: home, args: append([]string(nil), args...)}
		return nil
	}
	if err := VerifyFirewall(run, "/tmp/alias", "omac-eph-abcd1234"); err != nil {
		t.Fatalf("VerifyFirewall: %v", err)
	}
	want := []string{"shell", "omac-eph-abcd1234", "--", "sudo", "nft", "list", "table", "inet", "omac-vmguard"}
	if strings.Join(got.args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", got.args, want)
	}
	if got.home != "/tmp/alias" {
		t.Errorf("LIMA_HOME = %q, want the unit alias", got.home)
	}
}

func TestVerifyFirewallFailureBlocksSession(t *testing.T) {
	run := func(string, ...string) error { return errors.New("limactl shell failed") }
	err := VerifyFirewall(run, "/tmp/alias", "omac-eph-abcd1234")
	if err == nil {
		t.Fatal("a failed shell call must block the session")
	}
	if !strings.Contains(err.Error(), "omac-eph-abcd1234") {
		t.Errorf("error must name the VM: %v", err)
	}
}

func TestVerifyFirewallMissingTableBlocksSession(t *testing.T) {
	// `nft list table` exits non-zero when the table does not exist;
	// limactl shell propagates the guest exit code. From the host's
	// perspective this is the same failure path: the session must not
	// start without a demonstrably loaded vmguard table.
	run := func(string, ...string) error { return errors.New(`exit status 1: "Error: No such file or directory"`) }
	if err := VerifyFirewall(run, "/tmp/alias", "omac-eph-abcd1234"); err == nil {
		t.Fatal("a missing vmguard table must block the session")
	}
}

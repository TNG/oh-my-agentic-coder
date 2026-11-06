//go:build linux

package sandboxrun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The kernel's Landlock net-port rule (ABI 4+) scopes a TCP connect
// grant to a port number; it has no address dimension. Every launch
// that uses Landlock networking grants the supervisor proxy's port for
// connect (plus any profile open_port values), so that grant holds for
// every host address, not only the loopback interface the proxy listens
// on. This residual reach is an accepted, documented risk
// (docs/security.md; the LOW address-blind notice from profileaudit),
// not a code defect: the rule engine cannot express anything tighter
// today.
//
// This test pins the kernel-level behaviour so the documentation can
// never silently drift from the enforcement: while the limitation
// exists, the granted port is reachable on a second loopback address
// while an ungranted port is not. If the granted-port dial ever starts
// failing while the control dial is still denied, the kernel has grown
// address scoping: remove this pin together with the accepted-risk
// notes in docs/security.md and the profileaudit notice.

// pinProbeEnv marks a re-exec'd test-binary invocation that installs
// the Landlock ruleset and runs the probe dials. The ruleset is
// irreversible for the whole process, so it must be installed in a
// child, never in the test process itself.
const pinProbeEnv = "OMAC_LANDLOCK_PIN_PROBE"

// Probe parameters passed from parent to child.
const (
	pinAddrEnv    = "OMAC_LANDLOCK_PIN_ADDR"
	pinGrantedEnv = "OMAC_LANDLOCK_PIN_GRANTED_PORT"
	pinOtherEnv   = "OMAC_LANDLOCK_PIN_OTHER_PORT"
)

// TestSecurityLandlockPortGrantAddressBlindnessPinned pins the
// port-scoped (address-less) shape of a Landlock connect grant.
func TestSecurityLandlockPortGrantAddressBlindnessPinned(t *testing.T) {
	if os.Getenv(pinProbeEnv) == "1" {
		os.Exit(landlockPinProbe())
	}
	if !LandlockNetSupported() {
		t.Skipf("Landlock ABI %d < 4", LandlockABI())
	}

	// A second loopback address: the probe address must differ from the
	// primary one the proxy itself would listen on, or the dial could
	// not tell a port-scoped grant from an address-scoped one.
	const pinAddr = "127.0.0.2"
	granted, err := net.Listen("tcp", pinAddr+":0")
	if err != nil {
		t.Skipf("cannot listen on %s: %v", pinAddr, err)
	}
	defer granted.Close()
	other, err := net.Listen("tcp", pinAddr+":0")
	if err != nil {
		t.Skipf("cannot listen on %s: %v", pinAddr, err)
	}
	defer other.Close()
	for _, ln := range []net.Listener{granted, other} {
		go func(l net.Listener) {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}(ln)
	}

	testBin, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, testBin,
		"-test.run=^TestSecurityLandlockPortGrantAddressBlindnessPinned$", "-test.v")
	cmd.Env = append(os.Environ(),
		pinProbeEnv+"=1",
		pinAddrEnv+"="+pinAddr,
		pinGrantedEnv+"="+strconv.Itoa(granted.Addr().(*net.TCPAddr).Port),
		pinOtherEnv+"="+strconv.Itoa(other.Addr().(*net.TCPAddr).Port),
	)
	out, err := cmd.CombinedOutput()
	text := string(out)
	t.Logf("probe output:\n%s", text)
	if err == nil {
		return
	}
	switch {
	case strings.Contains(text, "apply-unavailable"):
		// The ruleset could not be installed in this environment (for
		// example a container blocking landlock_restrict_self), so the
		// kernel behaviour cannot be probed here.
		t.Skipf("Landlock net rules unavailable in this environment:\n%s", text)
	case strings.Contains(text, "grant-denied"):
		t.Fatalf("the granted port was NOT reachable on %s while the control port was still denied:\n%s\n"+
			"The kernel now scopes connect grants by address. Remove this pin and revisit the accepted-risk record in docs/security.md and the profileaudit notice.", pinAddr, text)
	case strings.Contains(text, "control-allowed"):
		t.Fatalf("the ungranted control port was reachable, so the ruleset was not in force and the probe proves nothing:\n%s", text)
	default:
		t.Fatalf("probe failed: %v\n%s", err, text)
	}
}

// landlockPinProbe runs in the re-exec'd child: install a connect grant
// for one port only, then dial that port and an ungranted port on a
// second loopback address. The granted dial succeeding is the pin (the
// grant is port-scoped, not address-scoped); the ungranted dial being
// denied proves the ruleset is in force, so the pin is not vacuous.
// Results are reported via stdout markers; exit code 0 means the pinned
// behaviour was observed.
func landlockPinProbe() int {
	addr := os.Getenv(pinAddrEnv)
	granted, err1 := strconv.Atoi(os.Getenv(pinGrantedEnv))
	other, err2 := strconv.Atoi(os.Getenv(pinOtherEnv))
	if addr == "" || err1 != nil || err2 != nil || granted == 0 || other == 0 {
		fmt.Println("PIN-RESULT: bad-env")
		return 1
	}
	if err := ApplyLandlockNet([]int{granted}, nil); err != nil {
		fmt.Printf("PIN-RESULT: apply-unavailable: %v\n", err)
		return 1
	}
	dial := func(port int) error {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(addr, strconv.Itoa(port)), 5*time.Second)
		if err == nil {
			c.Close()
			return nil
		}
		return err
	}
	if err := dial(granted); err != nil {
		fmt.Printf("PIN-RESULT: grant-denied: %v\n", err)
		return 1
	}
	if err := dial(other); err == nil {
		fmt.Println("PIN-RESULT: control-allowed")
		return 1
	} else if !errors.Is(err, syscall.EACCES) && !errors.Is(err, os.ErrPermission) {
		fmt.Printf("PIN-RESULT: control-allowed: %v\n", err)
		return 1
	}
	fmt.Printf("PIN-RESULT: ok (granted port %d reachable on %s, ungranted port %d denied)\n", granted, addr, other)
	return 0
}

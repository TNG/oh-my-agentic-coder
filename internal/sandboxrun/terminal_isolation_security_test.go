package sandboxrun

import (
	"slices"
	"testing"
)

// The controlling terminal.
//
// The sandbox is launched from the user's terminal and the child keeps it: the
// same controlling terminal, the same session. On Linux a process holding a
// terminal open can push characters into that terminal's input queue with the
// TIOCSTI ioctl, and the shell on the other side reads them as if the user had
// typed them. That is not an escalation of some other flaw — it is a complete
// escape on its own, from confined agent to arbitrary commands run as the user
// outside every boundary omac establishes. Filesystem grants, the network
// proxy and the approval store are all bypassed, because none of them is in
// the path of a command the user's own shell executes.
//
// The kernel can close the vector via dev.tty.legacy_tiocsti, which defaults
// to 0 from Linux 6.2, but that does not hold on older kernels. Debian 12
// ships 6.1, Ubuntu 22.04 ships 5.15, RHEL 9 ships 5.14, all supported for
// years yet and all without the sysctl. The sysctl can also be set to 1, and
// a container inherits whatever the host decided. So omac detaches the child
// from the terminal session with bwrap --new-session (setsid(2)).
//
// Detaching costs the child kernel-delivered SIGWINCH, so omac keeps the
// terminal foreground, receives SIGWINCH itself and relays it into the
// detached session (sandbox.ExecDetachedSession, covered by
// TestIntegrationResizeReachesSandboxedTUI). omac also refuses to stop on
// SIGTSTP: confined code can remap a key to send it, and a stopped omac would
// let the shell take the terminal back while the sandbox still reads it.

// TestSecuritySandboxDoesNotShareControllingTerminal asserts that the sandbox
// child leaves the terminal the user launched it from.
func TestSecuritySandboxDoesNotShareControllingTerminal(t *testing.T) {
	argv, err := BuildBwrapArgv(bwrapGrants(), []string{"/omac", "sandbox", "stage2", "--", "inner"})
	if err != nil {
		t.Fatalf("BuildBwrapArgv: %v", err)
	}

	// Control: the argv is the real thing and carries the other isolation
	// flags. Without it an empty or malformed argv would satisfy the
	// assertion below by containing nothing at all.
	for _, flag := range []string{"--unshare-pid", "--unshare-ipc", "--die-with-parent"} {
		if !slices.Contains(argv, flag) {
			t.Fatalf("argv is missing %s: the fixture is broken, not the security property", flag)
		}
	}

	if !slices.Contains(argv, "--new-session") {
		t.Error("the sandbox child keeps the terminal it was launched from: on any kernel without the dev.tty.legacy_tiocsti gate — 6.1 and earlier, which is what current long-term-support distributions ship — confined code can type commands into the user's shell and run them outside the sandbox entirely")
	}
}

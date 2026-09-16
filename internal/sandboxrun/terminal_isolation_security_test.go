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
// omac does not detach the terminal, and the reason is a real one: setsid(2)
// costs the child SIGWINCH, so a TUI inside the sandbox renders once and never
// reflows on resize. The judgement attached to it is that the kernel closes
// the vector anyway, via dev.tty.legacy_tiocsti, which defaults to 0 from
// Linux 6.2.
//
// That holds on a current kernel and not otherwise. Debian 12 ships 6.1,
// Ubuntu 22.04 ships 5.15, RHEL 9 ships 5.14 — all supported for years yet,
// all without the sysctl, and all ordinary places to run a coding agent. The
// sysctl is also a sysctl: it can be set to 1, and a container inherits
// whatever the host decided. So the mitigation is real but conditional, and
// omac neither tests the condition nor says anything when it does not hold. A
// broken terminal resize is a visible annoyance; a silent escape on a
// long-term-support kernel is not, and the two are not the same kind of cost.
//
// Detaching is not one fix among several. Recovering resize means giving the
// child its own pseudo-terminal and forwarding window changes to it, and a
// process only becomes the session leader of a new terminal by leaving the old
// session — which for a bwrap child is --new-session either way. The flag is
// the floor, not the ceiling: asserting it does not rule out the pty-proxying
// version, it is a component of it.

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
